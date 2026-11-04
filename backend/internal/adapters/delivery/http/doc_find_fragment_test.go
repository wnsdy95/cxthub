package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type docFindFragmentHTTPPort struct {
	calls   int
	repo    domain.ContentHash
	hash    domain.ContentHash
	request domain.DocFindFragmentRequest
	read    func(context.Context) (domain.DocFindFragmentPage, error)
}

func (port *docFindFragmentHTTPPort) ReadDocFindFragment(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindFragmentRequest) (domain.DocFindFragmentPage, error) {
	port.calls++
	port.repo, port.hash, port.request = repo, hash, request
	if port.read != nil {
		return port.read(ctx)
	}
	selection, err := domain.DocFindFragmentSelection(repo, hash, request)
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	projection, index := domain.DocFindFragmentProjection, selection.Next
	if request.Legacy {
		projection, index = domain.DocLegacyFindFragmentProjection, request.Index
	}
	return domain.DocFindFragmentPage{Projection: projection, Hash: hash, Generation: selection.Generation,
		Index: index, Offset: request.Offset, Total: 23, JSON: "\ud55c🙂", Next: request.Offset + 7, Previous: 0}, nil
}

func docFindFragmentHTTPServer() (*Server, *docVisibleHTTPBackend, *docFindFragmentHTTPPort) {
	server, backend, _ := docFindHTTPTestServer()
	port := &docFindFragmentHTTPPort{}
	server.SetDocumentFindFragments(port)
	return server, backend, port
}

func docFindFragmentHTTPRouter(handler http.HandlerFunc) http.Handler {
	router := http.NewServeMux()
	router.HandleFunc("GET /api/v1/repos/{repoID}/docs/{hash}/find-fragments", handler)
	return router
}

func docFindFragmentHTTPRequest(repo, hash domain.ContentHash, query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/docs/"+string(hash)+"/find-fragments?"+query, nil)
}

func docFindFragmentHTTPQueries(test *testing.T, repo, hash domain.ContentHash) []url.Values {
	test.Helper()
	selection := docVisibleTestSelector(repo, hash, "units")
	selection.View, selection.Limit, selection.Start = "all", 1, selection.Next
	return []url.Values{
		{"selection": {encodeDocVisibleTestSelector(test, selection)}, "offset": {"7"}, "budget": {"65536"}},
		{"legacy": {"1"}, "index": {"4"}, "total": {"12"}, "offset": {"7"}, "budget": {"65536"}},
	}
}

