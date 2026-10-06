package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.StagingStore = (*FileStore)(nil)
var _ outbound.StagingPins = (*FileStore)(nil)

func (s *FileStore) stagingPath() string {
	return filepath.Join(s.storeDir(), "worktrees", s.worktreeID, "index.json")
}
func (s *FileStore) readStagingIndex(repo string) (domain.StagingIndex, error) {
	if s.worktreeID == "" {
		return domain.StagingIndex{}, fmt.Errorf("staging requires a Git worktree identity")
	}
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return domain.StagingIndex{}, err
	}
	raw, err := readCxtFile(s.stagingPath())
	if os.IsNotExist(err) {
		return (domain.StagingIndex{Version: domain.StagingVersion, RepoID: repo, WorktreeID: s.worktreeID, Entries: []domain.StagedSession{}}).WithRevision(), nil
	}
	if err != nil {
		return domain.StagingIndex{}, err
	}
	var index domain.StagingIndex
	if err = json.Unmarshal(raw, &index); err != nil {
		return index, err
	}
	if err = domain.ValidateStagingIndex(index); err != nil {
		return index, err
	}
	if index.RepoID != repo || index.WorktreeID != s.worktreeID {
		return index, domain.ErrHashMismatch
	}
	return index, nil
}

// A missing position is represented without persisting an implicit selection.
func (s *FileStore) stagingPosition(ctx context.Context, repo string) (domain.WorkingPosition, error) {
	p, err := s.readPosition()
	if err == nil {
		if p.RepoID != repo {
			return p, domain.ErrHashMismatch
		}
		return p, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return p, err
	}
	branch := s.gitBranch
	if branch == "HEAD" {
		branch = ""
	}
	p = domain.WorkingPosition{RepoID: repo, WorktreeID: s.worktreeID, GitCommit: s.gitCommit, Branch: branch, BranchID: domain.LegacyContextBranchID(repo, branch)}
	if branch != "" {
		binding, err := s.ResolveLocalBranch(ctx, repo, branch)
		if err != nil {
			return p, err
		}
		if binding.Inactive {
			return p, domain.ErrBranchArchived
		}
		p.Branch = binding.Branch
		if p.Branch != branch {
			p.LocalBranch = branch
		}
		ref, err := s.getRefRaw(ctx, repo, domain.RefBranch, p.Branch)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return p, err
		}
		p.Snapshot, p.SharedTarget = ref.Target, ref.Target
		if ref.BranchID != "" {
			p.BranchID = ref.BranchID
		} else {
			p.BranchID = domain.LegacyContextBranchID(repo, p.Branch)
		}
	}
	return p, nil
}

func (s *FileStore) ReadStaging(ctx context.Context, repo string) (domain.StagingIndex, domain.WorkingPosition, error) {
	if err := ctx.Err(); err != nil {
		return domain.StagingIndex{}, domain.WorkingPosition{}, err
	}
	index, err := s.readStagingIndex(repo)
	if err != nil {
		return index, domain.WorkingPosition{}, err
	}
	p, err := s.stagingPosition(ctx, repo)
	return index, p, err
}

func (s *FileStore) writeStaging(index domain.StagingIndex) error {
	if err := domain.ValidateStagingIndex(index); err != nil {
		return err
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return writeAtomic(s.stagingPath(), raw)
}

func (s *FileStore) verifyStagedDocs(ctx context.Context, index domain.StagingIndex) error {
	for _, e := range index.Entries {
		doc, err := s.GetDoc(ctx, e.DocHash)
		if err != nil {
			return fmt.Errorf("staged document %s: %w", e.DocHash, err)
		}
		if doc.CIR.Envelope.SourceProvider != e.Provider || doc.CIR.Envelope.SessionOriginID != e.SessionID || len(doc.CIR.Events) != e.Events {
			return domain.ErrHashMismatch
		}
	}
	return nil
}

func (s *FileStore) CompareAndSwapStaging(ctx context.Context, expected domain.ContentHash, next domain.StagingIndex, position domain.WorkingPosition) error {
	if err := domain.ValidateStagingIndex(next); err != nil {
		return err
	}
	if next.WorktreeID != s.worktreeID || position.RepoID != next.RepoID || position.WorktreeID != s.worktreeID {
		return domain.ErrHashMismatch
	}
	return s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			if err := s.reconcileAppliedStaging(ctx, next.RepoID); err != nil {
				return err
			}
			current, err := s.readStagingIndex(next.RepoID)
			if err != nil {
				return err
			}
			if current.Revision != expected || next.Sequence != current.Sequence+1 {
				return domain.ErrSyncConflict
			}
			p, err := s.stagingPosition(ctx, next.RepoID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(p, position) {
				return domain.ErrSelectionChanged
			}
			if err := s.verifyStagedDocs(ctx, next); err != nil {
				return err
			}
			return s.writeStaging(next)
		})
	})
}

