package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type snapshotGraftStoreProbe struct {
	test     *testing.T
	repo     domain.ContentHash
	reads    []domain.ContentHash
	readErr  map[domain.ContentHash]error
	target   domain.ContentHash
	parents  []domain.ContentHash
	expected uint64
	writes   int
	writeErr error
	observe  func(context.Context)
}

func (storage *snapshotGraftStoreProbe) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	storage.test.Helper()
	storage.observe(ctx)
	if repo != storage.repo {
		storage.test.Fatalf("snapshot read changed repository: %s", repo)
	}
	storage.reads = append(storage.reads, id)
	return domain.Snapshot{ID: id, RepoID: repo}, storage.readErr[id]
}

func (storage *snapshotGraftStoreProbe) AddGraftParentsCAS(ctx context.Context, repo, id domain.ContentHash, parents []domain.ContentHash, expected uint64) error {
	storage.test.Helper()
	storage.observe(ctx)
	if repo != storage.repo {
		storage.test.Fatalf("CAS changed repository: %s", repo)
	}
	storage.target, storage.parents, storage.expected = id, append([]domain.ContentHash(nil), parents...), expected
	storage.writes++
	return storage.writeErr
}

type snapshotGraftScopeProbe struct {
	bound context.Context
	calls int
	err   error
}

func (scope *snapshotGraftScopeProbe) WithinSnapshotGraftWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	scope.calls++
	if scope.err != nil {
		return scope.err
	}
	scope.bound = context.WithValue(ctx, struct{ graft bool }{true}, repo)
	defer func() { scope.bound = nil }()
	return write(scope.bound)
}

func TestSnapshotGraftCommandsConstructor(test *testing.T) {
	storage := &snapshotGraftStoreProbe{test: test}
	scope := &snapshotGraftScopeProbe{}
	for _, config := range []SnapshotGraftCommandsConfig{
		{Scope: scope}, {Store: storage},
		{Store: (*snapshotGraftStoreProbe)(nil), Scope: scope},
		{Store: storage, Scope: (*snapshotGraftScopeProbe)(nil)},
	} {
		if commands, err := NewSnapshotGraftCommands(config); commands != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatalf("accepted missing/typed-nil port: %v %v", commands, err)
		}
	}
	if commands, err := NewSnapshotGraftCommands(SnapshotGraftCommandsConfig{Store: storage, Scope: scope}); commands == nil || err != nil {
		test.Fatal(commands, err)
	}
	if scope.calls != 0 || len(storage.reads) != 0 || storage.writes != 0 {
		test.Fatal("constructor performed work")
	}
	filesystem := store.NewFSStore(test.TempDir())
	if commands, err := NewService(filesystem, filesystem, nil, nil, nil).SnapshotGraftCommands(); commands != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("complete factory accepted absent authority", commands, err)
	}
	if commands, err := (*Service)(nil).SnapshotGraftCommands(); commands != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("nil service accepted", commands, err)
	}
}

