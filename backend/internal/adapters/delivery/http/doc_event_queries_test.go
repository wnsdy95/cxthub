package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentEventHTTPQueries struct {
	read  func(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int) (domain.DocEventPage, error)
	calls int
}

type documentEventHTTPBackend struct {
	Backend
	inbound.DocumentEventQueries
}

func (queries *documentEventHTTPQueries) ReadDocEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	queries.calls++
	if queries.read != nil {
		return queries.read(ctx, repo, hash, base, offset, limit)
	}
	return domain.DocEventPage{Hash: hash, Next: -1, Events: []domain.CIREvent{}}, nil
}

func (*documentEventHTTPQueries) ReadDocFragments(context.Context, domain.ContentHash, domain.ContentHash, int, int, int, int) (domain.DocFragmentPage, error) {
	panic("HTTP event paging must not invoke fragment queries")
}

func (*documentEventHTTPQueries) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	panic("HTTP event paging must not invoke search queries")
}

func setServiceDocumentEventQueries(test testing.TB, server *Server, service *app.Service) {
	setServiceDocumentQueriesForProfile(test, server, service, inbound.DocumentQueryCompatibility)
}

func setServiceDocumentQueriesForProfile(test testing.TB, server *Server, service *app.Service, profile inbound.DocumentQueryProfile) {
	test.Helper()
	queries, err := service.DocumentQueriesForProfile(profile)
	if err != nil {
		test.Fatal(err)
	}
	if err := server.SetDocumentQueries(queries, profile); err != nil {
		test.Fatal(err)
	}
}

func TestDocumentEventQueriesHTTPRequiresExplicitInjection(test *testing.T) {
	legacy := &docVisibleLegacyBackend{}
	capable := &documentEventHTTPQueries{}
	repo, hash := domain.HashContent([]byte("event repo")), domain.HashContent([]byte("event doc"))
	for _, fixture := range []struct {
		name    string
		backend Backend
	}{
		{name: "legacy event capability", backend: legacy},
		{name: "complete event port capability", backend: &documentEventHTTPBackend{DocumentEventQueries: capable}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			server := NewServer(fixture.backend, nil)
			writer := httptest.NewRecorder()
			server.getDocEvents(writer, docVisibleTestRequest(repo, hash, ""))
			if writer.Code != http.StatusServiceUnavailable || !strings.Contains(writer.Body.String(), `"code":"unavailable"`) ||
				!strings.Contains(writer.Body.String(), "event paging unavailable") || writer.Header().Get("Retry-After") != "" || legacy.eventCalls != 0 || capable.calls != 0 {
				test.Fatalf("unconfigured event paging probed the broad backend or lost its unavailable response: status=%d legacy=%d capable=%d body=%s", writer.Code, legacy.eventCalls, capable.calls, writer.Body)
			}
		})
	}
}

func TestSetDocumentEventQueriesHTTPRejectsNil(test *testing.T) {
	var typedNil *documentEventHTTPQueries
	for _, fixture := range []struct {
		name    string
		queries inbound.DocumentEventQueries
	}{
		{name: "nil"},
		{name: "typed nil", queries: typedNil},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			server := NewServer(nil, nil)
			if err := server.SetDocumentEventQueries(fixture.queries, inbound.DocumentQueryCompatibility); !errors.Is(err, domain.ErrValidation) || server.documentEvents != nil {
				test.Fatal("nil event queries were accepted or installed", err)
			}
			configured := &documentEventHTTPQueries{}
			if err := server.SetDocumentEventQueries(configured, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			if err := server.SetDocumentEventQueries(fixture.queries, inbound.DocumentQueryCompatibility); !errors.Is(err, domain.ErrValidation) || server.documentEvents != configured {
				test.Fatal("rejected event dependency replaced valid explicit configuration", err)
			}
		})
	}
}

func TestSetDocumentEventQueriesHTTPRejectsUnknownProfile(test *testing.T) {
	server := NewServer(nil, nil)
	queries := &documentEventHTTPQueries{}
	if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryVerifiedRoots); err != nil {
		test.Fatal(err)
	}
	for _, profile := range []inbound.DocumentQueryProfile{0, 255} {
		if err := server.SetDocumentEventQueries(&documentEventHTTPQueries{}, profile); !errors.Is(err, domain.ErrValidation) ||
			server.documentEvents != queries || server.documentQueryProfile != inbound.DocumentQueryVerifiedRoots {
			test.Fatal("invalid profile replaced the pinned read owner or authorization mode", profile, err)
		}
	}
}

