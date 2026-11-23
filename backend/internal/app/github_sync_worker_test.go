package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave47GitHubTransactionKey struct{}

type wave47GitHubTransaction struct {
	storage *wave47GitHubStore
	write   bool
}

type wave47GitHubStore struct {
	*store.FSStore
	writes     int
	reads      int
	identities int
}

func (storage *wave47GitHubStore) WithinRepository(ctx context.Context, _ domain.ContentHash, execute func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		return execute(ctx)
	}
	storage.writes++
	return execute(context.WithValue(ctx, wave47GitHubTransactionKey{}, wave47GitHubTransaction{storage: storage, write: true}))
}

func (storage *wave47GitHubStore) WithinReadSnapshot(ctx context.Context, execute func(context.Context) error) error {
	if storage.InTransaction(ctx) {
		return execute(ctx)
	}
	storage.reads++
	return execute(context.WithValue(ctx, wave47GitHubTransactionKey{}, wave47GitHubTransaction{storage: storage}))
}

func (storage *wave47GitHubStore) InTransaction(ctx context.Context) bool {
	transaction, present := ctx.Value(wave47GitHubTransactionKey{}).(wave47GitHubTransaction)
	return present && transaction.storage == storage
}

func (storage *wave47GitHubStore) InReadOnlyTransaction(ctx context.Context) bool {
	transaction, present := ctx.Value(wave47GitHubTransactionKey{}).(wave47GitHubTransaction)
	return present && transaction.storage == storage && !transaction.write
}

func (storage *wave47GitHubStore) WithinIdentity(ctx context.Context, execute func(context.Context) error) error {
	storage.identities++
	return storage.WithinRepository(ctx, "", execute)
}

func (*wave47GitHubStore) LockRepositoryAccess(context.Context, string, string) error {
	return nil
}

type wave47GitHubQueue struct {
	outbound.GitHubStore
	mutex    sync.Mutex
	contexts []context.Context
	listErr  error
}

func (storage *wave47GitHubQueue) ListGitHubConnections(ctx context.Context) ([]domain.GitHubConnection, error) {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	storage.contexts = append(storage.contexts, ctx)
	return nil, storage.listErr
}

func (*wave47GitHubQueue) ListGitHubDeliveries(context.Context) ([]domain.GitHubDelivery, error) {
	return nil, nil
}

func (storage *wave47GitHubQueue) snapshot() []context.Context {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	return append([]context.Context(nil), storage.contexts...)
}

type wave47GitHubRemote struct {
	*githubFake
	storage *wave47GitHubStore
	test    *testing.T
}

func (remote *wave47GitHubRemote) checkOutsideTransaction(ctx context.Context) {
	remote.test.Helper()
	if remote.storage.InTransaction(ctx) || ctx.Value(identityMutationKey{}) != nil || ctx.Value(afterCommitKey{}) != nil {
		remote.test.Fatal("GitHub provider read retained an identity or repository transaction")
	}
}

func (remote *wave47GitHubRemote) Installation(ctx context.Context, installation int64) (domain.GitHubInstallation, error) {
	remote.checkOutsideTransaction(ctx)
	return remote.githubFake.Installation(ctx, installation)
}

func (remote *wave47GitHubRemote) TeamMembers(ctx context.Context, installation int64, organization, team string) ([]int64, error) {
	remote.checkOutsideTransaction(ctx)
	return remote.githubFake.TeamMembers(ctx, installation, organization, team)
}

type wave47GitHubLeaseStore struct {
	outbound.GitHubStore
	test  *testing.T
	reads int
}

func (storage *wave47GitHubLeaseStore) GetGitHubDelivery(ctx context.Context, identifier string) (domain.GitHubDelivery, error) {
	job, err := storage.GitHubStore.GetGitHubDelivery(ctx, identifier)
	storage.reads++
	if err == nil && storage.reads == 2 {
		job.Lease = "wave47-replacement-lease"
		job.LeaseUntil = time.Now().UTC().Add(2 * time.Minute)
		err = storage.GitHubStore.PutGitHubDelivery(ctx, job)
	}
	return job, err
}

func (storage *wave47GitHubLeaseStore) PutGitHubDelivery(ctx context.Context, job domain.GitHubDelivery) error {
	storage.test.Helper()
	if job.Lease == "" || job.Attempts != 1 || time.Until(job.LeaseUntil) <= 0 || time.Until(job.LeaseUntil) > 2*time.Minute {
		storage.test.Fatal("actual delivery claim changed its original two-minute lease or first attempt", job)
	}
	return storage.GitHubStore.PutGitHubDelivery(ctx, job)
}