func TestDocFindFragmentHTTPExactContract(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, query := range docFindFragmentHTTPQueries(test, repo, hash) {
		server, backend, port := docFindFragmentHTTPServer()
		writer := httptest.NewRecorder()
		docFindFragmentHTTPRouter(server.getDocFindFragment).ServeHTTP(writer, docFindFragmentHTTPRequest(repo, hash, query.Encode()))
		var page domain.DocFindFragmentPage
		if err := json.Unmarshal(writer.Body.Bytes(), &page); err != nil {
			test.Fatal(err)
		}
		if writer.Code != http.StatusOK || port.calls != 1 || port.repo != repo || port.hash != hash || port.request.Offset != 7 || port.request.Budget != 65536 || page.Index != 4 || page.Offset != 7 || page.Next != 14 || page.Previous != 0 || page.JSON != "\ud55c🙂" {
			test.Fatal("fragment contract changed", writer.Code, port, page)
		}
		if query.Has("legacy") {
			if !port.request.Legacy || port.request.Total != 12 || port.request.Selection != "" || page.Generation != "" || page.Projection != domain.DocLegacyFindFragmentProjection {
				test.Fatal("legacy response invented Root proof", port.request, page)
			}
		} else if port.request.Legacy || page.Generation == "" || page.Projection != domain.DocFindFragmentProjection {
			test.Fatal("Root response lost generation", port.request, page)
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
	}
}

func TestDocFindFragmentHTTPRejectsInvalidQueriesBeforePort(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	queries := []string{"", "legacy=1", "selection=x", "budget=4&offset=0"}
	for _, initial := range docFindFragmentHTTPQueries(test, repo, hash) {
		for key := range initial {
			for _, mutate := range []func(url.Values){
				func(copy url.Values) { copy.Del(key) },
				func(copy url.Values) { copy.Set(key, "") },
				func(copy url.Values) { copy.Add(key, copy.Get(key)) },
				func(copy url.Values) { copy.Add(key, "") },
			} {
				copy, err := url.ParseQuery(initial.Encode())
				if err != nil {
					test.Fatal(err)
				}
				mutate(copy)
				queries = append(queries, copy.Encode())
			}
		}
		for _, extra := range []string{"&unknown=value", "&=value", "&projection=find-fragment-v1", "&base=" + string(hash), "&q=needle", "&cursor=x", "&offset=%zz", "&%zz=value", "&ignored=value;other=value"} {
			queries = append(queries, initial.Encode()+extra)
		}
		for _, key := range []string{"offset", "budget", "index", "total"} {
			if !initial.Has(key) {
				continue
			}
			for _, raw := range []string{"-1", "-0", "00", "01", "+1", " 1", "1 ", "1.0", "1e1", "0x1", "１", "999999999999999999999999"} {
				copy, err := url.ParseQuery(initial.Encode())
				if err != nil {
					test.Fatal(err)
				}
				copy.Set(key, raw)
				queries = append(queries, copy.Encode())
			}
		}
		for _, budget := range []string{"0", "3", "65537"} {
			copy, err := url.ParseQuery(initial.Encode())
			if err != nil {
				test.Fatal(err)
			}
			copy.Set("budget", budget)
			queries = append(queries, copy.Encode())
		}
		if initial.Has("legacy") {
			queries = append(queries, initial.Encode()+"&selection=x")
			for _, legacy := range []string{"0", "true", "01"} {
				copy, err := url.ParseQuery(initial.Encode())
				if err != nil {
					test.Fatal(err)
				}
				copy.Set("legacy", legacy)
				queries = append(queries, copy.Encode())
			}
		} else {
			queries = append(queries, initial.Encode()+"&legacy=1", initial.Encode()+"&index=0", initial.Encode()+"&total=12", initial.Encode()+"&legacy=")
			for _, mutate := range []func(*domain.DocVisibleSelector){
				func(value *domain.DocVisibleSelector) { value.Repo = domain.HashContent([]byte("other repo")) },
				func(value *domain.DocVisibleSelector) { value.Hash = domain.HashContent([]byte("other doc")) },
				func(value *domain.DocVisibleSelector) { value.View = "chat" },
				func(value *domain.DocVisibleSelector) { value.Purpose = "fold" },
				func(value *domain.DocVisibleSelector) { value.Limit = 20 },
				func(value *domain.DocVisibleSelector) {
					value.Start, value.Next, value.Unit = value.Total, value.Total, 0
				},
			} {
				copy, err := url.ParseQuery(initial.Encode())
				if err != nil {
					test.Fatal(err)
				}
				selection, err := domain.DecodeDocVisibleSelector(copy.Get("selection"))
				if err != nil {
					test.Fatal(err)
				}
				mutate(&selection)
				copy.Set("selection", encodeDocVisibleTestSelector(test, selection))
				queries = append(queries, copy.Encode())
			}
		}
	}
	for _, query := range queries {
		server, backend, port := docFindFragmentHTTPServer()
		writer := httptest.NewRecorder()
		docFindFragmentHTTPRouter(server.getDocFindFragment).ServeHTTP(writer, docFindFragmentHTTPRequest(repo, hash, query))
		if writer.Code != http.StatusUnprocessableEntity || port.calls != 0 {
			test.Fatalf("unsafe query reached fragment port: %s: status=%d calls=%d body=%s", query, writer.Code, port.calls, writer.Body)
		}
		assertDocFindHTTPNoLegacyReads(test, backend)
	}
}

func TestDocFindFragmentHTTPDeferredPermissionAndDeadline(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, query := range docFindFragmentHTTPQueries(test, repo, hash) {
		for _, revocation := range []string{"none", "membership", "credential", "foreign_repo"} {
			policy := &docVisiblePolicyBackend{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}
			identity := &docVisiblePolicyIdentity{repository: domain.Repository{ID: "repository"}, user: domain.User{ID: "reader"}, role: domain.RoleViewer}
			server, backend, port := docFindFragmentHTTPServer()
			backend.Backend, server.id = policy, identity
			var observed context.Context
			port.read = func(ctx context.Context) (domain.DocFindFragmentPage, error) {
				observed = ctx
				if policy.calls != 1 || identity.resolveCalls != 1 || inbound.DocumentReadAuthorizer(ctx) == nil {
					test.Fatal("missing or premature deferred authorization")
				}
				requestedRepo := repo
				switch revocation {
				case "membership":
					identity.role = ""
				case "credential":
					identity.credentialErr = domain.ErrUnauthorized
				case "foreign_repo":
					requestedRepo = domain.HashContent([]byte("foreign repo"))
				}
				current, err := inbound.DocumentReadAuthorizer(ctx)(ctx, requestedRepo)
				if err != nil {
					return domain.DocFindFragmentPage{}, err
				}
				actor, system := inbound.RepositoryActor(current)
				if actor != "reader" || system {
					test.Fatal("fresh permission lost request actor")
				}
				return domain.DocFindFragmentPage{Projection: domain.DocFindFragmentProjection}, nil
			}
			request := docFindFragmentHTTPRequest(repo, hash, query.Encode())
			request.Header.Set("Authorization", "Bearer session")
			parent, cancel := context.WithTimeout(request.Context(), 30*time.Second)
			defer cancel()
			writer := &docVisibleDeadlineWriter{deadlineDocWriter: &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder()}}
			docFindFragmentHTTPRouter(server.guard(domain.RoleViewer, server.getDocFindFragment)).ServeHTTP(writer, request.WithContext(parent))
			want := http.StatusOK
			if revocation == "credential" {
				want = http.StatusUnauthorized
			} else if revocation != "none" {
				want = http.StatusForbidden
			}
			if writer.Code != want || port.calls != 1 || observed == nil || writer.readCalls != 1 || writer.writeCalls != 1 {
				test.Fatal("deferred permission or deadline bypassed", writer.Code, port.calls, writer.Body)
			}
			deadline, present := observed.Deadline()
			parentDeadline, _ := parent.Deadline()
			if !present || !deadline.Equal(parentDeadline) || !deadline.Equal(writer.read) || !deadline.Equal(writer.write) || !errors.Is(observed.Err(), context.Canceled) || parent.Err() != nil {
				test.Fatal("request deadline extended or context not released")
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
	}
}

func TestDocFindFragmentHTTPErrorMappingAndUnavailablePort(test *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, query := range docFindFragmentHTTPQueries(test, repo, hash) {
		for _, failure := range []struct {
			err    error
			status int
		}{
			{domain.ErrValidation, http.StatusUnprocessableEntity},
			{domain.ErrUnauthorized, http.StatusUnauthorized},
			{domain.ErrForbidden, http.StatusForbidden},
			{domain.ErrNotFound, http.StatusNotFound},
			{domain.ErrIntegrity, http.StatusUnprocessableEntity},
			{domain.ErrDocVisibleGenerationStale, http.StatusConflict},
			{domain.ErrDocVisibleProjectionUnsupported, http.StatusConflict},
			{domain.ErrDocRangeIndexUnavailable, http.StatusServiceUnavailable},
			{domain.ErrResourceOverloaded, http.StatusServiceUnavailable},
		} {
			server, backend, port := docFindFragmentHTTPServer()
			port.read = func(context.Context) (domain.DocFindFragmentPage, error) {
				return domain.DocFindFragmentPage{}, failure.err
			}
			writer := httptest.NewRecorder()
			docFindFragmentHTTPRouter(server.getDocFindFragment).ServeHTTP(writer, docFindFragmentHTTPRequest(repo, hash, query.Encode()))
			if writer.Code != failure.status || port.calls != 1 || strings.Contains(writer.Body.String(), "json_fragment") {
				test.Fatal("failed fragment returned data or wrong status", failure.err, writer.Code, writer.Body)
			}
			assertDocFindHTTPNoLegacyReads(test, backend)
		}
		server, _, port := docFindFragmentHTTPServer()
		server.SetDocumentFindFragments(nil)
		writer := httptest.NewRecorder()
		docFindFragmentHTTPRouter(server.getDocFindFragment).ServeHTTP(writer, docFindFragmentHTTPRequest(repo, hash, query.Encode()))
		if writer.Code != http.StatusServiceUnavailable || !reflect.DeepEqual(port.request, domain.DocFindFragmentRequest{}) || port.calls != 0 {
			test.Fatal("unconfigured port silently fell back", writer.Code, writer.Body)
		}
	}
}
