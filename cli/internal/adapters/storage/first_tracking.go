package storage

import (
	"bytes"
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

// Shared across worktrees/processes; nested writes reuse the capture context.
func (s *FileStore) WithCaptureTrackingGate(ctx context.Context, fn func(context.Context) error) error {
	return providerfs.WithCaptureGate(ctx, s.repoRoot, fn)
}

func (s *FileStore) TrackingPristine(ctx context.Context, repo string) (bool, error) {
	var pristine bool
	_, err := s.withOSLock(ctx, "first-tracking", "repo", syscall.LOCK_EX, true, func() error {
		return s.withRefMutationLock(ctx, func() error {
			_, err := s.withOSLock(ctx, "first-tracking", "snapshots", syscall.LOCK_EX, true, func() error {
				clean, err := s.trackingStatePristine(ctx)
				if err != nil || !clean {
					pristine = clean
					return err
				}
				allowed, err := s.firstTrackingCache(ctx, repo)
				if err != nil {
					return err
				}
				pristine, err = s.trackingPristine(ctx, repo, allowed)
				return err
			})
			return err
		})
	})
	return pristine, err
}

// Called with capture exclusion and the ref lock, after journal recovery.
// Object/observation caches are harmless. A symbolic HEAD/empty init position
// is not a capture. Any local durable selection, pending, replay or outbox is.
func (s *FileStore) trackingPristine(ctx context.Context, repo string, observed []domain.Snapshot) (bool, error) {
	clean, err := s.trackingStatePristine(ctx)
	if err != nil || !clean {
		return clean, err
	}
	return s.trackingObjectsPristine(ctx, repo, observed)
}

func (s *FileStore) trackingStatePristine(ctx context.Context) (bool, error) {
	for _, name := range []string{"refs", "history", "branch-bindings", "pending", "capture", "stash.json", "grafts.json", "promotions.json", "unsync.json", "historical-backfill", "capture-collection", "repair-intents"} {
		empty, err := emptyTrackingPath(ctx, filepath.Join(s.storeDir(), name))
		if err != nil || !empty {
			return false, err
		}
	}
	raw, err := readCxtFile(filepath.Join(s.storeDir(), "HEAD"))
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err == nil {
		value := strings.TrimSuffix(string(raw), "\n")
		if strings.HasPrefix(value, "ref: refs/heads/") {
			if domain.ValidateBranchName(strings.TrimPrefix(value, "ref: refs/heads/")) != nil {
				return false, domain.ErrHashMismatch
			}
		} else if domain.ValidateContentHash(domain.ContentHash(value)) == nil {
			return false, nil
		} else {
			return false, domain.ErrHashMismatch
		}
	}
	owners, err := readCxtDir(filepath.Join(s.storeDir(), "worktrees"))
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, owner := range owners {
		dir := filepath.Join(s.storeDir(), "worktrees", owner.Name())
		files, err := readCxtDir(dir)
		if err != nil {
			return false, err
		}
		for _, file := range files {
			path := filepath.Join(dir, file.Name())
			if file.Name() != "position.json" {
				empty, err := emptyTrackingPath(ctx, path)
				if err != nil || !empty {
					return false, err
				}
				continue
			}
			raw, err := readCxtFile(path)
			if err != nil {
				return false, err
			}
			var p domain.WorkingPosition
			if json.Unmarshal(raw, &p) != nil || p.WorktreeID != owner.Name() {
				return false, domain.ErrHashMismatch
			}
			if !emptyInitialTrackingPosition(p) {
				return false, nil
			}
		}
	}
	return true, nil
}

// Conservatively refuse even malformed/unknown local evidence; never interpret
// an unreadable file or symlink as an empty replica.
func emptyTrackingPath(ctx context.Context, path string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, domain.ErrHashMismatch
	}
	if !info.IsDir() {
		return false, nil
	}
	entries, err := readCxtDir(path)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		empty, err := emptyTrackingPath(ctx, filepath.Join(path, entry.Name()))
		if err != nil || !empty {
			return false, err
		}
	}
	return true, nil
}

