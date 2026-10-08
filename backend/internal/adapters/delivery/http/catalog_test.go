package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type catalogHTTPBackend struct {
	Backend
	repo  domain.Repo
	calls int
	got   domain.CatalogRequest
	page  domain.CatalogPage
	err   error
}

func (b *catalogHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return b.repo, nil
}
func (b *catalogHTTPBackend) CatalogChanges(_ context.Context, _ domain.ContentHash, r domain.CatalogRequest) (domain.CatalogPage, error) {
	b.calls++
	b.got = r
	return b.page, b.err
}

func catalogHTTPFixture() (*catalogHTTPBackend, *branchPullHTTPIdentity, string) {
	repo := domain.Repo{ID: domain.HashContent([]byte("http catalog")), RepositoryID: "ws_" + strings.Repeat("e", 32)}
	checkpoint := &domain.CatalogCheckpoint{Version: 1, RepoID: repo.ID, Epoch: "e66213bd-003e-47d1-b965-e690036bfb08", Sequence: 4}
	b := &catalogHTTPBackend{repo: repo, page: domain.CatalogPage{Version: 1, RepoID: repo.ID, Epoch: checkpoint.Epoch, Through: 4, Mode: "baseline", Entries: []domain.CatalogEntry{}, Checkpoint: checkpoint}}
	id := &branchPullHTTPIdentity{repo: domain.Repository{ID: repo.RepositoryID}, role: domain.RolePuller}
	return b, id, "/api/v1/repos/" + string(repo.ID) + "/pull/catalog"
}

func TestCatalogHTTPAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		role                                         domain.MemberRole
		public, puller, archived, breakGlass, authed bool
		status                                       int
	}{
		{name: "private_anonymous", status: 401},
		{name: "private_viewer", role: domain.RoleViewer, authed: true, status: 403},
		{name: "private_puller", role: domain.RolePuller, authed: true, status: 200},
		{name: "member", role: domain.RoleMember, authed: true, status: 200},
		{name: "public_viewer", public: true, status: 401},
		{name: "public_puller", public: true, puller: true, status: 200},
		{name: "archived", role: domain.RoleOwner, archived: true, authed: true, status: 403},
		{name: "break_glass", breakGlass: true, authed: true, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, id, path := catalogHTTPFixture()
			id.role, id.repo.Archived, id.breakGlass = tc.role, tc.archived, tc.breakGlass
			if tc.public {
				id.repo.Visibility = domain.VisibilityPublic
			}
			if tc.puller {
				id.repo.PublicRole = "puller"
			}
			w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodPost, path, `{"version":1}`, tc.authed)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.status, w.Body.String())
			}
			wantCalls := 0
			if tc.status == 200 {
				wantCalls = 1
			}
			if b.calls != wantCalls {
				t.Fatal("catalog called before authorization")
			}
		})
	}
}

