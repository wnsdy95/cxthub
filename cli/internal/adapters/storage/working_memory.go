package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// One redo covers the snapshot attachment and its optional worktree pin. Ref
// exclusion precedes snapshot exclusion. The journal owns its worktree, not the
// process which happens to recover it. No shared branch ref is advanced.
type workingMemory struct {
	Version int                          `json:"version"`
	Commit  outbound.WorkingMemoryCommit `json:"commit"`
	Next    *domain.WorkingPosition      `json:"next,omitempty"`
}

func (s *FileStore) workingMemoryPath() string {
	return filepath.Join(s.storeDir(), "working-memory.json")
}

func memoryPosition(c outbound.WorkingMemoryCommit, predecessor *domain.HistoryEvent, at time.Time) domain.WorkingPosition {
	p := *c.ExpectedPosition
	raw, _ := json.Marshal(c)
	key := sha256.Sum256(append([]byte("working-memory/v1\x00"), raw...))
	e := domain.HistoryEvent{ID: fmt.Sprintf("%x", key[:16]), RepoID: p.RepoID, BranchID: p.BranchID, Branch: p.Branch, LocalBranch: p.LocalBranch,
		Kind: "position", Source: c.Snapshot, Target: c.Snapshot, MemoryHash: c.Memory, MemoryPinned: true, GitAfter: p.GitCommit, WorktreeID: p.WorktreeID, CreatedAt: at}
	if predecessor != nil {
		e.MemorySelectionParent = predecessor.ID
		if !domain.IsInitialMemorySelection(*predecessor, e) {
			e.MemorySelectionParent = ""
		}
	}
	p.MemoryHash, p.MemorySource, p.MemoryPinned, p.Selection = c.Memory, "", true, &e
	return p
}

// A staged publication is not itself a memory decision. Its deterministic
// ordinary witness must already exist with the entire frozen payload, including
// memory fields. Keep the original publish in the local CAS expectation.
func (s *FileStore) workingMemoryPredecessor(c outbound.WorkingMemoryCommit, digest domain.MemoryDigest) (*domain.HistoryEvent, error) {
	p := c.ExpectedPosition
	if p == nil || p.Selection == nil || digest.PreviousMemoryHash != "" {
		return nil, nil
	}
	if p.Selection.Kind != "publish" {
		return p.Selection, nil
	}
	observations := domain.StagingObservations(domain.StagingCommit{Version: domain.StagingCommitVersion, Position: *p})
	want := observations[0]
	raw, err := readCxtFile(filepath.Join(s.storeDir(), "history", want.ID+".json"))
	if err != nil {
		return nil, fmt.Errorf("staged memory selection witness: %w", err)
	}
	var actual domain.HistoryEvent
	if json.Unmarshal(raw, &actual) != nil || !reflect.DeepEqual(want, actual) {
		return nil, domain.ErrHashMismatch
	}
	return &actual, nil
}

func (s *FileStore) prepareWorkingMemory(ctx context.Context, c outbound.WorkingMemoryCommit) (workingMemory, error) {
	j := workingMemory{Version: 1, Commit: c}
	p := c.ExpectedPosition
	if p != nil {
		if p.WorktreeID != s.worktreeID {
			return j, domain.ErrSelectionChanged
		}
		if p.Snapshot == c.Snapshot && !p.Rewound && p.GitBranch() == s.gitBranch && p.GitCommit == s.gitCommit && !(p.MemoryPinned && p.MemoryHash == c.Memory && p.MemorySource == "" && p.Selection != nil) {
			digest, err := s.GetMemory(ctx, c.Memory)
			if err != nil {
				return j, err
			}
			predecessor, err := s.workingMemoryPredecessor(c, digest)
			if err != nil {
				return j, err
			}
			makeNext := func(at time.Time) domain.WorkingPosition {
				next := memoryPosition(c, predecessor, at)
				if digest.PreviousMemoryHash != "" {
					next.Selection.MemorySelectionParent = ""
				}
				return next
			}
			next := makeNext(time.Now().UTC())
			// Reuse the exact accepted event after a lost success response.
			raw, err := readCxtFile(filepath.Join(s.storeDir(), "history", next.Selection.ID+".json"))
			if err == nil {
				var prior domain.HistoryEvent
				if json.Unmarshal(raw, &prior) != nil {
					return j, domain.ErrHashMismatch
				}
				next = makeNext(prior.CreatedAt)
				if !reflect.DeepEqual(prior, *next.Selection) {
					return j, domain.ErrHashMismatch
				}
			} else if !os.IsNotExist(err) {
				return j, err
			}
			j.Next = &next
		}
	}
	return j, s.validateWorkingMemory(ctx, j)
}

