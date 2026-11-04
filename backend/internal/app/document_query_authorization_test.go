package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentNativeAuthorizationFixture struct {
	scope   *documentQueryScope
	storage *store.FSStore
	ctx     context.Context
	repo    domain.ContentHash
	gate    *resourceAdmission
}

func newDocumentNativeAuthorizationFixture(test *testing.T) documentNativeAuthorizationFixture {
	test.Helper()
	storage := store.NewFSStore(test.TempDir())
	repo := hh(test.Name())
	if _, err := storage.PutRepo(systemTestContext(), domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	if _, transactional := any(storage).(documentQuerySnapshots); transactional {
		test.Fatal("native authorization fixture unexpectedly implements SQL snapshots")
	}
	ctx, gate := admissionTestContext(time.Second)
	ctx = inbound.WithRepositoryActor(ctx, "native-reader")
	scope := &documentQueryScope{metadata: storage, supported: []domain.DocumentIdentity{domain.DocumentIdentityLegacy},
		requireAuthorization: true}
	return documentNativeAuthorizationFixture{scope: scope, storage: storage, ctx: ctx, repo: repo, gate: gate}
}

func documentNativeAuthorizationPermit(test *testing.T, ctx context.Context) (context.Context, *resourcePermit, func()) {
	test.Helper()
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(release)
	permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
	if permit == nil || !permit.live.Load() {
		test.Fatal("native authorization fixture has no live resource permit")
	}
	return bound, permit, release
}

type documentNativeAuthorizationValueKey struct{}

func TestDocumentNativeAuthorizationReusesOnlyReturnedContextOnSameLivePermit(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	calls := 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
		calls++
		if repo != fixture.repo {
			return bound, domain.ErrForbidden
		}
		return context.WithValue(bound, documentNativeAuthorizationValueKey{}, "current native grant"), nil
	})
	bound, permit, release := documentNativeAuthorizationPermit(test, ctx)
	authorized, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
	if err != nil || calls != 1 {
		test.Fatal("initial native authorization failed", calls, err)
	}
	reads := 0
	read := func(current context.Context) error {
		reads++
		if current.Value(resourcePermitKey{}) != permit || !permit.live.Load() ||
			current.Value(documentNativeAuthorizationValueKey{}) != "current native grant" {
			return errors.New("native read lost its admitted authorization context")
		}
		return nil
	}
	for range 3 {
		if _, err := fixture.scope.AuthorizeDocumentRead(authorized, fixture.repo); err != nil {
			test.Fatal(err)
		}
		if err := fixture.scope.WithinDocumentRead(authorized, fixture.repo, read); err != nil {
			test.Fatal(err)
		}
		if err := fixture.scope.WithinDocumentSnapshot(authorized, fixture.repo, read); err != nil {
			test.Fatal(err)
		}
	}
	fixture.gate.mu.Lock()
	active, bulk := fixture.gate.active, fixture.gate.bulk
	fixture.gate.mu.Unlock()
	if calls != 1 || reads != 6 || active != 1 || bulk != 1 || !permit.live.Load() {
		test.Fatal("same native scope repeated authorization or changed the caller's permit", calls, reads, active, bulk)
	}
	if _, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo); err != nil || calls != 2 {
		test.Fatal("a grant leaked into the caller context that never received its marker", calls, err)
	}
	release()
	assertResourcesReleased(test, fixture.gate)
}