func TestSnapshotGraftCommandsBoundValidationAndCAS(test *testing.T) {
	repo := domain.HashContent([]byte("graft owner repository"))
	target := domain.HashContent([]byte("graft owner target"))
	parent := domain.HashContent([]byte("graft owner parent"))
	for _, scenario := range []struct {
		name      string
		repo      domain.ContentHash
		target    domain.ContentHash
		parents   []domain.ContentHash
		readErr   map[domain.ContentHash]error
		scopeErr  error
		writeErr  error
		want      error
		wantReads []domain.ContentHash
		writes    int
	}{
		{name: "exact CAS", repo: repo, target: target, parents: []domain.ContentHash{parent, parent}, wantReads: []domain.ContentHash{target, parent, parent}, writes: 1},
		{name: "CAS conflict is not retried", repo: repo, target: target, parents: []domain.ContentHash{parent}, writeErr: domain.ErrConflict, want: domain.ErrConflict, wantReads: []domain.ContentHash{target, parent}, writes: 1},
		{name: "authority precedes validation", repo: repo, target: "bad", scopeErr: domain.ErrForbidden, want: domain.ErrForbidden},
		{name: "invalid repository", repo: "bad", target: target, parents: []domain.ContentHash{parent}, want: domain.ErrIntegrity},
		{name: "invalid target", repo: repo, target: "bad", parents: []domain.ContentHash{parent}, want: domain.ErrIntegrity},
		{name: "empty parents", repo: repo, target: target, want: domain.ErrValidation},
		{name: "too many parents", repo: repo, target: target, parents: make([]domain.ContentHash, 17), want: domain.ErrValidation},
		{name: "missing target", repo: repo, target: target, parents: []domain.ContentHash{parent}, readErr: map[domain.ContentHash]error{target: domain.ErrNotFound}, want: domain.ErrNotFound, wantReads: []domain.ContentHash{target}},
		{name: "self graft", repo: repo, target: target, parents: []domain.ContentHash{target}, want: domain.ErrValidation, wantReads: []domain.ContentHash{target}},
		{name: "invalid parent", repo: repo, target: target, parents: []domain.ContentHash{"bad"}, want: domain.ErrIntegrity, wantReads: []domain.ContentHash{target}},
		{name: "parent read retains not-found mapping", repo: repo, target: target, parents: []domain.ContentHash{parent}, readErr: map[domain.ContentHash]error{parent: domain.ErrConflict}, want: domain.ErrNotFound, wantReads: []domain.ContentHash{target, parent}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			scope := &snapshotGraftScopeProbe{err: scenario.scopeErr}
			storage := &snapshotGraftStoreProbe{test: test, repo: scenario.repo, readErr: scenario.readErr, writeErr: scenario.writeErr}
			storage.observe = func(ctx context.Context) {
				if scope.bound == nil || ctx != scope.bound {
					test.Fatal("storage escaped caller's write scope")
				}
			}
			commands, err := NewSnapshotGraftCommands(SnapshotGraftCommandsConfig{Store: storage, Scope: scope})
			if err != nil {
				test.Fatal(err)
			}
			const expected uint64 = 9007199254740993
			err = commands.GraftSnapshotParents(test.Context(), scenario.repo, scenario.target, scenario.parents, expected)
			if !errors.Is(err, scenario.want) || scope.calls != 1 || storage.writes != scenario.writes || !reflect.DeepEqual(storage.reads, scenario.wantReads) {
				test.Fatalf("error=%v scope=%d writes=%d reads=%v", err, scope.calls, storage.writes, storage.reads)
			}
			if storage.writes != 0 && (storage.target != scenario.target || storage.expected != expected || !reflect.DeepEqual(storage.parents, scenario.parents)) {
				test.Fatal("CAS target, ordered parents or expected sequence changed", storage)
			}
		})
	}
}

func TestSnapshotGraftCommandsExistingWorkAndAuthority(test *testing.T) {
	repo := domain.HashContent([]byte("graft transaction repository"))
	target := domain.HashContent([]byte("graft transaction target"))
	parent := domain.HashContent([]byte("graft transaction parent"))
	for _, scenario := range []struct {
		name      string
		actor     string
		system    bool
		nested    bool
		cancelled bool
		commitErr error
		writeErr  error
		want      error
		calls     []string
		writes    int
	}{
		{name: "owned transaction", system: true, calls: []string{"write", "metadata", "graph"}, writes: 1},
		{name: "borrowed transaction", system: true, nested: true, calls: []string{"write", "metadata", "write", "metadata", "graph"}, writes: 1},
		{name: "anonymous", want: domain.ErrUnauthorized, calls: []string{"write"}},
		{name: "unbound actor", actor: "outsider", want: domain.ErrForbidden, calls: []string{"write"}},
		{name: "cancelled", system: true, cancelled: true, want: context.Canceled, calls: []string{"write"}},
		{name: "CAS conflict", system: true, writeErr: domain.ErrConflict, want: domain.ErrConflict, calls: []string{"write", "metadata"}, writes: 1},
		{name: "commit failure", system: true, commitErr: domain.ErrConflict, want: domain.ErrConflict, calls: []string{"write", "metadata", "graph"}, writes: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			metadata := &wave42WorkStore{commitError: scenario.commitErr, get: func(ctx context.Context, actual domain.ContentHash) (domain.Repo, error) {
				if actual != repo || ctx.Value(wave42TransactionKey{}) != "write" {
					test.Fatal("identity read escaped transaction")
				}
				return domain.Repo{ID: repo}, nil
			}}
			work := newRepositoryWork(metadata, nil, nil)
			storage := &snapshotGraftStoreProbe{test: test, repo: repo, writeErr: scenario.writeErr, observe: func(ctx context.Context) {
				if ctx.Value(wave42TransactionKey{}) != "write" || ctx.Value(afterCommitKey{}) == nil {
					test.Fatal("graft escaped existing transaction")
				}
			}}
			commands, err := snapshotGraftsForWork(storage, work)
			if err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithCancel(test.Context())
			defer cancel()
			if scenario.cancelled {
				cancel()
			}
			if scenario.system {
				ctx = inbound.WithSystemActor(ctx)
			} else if scenario.actor != "" {
				ctx = inbound.WithRepositoryActor(ctx, scenario.actor)
			}
			write := func(bound context.Context) error {
				return commands.GraftSnapshotParents(bound, repo, target, []domain.ContentHash{parent}, 7)
			}
			if scenario.nested {
				err = repositoryWriteErrorWithWork(ctx, work, repo, write)
			} else {
				err = write(ctx)
			}
			if !errors.Is(err, scenario.want) || storage.writes != scenario.writes || !reflect.DeepEqual(metadata.calls, scenario.calls) {
				test.Fatalf("error=%v writes=%d calls=%v", err, storage.writes, metadata.calls)
			}
		})
	}
}

