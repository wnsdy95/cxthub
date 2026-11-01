package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func withIdentityTransactions(ctx context.Context, transactions outbound.IdentityTransactions, fn func(context.Context) error) error {
	if transactions != nil {
		return transactions.WithinIdentity(ctx, fn)
	}
	// Development stores serialize commands in-process; only the production
	// transaction port promises rollback and cross-server atomicity.
	if ctx.Value(identityMutationKey{}) != nil {
		return fn(ctx)
	}
	localIdentityMutation.Lock()
	defer localIdentityMutation.Unlock()
	return fn(context.WithValue(ctx, identityMutationKey{}, true))
}

func identityResultWithTransactions[T any](ctx context.Context, transactions outbound.IdentityTransactions, fn func(context.Context) (T, error)) (out T, err error) {
	err = withIdentityTransactions(ctx, transactions, func(ctx context.Context) error { out, err = fn(ctx); return err })
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

func (service *IdentityService) withRepositoryMetadata(ctx context.Context, repository string, mutate func(context.Context) error) error {
	if service.repositoryMetadata != nil {
		return service.repositoryMetadata.WithinRepositoryMetadata(ctx, repository, mutate)
	}
	return service.withIdentity(ctx, mutate)
}

func repositoryIdentityResult[T any](ctx context.Context, service *IdentityService, repository string, mutate func(context.Context) (T, error)) (out T, err error) {
	err = service.withRepositoryMetadata(ctx, repository, func(bound context.Context) error {
		out, err = mutate(bound)
		return err
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}
