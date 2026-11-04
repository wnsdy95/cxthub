package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/graphwire"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type repositoryGraphHTTPCall struct {
	method   string
	repo     domain.ContentHash
	position string
}

type repositoryGraphHTTPQueries struct {
	view    domain.RepositoryView
	pending domain.PendingView
	graph   domain.GraphState
	cause   error
	read    func(context.Context) error
	calls   []repositoryGraphHTTPCall
}

func (queries *repositoryGraphHTTPQueries) record(ctx context.Context, method string, repo domain.ContentHash, position string) error {
	queries.calls = append(queries.calls, repositoryGraphHTTPCall{method: method, repo: repo, position: position})
	if queries.read != nil {
		return queries.read(ctx)
	}
	return queries.cause
}

func (queries *repositoryGraphHTTPQueries) GetRepositoryView(ctx context.Context, repo domain.ContentHash) (domain.RepositoryView, error) {
	return queries.view, queries.record(ctx, "view", repo, "")
}

func (queries *repositoryGraphHTTPQueries) GetPendingView(ctx context.Context, repo domain.ContentHash) (domain.PendingView, error) {
	return queries.pending, queries.record(ctx, "pending-view", repo, "")
}

func (queries *repositoryGraphHTTPQueries) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	return queries.graph, queries.record(ctx, "graph-state", repo, position)
}

func repositoryGraphHTTPFixture() *repositoryGraphHTTPQueries {
	generation := &domain.GraphReadGeneration{
		Generation:  domain.HashContent([]byte("explicit graph generation")),
		SourceEpoch: "12345678-abcd-4321-9876-123456789abc:7", AppliedEpoch: "12345678-abcd-4321-9876-123456789abc:5",
		Rules: 2, Complete: true, Current: false,
	}
	snapshot := domain.HashContent([]byte("explicit graph snapshot"))
	graph := domain.GraphState{
		Version: 1, ReadGeneration: generation, PrimaryBranch: "explicit-owner", PositionEvent: "selected-position",
		Revision: domain.RepositoryRevision{Graph: 17, Pending: 19}, SnapshotIDs: []domain.ContentHash{snapshot},
		BranchSnapshots: map[string][]domain.ContentHash{"explicit-owner": {snapshot}},
		BranchContexts:  map[string]domain.BranchContext{"explicit-owner": {Roots: []domain.ContentHash{snapshot}, SnapshotIDs: []domain.ContentHash{snapshot}}},
	}
	return &repositoryGraphHTTPQueries{
		graph: graph,
		view:  domain.RepositoryView{Graph: &graph, ReadGeneration: generation, Revision: graph.Revision, DefaultBranch: "explicit-owner"},
		pending: domain.PendingView{Graph: &graph, ReadGeneration: generation, Revision: graph.Revision,
			Pending: []domain.Pending{{SessionID: "explicit-pending"}}},
	}
}

type repositoryGraphPoisonedBackend struct{ Backend }

func (*repositoryGraphPoisonedBackend) GetRepositoryView(context.Context, domain.ContentHash) (domain.RepositoryView, error) {
	panic("repository view read reached the broad backend")
}

func (*repositoryGraphPoisonedBackend) GetPendingView(context.Context, domain.ContentHash) (domain.PendingView, error) {
	panic("legacy pending view read reached the broad backend")
}

func (*repositoryGraphPoisonedBackend) QueryGraphState(context.Context, domain.ContentHash, string) (domain.GraphState, error) {
	panic("graph state read reached the broad backend")
}

func repositoryGraphHTTPHandlers(server *Server) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{"view": server.repositoryView, "pending-view": server.pendingView, "graph-state": server.graphState}
}

