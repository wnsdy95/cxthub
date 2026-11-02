//go:build postgres

package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentResponsePGStore struct {
	*store.PostgresStore
	calls  atomic.Int64
	active atomic.Int64
}

func (storage *documentResponsePGStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	storage.calls.Add(1)
	storage.active.Add(1)
	defer storage.active.Add(-1)
	return storage.PostgresStore.WithinReadSnapshot(ctx, read)
}

type documentResponsePGQueries struct {
	inbound.DocumentQueries
	retain func()
}

func (queries documentResponsePGQueries) responseContext(ctx context.Context) context.Context {
	return inbound.WithDocumentResponseRetainer(ctx, func(release func()) bool {
		queries.retain()
		return inbound.RetainDocumentResponse(ctx, release)
	})
}

func (queries documentResponsePGQueries) ReadDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	return queries.DocumentQueries.ReadDocViewEvents(queries.responseContext(ctx), repo, hash, base, offset, limit, view)
}

func (queries documentResponsePGQueries) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	return queries.DocumentQueries.ReadDocViewFragment(queries.responseContext(ctx), repo, hash, index, offset, budget)
}

func (queries documentResponsePGQueries) ReadDocVisiblePage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisiblePage, error) {
	return queries.DocumentQueries.ReadDocVisiblePage(queries.responseContext(ctx), repo, hash, request)
}

func (queries documentResponsePGQueries) FindDocPage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindPageRequest) (domain.DocFindPage, error) {
	return queries.DocumentQueries.FindDocPage(queries.responseContext(ctx), repo, hash, request)
}

func (queries documentResponsePGQueries) ReadDocFindFragment(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindFragmentRequest) (domain.DocFindFragmentPage, error) {
	return queries.DocumentQueries.ReadDocFindFragment(queries.responseContext(ctx), repo, hash, request)
}

func documentResponseAssertBothSlotsReleased(test *testing.T) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	for _, class := range []app.ResourceClass{app.ResourceBulk, app.ResourceInteractive} {
		_, release, err := app.AcquireResources(ctx, class)
		if err != nil {
			test.Fatal("document response leaked a shared execution slot", class, err)
		}
		defer release()
	}
}