func TestDocumentNativeAuthorizationInvalidatesActorSystemScopeAndRepository(test *testing.T) {
	for _, change := range []string{"actor", "native to system", "system to native", "scope", "repository"} {
		test.Run(change, func(test *testing.T) {
			fixture := newDocumentNativeAuthorizationFixture(test)
			ctx := fixture.ctx
			if change == "native to system" {
				ctx = inbound.WithRepositoryActor(ctx, "")
			} else if change == "system to native" {
				ctx = inbound.WithSystemActor(ctx)
			}
			allowed, calls := true, 0
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
				calls++
				if !allowed {
					return bound, domain.ErrForbidden
				}
				return bound, nil
			})
			bound, permit, release := documentNativeAuthorizationPermit(test, ctx)
			authorized, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
			if err != nil || calls != 1 {
				test.Fatal("initial native grant failed", calls, err)
			}
			current, scope, repo := authorized, fixture.scope, fixture.repo
			switch change {
			case "actor":
				current = inbound.WithRepositoryActor(current, "another-reader")
			case "native to system":
				current = inbound.WithSystemActor(current)
			case "system to native":
				current = inbound.WithRepositoryActor(current, "")
			case "scope":
				other := *fixture.scope
				scope = &other
			case "repository":
				repo = hh("another native document repository")
			}
			allowed = false
			read := false
			err = scope.WithinDocumentSnapshot(current, repo, func(context.Context) error { read = true; return nil })
			if !errors.Is(err, domain.ErrForbidden) || calls != 2 || read || current.Value(resourcePermitKey{}) != permit || !permit.live.Load() {
				test.Fatal("changed native authority reused a different binding's marker", change, calls, read, err)
			}
			release()
			assertResourcesReleased(test, fixture.gate)
		})
	}
}

func TestDocumentNativeAuthorizationCallbackRebindingInvalidatesSamePermitGrant(test *testing.T) {
	for _, binding := range []string{"different closure captures", "same callback rebound"} {
		test.Run(binding, func(test *testing.T) {
			fixture := newDocumentNativeAuthorizationFixture(test)
			allowed, allowCalls, denyCalls := true, 0, 0
			factory := func(grant bool) inbound.DocumentReadAuthorization {
				return func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
					if grant && allowed && repo == fixture.repo {
						allowCalls++
						return bound, nil
					}
					denyCalls++
					return bound, domain.ErrForbidden
				}
			}
			callback := factory(true)
			ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, callback)
			bound, permit, release := documentNativeAuthorizationPermit(test, ctx)
			authorized, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
			if err != nil || allowCalls != 1 || denyCalls != 0 {
				test.Fatal("initial callback did not issue its native grant", allowCalls, denyCalls, err)
			}
			if _, err := fixture.scope.AuthorizeDocumentRead(authorized, fixture.repo); err != nil || allowCalls != 1 {
				test.Fatal("unchanged callback binding did not reuse its live grant", allowCalls, err)
			}
			replacement := factory(false)
			if binding == "same callback rebound" {
				allowed = false
				replacement = callback
			}
			rebound := inbound.WithDocumentReadAuthorizer(authorized, replacement)
			actor, system := inbound.RepositoryActor(authorized)
			currentActor, currentSystem := inbound.RepositoryActor(rebound)
			if rebound.Value(resourcePermitKey{}) != permit || actor != currentActor || system != currentSystem || !permit.live.Load() {
				test.Fatal("callback-only negative control changed an unrelated marker binding")
			}
			if _, err := fixture.scope.AuthorizeDocumentRead(rebound, fixture.repo); !errors.Is(err, domain.ErrForbidden) || denyCalls != 1 || allowCalls != 1 {
				test.Fatal("new authorizer binding reused a prior callback's grant", binding, allowCalls, denyCalls, err)
			}
			read := false
			err = fixture.scope.WithinDocumentSnapshot(rebound, fixture.repo, func(context.Context) error { read = true; return nil })
			if !errors.Is(err, domain.ErrForbidden) || denyCalls != 2 || read {
				test.Fatal("snapshot read bypassed the callback-only authority replacement", denyCalls, read, err)
			}
			release()
			assertResourcesReleased(test, fixture.gate)
		})
	}
}

