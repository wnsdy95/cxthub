package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.SaveSecrets = (*Service)(nil)

// SaveSecrets authorizes and validates an edit at its original baseline. The
// envelope, repository revision and notification outbox share the PostgreSQL
// transaction. Development FS storage only guarantees the ciphertext byte CAS.
func (s *Service) SaveSecrets(ctx context.Context, in inbound.SaveSecretsInput) (inbound.SaveSecretsOutput, error) {
	ctx = auditOperation(inbound.WithRepositoryActor(ctx, in.ActorID), "repository.secrets.updated")
	if err := domain.ValidateContentHash(in.RepoID); err != nil {
		return inbound.SaveSecretsOutput{}, err
	}
	if in.ActorID == "" {
		return inbound.SaveSecretsOutput{}, domain.ErrUnauthorized
	}
	st, ok := s.meta.(outbound.SecretsCASStore)
	if !ok {
		return inbound.SaveSecretsOutput{}, domain.ErrSecretsConsistency
	}
	return repositoryWrite(ctx, s, in.RepoID, func(ctx context.Context) (inbound.SaveSecretsOutput, error) {
		if err := s.authorizeSecretsEdit(ctx, in); err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		existing, err := s.meta.GetSecretsEnvelope(ctx, in.RepoID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return inbound.SaveSecretsOutput{}, fmt.Errorf("%w: %w", domain.ErrSecretsConsistency, err)
		}
		if errors.Is(err, domain.ErrNotFound) {
			existing = nil
		}
		if err := domain.ValidateSecretsEdit(existing, in.Envelope, in.Edit); err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		next, err := domain.WithSecretsRevision(in.Envelope, "")
		if err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		// The server's generation must also fit the stored-envelope size limit.
		if len(next) > domain.MaxSecretsEnvelopeBytes {
			return inbound.SaveSecretsOutput{}, fmt.Errorf("%w: envelope exceeds 256KiB", domain.ErrIntegrity)
		}
		if err := st.CompareAndSwapSecrets(ctx, in.RepoID, existing, next); err != nil {
			if errors.Is(err, domain.ErrRefConflict) {
				err = domain.ErrSecretsConflict
			}
			return inbound.SaveSecretsOutput{}, err
		}
		if err := s.notifySecretsChanged(ctx, in.RepoID); err != nil {
			return inbound.SaveSecretsOutput{}, err
		}
		return inbound.SaveSecretsOutput{Status: "stored", Revision: domain.SecretsRevision(next)}, nil
	})
}

func (s *Service) authorizeSecretsEdit(ctx context.Context, in inbound.SaveSecretsInput) error {
	if s.repositories == nil {
		return domain.ErrForbidden
	}
	repo, err := s.meta.GetRepo(ctx, in.RepoID)
	if err != nil {
		return err
	}
	if repo.RepositoryID == "" {
		return domain.ErrForbidden
	}
	// Prevent revocation, archival or policy changes racing an authorized write.
	// A transactional store without this capability must not silently weaken it.
	if _, transactional := s.meta.(outbound.RepositoryTransactions); transactional {
		locker, ok := s.repositories.(outbound.RepositoryAccessLocker)
		if !ok {
			return domain.ErrSecretsConsistency
		}
		if err := locker.LockRepositoryAccess(ctx, repo.RepositoryID, in.ActorID); err != nil {
			return err
		}
	}
	repositoryRecord, err := s.repositories.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		return err
	}
	role, ok, err := repositoryRoleFor(ctx, s.repositories, repositoryRecord, in.ActorID)
	if err != nil {
		return err
	}
	if !ok || !domain.CanEditSecrets(repositoryRecord, role) {
		return domain.ErrForbidden
	}
	return nil
}