func (s *FileStore) CommitWorkingMemory(ctx context.Context, c outbound.WorkingMemoryCommit) error {
	// Copy nested event payloads before acceptance; callers cannot mutate redo.
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var frozen outbound.WorkingMemoryCommit
	if err = json.Unmarshal(raw, &frozen); err != nil {
		return err
	}
	c = frozen
	return s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			return s.withSnapshotMutationLock(ctx, c.Snapshot, func() error {
				j, err := s.prepareWorkingMemory(ctx, c)
				if err != nil {
					return err
				}
				if err = s.checkWorkingMemoryCurrent(ctx, j); err != nil {
					return err
				}
				raw, err := json.Marshal(j)
				if err != nil {
					return err
				}
				if err = writeAtomic(s.workingMemoryPath(), raw); err != nil {
					return err
				}
				return s.applyWorkingMemory(ctx, j)
			})
		})
	})
}

func (s *FileStore) readWorkingMemory() (workingMemory, error) {
	var j workingMemory
	raw, err := readCxtFile(s.workingMemoryPath())
	if err != nil {
		return j, err
	}
	if err = json.Unmarshal(raw, &j); err != nil {
		return j, err
	}
	return j, nil
}

func (s *FileStore) validateWorkingMemory(ctx context.Context, j workingMemory) error {
	c := j.Commit
	if j.Version != 1 || c.RepoID == "" {
		return domain.ErrHashMismatch
	}
	if err := validateHashes(c.Snapshot, c.Memory); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(c.ExpectedMemory); err != nil {
		return err
	}
	snap, err := s.GetSnapshot(ctx, c.Snapshot)
	if err != nil {
		return err
	}
	if snap.RepoID != c.RepoID {
		return domain.ErrHashMismatch
	}
	if _, err = s.GetDoc(ctx, snap.DocHash); err != nil {
		return err
	}
	next, err := s.GetMemory(ctx, c.Memory)
	if err != nil {
		return err
	}
	if next.SnapshotID != c.Snapshot || (c.Memory != c.ExpectedMemory && next.PreviousMemoryHash != c.ExpectedMemory) {
		return domain.ErrHashMismatch
	}
	if c.ExpectedMemory != "" {
		old, err := s.GetMemory(ctx, c.ExpectedMemory)
		if err != nil {
			return err
		}
		if old.SnapshotID != c.Snapshot {
			return domain.ErrHashMismatch
		}
	}
	p := c.ExpectedPosition
	if p == nil {
		if j.Next != nil {
			return domain.ErrHashMismatch
		}
		return nil
	}
	if p.RepoID != c.RepoID || len(p.WorktreeID) != 32 || strings.ToLower(p.WorktreeID) != p.WorktreeID {
		return domain.ErrHashMismatch
	}
	if _, err := hex.DecodeString(p.WorktreeID); err != nil {
		return domain.ErrHashMismatch
	}
	for _, h := range []domain.ContentHash{p.Snapshot, p.SharedTarget, p.MemoryHash, p.MemorySource} {
		if err := domain.ValidateOptionalContentHash(h); err != nil {
			return err
		}
	}
	if p.Selection != nil {
		e := p.Selection
		if err := domain.ValidateHistoryEvent(*e); err != nil {
			return err
		}
		if e.WorktreeID != p.WorktreeID || e.RepoID != p.RepoID || e.BranchID != p.BranchID || e.Branch != p.Branch || e.LocalBranch != p.LocalBranch || e.GitAfter != p.GitCommit || e.Target != p.Snapshot || e.MemoryHash != p.MemoryHash || e.MemorySource != p.MemorySource || e.MemoryPinned != p.MemoryPinned {
			return domain.ErrHashMismatch
		}
	}
	if p.MemoryHash != "" {
		old, err := s.GetMemory(ctx, p.MemoryHash)
		if err != nil {
			return err
		}
		owner := p.MemorySource
		if owner == "" {
			owner = p.Snapshot
		}
		if old.SnapshotID != owner {
			return domain.ErrHashMismatch
		}
		source, err := s.GetSnapshot(ctx, owner)
		if err != nil {
			return err
		}
		if source.RepoID != c.RepoID {
			return domain.ErrHashMismatch
		}
		if j.Next != nil && owner == c.Snapshot {
			if err = s.memoryDescendsFrom(ctx, c.Snapshot, c.Memory, p.MemoryHash); err != nil {
				return err
			}
		}
	} else if p.MemorySource != "" {
		return domain.ErrHashMismatch
	}
	if j.Next != nil {
		if p.Rewound || p.Orphan || p.Snapshot != c.Snapshot || j.Next.Selection == nil {
			return domain.ErrHashMismatch
		}
		predecessor, err := s.workingMemoryPredecessor(c, next)
		if err != nil {
			return err
		}
		want := memoryPosition(c, predecessor, j.Next.Selection.CreatedAt)
		if next.PreviousMemoryHash != "" {
			want.Selection.MemorySelectionParent = ""
		}
		if !reflect.DeepEqual(want, *j.Next) {
			return domain.ErrHashMismatch
		}
		if want.Selection.MemorySelectionParent != "" && p.MemoryHash != "" {
			if err := s.memoryDescendsFrom(ctx, p.MemorySource, p.MemoryHash, ""); err != nil {
				return err
			}
		}
		if err := s.validateConditionalPosition(ctx, *j.Next); err != nil {
			return err
		}
	}
	// Check immutable IDs before acceptance as well as during replay.
	for _, position := range []*domain.WorkingPosition{p, j.Next} {
		if position == nil || position.Selection == nil {
			continue
		}
		e := position.Selection
		raw, err := readCxtFile(filepath.Join(s.storeDir(), "history", e.ID+".json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var old domain.HistoryEvent
		if json.Unmarshal(raw, &old) != nil || !reflect.DeepEqual(old, *e) {
			return domain.ErrHashMismatch
		}
	}
	return nil
}

func (s *FileStore) memoryDescendsFrom(ctx context.Context, owner, next, prior domain.ContentHash) error {
	seen := map[domain.ContentHash]bool{}
	for next != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[next] {
			return domain.ErrHashMismatch
		}
		seen[next] = true
		d, err := s.GetMemory(ctx, next)
		if err != nil {
			return err
		}
		if d.SnapshotID != owner {
			return domain.ErrHashMismatch
		}
		if next == prior {
			return nil
		}
		next = d.PreviousMemoryHash
	}
	if prior == "" {
		return nil
	}
	return domain.ErrSyncConflict
}

func (s *FileStore) checkWorkingMemoryCurrent(ctx context.Context, j workingMemory) error {
	c := j.Commit
	snap, err := s.GetSnapshot(ctx, c.Snapshot)
	if err != nil {
		return err
	}
	if snap.MemoryHash != c.ExpectedMemory && snap.MemoryHash != c.Memory {
		return domain.ErrSyncConflict
	}
	if c.ExpectedPosition != nil {
		owner := *s
		owner.worktreeID = c.ExpectedPosition.WorktreeID
		p, err := owner.readPosition()
		if err != nil {
			return fmt.Errorf("working memory selection unavailable: %w", err)
		}
		if !reflect.DeepEqual(p, *c.ExpectedPosition) && (j.Next == nil || !reflect.DeepEqual(p, *j.Next)) {
			return domain.ErrSelectionChanged
		}
	}
	return nil
}

// Called only after validation/current checks under both locks. Acceptance
// and recovery share these writes; acceptance need not reverify the same body.
func (s *FileStore) applyWorkingMemory(ctx context.Context, j workingMemory) error {
	if j.Next != nil {
		if p := j.Commit.ExpectedPosition; p.Selection != nil {
			if err := s.putHistoryEvent(*p.Selection); err != nil {
				return err
			}
		}
		if err := s.putHistoryEvent(*j.Next.Selection); err != nil {
			return err
		}
	}
	// Update only the attachment, preserving grafts/promotions imported meanwhile.
	snap, err := s.GetSnapshot(ctx, j.Commit.Snapshot)
	if err != nil {
		return err
	}
	snap.MemoryHash = j.Commit.Memory
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err = writeAtomic(s.objectPath("snapshots", snap.ID), raw); err != nil {
		return err
	}
	if j.Next != nil {
		owner := *s
		owner.worktreeID = j.Next.WorktreeID
		if err = owner.writePosition(*j.Next); err != nil {
			return err
		}
	}
	if err = os.Remove(s.workingMemoryPath()); err != nil {
		return err
	}
	return syncCxtParents(s.workingMemoryPath())
}

func (s *FileStore) recoverWorkingMemory(ctx context.Context) error {
	j, err := s.readWorkingMemory()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.withSnapshotMutationLock(ctx, j.Commit.Snapshot, func() error {
		if err := s.validateWorkingMemory(ctx, j); err != nil {
			return err
		}
		if err := s.checkWorkingMemoryCurrent(ctx, j); err != nil {
			return err
		}
		return s.applyWorkingMemory(ctx, j)
	})
}

func (s *FileStore) HasWorkingMemoryPin(ctx context.Context, id domain.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return false, err
	}
	j, err := s.readWorkingMemory()
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = s.validateWorkingMemory(ctx, j); err != nil {
		return false, err
	}
	if id == j.Commit.Snapshot {
		return true, nil
	}
	for _, p := range []*domain.WorkingPosition{j.Commit.ExpectedPosition, j.Next} {
		if p != nil && (id == p.Snapshot || id == p.SharedTarget || id == p.MemorySource) {
			return true, nil
		}
	}
	return false, nil
}

var _ outbound.WorkingMemoryStore = (*FileStore)(nil)
var _ outbound.WorkingMemoryPins = (*FileStore)(nil)
