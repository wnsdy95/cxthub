package app

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type repositoryWork struct {
	meta               outbound.MetadataStore
	repositories       outbound.RepositoryStore
	supported          []domain.DocumentIdentity
	authority          *RepositoryAuthority
	transactions       outbound.RepositoryTransactions
	revisions          outbound.RepositoryRevisions
	evidenceRevisions  outbound.EvidenceRevisions
	configurationError error
}

func newRepositoryWork(metadata outbound.MetadataStore, repositories outbound.RepositoryStore, supported []domain.DocumentIdentity) *repositoryWork {
	transactions, _ := metadata.(outbound.RepositoryTransactions)
	revisions, _ := metadata.(outbound.RepositoryRevisions)
	evidence, _ := metadata.(outbound.EvidenceRevisions)
	authority, err := repositoryAuthorityFromStores(metadata, repositories, transactions != nil, RepositoryAuthorityCompatibility)
	for _, capability := range []any{transactions, revisions, evidence} {
		if capability != nil && !rootAdapterPresent(capability) {
			err = domain.ErrValidation
		}
	}
	return &repositoryWork{meta: metadata, repositories: repositories, supported: append([]domain.DocumentIdentity(nil), supported...),
		authority: authority, transactions: transactions, revisions: revisions, evidenceRevisions: evidence, configurationError: err}
}

func (work *repositoryWork) checkDocumentIdentity(ctx context.Context, repo domain.ContentHash, allowMissing bool) error {
	if work.configurationError != nil {
		return work.configurationError
	}
	return checkRepositoryDocumentIdentity(ctx, work.meta, work.supported, repo, allowMissing)
}

func (work *repositoryWork) revisionWrite(ctx context.Context, repo domain.ContentHash) error {
	if work.configurationError != nil {
		return work.configurationError
	}
	if scope, _ := ctx.Value(revisionScopeKey{}).(string); scope == "none" {
		return nil
	}
	if scope, _ := ctx.Value(revisionScopeKey{}).(string); scope == "evidence" {
		if work.evidenceRevisions != nil {
			return work.evidenceRevisions.AdvanceEvidenceRevision(ctx, repo)
		}
	}
	if work.revisions != nil {
		scope, _ := ctx.Value(revisionScopeKey{}).(string)
		return work.revisions.AdvanceRepositoryRevision(ctx, repo, scope == "pending")
	}
	return nil
}

func (work *repositoryWork) authorizeRepositoryWrite(ctx context.Context, repoID domain.ContentHash) error {
	if work.configurationError != nil {
		return work.configurationError
	}
	return work.authority.AuthorizeWrite(ctx, repoID)
}

func (work *repositoryWork) auditRepositoryWrite(ctx context.Context, repo domain.ContentHash) error {
	if work.configurationError != nil {
		return work.configurationError
	}
	return work.authority.AuditWrite(ctx, repo)
}

func repositoryWriteWithWork[T any](ctx context.Context, work *repositoryWork, repo domain.ContentHash, fn func(context.Context) (T, error)) (out T, err error) {
	if work == nil || fn == nil {
		return out, domain.ErrValidation
	}
	if work.configurationError != nil {
		return out, work.configurationError
	}
	original := fn
	fn = func(ctx context.Context) (T, error) {
		if e := work.authorizeRepositoryWrite(ctx, repo); e != nil {
			var zero T
			return zero, e
		}
		if err := work.checkDocumentIdentity(ctx, repo, true); err != nil {
			var zero T
			return zero, err
		}
		return original(ctx)
	}
	tx := work.transactions
	if tx == nil {
		out, err = fn(ctx)
		if err == nil {
			err = work.revisionWrite(ctx, repo)
			if err == nil {
				err = work.auditRepositoryWrite(ctx, repo)
			}
		}
		return out, err
	}
	if _, nested := ctx.Value(afterCommitKey{}).(*afterCommitActions); nested {
		err = tx.WithinRepository(ctx, repo, func(bound context.Context) error { out, err = fn(bound); return err })
	} else {
		for attempt := 0; attempt < 3; attempt++ {
			after := &afterCommitActions{}
			err = tx.WithinRepository(ctx, repo, func(bound context.Context) error {
				out, err = fn(context.WithValue(bound, afterCommitKey{}, after))
				if err == nil {
					err = work.revisionWrite(bound, repo)
					if err == nil {
						err = work.auditRepositoryWrite(bound, repo)
					}
				}
				return err
			})
			if err == nil {
				for _, action := range after.actions {
					action()
				}
				break
			}
			var state interface{ SQLState() string }
			if attempt == 2 || !errors.As(err, &state) || (state.SQLState() != "40001" && state.SQLState() != "40P01") {
				break
			}
			if cancelled := ctx.Err(); cancelled != nil {
				var zero T
				return zero, cancelled
			}
			timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				var zero T
				return zero, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

func repositoryReadWithWork[T any](ctx context.Context, work *repositoryWork, fn func(context.Context) (T, error)) (out T, err error) {
	if work == nil || fn == nil {
		return out, domain.ErrValidation
	}
	if work.configurationError != nil {
		return out, work.configurationError
	}
	if tx := work.transactions; tx != nil {
		err = tx.WithinReadSnapshot(ctx, func(bound context.Context) error { out, err = fn(bound); return err })
		if err != nil {
			var zero T
			return zero, err
		}
		return out, nil
	}
	return fn(ctx)
}

func repositoryWriteErrorWithWork(ctx context.Context, work *repositoryWork, repo domain.ContentHash, write func(context.Context) error) error {
	_, err := repositoryWriteWithWork(ctx, work, repo, func(bound context.Context) (struct{}, error) { return struct{}{}, write(bound) })
	return err
}
