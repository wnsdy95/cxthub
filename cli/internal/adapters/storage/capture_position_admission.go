package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.CapturePositionStore = (*FileStore)(nil)

// EnsureCapturePosition crosses only the init -> connected repository boundary.
// An empty cursor carries no context authority. Pending bytes are retained, not
// selected. No HEAD, ref, history, binding, or other worktree is written.
func (s *FileStore) EnsureCapturePosition(ctx context.Context, repo string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(repo)); err != nil {
		return err
	}
	expected, err := s.readPosition()
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := domain.ValidateContentHash(domain.ContentHash(expected.RepoID)); err != nil {
		return err
	}
	if expected.RepoID == repo {
		return nil
	}
	if providerfs.CaptureGateHeld(ctx, s.repoRoot) {
		return fmt.Errorf("normalize capture position before shared capture admission: %w", domain.ErrSelectionChanged)
	}
	if !canonicalEmptyCapturePosition(expected) || expected.GitBranch() != s.gitBranch {
		return fmt.Errorf("existing context cannot change repository identity: %w", domain.ErrSelectionChanged)
	}
	next := expected
	next.RepoID = repo
	next.BranchID = domain.LegacyContextBranchID(repo, next.Branch)
	_, err = s.withOSLock(ctx, "first-tracking", "repo", syscall.LOCK_EX, true, func() error {
		return s.withRefMutationLock(ctx, func() error {
			_, err := s.withOSLock(ctx, "first-tracking", "snapshots", syscall.LOCK_EX, true, func() error {
				current, err := s.readPosition()
				if err != nil {
					return err
				}
				// A concurrent identical admission is an idempotent success;
				// any other selection change must be retried from new evidence.
				if reflect.DeepEqual(current, next) {
					return nil
				}
				if !reflect.DeepEqual(current, expected) {
					return fmt.Errorf("capture position changed during admission: %w", domain.ErrSelectionChanged)
				}
				if err := s.captureBootstrapEvidence(ctx, repo); err != nil {
					return err
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				raw, err := json.Marshal(next)
				if err != nil {
					return err
				}
				return writeAtomic(s.positionPath(), raw)
			})
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("empty capture position normalization: %w", err)
	}
	return nil
}

func canonicalEmptyCapturePosition(p domain.WorkingPosition) bool {
	return emptyInitialTrackingPosition(p) && p.LocalBranch == "" &&
		domain.ValidateBranchName(p.Branch) == nil &&
		(p.GitCommit == "" || domain.ValidGitOID(p.GitCommit)) &&
		p.BranchID == domain.LegacyContextBranchID(p.RepoID, p.Branch)
}

// Called under capture EX -> ref -> snapshot EX. Unlike setup's pristine
// predicate, this admits exact new-repo pending roots. Raw directory checks
// deliberately reject malformed/unknown entries which list APIs may skip.
func (s *FileStore) captureBootstrapEvidence(ctx context.Context, repo string) error {
	// .cxt/capture, app-sessions and session-affinity are capture bookkeeping,
	// not context selections. Preserve them, as well as immutable settings.
	// Frozen commit intents are checked below in every worktree directory.
	for _, name := range []string{"refs", "history", "branch-bindings", "stash.json", "grafts.json", "promotions.json", "unsync.json", "historical-backfill", "capture-collection", "repair-intents"} {
		empty, err := emptyTrackingPath(ctx, filepath.Join(s.storeDir(), name))
		if err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("capture bootstrap has existing authority or intent (%s): %w", name, domain.ErrSelectionChanged)
		}
	}
	raw, err := readCxtFile(filepath.Join(s.storeDir(), "HEAD"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		head := strings.TrimSuffix(string(raw), "\n")
		if !strings.HasPrefix(head, "ref: refs/heads/") || domain.ValidateBranchName(strings.TrimPrefix(head, "ref: refs/heads/")) != nil {
			return domain.ErrSelectionChanged
		}
	}
	owners, err := readCxtDir(filepath.Join(s.storeDir(), "worktrees"))
	if err != nil {
		return err
	}
	for _, owner := range owners {
		dir := filepath.Join(s.storeDir(), "worktrees", owner.Name())
		entries, err := readCxtDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if entry.Name() != "position.json" {
				empty, err := emptyTrackingPath(ctx, path)
				if err != nil {
					return err
				}
				if !empty {
					return fmt.Errorf("worktree capture intent or staging retained: %w", domain.ErrSelectionChanged)
				}
				continue
			}
			raw, err := readCxtFile(path)
			if err != nil {
				return err
			}
			var p domain.WorkingPosition
			if json.Unmarshal(raw, &p) != nil || p.WorktreeID != owner.Name() {
				return domain.ErrHashMismatch
			}
			if !canonicalEmptyCapturePosition(p) {
				return fmt.Errorf("worktree has an existing selection or nonlegacy identity: %w", domain.ErrSelectionChanged)
			}
			if p.WorktreeID != s.worktreeID && p.RepoID != repo {
				return fmt.Errorf("another worktree still owns a different repository identity: %w", domain.ErrSelectionChanged)
			}
		}
	}
	entries, err := readCxtDir(filepath.Join(s.storeDir(), "pending"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	allowed := make(map[domain.ContentHash]domain.Snapshot)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(s.storeDir(), "pending", entry.Name())
		raw, err := readCxtFile(path)
		if err != nil {
			return err
		}
		var p domain.Pending
		if json.Unmarshal(raw, &p) != nil || p.SessionID == "" || len(p.SessionID) > 128 ||
			domain.ValidateBranchName(p.Branch) != nil || domain.ValidateContentHash(p.Target) != nil ||
			(path != s.pendingPath(p.SessionID) && path != s.legacyPendingPath(p.SessionID)) {
			return fmt.Errorf("invalid pending capture metadata: %w", domain.ErrHashMismatch)
		}
		if p.RepoID != repo {
			return fmt.Errorf("pending capture belongs to another repository: %w", domain.ErrSelectionChanged)
		}
		snap, err := s.GetSnapshot(ctx, p.Target)
		if err != nil {
			return err
		}
		if snap.RepoID != repo || snap.Branch != p.Branch || snap.SessionID != p.SessionID || snap.Provider != p.Provider {
			return fmt.Errorf("pending target does not match its repository/session/provider/branch: %w", domain.ErrHashMismatch)
		}
		// A selected ancestry or graft is not an unselected
		// first pending capture, even when its repository membership matches.
		if len(snap.Parents) != 0 || len(snap.GraftParents) != 0 || snap.Grafted || snap.GraftSeq != 0 {
			return fmt.Errorf("pending capture has existing context ancestry: %w", domain.ErrSelectionChanged)
		}
		allowed[snap.ID] = snap
	}
	snapshots := make([]domain.Snapshot, 0, len(allowed))
	for _, snap := range allowed {
		snapshots = append(snapshots, snap)
	}
	clean, err := s.trackingObjectsPristine(ctx, repo, snapshots)
	if err != nil {
		return err
	}
	if !clean {
		return domain.ErrSelectionChanged
	}
	// Memories carry an owner even when their mutable snapshot pointer has
	// advanced. Preserve old versions only for the admitted pending owners.
	memories, err := readCxtDir(filepath.Join(s.storeDir(), "objects", "memories"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range memories {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, ok := hashFromObjectName(entry.Name())
		if !ok {
			return domain.ErrHashMismatch
		}
		memory, err := s.GetMemory(ctx, id)
		if err != nil {
			return err
		}
		if _, ok := allowed[memory.SnapshotID]; !ok {
			return domain.ErrSelectionChanged
		}
	}
	return nil
}