func TestDocumentNativeAuthorizationRejectsClosedPermitAndReauthorizesFreshPermit(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	calls := 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		calls++
		return bound, nil
	})
	bound, permit, release := documentNativeAuthorizationPermit(test, ctx)
	authorized, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
	if err != nil || calls != 1 {
		test.Fatal(err, calls)
	}
	release()
	assertResourcesReleased(test, fixture.gate)
	if _, err := fixture.scope.AuthorizeDocumentRead(authorized, fixture.repo); !errors.Is(err, domain.ErrResourceOverloaded) || calls != 1 {
		test.Fatal("closed native permit reached the callback or reused its grant", calls, err)
	}
	read := false
	if err := fixture.scope.WithinDocumentSnapshot(authorized, fixture.repo, func(context.Context) error { read = true; return nil }); !errors.Is(err, domain.ErrResourceOverloaded) || read || calls != 1 {
		test.Fatal("closed native snapshot scope reached authorization or payload", calls, read, err)
	}
	fresh, freshPermit, releaseFresh := documentNativeAuthorizationPermit(test, authorized)
	if freshPermit == permit || permit.live.Load() || freshPermit.owner != fixture.gate {
		test.Fatal("fresh admission reused a closed native permit")
	}
	current, err := fixture.scope.AuthorizeDocumentRead(fresh, fixture.repo)
	if err != nil || calls != 2 {
		test.Fatal("old marker authorized a newly acquired permit", calls, err)
	}
	if _, err := fixture.scope.AuthorizeDocumentRead(current, fixture.repo); err != nil || calls != 2 {
		test.Fatal("fresh native grant did not remain local to its own live permit", calls, err)
	}
	releaseFresh()
	assertResourcesReleased(test, fixture.gate)
}

func TestDocumentNativeAuthorizationDoesNotCacheAcrossRequestsOrWithoutPermit(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	calls := 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		calls++
		return bound, nil
	})
	current := ctx
	for expected := 1; expected <= 2; expected++ {
		var err error
		current, err = fixture.scope.AuthorizeDocumentRead(current, fixture.repo)
		if err != nil || calls != expected {
			test.Fatal("native authority was cached without a resource permit", calls, err)
		}
	}
	var previous *resourcePermit
	for expected := 3; expected <= 4; expected++ {
		err := fixture.scope.WithinDocumentRead(ctx, fixture.repo, func(bound context.Context) error {
			permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
			if permit == nil || !permit.live.Load() || permit == previous || previous != nil && previous.live.Load() {
				return errors.New("independent native requests shared a permit lifetime")
			}
			previous = permit
			return nil
		})
		if err != nil || calls != expected || previous == nil || previous.live.Load() {
			test.Fatal("independent native request reused another request's authority", calls, err)
		}
		assertResourcesReleased(test, fixture.gate)
	}
	denials := 0
	denied := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		denials++
		return bound, domain.ErrForbidden
	})
	read := false
	if err := fixture.scope.WithinDocumentRead(denied, fixture.repo, func(context.Context) error { read = true; return nil }); !errors.Is(err, domain.ErrForbidden) || denials != 1 || read || calls != 4 {
		test.Fatal("same actor and repository reused global authority from an earlier request", calls, denials, read, err)
	}
	assertResourcesReleased(test, fixture.gate)
}

func TestDocumentNativeAuthorizationFailureAndCancellationNeverGrantOrLeak(test *testing.T) {
	for _, failure := range []string{"authorization", "callback cancellation", "read failure"} {
		test.Run(failure, func(test *testing.T) {
			fixture := newDocumentNativeAuthorizationFixture(test)
			cause := errors.New("native document scope failure")
			allowed, calls, reads := false, 0, 0
			base, cancel := context.WithCancel(fixture.ctx)
			defer cancel()
			ctx := inbound.WithDocumentReadAuthorizer(base, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
				calls++
				if !allowed {
					if failure == "authorization" {
						return bound, cause
					}
					if failure == "callback cancellation" {
						cancel()
						return bound, bound.Err()
					}
				}
				return bound, nil
			})
			var saved context.Context
			err := fixture.scope.WithinDocumentRead(ctx, fixture.repo, func(bound context.Context) error {
				reads++
				saved = bound
				return cause
			})
			want := cause
			if failure == "callback cancellation" {
				want = context.Canceled
			}
			wantReads := 0
			if failure == "read failure" {
				wantReads = 1
			}
			if !errors.Is(err, want) || calls != 1 || reads != wantReads {
				test.Fatal("failed native scope changed its error or executed unauthorized payload", calls, reads, err)
			}
			assertResourcesReleased(test, fixture.gate)
			if saved != nil {
				if _, err := fixture.scope.AuthorizeDocumentRead(saved, fixture.repo); !errors.Is(err, domain.ErrResourceOverloaded) || calls != 1 {
					test.Fatal("failed read retained a usable native grant", calls, err)
				}
			}
			if failure == "callback cancellation" {
				if _, err := fixture.scope.AuthorizeDocumentRead(ctx, fixture.repo); !errors.Is(err, context.Canceled) || calls != 1 {
					test.Fatal("canceled native request reinvoked authorization", calls, err)
				}
				return
			}
			allowed = true
			if err := fixture.scope.WithinDocumentRead(ctx, fixture.repo, func(context.Context) error { reads++; return nil }); err != nil || calls != 2 {
				test.Fatal("retry inherited a failed grant instead of obtaining new authority", calls, err)
			}
			assertResourcesReleased(test, fixture.gate)
		})
	}
}