func TestSetRepositoryGraphQueriesIsAtomic(test *testing.T) {
	var typedNil *repositoryGraphHTTPQueries
	for _, initialProfile := range []inbound.RepositoryGraphQueryProfile{0, inbound.RepositoryGraphCompatibility, inbound.RepositoryGraphPinnedSnapshots} {
		for _, fixture := range []struct {
			name    string
			queries inbound.RepositoryGraphQueries
			profile inbound.RepositoryGraphQueryProfile
		}{
			{name: "missing profile", queries: &repositoryGraphHTTPQueries{}},
			{name: "unknown profile", queries: &repositoryGraphHTTPQueries{}, profile: 99},
			{name: "nil compatibility", profile: inbound.RepositoryGraphCompatibility},
			{name: "nil pinned", profile: inbound.RepositoryGraphPinnedSnapshots},
			{name: "typed nil compatibility", queries: typedNil, profile: inbound.RepositoryGraphCompatibility},
			{name: "typed nil pinned", queries: typedNil, profile: inbound.RepositoryGraphPinnedSnapshots},
		} {
			test.Run(fmt.Sprintf("initial=%d/%s", initialProfile, fixture.name), func(test *testing.T) {
				server := NewServer(nil, nil)
				var original inbound.RepositoryGraphQueries
				if initialProfile != 0 {
					original = &repositoryGraphHTTPQueries{}
					if err := server.SetRepositoryGraphQueries(original, initialProfile); err != nil {
						test.Fatal(err)
					}
				}
				if err := server.SetRepositoryGraphQueries(fixture.queries, fixture.profile); !errors.Is(err, domain.ErrValidation) ||
					server.repositoryGraphs != original || server.repositoryGraphProfile != initialProfile {
					test.Fatal("rejected graph query configuration changed its owner or profile", err, server.repositoryGraphProfile)
				}
			})
		}
	}
	server := NewServer(nil, nil)
	for _, profile := range []inbound.RepositoryGraphQueryProfile{inbound.RepositoryGraphCompatibility, inbound.RepositoryGraphPinnedSnapshots} {
		queries := &repositoryGraphHTTPQueries{}
		if err := server.SetRepositoryGraphQueries(queries, profile); err != nil || server.repositoryGraphs != queries || server.repositoryGraphProfile != profile {
			test.Fatal("valid graph query configuration was not installed", err)
		}
	}
}

func TestRepositoryGraphHTTPRequiresExplicitQueries(test *testing.T) {
	server := NewServer(&repositoryGraphPoisonedBackend{}, nil)
	if err := server.SetRepositoryStateQueries(&repositoryStateHTTPQueries{}, inbound.RepositoryStatePinnedSnapshots); err != nil {
		test.Fatal(err)
	}
	if err := server.SetDocumentQueries(&docAllHTTPQueries{}, inbound.DocumentQueryVerifiedRoots); err != nil {
		test.Fatal(err)
	}
	for route, handler := range repositoryGraphHTTPHandlers(server) {
		test.Run(route, func(test *testing.T) {
			response := httptest.NewRecorder()
			handler(response, httptest.NewRequest(http.MethodGet, "/"+route, nil))
			if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), `"code":"graph_query_unavailable"`) ||
				!strings.Contains(response.Body.String(), "repository graph query unavailable") || response.Header().Get("Retry-After") != "" ||
				server.repositoryGraphs != nil || server.repositoryGraphProfile != 0 {
				test.Fatal("unconfigured graph read discovered a backend fallback or lost its named 501", response.Code, response.Body)
			}
		})
	}
}