// Cached observations permit retry after a failed preparation without treating
// same-repo objects as automatically safe. Every snapshot and document must be
// accounted for by exact verified metadata, including across provisional IDs.
func (s *FileStore) firstTrackingCache(ctx context.Context, repo string) ([]domain.Snapshot, error) {
	entries, err := readCxtDir(filepath.Join(s.storeDir(), "remote-observations"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshots []domain.Snapshot
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := readCxtFile(filepath.Join(s.storeDir(), "remote-observations", entry.Name()))
		if err != nil {
			return nil, err
		}
		var value outbound.RemoteObservation
		if json.Unmarshal(raw, &value) != nil {
			return nil, domain.ErrHashMismatch
		}
		checked, err := s.readRemoteObservation(ctx, value.RepoID, value.Remote, value.Branch)
		if err != nil || !reflect.DeepEqual(checked, value) {
			return nil, domain.ErrHashMismatch
		}
		path, err := s.scopedRemoteObservationPath(value.RepoID, value.Remote, value.Branch)
		if err != nil || filepath.Base(path) != entry.Name() {
			return nil, domain.ErrHashMismatch
		}
		if value.RepoID == repo {
			snapshots = append(snapshots, value.Snapshots...)
		}
	}
	return snapshots, nil
}

func (s *FileStore) trackingObjectsPristine(ctx context.Context, repo string, observed []domain.Snapshot) (bool, error) {
	allowed := map[domain.ContentHash]domain.Snapshot{}
	for _, snap := range observed {
		if snap.RepoID != repo {
			return false, domain.ErrHashMismatch
		}
		if old, ok := allowed[snap.ID]; ok && !sameFirstTrackingSnapshot(old, snap) {
			return false, domain.ErrSyncConflict
		}
		allowed[snap.ID] = snap
	}
	entries, err := readCxtDir(filepath.Join(s.storeDir(), "objects", "snapshots"))
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, entry := range entries {
		if _, ok := hashFromObjectName(entry.Name()); !ok || entry.IsDir() {
			return false, domain.ErrHashMismatch
		}
		if err := validateCxtWritePath(filepath.Join(s.storeDir(), "objects", "snapshots", entry.Name())); err != nil {
			return false, err
		}
	}
	snapshots, err := s.ListSnapshots(ctx, "", "")
	if err != nil {
		return false, err
	}
	for _, snap := range snapshots {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		want, ok := allowed[snap.ID]
		if !ok || !sameFirstTrackingSnapshot(want, snap) {
			return false, fmt.Errorf("unproven local snapshot retained; use explicit cxt pull: %w", domain.ErrSyncConflict)
		}
	}
	docs, err := readCxtDir(filepath.Join(s.storeDir(), "objects", "docs"))
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, doc := range docs {
		id, ok := hashFromObjectName(doc.Name())
		if !ok || doc.IsDir() {
			return false, domain.ErrHashMismatch
		}
		if _, ok := allowed[id]; !ok {
			return false, fmt.Errorf("local captured document retained; use explicit cxt pull: %w", domain.ErrSyncConflict)
		}
		if err := validateCxtWritePath(filepath.Join(s.storeDir(), "objects", "docs", doc.Name())); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s *FileStore) InitializeHeadIfAbsent(ctx context.Context, ref domain.Ref) error {
	if ref.Kind != domain.RefHEAD || ref.Name != "HEAD" || ref.Target != "" || ref.Symbolic == "" {
		return domain.ErrInvalidRef
	}
	if err := domain.ValidateRef(ref); err != nil {
		return err
	}
	return s.withRefMutationLock(ctx, func() error {
		_, err := readCxtFile(filepath.Join(s.storeDir(), "HEAD"))
		if err == nil {
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		// A manual first capture may have published a ref before writing HEAD.
		pristine, err := s.trackingPristine(ctx, ref.RepoID, nil)
		if err != nil {
			return err
		}
		if !pristine {
			return fmt.Errorf("existing context must be inspected before setup initialization: %w", domain.ErrSyncConflict)
		}
		return s.putRefRaw(ref)
	})
}

func emptyInitialTrackingPosition(p domain.WorkingPosition) bool {
	return domain.ValidateContentHash(domain.ContentHash(p.RepoID)) == nil && p.Snapshot == "" && p.SharedTarget == "" && p.MemoryHash == "" && p.MemorySource == "" && p.Selection == nil && !p.Orphan && !p.Rewound && !p.MemoryPinned
}

func sameFirstTrackingSnapshot(left, right domain.Snapshot) bool {
	// Match the durable JSON shape (omitempty makes nil/empty slices equivalent).
	a, err := json.Marshal(left)
	if err != nil {
		return false
	}
	b, err := json.Marshal(right)
	return err == nil && bytes.Equal(a, b)
}

// InitializeCapturePosition admits a canonical empty cursor, not an attachment.
// The single atomic position write needs no history journal or server receipt.
func (s *FileStore) InitializeCapturePosition(ctx context.Context, expected *domain.WorkingPosition, next domain.WorkingPosition) error {
	if !emptyInitialTrackingPosition(next) || domain.ValidateBranchName(next.Branch) != nil ||
		!domain.ValidGitOID(next.GitCommit) || next.LocalBranch != "" || next.WorktreeID == "" ||
		next.BranchID != domain.LegacyContextBranchID(next.RepoID, next.Branch) {
		return domain.ErrHashMismatch
	}
	if s.worktreeID != next.WorktreeID || s.gitBranch != next.Branch || s.gitCommit != next.GitCommit {
		return domain.ErrCodePositionMismatch
	}
	_, err := s.withOSLock(ctx, "first-tracking", "repo", syscall.LOCK_EX, true, func() error {
		return s.withRefMutationLock(ctx, func() error {
			_, err := s.withOSLock(ctx, "first-tracking", "snapshots", syscall.LOCK_EX, true, func() error {
				pristine, err := s.trackingPristine(ctx, next.RepoID, nil)
				if err != nil {
					return err
				}
				if !pristine {
					return fmt.Errorf("local context appeared during setup; preserved without initialization: %w", domain.ErrSyncConflict)
				}
				current, err := s.readPosition()
				if err != nil && !errors.Is(err, domain.ErrNotFound) {
					return err
				}
				if (expected == nil && err == nil) || (expected != nil && (err != nil || !reflect.DeepEqual(current, *expected))) {
					return domain.ErrSyncConflict
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return s.writePosition(next)
			})
			return err
		})
	})
	return err
}