func TestDocumentNativeAuthorizationFailedCallbackDoesNotMarkReturnedContext(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	allowed, calls := false, 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		calls++
		if !allowed {
			return bound, domain.ErrForbidden
		}
		return bound, nil
	})
	bound, _, release := documentNativeAuthorizationPermit(test, ctx)
	rejected, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
	if !errors.Is(err, domain.ErrForbidden) || rejected == nil || calls != 1 {
		test.Fatal("failed callback did not preserve the rejected native context", calls, err)
	}
	allowed = true
	current, err := fixture.scope.AuthorizeDocumentRead(rejected, fixture.repo)
	if err != nil || calls != 2 {
		test.Fatal("failed callback left a reusable native authorization marker", calls, err)
	}
	if _, err := fixture.scope.AuthorizeDocumentRead(current, fixture.repo); err != nil || calls != 2 {
		test.Fatal("new successful callback did not issue its own live grant", calls, err)
	}
	release()
	assertResourcesReleased(test, fixture.gate)
}

func TestDocumentNativeAuthorizationCancellationPrecedesLiveMarkerReuse(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	calls := 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		calls++
		return bound, nil
	})
	bound, _, release := documentNativeAuthorizationPermit(test, ctx)
	authorized, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	for _, deadline := range []bool{false, true} {
		var current context.Context
		var cancel context.CancelFunc
		want := context.Canceled
		if deadline {
			current, cancel = context.WithDeadline(authorized, time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			current, cancel = context.WithCancel(authorized)
			cancel()
		}
		read := false
		err := fixture.scope.WithinDocumentSnapshot(current, fixture.repo, func(context.Context) error { read = true; return nil })
		cancel()
		if !errors.Is(err, want) || calls != 1 || read {
			test.Fatal("live native marker hid a canceled context", deadline, calls, read, err)
		}
	}
	release()
	assertResourcesReleased(test, fixture.gate)
}

func TestDocumentAuthorizationSnapshotCallbackAlwaysRechecksNativeGrant(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	generalCalls, snapshotCalls := 0, 0
	ctx := inbound.WithDocumentReadAuthorizer(fixture.ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		generalCalls++
		return bound, nil
	})
	bound, permit, release := documentNativeAuthorizationPermit(test, ctx)
	authorized, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	allowed := true
	current := inbound.WithDocumentSnapshotReadAuthorizer(authorized, func(read context.Context, repo domain.ContentHash) (context.Context, error) {
		snapshotCalls++
		if repo != fixture.repo || read.Value(resourcePermitKey{}) != permit || !allowed {
			return read, domain.ErrForbidden
		}
		return read, nil
	})
	reads := 0
	if err := fixture.scope.WithinDocumentSnapshot(current, fixture.repo, func(context.Context) error { reads++; return nil }); err != nil {
		test.Fatal(err)
	}
	allowed = false
	if err := fixture.scope.WithinDocumentSnapshot(current, fixture.repo, func(context.Context) error { reads++; return nil }); !errors.Is(err, domain.ErrForbidden) || generalCalls != 1 || snapshotCalls != 2 || reads != 1 {
		test.Fatal("native marker replaced current snapshot authorization", generalCalls, snapshotCalls, reads, err)
	}
	release()
	assertResourcesReleased(test, fixture.gate)
}