func TestRepositoryGraphHTTPUsesExplicitOwner(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	for _, encoding := range []string{"indexed-v1", "indexed-v2"} {
		for _, route := range []string{"view", "pending-view", "graph-state"} {
			test.Run(route+"/"+encoding, func(test *testing.T) {
				queries := repositoryGraphHTTPFixture()
				server := NewServer(&repositoryGraphPoisonedBackend{}, nil)
				if err := server.SetRepositoryGraphQueries(queries, inbound.RepositoryGraphCompatibility); err != nil {
					test.Fatal(err)
				}
				contextKey := struct{ name string }{"graph request"}
				parent, cancel := context.WithTimeout(context.WithValue(context.Background(), contextKey, "retained"), 30*time.Second)
				defer cancel()
				parent = inbound.WithRepositoryActor(inbound.WithDocumentIdentities(parent, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}), "request-actor")
				var observed context.Context
				queries.read = func(ctx context.Context) error {
					observed = ctx
					if ctx.Value(contextKey) != "retained" || ctx.Err() != nil {
						test.Fatal("graph query lost its request context", ctx.Err())
					}
					if actor, system := inbound.RepositoryActor(ctx); actor != "request-actor" || system {
						test.Fatal("graph query lost its request actor", actor, system)
					}
					if identities := inbound.DocumentIdentities(ctx); len(identities) != 1 || identities[0] != domain.DocumentIdentityRootV1 {
						test.Fatal("graph query lost document identity negotiation", identities)
					}
					deadline, present := ctx.Deadline()
					if original, _ := parent.Deadline(); !present || !deadline.Equal(original) {
						test.Fatal("graph query changed its original deadline", deadline, original)
					}
					authorize := inbound.DocumentReadAuthorizer(ctx)
					if authorize == nil {
						test.Fatal("graph query has no deferred current-rights authorizer")
					}
					denied, err := authorize(inbound.WithSystemActor(ctx), domain.HashContent([]byte("foreign repository")))
					if !errors.Is(err, domain.ErrForbidden) {
						test.Fatal("graph authorizer accepted a foreign repository", err)
					}
					if actor, system := inbound.RepositoryActor(denied); actor != "" || system {
						test.Fatal("graph authorizer retained inherited authority", actor, system)
					}
					_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
					if err != nil {
						test.Fatal("graph query lost response-scoped admission", err)
					}
					defer release()
					return nil
				}
				request := httptest.NewRequest(http.MethodGet, "/"+route+"?position=selected-position&graph_encoding="+encoding, nil).WithContext(parent)
				request.SetPathValue("repoID", string(repo))
				response := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder()}
				repositoryGraphHTTPHandlers(server)[route](response, request)
				expectedCall := repositoryGraphHTTPCall{method: route, repo: repo}
				if route == "graph-state" {
					expectedCall.position = "selected-position"
				}
				if response.Code != http.StatusOK || !reflect.DeepEqual(queries.calls, []repositoryGraphHTTPCall{expectedCall}) ||
					observed == nil || !errors.Is(observed.Err(), context.Canceled) || !response.checked || !errors.Is(response.err, context.DeadlineExceeded) {
					test.Fatal("graph read lost explicit dispatch, request lifetime or admission through body writing", response.Code, response.Body, queries.calls, response.err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
					test.Fatal(err)
				}
				graphBody := response.Body.Bytes()
				if route != "graph-state" {
					graphBody = fields["graph"]
					var generation domain.GraphReadGeneration
					if err := json.Unmarshal(fields["read_generation"], &generation); err != nil || generation != *queries.graph.ReadGeneration {
						test.Fatal("graph envelope lost generation metadata", response.Body, err)
					}
					if route == "view" && string(fields["default_branch"]) != `"explicit-owner"` ||
						route == "pending-view" && !strings.Contains(string(fields["pending"]), "explicit-pending") {
						test.Fatal("graph envelope did not come from its explicit query", response.Body)
					}
				}
				var expectedGraph any = graphwire.Encode(queries.graph)
				if encoding == "indexed-v2" {
					expectedGraph = graphwire.EncodeV2(queries.graph)
				}
				encoded, err := json.Marshal(expectedGraph)
				if err != nil {
					test.Fatal(err)
				}
				if strings.TrimSpace(string(graphBody)) != string(encoded) {
					test.Fatal("graph wire encoding or generation metadata changed", response.Body)
				}
			})
		}
	}
}

