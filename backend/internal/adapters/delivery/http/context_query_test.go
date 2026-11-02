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

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type contextHTTPQueryFunc func(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error)

func (query contextHTTPQueryFunc) QueryContext(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	return query(ctx, repo, selection)
}

type contextHTTPPoisonedBackend struct{ Backend }

func (*contextHTTPPoisonedBackend) QueryContext(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error) {
	panic("context query reached the broad HTTP backend")
}

func TestSetContextQueryHTTPRejectsInvalidConfiguration(test *testing.T) {
	var nilPointer *contextHTTPPoisonedBackend
	var nilFunction contextHTTPQueryFunc
	valid := contextHTTPQueryFunc(func(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error) {
		return domain.ContextQueryView{}, nil
	})
	for _, installed := range []bool{false, true} {
		for _, rejected := range []struct {
			name    string
			query   inbound.ContextQuery
			profile inbound.ContextQueryProfile
		}{
			{name: "nil", profile: inbound.ContextQueryCompatibility},
			{name: "typed pointer", query: nilPointer, profile: inbound.ContextQueryCompatibility},
			{name: "typed function", query: nilFunction, profile: inbound.ContextQueryCompatibility},
			{name: "missing profile", query: valid},
			{name: "unknown profile", query: valid, profile: 255},
		} {
			test.Run(fmt.Sprintf("installed=%t/%s", installed, rejected.name), func(test *testing.T) {
				server := NewServer(nil, nil)
				owner := &contextHTTPPoisonedBackend{}
				if installed {
					if err := server.SetContextQuery(owner, inbound.ContextQueryPinnedSnapshots); err != nil {
						test.Fatal(err)
					}
				}
				if err := server.SetContextQuery(rejected.query, rejected.profile); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("invalid query configuration accepted", err)
				}
				if installed && (server.contextQueries != owner || server.contextQueryProfile != inbound.ContextQueryPinnedSnapshots) ||
					!installed && (server.contextQueries != nil || server.contextQueryProfile != 0) {
					test.Fatal("rejected configuration replaced the previous owner or profile")
				}
			})
		}
	}
}

func TestContextQueryHTTPRequiresExplicitPort(test *testing.T) {
	server := NewServer(&contextHTTPPoisonedBackend{}, nil)
	response := httptest.NewRecorder()
	server.contextQuery(response, httptest.NewRequest(http.MethodGet, "/context-query", nil))
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), `"code":"context_query_unavailable"`) || response.Header().Get("Retry-After") != "" {
		test.Fatal("absent context query lost its named 501 or used a backend fallback", response.Code, response.Body)
	}
}

