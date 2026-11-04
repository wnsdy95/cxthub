package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryStateHTTPQueries struct {
	revision      domain.RepositoryRevision
	state         domain.RepositoryChangeState
	pending       domain.PendingStateView
	err           error
	read          func(context.Context, domain.ContentHash)
	revisionCalls atomic.Int32
	stateCalls    atomic.Int32
	pendingCalls  atomic.Int32
}

func (queries *repositoryStateHTTPQueries) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	queries.revisionCalls.Add(1)
	if queries.read != nil {
		queries.read(ctx, repo)
	}
	return queries.revision, queries.err
}

func (queries *repositoryStateHTTPQueries) RepositoryChangeState(ctx context.Context, repo domain.ContentHash) (domain.RepositoryChangeState, error) {
	queries.stateCalls.Add(1)
	if queries.read != nil {
		queries.read(ctx, repo)
	}
	return queries.state, queries.err
}

func (queries *repositoryStateHTTPQueries) GetPendingState(ctx context.Context, repo domain.ContentHash) (domain.PendingStateView, error) {
	queries.pendingCalls.Add(1)
	if queries.read != nil {
		queries.read(ctx, repo)
	}
	return queries.pending, queries.err
}

type repositoryStatePoisonedBackend struct{ Backend }

func (*repositoryStatePoisonedBackend) RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
	panic("repository revision read reached the broad backend")
}

func (*repositoryStatePoisonedBackend) RepositoryChangeState(context.Context, domain.ContentHash) (domain.RepositoryChangeState, error) {
	panic("repository change state read reached the broad backend")
}

func (*repositoryStatePoisonedBackend) GetPendingState(context.Context, domain.ContentHash) (domain.PendingStateView, error) {
	panic("On Hold read reached the broad backend")
}

func TestSetRepositoryStateQueriesRejectsNil(test *testing.T) {
	for _, profile := range []inbound.RepositoryStateQueryProfile{0, 99} {
		server := NewServer(nil, nil)
		if err := server.SetRepositoryStateQueries(&repositoryStateHTTPQueries{}, profile); !errors.Is(err, domain.ErrValidation) || server.repositoryStates != nil || server.repositoryStateProfile != 0 {
			test.Fatal("missing/unknown repository query profile was accepted", err)
		}
	}
	var typedNil *repositoryStateHTTPQueries
	for _, fixture := range []struct {
		name    string
		queries inbound.RepositoryStateQueries
	}{
		{name: "nil"},
		{name: "typed nil", queries: typedNil},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			server := NewServer(nil, nil)
			if err := server.SetRepositoryStateQueries(fixture.queries, inbound.RepositoryStateCompatibility); !errors.Is(err, domain.ErrValidation) || server.repositoryStates != nil {
				test.Fatal("nil repository state queries were accepted or installed", err)
			}
			configured := &repositoryStateHTTPQueries{}
			if err := server.SetRepositoryStateQueries(configured, inbound.RepositoryStateCompatibility); err != nil {
				test.Fatal(err)
			}
			if err := server.SetRepositoryStateQueries(fixture.queries, inbound.RepositoryStateCompatibility); !errors.Is(err, domain.ErrValidation) || server.repositoryStates != configured {
				test.Fatal("rejected queries replaced the configured repository state owner", err)
			}
		})
	}
}

func TestRepositoryStateHTTPRequiresExplicitQueries(test *testing.T) {
	for _, fixture := range []struct {
		path string
		code string
	}{
		{path: "/pending-view?projection=on-hold-v1", code: "pending_projection_unavailable"},
		{path: "/revision", code: "catalog_unsupported"},
		{path: "/changes", code: "catalog_unsupported"},
	} {
		test.Run(fixture.path, func(test *testing.T) {
			server := NewServer(&repositoryStatePoisonedBackend{}, nil)
			request := httptest.NewRequest(http.MethodGet, fixture.path, nil)
			response := httptest.NewRecorder()
			switch request.URL.Path {
			case "/pending-view":
				server.pendingView(response, request)
			case "/revision":
				server.repositoryRevision(response, request)
			case "/changes":
				server.repositoryChanges(response, request)
			}
			if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), `"code":"`+fixture.code+`"`) ||
				response.Header().Get("Retry-After") != "" || strings.Contains(response.Body.String(), "event: revision") || len(server.changes.repos) != 0 {
				test.Fatal("unconfigured state query used a backend fallback or lost unsupported semantics", response.Code, response.Body)
			}
		})
	}
}