func (s *FileStore) stagingOperationPath(id string) (string, error) {
	if len(id) != 32 || id != strings.ToLower(id) {
		return "", domain.ErrHashMismatch
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", domain.ErrHashMismatch
	}
	return filepath.Join(s.storeDir(), "worktrees", s.worktreeID, "staging-commits", id+".json"), nil
}
func (s *FileStore) writeStagingOperation(op domain.StagingCommit) error {
	path, err := s.stagingOperationPath(op.ID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	return writeAtomic(path, raw)
}
func (s *FileStore) readStagingOperation(repo, id string) (domain.StagingCommit, error) {
	path, err := s.stagingOperationPath(id)
	if err != nil {
		return domain.StagingCommit{}, err
	}
	raw, err := readCxtFile(path)
	if os.IsNotExist(err) {
		return domain.StagingCommit{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.StagingCommit{}, err
	}
	var op domain.StagingCommit
	if err = json.Unmarshal(raw, &op); err != nil {
		return op, err
	}
	if err = s.validateStagingOperation(op); err != nil {
		return op, err
	}
	if op.ID != id || op.Index.RepoID != repo {
		return op, domain.ErrHashMismatch
	}
	return op, nil
}
func (s *FileStore) validateStagingOperation(op domain.StagingCommit) error {
	if op.Version != 1 && op.Version != domain.StagingCommitVersion {
		return domain.ErrStagingVersion
	}
	if _, err := s.stagingOperationPath(op.ID); err != nil {
		return err
	}
	if err := domain.ValidateStagingIndex(op.Index); err != nil {
		return err
	}
	if len(op.Index.Entries) == 0 || op.CreatedAt.IsZero() || op.Index.WorktreeID != s.worktreeID {
		return domain.ErrHashMismatch
	}
	p := op.Position
	before := op.ExpectedPosition
	if p.WorktreeID != s.worktreeID || before.WorktreeID != s.worktreeID || p.RepoID != op.Index.RepoID || before.RepoID != p.RepoID || p.Branch != before.Branch || p.BranchID != before.BranchID || p.GitCommit != before.GitCommit || p.GitBranch() != before.GitBranch() {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateStagingCode(p.GitCommit); err != nil {
		return err
	}
	if op.Ref.RepoID != p.RepoID || op.Ref.BranchID != p.BranchID || op.Ref.Target != p.Snapshot || op.Ref.Name != p.Branch || op.Ref.Kind != domain.RefBranch {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateRef(op.Ref); err != nil {
		return err
	}
	if op.ExpectedRef.RepoID != op.Ref.RepoID || op.ExpectedRef.Kind != op.Ref.Kind || op.ExpectedRef.Name != op.Ref.Name || op.ExpectedRef.BranchID != op.Ref.BranchID {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateOptionalContentHash(op.ExpectedRef.Target); err != nil {
		return err
	}
	if p.Selection == nil || p.Selection.ID != op.ID || p.Selection.Kind != "publish" || p.Selection.Target != p.Snapshot || p.Selection.MemoryHash != p.MemoryHash || p.Selection.MemorySource != p.MemorySource {
		return domain.ErrHashMismatch
	}
	if op.Advance != nil {
		journal := workingCommit{Ref: op.Ref, Expected: op.ExpectedRef.Target, Position: op.Position, Event: op.Advance}
		if err := validateWorkingCommit(journal); err != nil {
			return err
		}
	}
	allowed := map[domain.ContentHash]bool{p.Snapshot: true}
	for _, entry := range op.Index.Entries {
		allowed[entry.DocHash] = true
	}
	covered := map[domain.ContentHash]bool{p.Snapshot: true}
	publications := append(append([]domain.HistoryEvent{}, op.Publications...), *p.Selection)
	for _, e := range publications {
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return err
		}
		if !allowed[e.Target] {
			return domain.ErrHashMismatch
		}
		covered[e.Target] = true
		if e.Kind != "publish" || e.RepoID != p.RepoID || e.GitAfter != p.GitCommit || e.BranchID != p.BranchID || e.WorktreeID != s.worktreeID {
			return domain.ErrHashMismatch
		}
		if op.Version == domain.StagingCommitVersion && (!p.MemoryPinned || e.Branch != p.Branch || e.LocalBranch != p.LocalBranch || e.Source != e.Target || !e.MemoryPinned) {
			return domain.ErrHashMismatch
		}
	}
	for _, e := range op.Index.Entries {
		if !covered[e.DocHash] {
			return domain.ErrHashMismatch
		}
		if e.CodeCommit != p.GitCommit || e.BranchID != p.BranchID || e.Branch != p.Branch {
			return domain.ErrSelectionChanged
		}
	}
	return nil
}

// The existing working-commit journal remains the only ref/position publisher.
// The staging operation is durable before that journal, and its publish event
// proves application even if a later writer already moved the branch/position.
func (s *FileStore) FinalizeStagingCommit(ctx context.Context, op domain.StagingCommit) (domain.StagingCommit, error) {
	if err := s.validateStagingOperation(op); err != nil {
		return op, err
	}
	if op.LocalFinalized {
		return op, domain.ErrHashMismatch
	}
	err := s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			existing, err := s.readStagingOperation(op.Index.RepoID, op.ID)
			if err == nil {
				candidate := existing
				candidate.LocalFinalized = false
				if !reflect.DeepEqual(candidate, op) {
					return domain.ErrSyncConflict
				}
				op = existing
			} else if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			return s.finishStagingCommit(ctx, &op, true)
		})
	})
	return op, err
}