func TestContextQueryHTTPUsesInjectedOwnerAndRetainsAdmission(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	selection := domain.ContextSelection{Branch: "logical", Position: "main", Scope: "current", CodeCommit: strings.Repeat("a", 40),
		SegmentLimit: 1, SegmentOffset: 2, SegmentStateHash: domain.HashContent([]byte("state"))}
	view := domain.ContextQueryView{Version: 1, Branch: "explicit-owner", Position: repo, StateHash: selection.SegmentStateHash,
		Revision: domain.RepositoryRevision{Graph: 7, Pending: 11}, Snapshots: []domain.Snapshot{}, History: []domain.HistoryEvent{}}
	requestKey := struct{ name string }{"context request"}
	parent, cancel := context.WithTimeout(context.WithValue(context.Background(), requestKey, "retained"), 30*time.Second)
	defer cancel()
	parent = inbound.WithRepositoryActor(inbound.WithDocumentIdentities(parent, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}), "request-actor")
	var observed context.Context
	calls := 0
	query := contextHTTPQueryFunc(func(ctx context.Context, requested domain.ContentHash, actual domain.ContextSelection) (domain.ContextQueryView, error) {
		observed = ctx
		calls++
		if requested != repo || actual != selection || ctx.Value(requestKey) != "retained" || ctx.Err() != nil {
			test.Fatal("query lost its exact selection or request context", requested, actual, ctx.Err())
		}
		if actor, system := inbound.RepositoryActor(ctx); actor != "request-actor" || system {
			test.Fatal("query lost its original actor", actor, system)
		}
		if identities := inbound.DocumentIdentities(ctx); len(identities) != 1 || identities[0] != domain.DocumentIdentityRootV1 {
			test.Fatal("query lost document identity negotiation", identities)
		}
		deadline, present := ctx.Deadline()
		if original, _ := parent.Deadline(); !present || !deadline.Equal(original) {
			test.Fatal("query extended the request deadline", deadline, original)
		}
		authorize := inbound.DocumentReadAuthorizer(ctx)
		if authorize == nil {
			test.Fatal("query has no post-admission authorizer")
		}
		denied, err := authorize(inbound.WithSystemActor(ctx), domain.HashContent([]byte("foreign repository")))
		if actor, system := inbound.RepositoryActor(denied); !errors.Is(err, domain.ErrForbidden) || actor != "" || system {
			test.Fatal("authorizer accepted foreign repository or retained stale authority", actor, system, err)
		}
		_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
		if err != nil {
			test.Fatal(err)
		}
		defer release()
		return view, nil
	})
	server := NewServer(&contextHTTPPoisonedBackend{}, nil)
	if err := server.SetContextQuery(query, inbound.ContextQueryCompatibility); err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/context-query?branch=logical&position=main&scope=current&code_commit="+selection.CodeCommit+
		"&segment_limit=1&segment_offset=2&segment_state_hash="+string(selection.SegmentStateHash), nil).WithContext(parent)
	request.SetPathValue("repoID", string(repo))
	response := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	server.contextQuery(response, request)
	var actual domain.ContextQueryView
	if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil || response.Code != http.StatusOK || !reflect.DeepEqual(actual, view) || calls != 1 {
		test.Fatal("explicit context owner changed its response", response.Code, response.Body, actual, err)
	}
	if observed == nil || !errors.Is(observed.Err(), context.Canceled) || !response.checked || !errors.Is(response.err, context.DeadlineExceeded) {
		test.Fatal("query admission did not cover the final response or release", response.err)
	}
	probe, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	_, release, err := app.AcquireResources(probe, app.ResourceBulk)
	if err != nil {
		test.Fatal("response leaked admission", err)
	}
	release()
}