func TestSnapshotGraftCommandsRechecksCurrentMembership(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	repositoryID := domain.NewID("ws_")
	metadata := &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir()), get: func(context.Context, domain.ContentHash) (domain.Repo, error) {
		return domain.Repo{ID: repo, RepositoryID: repositoryID}, nil
	}}
	if err := metadata.CreateRepository(test.Context(), domain.Repository{ID: repositoryID, OwnerID: "owner", Slug: "graft", Name: "Graft"}); err != nil {
		test.Fatal(err)
	}
	if err := metadata.AddMember(test.Context(), domain.Membership{RepositoryID: repositoryID, UserID: "member", Role: domain.RoleMember}); err != nil {
		test.Fatal(err)
	}
	storage := &snapshotGraftStoreProbe{test: test, repo: repo, observe: func(ctx context.Context) {
		if ctx.Value(wave42TransactionKey{}) != "write" {
			test.Fatal("authorized graft escaped transaction")
		}
	}}
	commands, err := snapshotGraftsForWork(storage, newRepositoryWork(metadata, metadata, nil))
	if err != nil {
		test.Fatal(err)
	}
	ctx := inbound.WithRepositoryActor(test.Context(), "member")
	target, parent := domain.HashContent([]byte("head")), domain.HashContent([]byte("parent"))
	if err := commands.GraftSnapshotParents(ctx, repo, target, []domain.ContentHash{parent}, 0); err != nil {
		test.Fatal(err)
	}
	if err := metadata.RemoveMember(test.Context(), repositoryID, "member"); err != nil {
		test.Fatal(err)
	}
	if err := commands.GraftSnapshotParents(ctx, repo, target, []domain.ContentHash{parent}, 0); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("cached authorization survived membership removal", err)
	}
	if storage.writes != 1 || wave42Count(metadata.calls, "graph") != 1 || wave42Count(metadata.calls, "lock:"+repositoryID+":member") != 2 {
		test.Fatal("revoked replay changed graph or skipped current access lock", storage.writes, metadata.calls)
	}
}

func TestSnapshotGraftCommandsCapturedOwnerCASReplayCycle(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	service := NewService(storage, storage, nil, nil, storage)
	commands, err := service.SnapshotGraftCommands()
	if err != nil {
		test.Fatal(err)
	}
	*service = Service{}
	ctx := inbound.WithSystemActor(test.Context())
	repo := domain.HashContent([]byte(test.Name()))
	target := domain.HashContent([]byte("target"))
	first := domain.HashContent([]byte("first"))
	second := domain.HashContent([]byte("second"))
	for _, id := range []domain.ContentHash{target, first, second} {
		if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id, Branch: "main", Provider: "claude"}); err != nil {
			test.Fatal(err)
		}
	}
	for _, operation := range []struct {
		target   domain.ContentHash
		parent   domain.ContentHash
		expected uint64
		want     error
	}{
		{target, first, 0, nil},
		{target, first, 0, nil},
		{target, second, 0, domain.ErrConflict},
		{target, second, 1, nil},
		{target, first, 0, domain.ErrConflict},
		{first, target, 0, domain.ErrConflict},
	} {
		if err := commands.GraftSnapshotParents(ctx, repo, operation.target, []domain.ContentHash{operation.parent}, operation.expected); !errors.Is(err, operation.want) {
			test.Fatalf("operation=%+v error=%v", operation, err)
		}
	}
	actual, err := storage.GetSnapshot(ctx, repo, target)
	if err != nil || actual.GraftSeq != 2 || len(actual.Parents) != 0 || !reflect.DeepEqual(actual.GraftParents, []domain.ContentHash{first, second}) {
		test.Fatal("replay changed graph or sequence", actual, err)
	}
	actual, err = storage.GetSnapshot(ctx, repo, first)
	if err != nil || actual.GraftSeq != 0 || len(actual.GraftParents) != 0 {
		test.Fatal("rejected cycle was persisted", actual, err)
	}
}
