package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// NewWorktreeFileStore shares immutable objects and branch refs, but never HEAD.
// The key is Git's worktree admin directory, so moving a worktree keeps its key.
func NewWorktreeFileStore(repoRoot, gitDir, branch, commit string) *FileStore {
	s := NewFileStore(repoRoot)
	if gitDir != "" {
		hash := sha256.Sum256([]byte(gitDir))
		s.worktreeID = fmt.Sprintf("%x", hash[:16])
		s.gitBranch = branch
		s.gitCommit = commit
	}
	return s
}

func (s *FileStore) positionPath() string {
	return filepath.Join(s.storeDir(), "worktrees", s.worktreeID, "position.json")
}
func (s *FileStore) readPosition() (domain.WorkingPosition, error) {
	if s.worktreeID == "" {
		return domain.WorkingPosition{}, domain.ErrNotFound
	}
	raw, err := readCxtFile(s.positionPath())
	if os.IsNotExist(err) {
		return domain.WorkingPosition{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.WorkingPosition{}, err
	}
	var p domain.WorkingPosition
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if p.WorktreeID != s.worktreeID {
		return p, domain.ErrHashMismatch
	}
	if p.BranchID == "" {
		p.BranchID = domain.LegacyContextBranchID(p.RepoID, p.Branch)
		if p.Selection != nil && p.Selection.BranchID != "position" && p.Selection.BranchID != "continuation" {
			p.BranchID = p.Selection.BranchID
		}
	}
	for _, h := range []domain.ContentHash{p.Snapshot, p.SharedTarget, p.MemoryHash, p.MemorySource} {
		if err := domain.ValidateOptionalContentHash(h); err != nil {
			return p, err
		}
	}
	return p, nil
}
func (s *FileStore) GetWorkingPosition(ctx context.Context) (domain.WorkingPosition, error) {
	if s.worktreeID == "" {
		return domain.WorkingPosition{}, domain.ErrNotFound
	}
	var p domain.WorkingPosition
	err := s.withRefMutationLock(ctx, func() error { var err error; p, err = s.readPosition(); return err })
	return p, err
}
func (s *FileStore) PutWorkingPosition(ctx context.Context, p domain.WorkingPosition) error {
	if s.worktreeID == "" {
		return domain.ErrNotFound
	}
	return s.withRefMutationLock(ctx, func() error { return s.writePosition(p) })
}
func (s *FileStore) writePosition(p domain.WorkingPosition) error {
	p.WorktreeID = s.worktreeID
	if p.BranchID == "" {
		events, err := s.listHistoryEvents(p.RepoID)
		if err != nil {
			return err
		}
		branches, err := domain.ProjectContextBranches(events)
		if err != nil {
			return err
		}
		p.BranchID = branches.Identity(p.RepoID, p.Branch)
	}
	for _, h := range []domain.ContentHash{p.Snapshot, p.SharedTarget, p.MemoryHash, p.MemorySource} {
		if err := domain.ValidateOptionalContentHash(h); err != nil {
			return err
		}
	}
	if p.Selection == nil && p.Snapshot != "" {
		// The code/context/memory tuple is observed together at capture time.
		// A deterministic key lets a journal replay reuse its first timestamp.
		key := sha256.Sum256([]byte(p.RepoID + "\x00" + p.WorktreeID + "\x00" + p.BranchID + "\x00" + p.GitBranch() + "\x00" + p.Branch + "\x00" + p.GitCommit + "\x00" + string(p.Snapshot) + "\x00" + string(p.MemoryHash)))
		e := domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: p.RepoID, BranchID: p.BranchID, Branch: p.Branch, LocalBranch: p.LocalBranch, Kind: "position", Source: p.Snapshot, Target: p.Snapshot, MemoryHash: p.MemoryHash, MemorySource: p.MemorySource, MemoryPinned: true, GitAfter: p.GitCommit, WorktreeID: p.WorktreeID, CreatedAt: time.Now().UTC()}
		raw, err := readCxtFile(filepath.Join(s.storeDir(), "history", e.ID+".json"))
		if err == nil {
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if old, err := s.readPosition(); err == nil && old.Selection != nil && old.Selection.ID == e.ID {
			e = *old.Selection
		}
		p.Selection = &e
		p.MemoryPinned = true
	}
	if old, err := s.readPosition(); err == nil {
		if old.Selection != nil {
			if err := s.putHistoryEvent(*old.Selection); err != nil {
				return err
			}
		}
	} else if err != domain.ErrNotFound {
		return err
	}
	if p.Selection != nil {
		e := *p.Selection
		e.WorktreeID = s.worktreeID
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return err
		}
		if e.RepoID != p.RepoID || e.Target != p.Snapshot {
			return domain.ErrHashMismatch
		}
		if err := s.retainHistoryRoots(e); err != nil {
			return err
		}
		p.Selection = &e
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	// Keep the legacy opt-in sentinel for older clients and hook detection.
	if _, err := readCxtFile(filepath.Join(s.storeDir(), "HEAD")); os.IsNotExist(err) {
		branch := p.Branch
		if branch == "" {
			branch = "main"
		}
		if err = writeAtomic(filepath.Join(s.storeDir(), "HEAD"), []byte("ref: refs/heads/"+branch+"\n")); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return writeAtomic(s.positionPath(), raw)
}

// Called after a causal attachment CAS (including a retry/no-op). Updating a
// memory object must not silently repin a historical or another worktree's view.
func (s *FileStore) RecordWorkingMemory(ctx context.Context, snapshot, memory domain.ContentHash) error {
	if s.worktreeID == "" {
		return nil
	}
	return s.withRefMutationLock(ctx, func() error {
		p, err := s.readPosition()
		if err == domain.ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if p.Rewound || p.Snapshot != snapshot || p.GitBranch() != s.gitBranch || p.GitCommit != s.gitCommit || (p.MemoryHash == memory && p.MemoryPinned && p.Selection != nil) {
			return nil
		}
		digest, err := s.GetMemory(ctx, memory)
		if err != nil {
			return err
		}
		if digest.SnapshotID != snapshot {
			return domain.ErrHashMismatch
		}
		p.MemoryHash = memory
		p.MemoryPinned = true
		p.Selection = nil
		return s.writePosition(p)
	})
}
func (s *FileStore) writeWorkingHead(ref domain.Ref) error {
	branch := strings.TrimPrefix(ref.Symbolic, "refs/heads/")
	target := ref.Target
	if branch != "" {
		if head, err := s.getRefRaw(context.Background(), ref.RepoID, domain.RefBranch, branch); err == nil {
			target = head.Target
		} else if err != domain.ErrNotFound {
			return err
		}
	}
	if branch == "" {
		branch = s.gitBranch
	}
	p := domain.WorkingPosition{RepoID: ref.RepoID, Branch: branch, Snapshot: target, SharedTarget: target, GitCommit: s.gitCommit}
	if s.gitBranch != "" && s.gitBranch != branch {
		p.LocalBranch = s.gitBranch
	}
	if target != "" {
		snap, err := s.GetSnapshot(context.Background(), target)
		if err != nil {
			return err
		}
		p.MemoryHash = snap.MemoryHash
		if p.RepoID == "" {
			p.RepoID = snap.RepoID
		}
	}
	return s.writePosition(p)
}
func (s *FileStore) readWorkingHead(repoID string) (domain.Ref, error) {
	p, err := s.readPosition()
	if err == nil {
		if repoID != "" && p.RepoID != "" && repoID != p.RepoID {
			// init can precede the first server remote binding. Only an empty
			// replica may cross that bootstrap identity boundary.
			if p.Snapshot != "" || p.MemoryHash != "" || p.MemorySource != "" || p.Selection != nil {
				return domain.Ref{}, domain.ErrHashMismatch
			}
			all, err := s.ListSnapshots(context.Background(), "", "")
			if err != nil {
				return domain.Ref{}, err
			}
			history, err := s.listHistoryEvents("")
			if err != nil {
				return domain.Ref{}, err
			}
			// A first pull may already have staged the new server's objects.
			// Only evidence owned by the previous (or another) identity makes
			// this a conflicting rebind; imported new-repo data is not loss.
			for _, snap := range all {
				if snap.RepoID != repoID {
					return domain.Ref{}, domain.ErrHashMismatch
				}
			}
			for _, event := range history {
				if event.RepoID != repoID {
					return domain.Ref{}, domain.ErrHashMismatch
				}
			}
		}
		// A new Git checkout must never inherit the other branch's working cursor.
		if p.GitBranch() == s.gitBranch {
			if p.Orphan && p.Snapshot == "" {
				return domain.Ref{}, domain.ErrNotFound
			}
			if p.Snapshot != "" {
				return domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repoID, Target: p.Snapshot}, nil
			}
			return domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repoID, Symbolic: p.Branch}, nil
		}
	} else if err != domain.ErrNotFound {
		return domain.Ref{}, err
	}
	if s.gitBranch != "" {
		binding, err := s.ResolveLocalBranch(context.Background(), repoID, s.gitBranch)
		if err != nil {
			return domain.Ref{}, err
		}
		return domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repoID, Symbolic: binding.Branch}, nil
	}
	return domain.Ref{}, domain.ErrNotFound
}