func TestContextQueryHTTPParseAndErrorsDoNotFallBack(test *testing.T) {
	for _, fixture := range []struct {
		cause  error
		status int
		code   string
		retry  string
	}{
		{domain.ErrValidation, http.StatusUnprocessableEntity, "validation", ""},
		{domain.ErrNotFound, http.StatusNotFound, "not_found", ""},
		{domain.ErrConflict, http.StatusConflict, "conflict", ""},
		{domain.ErrUnauthorized, http.StatusUnauthorized, "unauthenticated", ""},
		{domain.ErrForbidden, http.StatusForbidden, "forbidden", ""},
		{domain.ErrResourceOverloaded, http.StatusServiceUnavailable, "resource_overloaded", "1"},
	} {
		test.Run(fixture.cause.Error(), func(test *testing.T) {
			calls := 0
			query := contextHTTPQueryFunc(func(context.Context, domain.ContentHash, domain.ContextSelection) (domain.ContextQueryView, error) {
				calls++
				return domain.ContextQueryView{}, fixture.cause
			})
			server := NewServer(&contextHTTPPoisonedBackend{}, nil)
			if err := server.SetContextQuery(query, inbound.ContextQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			for _, field := range []string{"segment_limit", "segment_offset"} {
				response := httptest.NewRecorder()
				server.contextQuery(response, httptest.NewRequest(http.MethodGet, "/context-query?"+field+"=invalid", nil))
				if response.Code != http.StatusUnprocessableEntity || calls != 0 {
					test.Fatal("invalid integer reached the query or changed parse semantics", response.Code, calls)
				}
			}
			response := httptest.NewRecorder()
			server.contextQuery(response, httptest.NewRequest(http.MethodGet, "/context-query", nil))
			if response.Code != fixture.status || response.Header().Get("Retry-After") != fixture.retry ||
				!strings.Contains(response.Body.String(), `"code":"`+fixture.code+`"`) || calls != 1 {
				test.Fatal("explicit query changed error or retry semantics", response.Code, response.Header(), response.Body, calls)
			}
		})
	}
}

func TestContextQueryHTTPAuthorizationProfileIsIndependent(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	for _, profile := range []inbound.ContextQueryProfile{inbound.ContextQueryCompatibility, inbound.ContextQueryPinnedSnapshots} {
		for _, stateProfile := range []inbound.RepositoryStateQueryProfile{0, inbound.RepositoryStateCompatibility, inbound.RepositoryStatePinnedSnapshots} {
			for _, graphProfile := range []inbound.RepositoryGraphQueryProfile{0, inbound.RepositoryGraphCompatibility, inbound.RepositoryGraphPinnedSnapshots} {
				for _, documentProfile := range []inbound.DocumentQueryProfile{0, inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
					for _, revoked := range []bool{false, true} {
						test.Run(fmt.Sprintf("context=%d/state=%d/graph=%d/document=%d/revoked=%t", profile, stateProfile, graphProfile, documentProfile, revoked), func(test *testing.T) {
							policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
							identity := &docAllHTTPIdentity{test: test, docVisiblePolicyIdentity: &docVisiblePolicyIdentity{
								repository: domain.Repository{ID: "repository", Visibility: domain.VisibilityPrivate}, user: domain.User{ID: "reader"}, role: domain.RoleViewer,
							}}
							calls, reads := 0, 0
							query := contextHTTPQueryFunc(func(ctx context.Context, requested domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
								calls++
								if requested != repo || policy.calls != 1 || identity.resolveCalls != 1 {
									test.Fatal("context query authorization preceded its admission or initial guard")
								}
								authorize := inbound.DocumentReadAuthorizer(ctx)
								if authorize == nil {
									test.Fatal("context query received no current authorizer")
								}
								pinned := profile == inbound.ContextQueryPinnedSnapshots
								if revoked && !pinned {
									identity.role = ""
								}
								bound, err := authorize(inbound.WithSystemActor(ctx), repo)
								if err != nil {
									return domain.ContextQueryView{}, err
								}
								snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
								if pinned {
									if snapshot == nil || identity.admissions != 1 || identity.resolveCalls != 2 {
										test.Fatal("pinned context admission lost current general authentication")
									}
									if revoked {
										identity.snapshotErr = domain.ErrForbidden
									}
									bound, err = snapshot(context.WithValue(inbound.WithSystemActor(bound), docAllHTTPSnapshotKey{}, true), repo)
								} else if snapshot != nil || identity.admissions != 0 {
									test.Fatal("compatibility context query adopted an unrelated pinned profile")
								}
								if err != nil {
									return domain.ContextQueryView{}, err
								}
								if actor, system := inbound.RepositoryActor(bound); actor != "reader" || system {
									test.Fatal("context query retained stale authority", actor, system)
								}
								reads++
								return domain.ContextQueryView{Version: 1}, nil
							})
							server := NewServer(&contextHTTPPoisonedBackend{Backend: policy}, identity)
							if err := server.SetContextQuery(query, profile); err != nil {
								test.Fatal(err)
							}
							states := &repositoryStateHTTPQueries{}
							graphs := repositoryGraphHTTPFixture()
							documents := &docAllHTTPQueries{}
							if stateProfile != 0 {
								if err := server.SetRepositoryStateQueries(states, stateProfile); err != nil {
									test.Fatal(err)
								}
							}
							if graphProfile != 0 {
								if err := server.SetRepositoryGraphQueries(graphs, graphProfile); err != nil {
									test.Fatal(err)
								}
							}
							if documentProfile != 0 {
								if err := server.SetDocumentQueries(documents, documentProfile); err != nil {
									test.Fatal(err)
								}
							}
							request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/context-query", nil)
							request.Header.Set("Authorization", "Bearer session")
							response := httptest.NewRecorder()
							server.Handler().ServeHTTP(response, request)
							status, expectedReads, expectedSnapshots := http.StatusOK, 1, 0
							if revoked {
								status, expectedReads = http.StatusForbidden, 0
							}
							if profile == inbound.ContextQueryPinnedSnapshots {
								expectedSnapshots = 1
							}
							if response.Code != status || reads != expectedReads || calls != 1 || identity.resolveCalls != 2 ||
								identity.admissions != expectedSnapshots || identity.snapshotResolves != expectedSnapshots || identity.snapshotPermits != expectedSnapshots ||
								states.stateCalls.Load()+states.revisionCalls.Load()+states.pendingCalls.Load() != 0 || len(graphs.calls) != 0 || len(documents.calls) != 0 {
								test.Fatal("context query followed unrelated authority or query owners", response.Code, response.Body, reads,
									identity.resolveCalls, identity.admissions, identity.snapshotResolves, identity.snapshotPermits)
							}
						})
					}
				}
			}
		}
	}
}