func TestPGDocumentHTTPResponseRetainsPermitAfterSnapshotUntilWrite(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	test.Cleanup(cancel)
	ctx := inbound.WithDocumentIdentities(inbound.WithSystemActor(parent), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	owner, session, err := identity.Login(ctx, "dev:"+domain.NewID("document-response-")+"@example.test", "document response owner")
	if err != nil {
		test.Fatal(err)
	}
	repository, err := identity.CreateRepository(ctx, owner, "Document-response")
	if err != nil {
		test.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		test.Fatal(err)
	}
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo.ID),
		SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "retained needle \ud55c🙂"}}},
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: "second needle response"}}},
	}}
	manifest, chunks, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal(err)
	}
	canonical, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := storage.PutChunks(ctx, repo.ID, chunks); err != nil {
		test.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, hash, canonical); err != nil {
		test.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, repo.ID, hash); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: hash, RepoID: repo.ID, DocHash: hash,
		DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo.ID, hash); err != nil {
		test.Fatal(err)
	}
	observed := &documentResponsePGStore{PostgresStore: storage}
	service := app.NewService(observed, observed, nil, nil, observed)
	var visible domain.DocVisiblePage
	for attempt := 0; attempt < 3; attempt++ {
		visible, err = service.ReadDocVisiblePage(ctx, repo.ID, hash, domain.DocVisiblePageRequest{View: "all", Limit: 2})
		if err == nil {
			break
		}
		if !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal(err)
		}
		if err := service.ProcessDocRangeJobs(ctx, 1); err != nil {
			test.Fatal(err)
		}
	}
	if err != nil || visible.ReplayCursor == "" || visible.Total != 2 {
		test.Fatal("actual root visible selection did not become ready", visible, err)
	}
	selection, err := domain.DecodeDocVisibleSelector(visible.ReplayCursor)
	if err != nil {
		test.Fatal(err)
	}
	selection.View, selection.Limit, selection.Start, selection.Next, selection.Unit = "all", 1, 0, 0, 0
	fragmentSelection, err := selection.Encode()
	if err != nil {
		test.Fatal(err)
	}
	queries, err := service.DocumentQueriesForProfile(documentquery.VerifiedRoots)
	if err != nil {
		test.Fatal(err)
	}
	base := "/api/v1/repos/" + url.PathEscape(string(repo.ID)) + "/docs/" + url.PathEscape(string(hash))
	for _, route := range []struct {
		name, path, expected string
	}{
		{name: "root view", path: "/events?view=chat&offset=0&limit=2", expected: `"total":2`},
		{name: "root fragment", path: "/event-fragments?index=0&offset=0&budget=128", expected: "needle"},
		{name: "visible", path: "/events?projection=" + domain.DocVisibleProjection + "&view=all&limit=2", expected: `"total":2`},
		{name: "find", path: "/find?" + url.Values{"q": {"needle"}, "selection": {visible.ReplayCursor}, "from": {"0"}, "to": {"2"}}.Encode(), expected: `"matches":[{`},
		{name: "find fragment", path: "/find-fragments?" + url.Values{"selection": {fragmentSelection}, "offset": {"0"}, "budget": {"128"}}.Encode(), expected: "needle"},
	} {
		for _, outcome := range []string{"complete", "cancel", "write failure", "borrowed"} {
			test.Run(route.name+"/"+outcome, func(test *testing.T) {
				requestCtx, stop := context.WithCancel(parent)
				defer stop()
				releaseBorrowed := func() {}
				if outcome == "borrowed" {
					requestCtx, releaseBorrowed, err = app.AcquireResources(requestCtx, app.ResourceInteractive)
					if err != nil {
						test.Fatal(err)
					}
					defer releaseBorrowed()
				}
				observed.calls.Store(0)
				retentions, writes := 0, 0
				server := NewServer(service, identity)
				boundary := documentResponsePGQueries{DocumentQueries: queries, retain: func() {
					retentions++
					if observed.active.Load() != 0 || observed.calls.Load() == 0 {
						test.Error("response release was transferred before the actual PG snapshot ended", observed.active.Load(), observed.calls.Load())
					}
				}}
				if err := server.SetDocumentQueries(boundary, inbound.DocumentQueryVerifiedRoots); err != nil {
					test.Fatal(err)
				}
				assertHeld := func() {
					probe, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					_, release, err := app.AcquireResources(probe, app.ResourceInteractive)
					if err != nil {
						test.Fatal("remaining execution slot missing", err)
					}
					defer release()
					blocked, finish := context.WithTimeout(context.Background(), 20*time.Millisecond)
					defer finish()
					_, unexpected, err := app.AcquireResources(blocked, app.ResourceInteractive)
					if unexpected != nil {
						unexpected()
					}
					if !errors.Is(err, context.DeadlineExceeded) {
						test.Error("actual root response escaped aggregate admission", err)
					}
				}
				writer := &documentResponseWriter{ResponseRecorder: httptest.NewRecorder(), write: func() {
					writes++
					if observed.active.Load() != 0 || observed.calls.Load() == 0 || retentions != 1 {
						test.Error("actual response did not follow one completed PG read ownership transfer", observed.active.Load(), observed.calls.Load(), retentions)
					}
					assertHeld()
					if outcome == "cancel" {
						stop()
					}
				}}
				if outcome == "write failure" {
					writer.cause = io.ErrClosedPipe
				}
				request := httptest.NewRequest(http.MethodGet, base+route.path, nil).WithContext(requestCtx)
				request.Header.Set("Authorization", "Bearer "+session.Token)
				request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
				server.Handler().ServeHTTP(writer, request)
				if writer.Code != http.StatusOK || writes != 1 || retentions != 1 || observed.active.Load() != 0 {
					test.Fatal("actual PG document response failed", writer.Code, writes, retentions, observed.active.Load(), writer.Body)
				}
				if outcome != "write failure" && (!json.Valid(writer.Body.Bytes()) || !strings.Contains(writer.Body.String(), route.expected)) {
					test.Fatal("response ownership changed the actual document wire payload", writer.Body)
				}
				if outcome == "borrowed" {
					assertHeld()
					releaseBorrowed()
				}
				documentResponseAssertBothSlotsReleased(test)
			})
		}
	}
}
