package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type docAllHTTPCall struct {
	method    string
	repo      domain.ContentHash
	hash      domain.ContentHash
	arguments any
}

type docAllHTTPQueries struct {
	output any
	cause  error
	read   func(context.Context, string) error
	calls  []docAllHTTPCall
}

func (queries *docAllHTTPQueries) record(ctx context.Context, repo, hash domain.ContentHash, method string, arguments any) error {
	queries.calls = append(queries.calls, docAllHTTPCall{method: method, repo: repo, hash: hash, arguments: arguments})
	if queries.read != nil {
		return queries.read(ctx, method)
	}
	return queries.cause
}

func (queries *docAllHTTPQueries) ReadDocEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	page, _ := queries.output.(domain.DocEventPage)
	return page, queries.record(ctx, repo, hash, "events", []any{base, offset, limit})
}

func (queries *docAllHTTPQueries) ReadDocFragments(ctx context.Context, repo, hash domain.ContentHash, index, offset, limit, budget int) (domain.DocFragmentPage, error) {
	page, _ := queries.output.(domain.DocFragmentPage)
	return page, queries.record(ctx, repo, hash, "fragments", []int{index, offset, limit, budget})
}

func (queries *docAllHTTPQueries) SearchDocEvents(ctx context.Context, repo, hash domain.ContentHash, query string, after, limit int) ([]domain.DocEventIndex, error) {
	page, _ := queries.output.([]domain.DocEventIndex)
	return page, queries.record(ctx, repo, hash, "search", []any{query, after, limit})
}

func (queries *docAllHTTPQueries) ReadDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	page, _ := queries.output.(domain.DocViewPage)
	return page, queries.record(ctx, repo, hash, "view", []any{base, offset, limit, view})
}

func (queries *docAllHTTPQueries) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	page, _ := queries.output.(domain.DocViewFragment)
	return page, queries.record(ctx, repo, hash, "view-fragment", []int{index, offset, budget})
}

func (queries *docAllHTTPQueries) ReadDocVisiblePage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisiblePage, error) {
	page, _ := queries.output.(domain.DocVisiblePage)
	return page, queries.record(ctx, repo, hash, "visible", request)
}

func (queries *docAllHTTPQueries) FindDocPage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindPageRequest) (domain.DocFindPage, error) {
	page, _ := queries.output.(domain.DocFindPage)
	return page, queries.record(ctx, repo, hash, "find", request)
}

func (queries *docAllHTTPQueries) ReadDocFindFragment(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindFragmentRequest) (domain.DocFindFragmentPage, error) {
	page, _ := queries.output.(domain.DocFindFragmentPage)
	return page, queries.record(ctx, repo, hash, "find-fragment", request)
}

type docAllHTTPBackend struct {
	Backend
	inbound.DocumentQueries
}

type docAllHTTPRoute struct {
	name          string
	method        string
	path          string
	arguments     any
	output        any
	missingStatus int
	missingCode   string
}