func (s *FileStore) stagingApplied(op domain.StagingCommit) (bool, error) {
	p, err := s.readPosition()
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return false, err
	}
	if err == nil && p.Selection != nil && p.Selection.ID == op.ID {
		if !reflect.DeepEqual(*p.Selection, *op.Position.Selection) {
			return false, domain.ErrHashMismatch
		}
		return true, nil
	}
	raw, err := readCxtFile(filepath.Join(s.storeDir(), "history", op.ID+".json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var e domain.HistoryEvent
	if json.Unmarshal(raw, &e) != nil || !reflect.DeepEqual(e, *op.Position.Selection) {
		return false, domain.ErrHashMismatch
	}
	return true, nil
}

func (s *FileStore) finishStagingCommit(ctx context.Context, op *domain.StagingCommit, apply bool) error {
	if op.LocalFinalized {
		return nil
	}
	applied, err := s.stagingApplied(*op)
	if err != nil {
		return err
	}
	if !applied {
		if !apply {
			return nil
		}
		if err := s.verifyStagedDocs(ctx, op.Index); err != nil {
			return err
		}
		index, err := s.readStagingIndex(op.Index.RepoID)
		if err != nil {
			return err
		}
		accepted, acceptedErr := s.readStagingOperation(op.Index.RepoID, op.ID)
		if acceptedErr != nil && !errors.Is(acceptedErr, domain.ErrNotFound) {
			return acceptedErr
		}
		// Once the operation manifest is durable, unstage or re-add changes
		// the next index, not this accepted commit. Ref/position CAS below
		// still protects competing work, and consumption matches exact entries.
		if errors.Is(acceptedErr, domain.ErrNotFound) && index.Revision != op.Index.Revision {
			return domain.ErrSyncConflict
		}
		if acceptedErr == nil {
			expected := *op
			expected.LocalFinalized = accepted.LocalFinalized
			if !reflect.DeepEqual(accepted, expected) {
				return domain.ErrSyncConflict
			}
		}
		p, err := s.stagingPosition(ctx, op.Index.RepoID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(p, op.ExpectedPosition) || p.GitCommit != s.gitCommit || p.GitBranch() != s.gitBranch {
			return domain.ErrSelectionChanged
		}
		ref, err := s.getRefRaw(ctx, op.Ref.RepoID, op.Ref.Kind, op.Ref.Name)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if ref.Target != op.ExpectedRef.Target || (ref.BranchID != "" && ref.BranchID != op.ExpectedRef.BranchID) {
			return domain.ErrSyncConflict
		}
		refs, err := s.listRefsRaw(ctx, op.Ref.RepoID)
		if err != nil {
			return err
		}
		latest, ok, err := domain.LatestBranchLifecycle(refs, op.Ref.Name)
		if err != nil {
			return err
		}
		if ok && latest.State == domain.BranchArchived {
			return domain.ErrBranchArchived
		}
		// Verify every advertised contribution before publication, including retries.
		for _, entry := range op.Index.Entries {
			snap, err := s.GetSnapshot(ctx, entry.DocHash)
			if err != nil {
				return err
			}
			if snap.RepoID != op.Index.RepoID || snap.DocHash != entry.DocHash {
				return domain.ErrHashMismatch
			}
		}
		journal := workingCommit{Ref: op.Ref, Expected: op.ExpectedRef.Target, Position: op.Position, Event: op.Advance}
		if err := validateWorkingCommit(journal); err != nil {
			return err
		}
		if err := s.writeStagingOperation(*op); err != nil {
			return err
		}
		for _, event := range domain.StagingObservations(*op) {
			if err := s.putHistoryEvent(event); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(journal)
		if err != nil {
			return err
		}
		if err = writeAtomic(s.workingCommitPath(), raw); err != nil {
			return err
		}
		if err = s.recoverWorkingCommit(); err != nil {
			return err
		}
	}
	// Immutable publication records are the existing synchronization outbox. An
	// interrupted acknowledgement never requires reopening the provider source.
	for _, e := range append(append(domain.StagingObservations(*op), op.Publications...), *op.Position.Selection) {
		if err := s.putHistoryEvent(e); err != nil {
			return err
		}
	}
	index, err := s.readStagingIndex(op.Index.RepoID)
	if err != nil {
		return err
	}
	next := domain.ConsumeStagedEntries(index, op.Index.Entries)
	if next.Revision != index.Revision {
		if err := s.writeStaging(next); err != nil {
			return err
		}
	}
	op.LocalFinalized = true
	return s.writeStagingOperation(*op)
}

func (s *FileStore) ResumeStagingCommit(ctx context.Context, repo, id string) (op domain.StagingCommit, err error) {
	err = s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			var e error
			op, e = s.readStagingOperation(repo, id)
			if e != nil {
				return e
			}
			return s.finishStagingCommit(ctx, &op, true)
		})
	})
	return
}