func TestRepositoryStateHTTPUsesExplicitOwner(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	status := domain.PendingProjectionStatus{State: "catching_up", SourceEpoch: "12345678-abcd-4321-9876-123456789abc:42", Rules: 1}
	queries := &repositoryStateHTTPQueries{
		revision: domain.RepositoryRevision{Graph: 999, Pending: 999},
		state: domain.RepositoryChangeState{
			RepositoryRevision: domain.RepositoryRevision{Graph: 17, Pending: 19}, PendingProjection: &status,
		},
		pending: domain.PendingStateView{Projection: "on-hold-v1", DefaultBranch: "explicit-owner"},
	}
	server := NewServer(&repositoryStatePoisonedBackend{}, nil)
	if err := server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
		test.Fatal(err)
	}
	contextKey := struct{ name string }{"repository state request"}
	parent, cancel := context.WithTimeout(context.WithValue(context.Background(), contextKey, "retained"), 30*time.Second)
	defer cancel()
	parent = inbound.WithRepositoryActor(inbound.WithDocumentIdentities(parent, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}), "request-actor")
	var observed context.Context
	queries.read = func(ctx context.Context, requested domain.ContentHash) {
		observed = ctx
		if requested != repo || ctx.Value(contextKey) != "retained" || ctx.Err() != nil {
			test.Fatal("explicit state owner lost the request context or repository", requested, ctx.Err())
		}
		if actor, system := inbound.RepositoryActor(ctx); actor != "request-actor" || system {
			test.Fatal("state query lost the request actor", actor, system)
		}
		if identities := inbound.DocumentIdentities(ctx); len(identities) != 1 || identities[0] != domain.DocumentIdentityRootV1 {
			test.Fatal("state query lost document identity negotiation", identities)
		}
		if deadline, ok := ctx.Deadline(); !ok {
			test.Fatal("state query lost the request deadline")
		} else if original, _ := parent.Deadline(); !deadline.Equal(original) {
			test.Fatal("state query extended the request deadline", deadline, original)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/revision", nil).WithContext(parent)
	request.SetPathValue("repoID", string(repo))
	response := httptest.NewRecorder()
	server.repositoryRevision(response, request)
	var state domain.RepositoryChangeState
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil || response.Code != http.StatusOK || !state.Equal(queries.state) {
		test.Fatal("revision response lost explicit owner metadata", response.Code, response.Body, err)
	}
	verifyContext := queries.read
	queries.read = func(ctx context.Context, requested domain.ContentHash) {
		verifyContext(ctx, requested)
		authorize := inbound.DocumentReadAuthorizer(ctx)
		if authorize == nil {
			test.Fatal("On Hold query has no post-admission document authorizer")
		}
		denied, err := authorize(ctx, domain.HashContent([]byte("different repository")))
		if !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("On Hold authorizer accepted a different repository", err)
		}
		if actor, system := inbound.RepositoryActor(denied); actor != "" || system {
			test.Fatal("On Hold authorizer trusted the inherited actor", actor, system)
		}
		_, release, err := app.AcquireResources(ctx, app.ResourceInteractive)
		if err != nil {
			test.Fatal("On Hold query lost interactive admission", err)
		}
		defer release()
	}
	request = httptest.NewRequest(http.MethodGet, "/pending-view?projection=on-hold-v1", nil).WithContext(parent)
	request.SetPathValue("repoID", string(repo))
	response = httptest.NewRecorder()
	server.pendingView(response, request)
	var pending domain.PendingStateView
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		test.Fatal(err)
	}
	_, fullGraph := fields["graph"]
	if err := json.Unmarshal(response.Body.Bytes(), &pending); err != nil || response.Code != http.StatusOK || pending.Projection != "on-hold-v1" || pending.DefaultBranch != "explicit-owner" || fullGraph {
		test.Fatal("On Hold response did not come from the explicit owner", response.Code, response.Body, err)
	}
	if observed == nil || !errors.Is(observed.Err(), context.Canceled) || queries.stateCalls.Load() != 1 || queries.pendingCalls.Load() != 1 || queries.revisionCalls.Load() != 0 {
		test.Fatal("state reads leaked admission or used the legacy revision fallback", observed, queries.stateCalls.Load(), queries.pendingCalls.Load(), queries.revisionCalls.Load())
	}
}

type repositoryStateStreamWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (writer *repositoryStateStreamWriter) Flush() {
	writer.ResponseRecorder.Flush()
	writer.cancel()
}

func TestRepositoryStateSSEUsesExplicitOwner(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	status := domain.PendingProjectionStatus{State: "ready", SourceEpoch: "12345678-abcd-4321-9876-123456789abc:42", Rules: 1, Generation: domain.HashContent([]byte("ready"))}
	queries := &repositoryStateHTTPQueries{state: domain.RepositoryChangeState{
		RepositoryRevision: domain.RepositoryRevision{Graph: 23, Pending: 29}, PendingProjection: &status,
	}}
	var admissionReads, watchReads atomic.Int32
	queries.read = func(ctx context.Context, requested domain.ContentHash) {
		if requested != repo {
			test.Error("SSE queried a different repository", requested)
		}
		if identities := inbound.DocumentIdentities(ctx); len(identities) != 1 || identities[0] != domain.DocumentIdentityRootV1 {
			test.Error("SSE poll lost its negotiated identity class", identities)
		}
		actor, system := inbound.RepositoryActor(ctx)
		if system {
			test.Error("SSE acquired system authority")
		}
		switch actor {
		case "request-actor":
			admissionReads.Add(1)
		case "":
			watchReads.Add(1)
		default:
			test.Error("SSE poll borrowed unexpected authority", actor)
		}
	}
	server := NewServer(&repositoryStatePoisonedBackend{}, nil)
	if err := server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = inbound.WithRepositoryActor(inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}), "request-actor")
	request := httptest.NewRequest(http.MethodGet, "/changes", nil).WithContext(ctx)
	request.SetPathValue("repoID", string(repo))
	response := &repositoryStateStreamWriter{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	server.repositoryChanges(response, request)
	encoded, err := json.Marshal(queries.state)
	if err != nil {
		test.Fatal(err)
	}
	expected := "id: " + string(domain.HashContent(encoded)) + "\nevent: revision\ndata: " + string(encoded) + "\n\n"
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Accel-Buffering") != "no" || response.Body.String() != expected {
		test.Fatal("SSE lost explicit owner metadata or wire semantics", response.Code, response.Header(), response.Body)
	}
	server.changes.mu.Lock()
	defer server.changes.mu.Unlock()
	if queries.stateCalls.Load() < 2 || admissionReads.Load() != 1 || watchReads.Load() == 0 || queries.pendingCalls.Load() != 0 || queries.revisionCalls.Load() != 0 || len(server.changes.repos) != 0 {
		test.Fatal("SSE did not use the explicit state owner for admission and polling or leaked its observer", queries.stateCalls.Load(), len(server.changes.repos))
	}
}