func docAllHTTPRoutes(test *testing.T, repo, hash domain.ContentHash) []docAllHTTPRoute {
	test.Helper()
	generation := domain.HashContent([]byte("all query generation"))
	base := domain.HashContent([]byte("all query base"))
	selector := domain.DocVisibleSelector{Version: 1, Purpose: "units", Repo: repo, Hash: hash,
		Generation: generation, View: "chat", Total: 12, Unit: 1, Next: 4, Limit: 2}
	visibleCursor, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	findSelector := domain.DocFindSelector{Version: 1, Selection: selector, QueryHash: domain.HashContent([]byte("needle")),
		From: 0, To: 12, Index: 4, Field: 1, Byte: 2}
	findCursor, err := findSelector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	selector.View, selector.Limit, selector.Start = "all", 1, selector.Next
	fragmentSelection, err := selector.Encode()
	if err != nil {
		test.Fatal(err)
	}
	findPage := domain.DocFindPage{Projection: domain.DocFindProjection, Hash: hash, Generation: generation,
		From: 0, To: 12, Index: 4, Field: 1, Byte: 2, Matches: []domain.DocFindMatch{}, NextCursor: &findCursor}
	legacyPage := findPage
	legacyCursor, err := (domain.DocLegacyFindSelector{Version: 1, Repo: repo, Hash: hash, Total: 12,
		From: 0, To: 12, Index: 4, Field: 1, Byte: 2, QueryHash: domain.HashContent([]byte("needle"))}).Encode()
	if err != nil {
		test.Fatal(err)
	}
	legacyPage.Projection, legacyPage.NextCursor = domain.DocLegacyFindProjection, &legacyCursor
	fragmentPage := domain.DocFindFragmentPage{Projection: domain.DocFindFragmentProjection, Hash: hash,
		Generation: generation, Index: 4, Offset: 7, Total: 23, JSON: "\ud55c🙂", Next: 14, Previous: 0}
	legacyFragment := fragmentPage
	legacyFragment.Projection = domain.DocLegacyFindFragmentProjection
	return []docAllHTTPRoute{
		{name: "events", method: "events", path: "/events?offset=2&limit=1", arguments: []any{domain.ContentHash(""), 2, 1},
			output: domain.DocEventPage{Hash: hash, Total: 12, Offset: 2, Next: 3, Events: []domain.CIREvent{}}},
		{name: "view", method: "view", path: "/events?" + url.Values{"view": {"chat"}, "base": {string(base)}, "offset": {"2"}, "limit": {"1"}}.Encode(),
			arguments: []any{base, 2, 1, "chat"}, output: domain.DocViewPage{Hash: hash, Total: 12, Offset: 2, Next: 3, Events: []domain.DocViewEvent{}},
			missingStatus: 503, missingCode: "unavailable"},
		{name: "view fragment", method: "view-fragment", path: "/event-fragments?index=4&offset=7&budget=64", arguments: []int{4, 7, 64},
			output: domain.DocViewFragment{Index: 4, Offset: 7, Total: 23, JSON: "\ud55c🙂", Next: 14}, missingStatus: 503, missingCode: "unavailable"},
		{name: "visible cursor", method: "visible", path: "/events?" + url.Values{"projection": {domain.DocVisibleProjection}, "cursor": {visibleCursor}}.Encode(),
			arguments: domain.DocVisiblePageRequest{Cursor: visibleCursor},
			output: domain.DocVisiblePage{Projection: domain.DocVisibleProjection, Kind: "units", Hash: hash, Generation: generation, Rules: 1, View: "chat",
				Total: 12, From: 1, To: 4, Units: []domain.DocVisiblePageUnit{}, Events: []domain.DocVisibleEvent{}, NextCursor: &visibleCursor},
			missingStatus: 409, missingCode: "doc_visible_projection_unsupported"},
		{name: "find cursor", method: "find", path: "/find?" + url.Values{"q": {"needle"}, "cursor": {findCursor}}.Encode(),
			arguments: domain.DocFindPageRequest{Query: "needle", Cursor: findCursor}, output: findPage, missingStatus: 503, missingCode: "unavailable"},
		{name: "legacy find", method: "find", path: "/find?legacy=1&q=needle&from=0&to=12&total=12&inherited=0",
			arguments: domain.DocFindPageRequest{Legacy: true, Query: "needle", From: 0, To: 12, Total: 12}, output: legacyPage, missingStatus: 503, missingCode: "unavailable"},
		{name: "find fragment", method: "find-fragment", path: "/find-fragments?" + url.Values{"selection": {fragmentSelection}, "offset": {"7"}, "budget": {"64"}}.Encode(),
			arguments: domain.DocFindFragmentRequest{Selection: fragmentSelection, Offset: 7, Budget: 64}, output: fragmentPage, missingStatus: 503, missingCode: "unavailable"},
		{name: "legacy find fragment", method: "find-fragment", path: "/find-fragments?legacy=1&index=4&total=12&offset=7&budget=64",
			arguments: domain.DocFindFragmentRequest{Legacy: true, Index: 4, Total: 12, Offset: 7, Budget: 64}, output: legacyFragment, missingStatus: 503, missingCode: "unavailable"},
	}
}

func docAllHTTPRequest(repo, hash domain.ContentHash, route docAllHTTPRoute) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/docs/"+string(hash)+route.path, nil)
	request.SetPathValue("repoID", string(repo))
	request.SetPathValue("hash", string(hash))
	return request
}

func docAllHTTPHandler(server *Server, method string) http.HandlerFunc {
	switch method {
	case "events", "view", "visible":
		return server.getDocEvents
	case "view-fragment":
		return server.getDocEventFragment
	case "find":
		return server.getDocFindPage
	case "find-fragment":
		return server.getDocFindFragment
	default:
		panic("unknown all-query HTTP fixture method")
	}
}