func (s *FileStore) ListStagingCommits(ctx context.Context, repo string) ([]domain.StagingCommit, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.storeDir(), "worktrees", s.worktreeID, "staging-commits")
	entries, err := readCxtDir(dir)
	if os.IsNotExist(err) {
		return []domain.StagingCommit{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []domain.StagingCommit{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") || e.IsDir() {
			return nil, domain.ErrHashMismatch
		}
		op, err := s.readStagingOperation(repo, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}
func (s *FileStore) reconcileAppliedStaging(ctx context.Context, repo string) error {
	ops, err := s.ListStagingCommits(ctx, repo)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if !op.LocalFinalized {
			if err := s.finishStagingCommit(ctx, &op, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// Called under the retention lease: a collector cannot race publication of new
// pins. Missing index files do not hide operation/stash pins. Bad or unknown
// manifests stop collection instead of treating corruption as absence.
func (s *FileStore) HasStagingPin(ctx context.Context, hash domain.ContentHash) (bool, error) {
	if err := domain.ValidateContentHash(hash); err != nil {
		return false, err
	}
	worktrees, err := readCxtDir(filepath.Join(s.storeDir(), "worktrees"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	contains := func(index domain.StagingIndex) bool {
		for _, entry := range index.Entries {
			if entry.DocHash == hash || entry.Base == hash {
				return true
			}
		}
		return false
	}
	for _, entry := range worktrees {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !entry.IsDir() {
			continue
		}
		owner := *s
		owner.worktreeID = entry.Name()
		raw, err := readCxtFile(owner.stagingPath())
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		if err == nil {
			var index domain.StagingIndex
			if json.Unmarshal(raw, &index) != nil {
				return false, domain.ErrHashMismatch
			}
			if err := domain.ValidateStagingIndex(index); err != nil {
				return false, err
			}
			if index.WorktreeID != owner.worktreeID {
				return false, domain.ErrHashMismatch
			}
			if contains(index) {
				return true, nil
			}
		}
		for _, kind := range []string{"staging-commits", "index-stashes"} {
			dir := filepath.Join(s.storeDir(), "worktrees", owner.worktreeID, kind)
			records, err := readCxtDir(dir)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			for _, record := range records {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				if strings.HasPrefix(record.Name(), ".") {
					continue
				}
				raw, err := readCxtFile(filepath.Join(dir, record.Name()))
				if err != nil {
					return false, err
				}
				var index domain.StagingIndex
				var id string
				if kind == "staging-commits" {
					var op domain.StagingCommit
					if json.Unmarshal(raw, &op) != nil {
						return false, domain.ErrHashMismatch
					}
					if err := owner.validateStagingOperation(op); err != nil {
						return false, err
					}
					index, id = op.Index, op.ID
				} else {
					var stash domain.StagingStash
					if json.Unmarshal(raw, &stash) != nil {
						return false, domain.ErrHashMismatch
					}
					if err := owner.validateStagingStash(stash); err != nil {
						return false, err
					}
					index, id = stash.Index, stash.ID
				}
				if record.Name() != id+".json" {
					return false, domain.ErrHashMismatch
				}
				if contains(index) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