func TestPendingStateHTTPDeferredAuthorizerMatchesProfile(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	for _, fixture := range []struct {
		name            string
		profile         inbound.RepositoryStateQueryProfile
		documentProfile inbound.DocumentQueryProfile
	}{
		{name: "native compatibility", profile: inbound.RepositoryStateCompatibility},
		{name: "compatibility with unrelated document profile", profile: inbound.RepositoryStateCompatibility, documentProfile: inbound.DocumentQueryVerifiedRoots},
		{name: "pinned without document query", profile: inbound.RepositoryStatePinnedSnapshots},
		{name: "pinned with unrelated document profile", profile: inbound.RepositoryStatePinnedSnapshots, documentProfile: inbound.DocumentQueryCompatibility},
	} {
		for _, revoked := range []bool{false, true} {
			name := fixture.name
			if revoked {
				name += "/revoked"
			}
			test.Run(name, func(test *testing.T) {
				policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
				identity := &docAllHTTPIdentity{test: test, docVisiblePolicyIdentity: &docVisiblePolicyIdentity{
					repository: domain.Repository{ID: "repository", Visibility: domain.VisibilityPrivate}, user: domain.User{ID: "reader"}, role: domain.RoleViewer,
				}}
				queries := &repositoryStateHTTPQueries{pending: domain.PendingStateView{Projection: "on-hold-v1"}}
				bodyReads := 0
				queries.read = func(ctx context.Context, requested domain.ContentHash) {
					if requested != repo || policy.calls != 1 || identity.resolveCalls != 1 {
						test.Fatal("On Hold authorization ran before its query admission or initial guard", requested, policy.calls, identity.resolveCalls)
					}
					authorize := inbound.DocumentReadAuthorizer(ctx)
					if authorize == nil {
						test.Fatal("On Hold query did not receive its deferred authorizer")
					}
					verified := fixture.profile == inbound.RepositoryStatePinnedSnapshots
					if revoked && !verified {
						identity.role = ""
					}
					bound, err := authorize(inbound.WithSystemActor(ctx), repo)
					if err == nil {
						snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
						if verified {
							if snapshot == nil || identity.admissions != 1 || identity.resolveCalls != 2 {
								test.Fatal("verified On Hold read lost general admission before snapshot authorization", identity.admissions, identity.resolveCalls)
							}
							if revoked {
								identity.snapshotErr = domain.ErrForbidden
							}
							bound, err = snapshot(context.WithValue(inbound.WithSystemActor(bound), docAllHTTPSnapshotKey{}, true), repo)
						} else if snapshot != nil || identity.admissions != 0 {
							test.Fatal("native or compatibility On Hold read adopted snapshot-only identity policy")
						}
					}
					queries.err = err
					if err == nil {
						if actor, system := inbound.RepositoryActor(bound); actor != "reader" || system {
							test.Fatal("On Hold authorization retained stale authority", actor, system)
						}
						bodyReads++
					}
				}
				server := NewServer(&repositoryStatePoisonedBackend{Backend: policy}, identity)
				if fixture.documentProfile != 0 {
					if err := server.SetDocumentEventQueries(&documentEventHTTPQueries{}, fixture.documentProfile); err != nil {
						test.Fatal(err)
					}
				}
				if err := server.SetRepositoryStateQueries(queries, fixture.profile); err != nil {
					test.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/pending-view?projection=on-hold-v1", nil)
				request.Header.Set("Authorization", "Bearer session")
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				expectedStatus, expectedReads, expectedSnapshots := http.StatusOK, 1, 0
				if revoked {
					expectedStatus, expectedReads = http.StatusForbidden, 0
				}
				if fixture.profile == inbound.RepositoryStatePinnedSnapshots {
					expectedSnapshots = 1
				}
				if response.Code != expectedStatus || bodyReads != expectedReads || queries.pendingCalls.Load() != 1 || identity.resolveCalls != 2 ||
					identity.admissions != expectedSnapshots || identity.snapshotResolves != expectedSnapshots || identity.snapshotPermits != expectedSnapshots {
					test.Fatal("On Hold lost profile-specific current authorization", response.Code, response.Body, bodyReads,
						identity.resolveCalls, identity.admissions, identity.snapshotResolves, identity.snapshotPermits)
				}
			})
		}
	}
}
