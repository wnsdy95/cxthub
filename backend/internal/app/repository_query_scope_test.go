package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryScopeReadKey struct{}

type repositoryScopeReadStore struct {
	*store.FSStore
	snapshots, metadataReads, revisionReads int
	finishFailure                           error
}

func (storage *repositoryScopeReadStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if ctx.Value(repositoryScopeReadKey{}) == storage {
		return read(ctx)
	}
	storage.snapshots++
	return errors.Join(read(context.WithValue(ctx, repositoryScopeReadKey{}, storage)), storage.finishFailure)
}

func (storage *repositoryScopeReadStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.metadataReads++
	if ctx.Value(repositoryScopeReadKey{}) != storage {
		return domain.Repo{}, domain.ErrIntegrity
	}
	return storage.FSStore.GetRepo(ctx, repo)
}

func (storage *repositoryScopeReadStore) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	storage.revisionReads++
	if ctx.Value(repositoryScopeReadKey{}) != storage {
		return domain.RepositoryRevision{}, domain.ErrIntegrity
	}
	return storage.FSStore.RepositoryRevision(ctx, repo)
}

func newRepositoryScopeQueries(test *testing.T) (*Service, inbound.RepositoryStateQueries, *repositoryScopeReadStore, domain.ContentHash) {
	test.Helper()
	service, filesystem := newFsckSvc(test)
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := filesystem.PutRepo(context.Background(), domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	if err := filesystem.AdvanceRepositoryRevision(context.Background(), repo, false); err != nil {
		test.Fatal(err)
	}
	storage := &repositoryScopeReadStore{FSStore: filesystem}
	service.meta = storage
	queries, err := service.RepositoryStateQueriesForProfile(repositoryquery.PinnedSnapshots)
	if err != nil {
		test.Fatal(err)
	}
	if _, owned := queries.(*repositoryquery.Queries); !owned {
		test.Fatal("repository state methods remain owned by the parent Service")
	}
	return service, queries, storage, repo
}

func TestRepositoryStateQueriesUseOriginalBoundPortsWithoutServiceBackReference(test *testing.T) {
	service, queries, storage, repo := newRepositoryScopeQueries(test)
	service.meta = store.NewFSStore(test.TempDir())
	got, err := queries.RepositoryChangeState(context.Background(), repo)
	if err != nil || got.Graph != 1 || storage.revisionReads != 1 || storage.snapshots != 1 || storage.metadataReads < 1 {
		test.Fatal("query rediscovered the Service store or lost the pinned scope", got, err, storage)
	}
	storage.finishFailure = domain.ErrConflict
	failed, err := queries.RepositoryChangeState(context.Background(), repo)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(failed, domain.RepositoryChangeState{}) || storage.revisionReads != 2 {
		test.Fatal("snapshot completion failure leaked a non-atomic result", failed, err, storage.revisionReads)
	}
}

func TestRepositoryStateQueriesRecheckReadOnlyAuthorizationAndReleaseSharedPermit(test *testing.T) {
	_, queries, storage, repo := newRepositoryScopeQueries(test)
	ctx, gate := admissionTestContext(50 * time.Millisecond)
	generalCalls, snapshotCalls := 0, 0
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
		generalCalls++
		if selected != repo || bound.Value(repositoryScopeReadKey{}) != nil {
			return bound, domain.ErrIntegrity
		}
		return bound, nil
	})
	ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, selected domain.ContentHash) (context.Context, error) {
		snapshotCalls++
		if selected != repo || bound.Value(repositoryScopeReadKey{}) != storage {
			return bound, domain.ErrIntegrity
		}
		return bound, domain.ErrForbidden
	})
	denied, err := queries.GetPendingState(ctx, repo)
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(denied, domain.PendingStateView{}) || generalCalls != 1 || snapshotCalls != 1 || storage.metadataReads != 0 || storage.revisionReads != 0 {
		test.Fatal("read bypassed current authorization or invoked general auth inside snapshot", denied, err, generalCalls, snapshotCalls, storage)
	}
	assertResourcesReleased(test, gate)
}

func TestRepositoryStateQueryPinnedProfileRejectsMissingSnapshotStore(test *testing.T) {
	service, _ := newFsckSvc(test)
	if queries, err := service.RepositoryStateQueriesForProfile(repositoryquery.PinnedSnapshots); queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("pinned query silently downgraded to unpinned storage", queries, err)
	}
}

func TestRepositoryStateQueryRejectsGeneralOnlyAuthorizationInsidePinnedRead(test *testing.T) {
	_, queries, storage, repo := newRepositoryScopeQueries(test)
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		if bound.Value(repositoryScopeReadKey{}) != nil {
			test.Fatal("general authorization was moved into the snapshot")
		}
		return bound, nil
	})
	result, err := queries.GetPendingState(ctx, repo)
	if !errors.Is(err, domain.ErrCatalogUnsupported) || !reflect.DeepEqual(result, domain.PendingStateView{}) || storage.metadataReads != 0 || storage.revisionReads != 0 {
		test.Fatal("pinned query reused preflight authorization or touched payload", result, err, storage)
	}
}

func TestRepositoryStateCompatibilityDoesNotInvokeSnapshotOnlyAuthentication(test *testing.T) {
	service, filesystem := newFsckSvc(test)
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := filesystem.PutRepo(context.Background(), domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	queries, err := service.RepositoryStateQueriesForProfile(repositoryquery.LegacyCompatibility)
	if err != nil {
		test.Fatal(err)
	}
	generalCalls, snapshotCalls := 0, 0
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		generalCalls++
		return bound, nil
	})
	ctx = inbound.WithDocumentSnapshotReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		snapshotCalls++
		return bound, domain.ErrConflict
	})
	pending, err := queries.GetPendingState(ctx, repo)
	if err != nil || pending.Projection != "on-hold-v1" || generalCalls != 1 || snapshotCalls != 0 {
		test.Fatal("compatibility reader invoked a PostgreSQL-only authentication path", pending, err, generalCalls, snapshotCalls)
	}
}

func TestRepositoryRevisionPreservesEnclosingOwnerAuthorizationContract(test *testing.T) {
	_, queries, storage, repo := newRepositoryScopeQueries(test)
	calls := 0
	authorize := func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		calls++
		if bound.Value(repositoryScopeReadKey{}) != nil {
			test.Fatal("nested revision moved the enclosing owner's general auth into a snapshot")
		}
		return bound, nil
	}
	ctx := inbound.WithDocumentReadAuthorizer(context.Background(), authorize)
	ctx, err := authorize(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	revision, err := queries.RepositoryRevision(ctx, repo)
	if err != nil || revision.Graph != 1 || storage.revisionReads != 1 || calls != 1 {
		test.Fatal("revision imposed On Hold's admission protocol on an existing enclosing graph owner", revision, err, calls)
	}
}
