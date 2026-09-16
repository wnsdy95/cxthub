package storage

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type workingCommit struct {
	Ref      domain.Ref             `json:"ref"`
	Expected domain.ContentHash     `json:"expected"`
	Position domain.WorkingPosition `json:"position"`
	Event    *domain.HistoryEvent   `json:"event,omitempty"`
}

func (s *FileStore) CommitWorkingSnapshot(ctx context.Context, ref domain.Ref, expected domain.ContentHash, p domain.WorkingPosition, event *domain.HistoryEvent) error {
	return s.commitWorkingSnapshot(ctx, ref, expected, nil, p, event)
}

func (s *FileStore) CommitWorkingSnapshotIfCurrent(ctx context.Context, ref domain.Ref, expected domain.ContentHash, expectedPosition, p domain.WorkingPosition, event *domain.HistoryEvent) error {
	if s.worktreeID == "" || expectedPosition.WorktreeID != s.worktreeID || expectedPosition.RepoID != p.RepoID || expectedPosition.Branch != p.Branch || expectedPosition.BranchID != p.BranchID || expectedPosition.GitBranch() != p.GitBranch() {
		return domain.ErrSyncConflict
	}
	return s.commitWorkingSnapshot(ctx, ref, expected, &expectedPosition, p, event)
}

func (s *FileStore) commitWorkingSnapshot(ctx context.Context, ref domain.Ref, expected domain.ContentHash, expectedPosition *domain.WorkingPosition, p domain.WorkingPosition, event *domain.HistoryEvent) error {
	if s.worktreeID == "" {
		return domain.ErrNotFound
	}
	p.WorktreeID = s.worktreeID
	if ref.BranchID == "" {
		ref.BranchID = p.BranchID
	}
	if p.GitBranch() == s.gitBranch {
		p.GitCommit = s.gitCommit
		if event != nil {
			copy := *event
			copy.GitAfter = s.gitCommit
			event = &copy
		}
	}
	op := workingCommit{Ref: ref, Expected: expected, Position: p, Event: event}
	if err := validateWorkingCommit(op); err != nil {
		return err
	}
	return s.withRefMutationLock(ctx, func() error {
		if expectedPosition != nil {
			currentPosition, err := s.readPosition()
			if err == domain.ErrNotFound {
				return domain.ErrSyncConflict
			}
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(currentPosition, *expectedPosition) {
				return domain.ErrSyncConflict
			}
		}
		current, err := s.getRefRaw(ctx, ref.RepoID, ref.Kind, ref.Name)
		if err != nil && err != domain.ErrNotFound {
			return err
		}
		if current.Target != expected {
			return domain.ErrSyncConflict
		}
		if current.BranchID != "" && current.BranchID != ref.BranchID {
			return domain.ErrSyncConflict
		}
		refs, err := s.listRefsRaw(ctx, ref.RepoID)
		if err != nil {
			return err
		}
		latest, ok, err := domain.LatestBranchLifecycle(refs, ref.Name)
		if err != nil {
			return err
		}
		if ok && latest.State == domain.BranchArchived {
			return domain.ErrBranchArchived
		}
		// The expected selection is consumed here, before the durable journal.
		// Recovery completes an accepted operation before any subsequent writer
		// can enter this lock, including when its position was already written.
		raw, err := json.Marshal(op)
		if err != nil {
			return err
		}
		if err = writeAtomic(s.workingCommitPath(), raw); err != nil {
			return err
		}
		return s.recoverWorkingCommit()
	})
}

func (s *FileStore) workingCommitPath() string {
	return filepath.Join(s.storeDir(), "working-commit.json")
}

func validateWorkingCommit(op workingCommit) error {
	if op.Ref.BranchID != "" && op.Ref.BranchID != op.Position.BranchID {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateRef(op.Ref); err != nil {
		return err
	}
	if op.Ref.Kind != domain.RefBranch || op.Ref.Target != op.Position.Snapshot || op.Ref.RepoID != op.Position.RepoID || op.Ref.Name != op.Position.Branch {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateOptionalContentHash(op.Expected); err != nil {
		return err
	}
	if len(op.Position.WorktreeID) != 32 {
		return domain.ErrHashMismatch
	}
	if _, err := hex.DecodeString(op.Position.WorktreeID); err != nil {
		return err
	}
	if op.Event != nil {
		if err := domain.ValidateHistoryEvent(*op.Event); err != nil {
			return err
		}
		if op.Event.Kind != "advance" || op.Event.RepoID != op.Ref.RepoID || op.Event.Source != op.Expected || op.Event.Target != op.Ref.Target || op.Event.Branch != op.Ref.Name || (op.Position.BranchID != "" && op.Event.BranchID != op.Position.BranchID) {
			return domain.ErrHashMismatch
		}
	}
	return nil
}

// All ref mutations recover this write-ahead record first. A crash after the
// ref write therefore cannot lose retention or leave another worktree's cursor
// permanently out of sync. Recovery never recreates a provider conversation.
func (s *FileStore) recoverWorkingCommit() error {
	raw, err := readCxtFile(s.workingCommitPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var op workingCommit
	if err = json.Unmarshal(raw, &op); err != nil {
		return err
	}
	if err = validateWorkingCommit(op); err != nil {
		return err
	}
	// A valid journal is not evidence that its referenced content survived.
	// Verify before publishing refs; preserve the journal on any corruption.
	for _, id := range []domain.ContentHash{op.Expected, op.Position.Snapshot, op.Position.MemorySource} {
		if id == "" {
			continue
		}
		snapshot, err := s.GetSnapshot(context.Background(), id)
		if err != nil {
			return err
		}
		if snapshot.RepoID != op.Ref.RepoID {
			return domain.ErrHashMismatch
		}
		if _, err := s.GetDoc(context.Background(), snapshot.DocHash); err != nil {
			return err
		}
	}
	if op.Position.MemoryHash != "" {
		memory, err := s.GetMemory(context.Background(), op.Position.MemoryHash)
		if err != nil {
			return err
		}
		if memory.SnapshotID != op.Position.Snapshot && memory.SnapshotID != op.Position.MemorySource {
			return domain.ErrHashMismatch
		}
	}
	current, err := s.getRefRaw(context.Background(), op.Ref.RepoID, op.Ref.Kind, op.Ref.Name)
	if err != nil && err != domain.ErrNotFound {
		return err
	}
	if current.Target != op.Expected && current.Target != op.Ref.Target {
		return domain.ErrSyncConflict
	}
	if current.BranchID != "" && current.BranchID != op.Ref.BranchID && (op.Ref.BranchID != "" || current.BranchID != domain.LegacyContextBranchID(op.Ref.RepoID, op.Ref.Name)) {
		return domain.ErrSyncConflict
	}
	if op.Event != nil {
		if err = s.putHistoryEvent(*op.Event); err != nil {
			return err
		}
	}
	// Worktree ID belongs to the journal author, not the process doing recovery.
	owner := *s
	owner.worktreeID = op.Position.WorktreeID
	if err = s.putRefRaw(op.Ref); err != nil {
		return err
	}
	if err = owner.writePosition(op.Position); err != nil {
		return err
	}
	if err := os.Remove(s.workingCommitPath()); err != nil {
		return err
	}
	return syncCxtParents(s.workingCommitPath())
}