func TestRepositoryGraphHTTPErrorContract(test *testing.T) {
	for _, fixture := range []struct {
		cause  error
		status int
		code   string
		retry  string
	}{
		{cause: domain.ErrValidation, status: http.StatusUnprocessableEntity, code: "validation"},
		{cause: domain.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
		{cause: domain.ErrForbidden, status: http.StatusForbidden, code: "forbidden"},
		{cause: domain.ErrUnauthorized, status: http.StatusUnauthorized, code: "unauthenticated"},
		{cause: domain.ErrResourceOverloaded, status: http.StatusServiceUnavailable, code: "resource_overloaded", retry: "1"},
		{cause: domain.ErrGraphProjectionNotReady, status: http.StatusServiceUnavailable, code: "graph_projection_pending", retry: "1"},
		{cause: fmt.Errorf("wrapped: %w", domain.ErrGraphProjectionNotReady), status: http.StatusServiceUnavailable, code: "graph_projection_pending", retry: "1"},
		{cause: domain.ErrGraphProjectionFailed, status: http.StatusServiceUnavailable, code: "graph_projection_failed"},
		{cause: errors.Join(domain.ErrGraphProjectionFailed, domain.ErrGraphProjectionNotReady), status: http.StatusServiceUnavailable, code: "graph_projection_failed"},
	} {
		for _, route := range []string{"view", "pending-view", "graph-state"} {
			test.Run(route+"/"+fixture.cause.Error(), func(test *testing.T) {
				queries := &repositoryGraphHTTPQueries{cause: fixture.cause}
				server := NewServer(&repositoryGraphPoisonedBackend{}, nil)
				if err := server.SetRepositoryGraphQueries(queries, inbound.RepositoryGraphCompatibility); err != nil {
					test.Fatal(err)
				}
				response := httptest.NewRecorder()
				repositoryGraphHTTPHandlers(server)[route](response, httptest.NewRequest(http.MethodGet, "/"+route, nil))
				if response.Code != fixture.status || response.Header().Get("Retry-After") != fixture.retry ||
					!strings.Contains(response.Body.String(), `"code":"`+fixture.code+`"`) || len(queries.calls) != 1 {
					test.Fatal("narrow graph query changed error, retry or dispatch semantics", response.Code, response.Header(), response.Body, queries.calls)
				}
			})
		}
	}
}

func TestRepositoryGraphHTTPAuthorizationProfileIsIndependent(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	for _, graphProfile := range []inbound.RepositoryGraphQueryProfile{inbound.RepositoryGraphCompatibility, inbound.RepositoryGraphPinnedSnapshots} {
		for _, stateProfile := range []inbound.RepositoryStateQueryProfile{0, inbound.RepositoryStateCompatibility, inbound.RepositoryStatePinnedSnapshots} {
			for _, documentProfile := range []inbound.DocumentQueryProfile{0, inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
				for _, route := range []string{"view", "pending-view", "graph-state"} {
					for _, revoked := range []bool{false, true} {
						test.Run(fmt.Sprintf("%s/graph=%d/state=%d/document=%d/revoked=%t", route, graphProfile, stateProfile, documentProfile, revoked), func(test *testing.T) {
							policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
							identity := &docAllHTTPIdentity{test: test, docVisiblePolicyIdentity: &docVisiblePolicyIdentity{
								repository: domain.Repository{ID: "repository", Visibility: domain.VisibilityPrivate}, user: domain.User{ID: "reader"}, role: domain.RoleViewer,
							}}
							queries := repositoryGraphHTTPFixture()
							bodyReads := 0
							queries.read = func(ctx context.Context) error {
								if policy.calls != 1 || identity.resolveCalls != 1 {
									test.Fatal("graph authorization ran before query admission or the initial guard", policy.calls, identity.resolveCalls)
								}
								authorize := inbound.DocumentReadAuthorizer(ctx)
								if authorize == nil {
									test.Fatal("graph query did not receive its deferred authorizer")
								}
								pinned := graphProfile == inbound.RepositoryGraphPinnedSnapshots
								if revoked && !pinned {
									identity.role = ""
								}
								bound, err := authorize(inbound.WithSystemActor(ctx), repo)
								if err != nil {
									return err
								}
								snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
								if pinned {
									if snapshot == nil || identity.admissions != 1 || identity.resolveCalls != 2 {
										test.Fatal("pinned graph profile lost its pre-snapshot identity admission", identity.admissions, identity.resolveCalls)
									}
									if revoked {
										identity.snapshotErr = domain.ErrForbidden
									}
									bound, err = snapshot(context.WithValue(inbound.WithSystemActor(bound), docAllHTTPSnapshotKey{}, true), repo)
								} else if snapshot != nil || identity.admissions != 0 {
									test.Fatal("compatibility graph profile borrowed unrelated snapshot identity policy")
								}
								if err != nil {
									return err
								}
								if actor, system := inbound.RepositoryActor(bound); actor != "reader" || system {
									test.Fatal("graph authorization retained stale authority", actor, system)
								}
								bodyReads++
								return nil
							}
							server := NewServer(&repositoryGraphPoisonedBackend{Backend: policy}, identity)
							states := &repositoryStateHTTPQueries{}
							documents := &docAllHTTPQueries{}
							if err := server.SetRepositoryGraphQueries(queries, graphProfile); err != nil {
								test.Fatal(err)
							}
							if stateProfile != 0 {
								if err := server.SetRepositoryStateQueries(states, stateProfile); err != nil {
									test.Fatal(err)
								}
							}
							if documentProfile != 0 {
								if err := server.SetDocumentQueries(documents, documentProfile); err != nil {
									test.Fatal(err)
								}
							}
							request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/"+route, nil)
							request.Header.Set("Authorization", "Bearer session")
							response := httptest.NewRecorder()
							server.Handler().ServeHTTP(response, request)
							expectedStatus, expectedReads, expectedSnapshots := http.StatusOK, 1, 0
							if revoked {
								expectedStatus, expectedReads = http.StatusForbidden, 0
							}
							if graphProfile == inbound.RepositoryGraphPinnedSnapshots {
								expectedSnapshots = 1
							}
							if response.Code != expectedStatus || bodyReads != expectedReads || len(queries.calls) != 1 || queries.calls[0].repo != repo ||
								identity.resolveCalls != 2 || identity.admissions != expectedSnapshots || identity.snapshotResolves != expectedSnapshots || identity.snapshotPermits != expectedSnapshots ||
								states.stateCalls.Load()+states.revisionCalls.Load()+states.pendingCalls.Load() != 0 || len(documents.calls) != 0 {
								test.Fatal("graph authorization followed an unrelated query owner or profile", response.Code, response.Body, bodyReads,
									identity.resolveCalls, identity.admissions, identity.snapshotResolves, identity.snapshotPermits)
							}
						})
					}
				}
			}
		}
	}
}

func TestRepositoryGraphProfileDoesNotChangeOnHoldOrNotices(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	for _, graphProfile := range []inbound.RepositoryGraphQueryProfile{0, inbound.RepositoryGraphCompatibility, inbound.RepositoryGraphPinnedSnapshots} {
		for _, stateProfile := range []inbound.RepositoryStateQueryProfile{inbound.RepositoryStateCompatibility, inbound.RepositoryStatePinnedSnapshots} {
			test.Run(fmt.Sprintf("graph=%d/state=%d", graphProfile, stateProfile), func(test *testing.T) {
				policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
				identity := &docAllHTTPIdentity{test: test, docVisiblePolicyIdentity: &docVisiblePolicyIdentity{
					repository: domain.Repository{ID: "repository", Visibility: domain.VisibilityPrivate}, user: domain.User{ID: "reader"}, role: domain.RoleViewer,
				}}
				queries := repositoryGraphHTTPFixture()
				states := &repositoryStateHTTPQueries{
					pending: domain.PendingStateView{Projection: "on-hold-v1", DefaultBranch: "state-owner"},
					state:   domain.RepositoryChangeState{RepositoryRevision: domain.RepositoryRevision{Graph: 23, Pending: 29}},
				}
				states.read = func(ctx context.Context, requested domain.ContentHash) {
					if requested != repo {
						test.Fatal("On Hold query lost its repository", requested)
					}
					authorize := inbound.DocumentReadAuthorizer(ctx)
					if authorize == nil {
						test.Fatal("On Hold query lost its deferred authorizer")
					}
					bound, err := authorize(ctx, repo)
					if err != nil {
						test.Fatal(err)
					}
					snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
					if stateProfile == inbound.RepositoryStatePinnedSnapshots {
						if snapshot == nil {
							test.Fatal("On Hold query borrowed the graph compatibility profile")
						}
						if _, err := snapshot(context.WithValue(bound, docAllHTTPSnapshotKey{}, true), repo); err != nil {
							test.Fatal(err)
						}
					} else if snapshot != nil {
						test.Fatal("On Hold query borrowed the pinned graph profile")
					}
				}
				server := NewServer(&repositoryGraphPoisonedBackend{Backend: policy}, identity)
				if graphProfile != 0 {
					if err := server.SetRepositoryGraphQueries(queries, graphProfile); err != nil {
						test.Fatal(err)
					}
				}
				if err := server.SetRepositoryStateQueries(states, stateProfile); err != nil {
					test.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/pending-view?projection=on-hold-v1", nil)
				request.Header.Set("Authorization", "Bearer session")
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"default_branch":"state-owner"`) || len(queries.calls) != 0 || states.pendingCalls.Load() != 1 {
					test.Fatal("graph configuration changed the On Hold state owner", response.Code, response.Body, queries.calls)
				}
				states.read = nil
				notice, err := server.readRepositoryChangeState(context.Background(), repo)
				if err != nil || !notice.Equal(states.state) || states.stateCalls.Load() != 1 || states.revisionCalls.Load() != 0 || len(queries.calls) != 0 {
					test.Fatal("graph configuration changed the current notice owner", notice, err)
				}
			})
		}
	}
}