func TestDocumentQueriesHTTPExplicitOwnerOverridesBroadBackend(test *testing.T) {
	repo, hash := domain.HashContent([]byte("all query repo")), domain.HashContent([]byte("all query doc"))
	for _, route := range docAllHTTPRoutes(test, repo, hash) {
		test.Run(route.name, func(test *testing.T) {
			broad := &docAllHTTPQueries{cause: errors.New("broad backend document query must not run")}
			owner := &docAllHTTPQueries{output: route.output, read: func(ctx context.Context, method string) error {
				if _, present := ctx.Deadline(); !present || inbound.DocumentReadAuthorizer(ctx) == nil || method != route.method {
					test.Fatal("explicit query lost its bounded context, deferred authorizer or dispatch", method)
				}
				return nil
			}}
			server := NewServer(&docAllHTTPBackend{DocumentQueries: broad}, nil)
			if err := server.SetDocumentQueries(owner, inbound.DocumentQueryCompatibility); err != nil {
				test.Fatal(err)
			}
			writer := httptest.NewRecorder()
			docAllHTTPHandler(server, route.method)(writer, docAllHTTPRequest(repo, hash, route))
			encoded, err := json.Marshal(route.output)
			if err != nil {
				test.Fatal(err)
			}
			expected := []docAllHTTPCall{{method: route.method, repo: repo, hash: hash, arguments: route.arguments}}
			if writer.Code != http.StatusOK || strings.TrimSpace(writer.Body.String()) != string(encoded) ||
				writer.Header().Get("Content-Type") != "application/json; charset=utf-8" || !reflect.DeepEqual(owner.calls, expected) || len(broad.calls) != 0 {
				test.Fatalf("full query injection changed dispatch, request, cursor or wire: status=%d calls=%+v broad=%+v body=%s", writer.Code, owner.calls, broad.calls, writer.Body)
			}
			for _, field := range []string{"cursor", "selection"} {
				invalid := docAllHTTPRequest(repo, hash, route)
				query := invalid.URL.Query()
				if !query.Has(field) {
					continue
				}
				query.Set(field, "invalid-query-selector")
				invalid.URL.RawQuery = query.Encode()
				rejected := httptest.NewRecorder()
				docAllHTTPHandler(server, route.method)(rejected, invalid)
				if rejected.Code != http.StatusUnprocessableEntity || !strings.Contains(rejected.Body.String(), `"code":"validation"`) ||
					len(owner.calls) != 1 || len(broad.calls) != 0 {
					test.Fatal("malformed query cursor or selection reached a reader after full injection", field, rejected.Code, owner.calls, rejected.Body)
				}
			}
		})
	}
}

func TestDocumentQueriesHTTPMissingViewsNeverDiscoverBroadCapabilities(test *testing.T) {
	repo, hash := domain.HashContent([]byte("all query repo")), domain.HashContent([]byte("all query doc"))
	for _, eventOnly := range []bool{false, true} {
		for _, route := range docAllHTTPRoutes(test, repo, hash) {
			if route.missingStatus == 0 {
				continue
			}
			test.Run(fmt.Sprintf("%s/event-only=%t", route.name, eventOnly), func(test *testing.T) {
				broad, core := &docAllHTTPQueries{}, &docAllHTTPQueries{}
				server := NewServer(&docAllHTTPBackend{DocumentQueries: broad}, nil)
				if eventOnly {
					if err := server.SetDocumentEventQueries(core, inbound.DocumentQueryCompatibility); err != nil {
						test.Fatal(err)
					}
				}
				writer := httptest.NewRecorder()
				docAllHTTPHandler(server, route.method)(writer, docAllHTTPRequest(repo, hash, route))
				if writer.Code != route.missingStatus || !strings.Contains(writer.Body.String(), `"code":"`+route.missingCode+`"`) || len(broad.calls)+len(core.calls) != 0 {
					test.Fatal("missing narrow view/find port fell back to broad or event-only capabilities", writer.Code, broad.calls, core.calls, writer.Body)
				}
			})
		}
	}
}

