package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// ReuseMemoryDigest keeps the source read, integrity check and target CAS in one
// authorized repository transaction. It changes neither projection nor retention.
func (s *Service) ReuseMemoryDigest(ctx context.Context, repo, target domain.ContentHash, in inbound.MemoryReuse) (domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if in.Version != 1 {
		return "", domain.ErrValidation
	}
	if err := validateHashes(repo, target, in.BaseHash, in.MemoryHash); err != nil {
		return "", err
	}
	if err := domain.ValidateOptionalContentHash(in.PreviousMemoryHash); err != nil {
		return "", err
	}
	ctx = auditOperation(ctx, "context.memory.updated")
	return repositoryWrite(ctx, s, repo, func(ctx context.Context) (domain.ContentHash, error) {
		// A globally known blob hash is not authority to read another repo.
		d, err := s.blobs.GetMemory(ctx, repo, in.BaseHash)
		if err != nil {
			return "", err
		}
		base, err := domain.MemoryDigestHash(d)
		if err != nil || base != in.BaseHash {
			return "", domain.ErrIntegrity
		}
		d.SnapshotID, d.PreviousMemoryHash, d.Provider = target, in.PreviousMemoryHash, in.Provider
		got, err := domain.MemoryDigestHash(d)
		if err != nil || got != in.MemoryHash {
			return "", domain.ErrIntegrity
		}
		return s.putMemoryDigest(ctx, repo, d, d.PreviousMemoryHash, true)
	})
}
