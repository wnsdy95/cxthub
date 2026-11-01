package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryRetryCancelledStore struct {
	*wave42WorkStore
	cancel context.CancelFunc
}

func (storage *repositoryRetryCancelledStore) WithinRepository(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	err := storage.wave42WorkStore.WithinRepository(ctx, repo, write)
	if err != nil {
		storage.cancel()
	}
	return err
}

func TestRepositoryWorkRetryCancellationSuppressesSuccessResult(test *testing.T) {
	for _, state := range []wave42SQLState{"40001", "40P01"} {
		test.Run(string(state), func(test *testing.T) {
			ctx, cancel := context.WithCancel(systemTestContext())
			defer cancel()
			metadata := &repositoryRetryCancelledStore{
				wave42WorkStore: &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir()), commitError: state}, cancel: cancel,
			}
			repo := hh(test.Name())
			if _, err := metadata.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				test.Fatal(err)
			}
			work := newRepositoryWork(metadata, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
			completed := 0
			output, err := repositoryWriteWithWork(ctx, work, repo, func(bound context.Context) (inbound.CommitOutput, error) {
				afterRepositoryCommit(bound, func() { completed++ })
				return inbound.CommitOutput{StoredDocs: 1, StoredSnapshots: 1}, nil
			})
			if !errors.Is(err, context.Canceled) || output != (inbound.CommitOutput{}) || completed != 0 || wave42Count(metadata.calls, "write") != 1 {
				test.Fatal("cancelled retry exposed uncommitted success or ran completion/retry", output, err, completed, metadata.calls)
			}
		})
	}
}
