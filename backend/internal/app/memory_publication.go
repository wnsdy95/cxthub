package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// PublishMemoryArchive closes the cross-request collection gap. Staged chunks
// are immutable; archive ownership, the snapshot and its first memory pointer
// become visible together under the repository transaction. A later digest is
// still sent through ordinary causal CAS, never through a reset to the root.
func (s *Service) PublishMemoryArchive(ctx context.Context, in inbound.MemoryPublication) (domain.ContentHash, error) {
	objects, d := in.Objects, in.Memory
	if len(objects.Snapshots) != 1 || len(objects.Docs)+len(objects.ChunkedDocs) != 1 || len(objects.ChunkObjects) != 0 || d.PreviousMemoryHash != "" {
		return "", fmt.Errorf("%w: one snapshot, one document and a root memory are required", domain.ErrValidation)
	}
	snap := objects.Snapshots[0]
	if snap.ID != d.SnapshotID || snap.RepoID != objects.RepoID || snap.DocHash != snap.ID || snap.Grafted || len(snap.GraftParents) != 0 || snap.GraftSeq != 0 || snap.MemoryHash != "" {
		return "", domain.ErrIntegrity
	}
	if len(objects.Docs) == 1 && objects.Docs[0].Hash != snap.DocHash || len(objects.ChunkedDocs) == 1 && objects.ChunkedDocs[0].Hash != snap.DocHash {
		return "", domain.ErrIntegrity
	}
	if err := validateMemoryDigest(objects.RepoID, d); err != nil {
		return "", err
	}
	hash, err := domain.MemoryDigestHash(d)
	if err != nil {
		return "", err
	}
	return repositoryWrite(ctx, s, objects.RepoID, func(ctx context.Context) (domain.ContentHash, error) {
		existing, err := s.meta.GetSnapshot(ctx, objects.RepoID, snap.ID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return "", err
		}
		if err == nil && existing.MemoryHash != "" && existing.MemoryHash != hash {
			return "", domain.ErrConflict
		}
		if _, err := s.commit(ctx, objects); err != nil {
			return "", err
		}
		return s.putMemoryDigest(ctx, objects.RepoID, d, "", true)
	})
}
