package storage

import (
	"context"
	"path/filepath"
	"slices"
	"sort"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// InstallCatalogImage replaces acquired metadata after Merkle reconciliation.
// It does not verify document bytes or publish observations or working state.
func (s *FileStore) InstallCatalogImage(ctx context.Context, expected domain.ContentHash, repo, remote string, checkpoint domain.CatalogCheckpoint, entries []domain.CatalogEntry) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := s.catalogCachePath(repo, remote)
	if err != nil {
		return "", err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return "", err
	}
	var revision domain.ContentHash
	err = s.withMutationLock(ctx, catalogCacheNamespace, filepath.Base(path), func() error {
		// Even an epoch reset must verify the old complete image and pending
		// chain. A replacement is never authority to repair corrupt cache bytes.
		loaded, err := s.loadCatalogCache(ctx, repo, remote)
		if err != nil {
			return err
		}
		head := loaded.head
		if head.Revision != expected {
			return domain.ErrSyncConflict
		}
		if head.Generation == ^uint64(0) {
			return domain.ErrHashMismatch
		}
		if err := domain.ValidateCatalogCheckpoint(repo, checkpoint); err != nil {
			return err
		}
		if head.Checkpoint != nil && head.Checkpoint.Epoch == checkpoint.Epoch && checkpoint.Sequence < head.Checkpoint.Sequence {
			return domain.ErrHashMismatch
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Sequence > checkpoint.Sequence {
				return domain.ErrHashMismatch
			}
		}
		// Validates every entry, exactly one protocol, and semantic identities
		// (including ref-key aliases). Deletions and duplicates are not images.
		if _, _, err := domain.CatalogManifest(repo, entries); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		ordered := slices.Clone(entries)
		sort.Slice(ordered, func(i, j int) bool { return catalogCacheEntryLess(ordered[i], ordered[j]) })
		complete := catalogCacheComplete{
			Format: catalogCacheCompleteFormat, Version: 1, RepoID: repo, Remote: remote,
			Checkpoint: &checkpoint, ImageCheckpoint: &checkpoint,
		}
		for start := 0; start < len(ordered); start += catalogCacheImageSize {
			hash, err := s.storeCatalogCacheRecord(ctx, catalogCacheRecord{
				Format: catalogCacheImageFormat, Version: 1, RepoID: repo, Remote: remote,
				Entries: ordered[start:min(start+catalogCacheImageSize, len(ordered))],
			})
			if err != nil {
				return err
			}
			complete.Image = append(complete.Image, hash)
		}
		complete.CompactAfter = max(catalogCacheInitialThreshold, 2*len(complete.Image))
		head.Completed, err = s.storeCatalogCacheComplete(ctx, complete)
		if err != nil {
			return err
		}
		head.Checkpoint = &checkpoint
		head.PendingTail = ""
		head.PendingCount = 0
		// The sole mutable publication retains the existing generation/CAS and
		// final cancellation check. Unpublished immutable records are harmless.
		revision, err = s.publishCatalogCacheHead(ctx, path, head)
		return err
	})
	if err != nil {
		return "", err
	}
	return revision, nil
}