func TestDocumentEventQueriesHTTPUsesNarrowPortAndPreservesRequest(test *testing.T) {
	repo, hash, base := domain.HashContent([]byte("event repo")), domain.HashContent([]byte("event doc")), domain.HashContent([]byte("event base"))
	for _, fixture := range []struct {
		query         string
		base          domain.ContentHash
		offset, limit int
	}{
		{offset: -1, limit: 50},
		{query: "base=" + string(base) + "&offset=2&limit=1", base: base, offset: 2, limit: 1},
	} {
		test.Run(fixture.query, func(test *testing.T) {
			backend := &docVisibleLegacyBackend{}
			server := NewServer(backend, nil)
			contextKey := struct{ name string }{"event request"}
			parent, cancel := context.WithTimeout(context.WithValue(context.Background(), contextKey, "retained"), 30*time.Second)
			defer cancel()
			page := domain.DocEventPage{Hash: hash, Offset: fixture.offset, Next: -1, Total: 3,
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "explicit event owner"}}}}}
			var observed context.Context
			queries := &documentEventHTTPQueries{read: func(ctx context.Context, requestedRepo, requestedHash, requestedBase domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
				observed = ctx
				if requestedRepo != repo || requestedHash != hash || requestedBase != fixture.base || offset != fixture.offset || limit != fixture.limit || ctx.Value(contextKey) != "retained" || ctx.Err() != nil {
					test.Fatal("event port did not receive the original context, identity or range", requestedRepo, requestedHash, requestedBase, offset, limit)
				}
				if inbound.DocumentReadAuthorizer(ctx) == nil {
					test.Fatal("event port has no post-admission document read authorizer")
				}
				_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
				if err != nil {
					test.Fatal("event query lost the inherited bulk admission", err)
				}
				defer release()
				return page, nil
			}}
			if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			writer := &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder()}
			server.getDocEvents(writer, docVisibleTestRequest(repo, hash, fixture.query).WithContext(parent))
			encoded, err := json.Marshal(page)
			if err != nil {
				test.Fatal(err)
			}
			if writer.Code != http.StatusOK || strings.TrimSpace(writer.Body.String()) != string(encoded) || queries.calls != 1 || backend.eventCalls != 0 || observed == nil {
				test.Fatal("event page used the broad backend or changed its wire contract", writer.Code, writer.Body, queries.calls, backend.eventCalls)
			}
			expectedDeadline, _ := parent.Deadline()
			deadline, present := observed.Deadline()
			if !present || !deadline.Equal(expectedDeadline) || !writer.read.Equal(deadline) || !writer.write.Equal(deadline) || !errors.Is(observed.Err(), context.Canceled) || parent.Err() != nil {
				test.Fatal("event dependency injection changed deadline or cancellation ownership", deadline, writer.read, writer.write, observed.Err(), parent.Err())
			}
		})
	}
}

func TestDocumentEventQueriesHTTPPreservesErrorsAndRetryHeaders(test *testing.T) {
	repo, hash := domain.HashContent([]byte("event repo")), domain.HashContent([]byte("event doc"))
	for _, fixture := range []struct {
		cause  error
		status int
		code   string
		retry  string
	}{
		{domain.ErrValidation, 422, "validation", ""},
		{domain.ErrNotFound, 404, "not_found", ""},
		{domain.ErrUnauthorized, 401, "unauthenticated", ""},
		{domain.ErrForbidden, 403, "forbidden", ""},
		{domain.ErrUnsupportedDocumentIdentity, 409, "unsupported_document_identity", ""},
		{domain.ErrIntegrity, 422, "integrity_violation", ""},
		{domain.ErrDocRangeIndexPending, 503, "doc_range_index_pending", "3"},
		{domain.ErrDocRangeIndexUnavailable, 503, "doc_range_index_unavailable", "1"},
		{domain.ErrResourceOverloaded, 503, "resource_overloaded", "1"},
	} {
		test.Run(fixture.code, func(test *testing.T) {
			server := NewServer(nil, nil)
			queries := &documentEventHTTPQueries{read: func(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int) (domain.DocEventPage, error) {
				return domain.DocEventPage{Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "partial response must not leak"}}}}}, fmt.Errorf("event owner: %w", fixture.cause)
			}}
			if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			writer := httptest.NewRecorder()
			server.getDocEvents(writer, docVisibleTestRequest(repo, hash, ""))
			if writer.Code != fixture.status || !strings.Contains(writer.Body.String(), `"code":"`+fixture.code+`"`) || writer.Header().Get("Retry-After") != fixture.retry ||
				strings.Contains(writer.Body.String(), "partial response must not leak") || queries.calls != 1 {
				test.Fatal("event port changed error, retry or atomic response handling", writer.Code, writer.Header(), writer.Body)
			}
		})
	}
}