func TestDocumentAuthorizationTransactionalScopeNeverReusesNativeMarker(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	generalCalls := 0
	ctx := inbound.WithDocumentReadAuthorizer(inbound.WithSystemActor(fixture.ctx), func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		generalCalls++
		return bound, nil
	})
	bound, permit, release := documentNativeAuthorizationPermit(test, ctx)
	authorized, err := fixture.scope.AuthorizeDocumentRead(bound, fixture.repo)
	if err != nil || generalCalls != 1 {
		test.Fatal(err, generalCalls)
	}
	transactions := &graphReadTransactions{FSStore: fixture.storage}
	fixture.scope.transactions, fixture.scope.presence = transactions, transactions
	for expected := 2; expected <= 3; expected++ {
		if _, err := fixture.scope.AuthorizeDocumentRead(authorized, fixture.repo); err != nil || generalCalls != expected {
			test.Fatal("transactional scope reused a native-only grant", generalCalls, err)
		}
	}
	allowed, snapshotCalls, reads := true, 0, 0
	current := inbound.WithDocumentSnapshotReadAuthorizer(authorized, func(read context.Context, repo domain.ContentHash) (context.Context, error) {
		snapshotCalls++
		if !transactions.InTransaction(read) || read.Value(resourcePermitKey{}) != permit || repo != fixture.repo {
			return read, errors.New("snapshot authorization escaped the current transactional read")
		}
		if !allowed {
			return read, domain.ErrForbidden
		}
		return read, nil
	})
	if err := fixture.scope.WithinDocumentSnapshot(current, fixture.repo, func(context.Context) error { reads++; return nil }); err != nil {
		test.Fatal(err)
	}
	allowed = false
	if err := fixture.scope.WithinDocumentSnapshot(current, fixture.repo, func(context.Context) error { reads++; return nil }); !errors.Is(err, domain.ErrForbidden) || snapshotCalls != 2 || reads != 1 || transactions.reads.Load() != 2 || transactions.active.Load() != 0 {
		test.Fatal("transactional snapshot reused cached authority or retained a read", snapshotCalls, reads, err)
	}
	release()
	assertResourcesReleased(test, fixture.gate)
}

type documentOwnershipPanicStore struct {
	outbound.MetadataStore
	outbound.BlobStore
}

func (*documentOwnershipPanicStore) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	panic("independent full queries dereferenced replacement Service metadata")
}

func (*documentOwnershipPanicStore) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	panic("independent full queries dereferenced replacement Service snapshots")
}

func (*documentOwnershipPanicStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	panic("independent full queries dereferenced replacement Service documents")
}

func (*documentOwnershipPanicStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	panic("independent full queries dereferenced replacement Service manifests")
}

func (*documentOwnershipPanicStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	panic("independent full queries dereferenced replacement Service chunks")
}

func TestDocumentQueriesRetainSuccessfulNativeResponseUntilOwnerRelease(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	document := seedLegacyFindDocument(test, fixture.storage, fixture.repo, []domain.CIREvent{historyMessage(domain.RoleUser, "retained needle \ud55c🙂")})
	queries, err := NewService(fixture.storage, fixture.storage, nil, nil, nil).DocumentQueries()
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name string
		read func(context.Context) error
	}{
		{name: "view", read: func(ctx context.Context) error {
			_, err := queries.ReadDocViewEvents(ctx, fixture.repo, document.Hash, "", 0, 1, "all")
			return err
		}},
		{name: "view fragment", read: func(ctx context.Context) error {
			_, err := queries.ReadDocViewFragment(ctx, fixture.repo, document.Hash, 0, 0, 4096)
			return err
		}},
		{name: "legacy find", read: func(ctx context.Context) error {
			_, err := queries.FindDocPage(ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1})
			return err
		}},
		{name: "legacy find fragment", read: func(ctx context.Context) error {
			_, err := queries.ReadDocFindFragment(ctx, fixture.repo, document.Hash, domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4096})
			return err
		}},
	} {
		for _, retain := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/retain=%t", scenario.name, retain), func(test *testing.T) {
				var releases []func()
				ctx := inbound.WithDocumentResponseRetainer(inbound.WithSystemActor(fixture.ctx), func(release func()) bool {
					if retain {
						releases = append(releases, release)
					}
					return retain
				})
				defer func() {
					for _, release := range releases {
						release()
					}
				}()
				if err := scenario.read(ctx); err != nil {
					test.Fatal("actual native document query failed", err)
				}
				fixture.gate.mu.Lock()
				active := fixture.gate.active
				fixture.gate.mu.Unlock()
				if retain {
					if len(releases) != 1 || active != 1 {
						test.Fatal("successful read did not transfer exactly one live permit", len(releases), active)
					}
					releases[0]()
				}
				assertResourcesReleased(test, fixture.gate)
			})
		}
	}
}