func TestCatalogHTTPContinuationReauthorizes(t *testing.T) {
	b, id, path := catalogHTTPFixture()
	b.page.NextCursor, b.page.Checkpoint = "opaque", nil
	h := NewServer(b, id).Handler()
	w := bpHTTP(t, h, http.MethodPost, path, `{"version":1,"limit":1}`, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"checkpoint"`) || !strings.Contains(w.Body.String(), `"next_cursor":"opaque"`) {
		t.Fatalf("partial page: %d %s", w.Code, w.Body.String())
	}
	id.role = domain.RoleViewer
	w = bpHTTP(t, h, http.MethodPost, path, `{"version":1,"cursor":"opaque"}`, true)
	if w.Code != 403 || b.calls != 1 {
		t.Fatalf("revoked continuation: %d calls=%d", w.Code, b.calls)
	}
}

func TestCatalogHTTPInputValidation(t *testing.T) {
	b, id, path := catalogHTTPFixture()
	h := NewServer(b, id).Handler()
	checkpoint, err := json.Marshal(b.page.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"version":2}`, `{"version":1,"limit":-1}`, `{"version":1,"limit":1001}`,
		`{"version":1,"limit":1.5}`, `{"version":1,"limit":"1"}`, `{"version":1,"unexpected":true}`,
		`{"version":1,"limit":null}`, `{"version":1,"cursor":null}`, `{"version":1,"after":null}`,
		`{"version":2,"version":1}`, `{"Version":1}`, `{"version":1,"LIMIT":10}`,
		`{"version":1} {}`, `{"version":1} trailing`, `{"version":1,"after":{}}`,
		`{"version":1,"after":` + string(checkpoint) + `,"cursor":"opaque"}`,
		`{"version":1,"after":` + strings.Replace(string(checkpoint), `"sequence":4`, `"sequence":-1`, 1) + `}`,
		`{"version":1,"after":` + strings.TrimSuffix(string(checkpoint), "}") + `,"unexpected":true}}`,
		`{"version":1,"after":` + strings.TrimSuffix(string(checkpoint), "}") + `,"sequence":5}}`,
		`{"version":1,"after":` + strings.Replace(string(checkpoint), `,"sequence":4`, "", 1) + `}`,
	} {
		t.Run(body, func(t *testing.T) {
			w := bpHTTP(t, h, http.MethodPost, path, body, true)
			if w.Code != 400 {
				t.Fatalf("status=%d want=400: %s", w.Code, w.Body.String())
			}
		})
	}
	w := bpHTTP(t, h, http.MethodPost, strings.Replace(path, string(b.repo.ID), "invalid-hash", 1), `{"version":1}`, true)
	if w.Code != 400 {
		t.Fatalf("invalid repo: %d", w.Code)
	}
	for _, body := range []string{`{"version":1,"cursor":"` + strings.Repeat("a", 64<<10) + `"}`, `{"version":1}` + strings.Repeat(" ", 64<<10)} {
		w = bpHTTP(t, h, http.MethodPost, path, body, true)
		if w.Code != 413 {
			t.Fatalf("oversized body: %d %s", w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"version":1}`))
	r.Header.Set("Authorization", "Bearer fixture-token")
	r.Header.Set("Content-Type", "text/plain")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("content type: %d", w.Code)
	}
	if b.calls != 0 {
		t.Fatalf("invalid requests reached catalog: %d", b.calls)
	}
}

func TestCatalogHTTPErrorsAndOptionalCapability(t *testing.T) {
	b, id, path := catalogHTTPFixture()
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{domain.ErrCatalogResetRequired, 409, "reset_required"},
		{domain.ErrCatalogUnsupported, 501, "catalog_unsupported"},
		{domain.ErrValidation, 400, "bad_request"},
		{domain.ErrNotFound, 404, "not_found"},
	} {
		b.err = fmt.Errorf("catalog: %w", tc.err)
		w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodPost, path, `{"version":1}`, true)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body.String())
		}
	}
	// The mandatory Backend remains implementable without CatalogQuery.
	legacy := &branchPullHTTPBackend{repo: b.repo}
	w := bpHTTP(t, NewServer(legacy, id).Handler(), http.MethodPost, path, `{"version":1}`, true)
	if w.Code != 501 || !strings.Contains(w.Body.String(), "catalog_unsupported") {
		t.Fatalf("optional capability: %d %s", w.Code, w.Body.String())
	}
	fs := store.NewFSStore(t.TempDir())
	if _, err := fs.PutRepo(context.Background(), b.repo); err != nil {
		t.Fatal(err)
	}
	svc := app.NewService(fs, fs, nil, nil, nil)
	w = bpHTTP(t, NewServer(svc, id).Handler(), http.MethodPost, path, `{"version":1}`, true)
	if w.Code != 501 || !strings.Contains(w.Body.String(), "catalog_unsupported") {
		t.Fatalf("FS capability: %d %s", w.Code, w.Body.String())
	}
}

func TestCatalogHTTPPreservesRawValuesAndFinalCheckpoint(t *testing.T) {
	b, id, path := catalogHTTPFixture()
	b.page.Mode = "delta"
	b.page.Entries = []domain.CatalogEntry{
		{Sequence: 4, Kind: "snapshot", Key: string(domain.HashContent([]byte("doc"))), Value: json.RawMessage(`{"parents":["second","first"],"graft_parents":["graft"],"memory_hash":"mutable","created_at":"2026-10-07T00:00:00Z"}`)},
		{Sequence: 4, Kind: "ref", Key: `["tag","lifecycle/raw"]`, Value: json.RawMessage(`{"kind":"tag","name":"lifecycle/raw","branch_id":"identity"}`)},
		{Sequence: 4, Kind: "history", Key: "event", Value: json.RawMessage(`{"id":"event","future":{"complete":true}}`)},
		{Sequence: 4, Kind: "protocol", Key: string(b.repo.ID), Value: json.RawMessage(`{"context_protocol":1}`)},
	}
	raw, err := json.Marshal(domain.CatalogRequest{Version: 1, After: b.page.Checkpoint, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodPost, path, string(raw), true)
	var got domain.CatalogPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, b.page) {
		t.Fatalf("metadata changed: %d %s", w.Code, w.Body.String())
	}
	if b.got.After == nil || b.got.After.Sequence != 4 || b.got.Limit != 1000 {
		t.Fatalf("request changed: %+v", b.got)
	}
	b.page.Entries = nil
	w = bpHTTP(t, NewServer(b, id).Handler(), http.MethodPost, path, `{"version":1}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"entries":[]`) {
		t.Fatalf("empty image: %d %s", w.Code, w.Body.String())
	}
}

func TestCatalogOpenAPISchemaFieldDrift(t *testing.T) {
	for _, tc := range []struct {
		schema string
		typ    reflect.Type
	}{
		{"CatalogCheckpoint", reflect.TypeOf(domain.CatalogCheckpoint{})},
		{"CatalogRequest", reflect.TypeOf(domain.CatalogRequest{})},
		{"CatalogEntry", reflect.TypeOf(domain.CatalogEntry{})},
		{"CatalogPage", reflect.TypeOf(domain.CatalogPage{})},
	} {
		t.Run(tc.schema, func(t *testing.T) {
			props := schemaProps(t, "../../../../../schemas/openapi.yaml", tc.schema)
			fields := jsonFields(tc.typ)
			if len(props) != len(fields) {
				t.Fatalf("schema properties=%v Go fields=%v", props, fields)
			}
			for _, field := range fields {
				if !props[field] {
					t.Fatalf("schema missing %s", field)
				}
			}
		})
	}
}