func TestDocumentEventQueriesHTTPValidationAndMissingViews(test *testing.T) {
	repo, hash := domain.HashContent([]byte("event repo")), domain.HashContent([]byte("event doc"))
	backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}
	server := NewServer(backend, nil)
	queries := &documentEventHTTPQueries{}
	if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryCompatibility); err != nil {
		test.Fatal(err)
	}
	for _, query := range []string{"offset=bad", "limit=bad", "base=bad", "offset=0&offset=1", "view=%ZZ"} {
		writer := httptest.NewRecorder()
		server.getDocEvents(writer, docVisibleTestRequest(repo, hash, query))
		if writer.Code != http.StatusUnprocessableEntity || queries.calls != 0 || backend.eventCalls+backend.viewCalls+backend.calls != 0 {
			test.Fatal("malformed event request reached a document query", query, writer.Code, writer.Body)
		}
	}
	for _, fixture := range []struct {
		query  string
		status int
	}{{"view=chat", http.StatusServiceUnavailable}, {"projection=visible-v1&view=chat", http.StatusConflict}} {
		writer := httptest.NewRecorder()
		server.getDocEvents(writer, docVisibleTestRequest(repo, hash, fixture.query))
		if writer.Code != fixture.status || queries.calls != 0 || backend.eventCalls != 0 {
			test.Fatal("event-only port claimed an unconfigured view path", fixture.query, writer.Code, writer.Body)
		}
	}
	if backend.viewCalls != 0 || backend.calls != 0 {
		test.Fatal("unconfigured view queries rediscovered broad backend capabilities", backend.viewCalls, backend.calls)
	}
}

func TestDocumentEventQueriesHTTPDeferredAuthorization(test *testing.T) {
	repo, hash := domain.HashContent([]byte("event repo")), domain.HashContent([]byte("event doc"))
	for _, fixture := range []struct {
		name    string
		status  int
		code    string
		actor   string
		lookups int
	}{
		{name: "current identity", status: http.StatusOK, actor: "current", lookups: 2},
		{name: "revoked token", status: http.StatusUnauthorized, code: "unauthenticated", lookups: 2},
		{name: "revoked role", status: http.StatusForbidden, code: "forbidden", actor: "current", lookups: 2},
		{name: "foreign repo", status: http.StatusForbidden, code: "forbidden", lookups: 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
			identity := &docVisiblePolicyIdentity{
				repository: domain.Repository{ID: "repository", Visibility: domain.VisibilityPrivate},
				user:       domain.User{ID: "before-admission"},
				role:       domain.RoleViewer,
			}
			bodyReads := 0
			queries := &documentEventHTTPQueries{read: func(ctx context.Context, requestedRepo, requestedHash, _ domain.ContentHash, _, _ int) (domain.DocEventPage, error) {
				if policy.calls != 1 || identity.resolveCalls != 1 || requestedRepo != repo || requestedHash != hash {
					test.Fatal("initial event guard or document binding changed", policy.calls, identity.resolveCalls, requestedRepo, requestedHash)
				}
				authorize := inbound.DocumentReadAuthorizer(ctx)
				if authorize == nil {
					test.Fatal("event port context has no current document read authorizer")
				}
				user, authed := userFrom(ctx)
				if !authed || user.ID != "before-admission" {
					test.Fatal("initial event guard did not establish request identity", user, authed)
				}
				identity.user = domain.User{ID: "current"}
				switch fixture.name {
				case "revoked token":
					identity.credentialErr = domain.ErrUnauthorized
				case "revoked role":
					identity.role = ""
				case "foreign repo":
					requestedRepo = domain.HashContent([]byte("foreign event repo"))
				}
				current, err := authorize(inbound.WithSystemActor(ctx), requestedRepo)
				if current == nil {
					test.Fatal("current event authorization returned no context", err)
				}
				actor, system := inbound.RepositoryActor(current)
				user, authed = userFrom(current)
				if system || actor != fixture.actor || authed != (fixture.actor != "") || user.ID != fixture.actor {
					test.Fatal("event read retained stale request or system authority", actor, system, user, authed, err)
				}
				if err != nil {
					return domain.DocEventPage{}, err
				}
				bodyReads++
				return domain.DocEventPage{Hash: hash, Next: -1, Events: []domain.CIREvent{}}, nil
			}}
			server := NewServer(policy, identity)
			if err := server.SetDocumentEventQueries(queries, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			request := docVisibleTestRequest(repo, hash, "")
			request.Header.Set("Authorization", "Bearer session")
			writer := httptest.NewRecorder()
			server.Handler().ServeHTTP(writer, request)
			expectedReads := 0
			if fixture.status == http.StatusOK {
				expectedReads = 1
			}
			if writer.Code != fixture.status || queries.calls != 1 || bodyReads != expectedReads ||
				fixture.code != "" && !strings.Contains(writer.Body.String(), `"code":"`+fixture.code+`"`) {
				test.Fatal("post-admission event authorization did not reject changed authority before reading", writer.Code, queries.calls, bodyReads, writer.Body)
			}
			if policy.calls != fixture.lookups || identity.resolveCalls != fixture.lookups {
				test.Fatal("event query reused initial repository or credential policy", policy.calls, identity.resolveCalls, fixture.lookups)
			}
		})
	}
}

var _ inbound.DocumentEventQueries = (*documentEventHTTPQueries)(nil)