func TestDocumentQueriesFullNativeViewAndFindOwnOriginalDependencies(test *testing.T) {
	fixture := newDocumentNativeAuthorizationFixture(test)
	document := seedLegacyFindDocument(test, fixture.storage, fixture.repo, []domain.CIREvent{historyMessage(domain.RoleUser, "captured needle \ud55c🙂")})
	if err := fixture.storage.PutSnapshot(systemTestContext(), domain.Snapshot{ID: document.Hash, RepoID: fixture.repo,
		DocHash: document.Hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	service := NewService(fixture.storage, fixture.storage, nil, nil, nil)
	queries, err := service.DocumentQueries()
	if err != nil || queries == nil {
		test.Fatal("construct full native document queries", err)
	}
	if _, independent := queries.(*documentquery.Queries); !independent {
		test.Fatalf("full document query accessor returned its parent Service: %T", queries)
	}
	ctx := inbound.WithSystemActor(fixture.ctx)
	if page, err := queries.ReadDocEvents(ctx, fixture.repo, document.Hash, "", 0, 1); err != nil || len(page.Events) != 1 {
		test.Fatal("initial captured query could not read the native document", page, err)
	}
	assertResourcesReleased(test, fixture.gate)
	replacement := &documentOwnershipPanicStore{}
	service.meta, service.blobs = replacement, replacement
	page, err := queries.ReadDocViewEvents(ctx, fixture.repo, document.Hash, "", 0, 1, "all")
	if err != nil || len(page.Events) != 1 || page.Total != 1 || page.Events[0].Deferred || !reflect.DeepEqual(page.Events[0].Event, document.CIR.Events[0]) {
		test.Fatal("captured full queries lost their original native view dependencies", page, err)
	}
	assertResourcesReleased(test, fixture.gate)
	fragment, err := queries.ReadDocViewFragment(ctx, fixture.repo, document.Hash, 0, 0, 4096)
	var viewed domain.CIREvent
	if err != nil || fragment.Next != -1 || json.Unmarshal([]byte(fragment.JSON), &viewed) != nil || !reflect.DeepEqual(viewed, document.CIR.Events[0]) {
		test.Fatal("captured full queries lost their original view fragment dependencies", fragment, err)
	}
	assertResourcesReleased(test, fixture.gate)
	found, err := queries.FindDocPage(ctx, fixture.repo, document.Hash, domain.DocFindPageRequest{Legacy: true, Query: "needle", Total: 1, To: 1})
	if err != nil || found.Projection != domain.DocLegacyFindProjection || found.Hash != document.Hash || len(found.Matches) != 1 || found.Matches[0].Index != 0 {
		test.Fatal("captured full queries lost their original legacy find dependencies", found, err)
	}
	assertResourcesReleased(test, fixture.gate)
	findFragment, err := queries.ReadDocFindFragment(ctx, fixture.repo, document.Hash, domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4096})
	var revealed domain.CIREvent
	if err != nil || findFragment.Projection != domain.DocLegacyFindFragmentProjection || findFragment.Next != -1 ||
		json.Unmarshal([]byte(findFragment.JSON), &revealed) != nil || !reflect.DeepEqual(revealed, document.CIR.Events[0]) {
		test.Fatal("captured full queries lost their original find fragment dependencies", findFragment, err)
	}
	assertResourcesReleased(test, fixture.gate)
	visible, err := queries.ReadDocVisiblePage(ctx, fixture.repo, document.Hash, domain.DocVisiblePageRequest{View: "all", Limit: 1})
	if !errors.Is(err, domain.ErrDocVisibleProjectionUnsupported) || !reflect.DeepEqual(visible, domain.DocVisiblePage{}) {
		test.Fatal("native FS visible projection invented support or followed replacement ports", visible, err)
	}
	assertResourcesReleased(test, fixture.gate)
}