func TestDocumentQueriesHTTPSetterRejectsInvalidConfigurationAtomically(test *testing.T) {
	var typedNil *docAllHTTPQueries
	for _, profile := range []inbound.DocumentQueryProfile{inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
		server := NewServer(nil, nil)
		owner := &docAllHTTPQueries{}
		if err := server.SetDocumentQueries(owner, profile); err != nil {
			test.Fatal(err)
		}
		for _, invalid := range []struct {
			queries inbound.DocumentQueries
			profile inbound.DocumentQueryProfile
		}{
			{profile: inbound.DocumentQueryCompatibility},
			{queries: typedNil, profile: inbound.DocumentQueryVerifiedRoots},
			{queries: &docAllHTTPQueries{}, profile: 0},
			{queries: &docAllHTTPQueries{}, profile: 255},
		} {
			if err := server.SetDocumentQueries(invalid.queries, invalid.profile); !errors.Is(err, domain.ErrValidation) {
				test.Fatal("invalid full query dependency or profile was accepted", invalid.profile, err)
			}
			if server.documentEvents != owner || server.documentViews != owner || server.docFind != owner || server.docFindFragment != owner || server.documentQueryProfile != profile {
				test.Fatal("invalid full query setter partially replaced a valid owner or profile")
			}
			unconfigured := NewServer(nil, nil)
			if err := unconfigured.SetDocumentQueries(invalid.queries, invalid.profile); err == nil || unconfigured.documentEvents != nil ||
				unconfigured.documentViews != nil || unconfigured.docFind != nil || unconfigured.docFindFragment != nil || unconfigured.documentQueryProfile != 0 {
				test.Fatal("invalid full query setter initialized only part of an empty server", err)
			}
		}
	}
}

func TestDocumentQueriesHTTPPreservesCurrentErrorsWithoutPartialPayload(test *testing.T) {
	repo, hash := domain.HashContent([]byte("all query repo")), domain.HashContent([]byte("all query doc"))
	for _, route := range docAllHTTPRoutes(test, repo, hash) {
		for _, failure := range []struct {
			cause  error
			code   string
			status int
			retry  string
		}{
			{domain.ErrForbidden, "forbidden", 403, ""},
			{domain.ErrDocRangeIndexPending, "doc_range_index_pending", 503, "3"},
			{domain.ErrDocVisibleGenerationStale, "doc_visible_generation_stale", 409, ""},
		} {
			test.Run(route.name+"/"+failure.code, func(test *testing.T) {
				owner := &docAllHTTPQueries{output: route.output, cause: fmt.Errorf("full query read: %w", failure.cause)}
				server := NewServer(nil, nil)
				if err := server.SetDocumentQueries(owner, inbound.DocumentQueryCompatibility); err != nil {
					test.Fatal(err)
				}
				writer := httptest.NewRecorder()
				docAllHTTPHandler(server, route.method)(writer, docAllHTTPRequest(repo, hash, route))
				var body map[string]json.RawMessage
				if err := json.Unmarshal(writer.Body.Bytes(), &body); err != nil || len(body) != 1 || body["error"] == nil ||
					writer.Code != failure.status || !strings.Contains(writer.Body.String(), `"code":"`+failure.code+`"`) ||
					writer.Header().Get("Retry-After") != failure.retry || len(owner.calls) != 1 {
					test.Fatal("full query delivery changed error, retry or atomic response contract", writer.Code, writer.Header(), writer.Body, err)
				}
			})
		}
	}
}

type docAllHTTPSnapshotKey struct{}

type docAllHTTPPermit func(context.Context, string, string) error

func (permit docAllHTTPPermit) AuthorizeSnapshot(ctx context.Context, repository, actor string) error {
	return permit(ctx, repository, actor)
}

type docAllHTTPIdentity struct {
	*docVisiblePolicyIdentity
	test             *testing.T
	admissions       int
	snapshotResolves int
	snapshotPermits  int
	snapshotErr      error
}

func (identity *docAllHTTPIdentity) ResolveUser(ctx context.Context, token string) (domain.User, error) {
	if ctx.Value(docAllHTTPSnapshotKey{}) == true {
		identity.test.Fatal("ordinary ResolveUser ran inside a pinned document snapshot")
	}
	return identity.docVisiblePolicyIdentity.ResolveUser(ctx, token)
}

func (identity *docAllHTTPIdentity) AdmitRepositoryRead(ctx context.Context, repository, actor string) (inbound.RepositoryReadPermit, error) {
	if ctx.Value(docAllHTTPSnapshotKey{}) == true || repository != "repository" || actor != "reader" {
		identity.test.Fatal("document read admission did not precede its pinned snapshot", repository, actor)
	}
	identity.admissions++
	return docAllHTTPPermit(func(bound context.Context, requestedRepository, requestedActor string) error {
		if bound.Value(docAllHTTPSnapshotKey{}) != true || requestedRepository != repository || requestedActor != actor {
			identity.test.Fatal("snapshot permit lost its pinned context, repository or actor", requestedRepository, requestedActor)
		}
		identity.snapshotPermits++
		return identity.snapshotErr
	}), nil
}

func (identity *docAllHTTPIdentity) ResolveUserForSnapshot(ctx context.Context, token string) (domain.User, error) {
	if ctx.Value(docAllHTTPSnapshotKey{}) != true || token != "session" {
		identity.test.Fatal("snapshot identity resolution lost the original credential or pinned context", token)
	}
	identity.snapshotResolves++
	return identity.user, nil
}

func (*docAllHTTPIdentity) ResolveMCPUserForSnapshot(context.Context, string, ...string) (domain.User, error) {
	panic("HTTP document queries must not resolve an MCP snapshot identity")
}

func TestDocumentQueriesHTTPDeferredAuthorizerMatchesExplicitProfile(test *testing.T) {
	repo, hash := domain.HashContent([]byte("all query repo")), domain.HashContent([]byte("all query doc"))
	for _, profile := range []inbound.DocumentQueryProfile{inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
		for _, revoked := range []bool{false, true} {
			seen := make(map[string]bool)
			for _, route := range docAllHTTPRoutes(test, repo, hash) {
				if seen[route.method] {
					continue
				}
				seen[route.method] = true
				test.Run(fmt.Sprintf("%s/profile=%d/revoked=%t", route.name, profile, revoked), func(test *testing.T) {
					policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
					identity := &docAllHTTPIdentity{test: test, docVisiblePolicyIdentity: &docVisiblePolicyIdentity{
						repository: domain.Repository{ID: "repository", Visibility: domain.VisibilityPrivate}, user: domain.User{ID: "reader"}, role: domain.RoleViewer,
					}}
					bodyReads := 0
					owner := &docAllHTTPQueries{output: route.output, read: func(ctx context.Context, method string) error {
						if policy.calls != 1 || identity.resolveCalls != 1 || method != route.method {
							test.Fatal("initial guard or full query dispatch changed", policy.calls, identity.resolveCalls, method)
						}
						authorize := inbound.DocumentReadAuthorizer(ctx)
						if authorize == nil {
							test.Fatal("full query did not receive its deferred current authorizer")
						}
						if profile == inbound.DocumentQueryCompatibility && revoked {
							identity.role = ""
						}
						bound, err := authorize(ctx, repo)
						if profile == inbound.DocumentQueryVerifiedRoots {
							if err != nil {
								return err
							}
							snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
							if snapshot == nil || identity.admissions != 1 || identity.resolveCalls != 2 {
								test.Fatal("verified profile did not split normal admission from snapshot authorization", identity.admissions, identity.resolveCalls)
							}
							if revoked {
								identity.snapshotErr = domain.ErrForbidden
							}
							bound, err = snapshot(context.WithValue(bound, docAllHTTPSnapshotKey{}, true), repo)
						} else if inbound.DocumentSnapshotReadAuthorizer(bound) != nil || identity.admissions != 0 {
							test.Fatal("explicit compatibility profile silently adopted snapshot identity admission")
						}
						if err != nil {
							return err
						}
						actor, system := inbound.RepositoryActor(bound)
						if actor != "reader" || system {
							test.Fatal("current full query authorization retained stale authority", actor, system)
						}
						bodyReads++
						return nil
					}}
					server := NewServer(&docAllHTTPBackend{Backend: policy, DocumentQueries: &docAllHTTPQueries{}}, identity)
					if err := server.SetDocumentQueries(owner, profile); err != nil {
						test.Fatal(err)
					}
					request := docAllHTTPRequest(repo, hash, route)
					request.Header.Set("Authorization", "Bearer session")
					writer := httptest.NewRecorder()
					server.Handler().ServeHTTP(writer, request)
					expectedStatus, expectedReads, expectedSnapshots := http.StatusOK, 1, 0
					if revoked {
						expectedStatus, expectedReads = http.StatusForbidden, 0
					}
					if profile == inbound.DocumentQueryVerifiedRoots {
						expectedSnapshots = 1
					}
					if writer.Code != expectedStatus || bodyReads != expectedReads || len(owner.calls) != 1 ||
						identity.resolveCalls != 2 || identity.admissions != expectedSnapshots || identity.snapshotResolves != expectedSnapshots || identity.snapshotPermits != expectedSnapshots {
						test.Fatal("full query profile lost current authority or repeated ordinary identity resolution inside a snapshot", writer.Code, bodyReads,
							identity.resolveCalls, identity.admissions, identity.snapshotResolves, identity.snapshotPermits, writer.Body)
					}
				})
			}
		}
	}
}

var _ inbound.DocumentQueries = (*docAllHTTPQueries)(nil)
var _ inbound.SnapshotReadIdentity = (*docAllHTTPIdentity)(nil)
