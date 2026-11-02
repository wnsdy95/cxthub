package http

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleLegacyBackend struct {
	Backend
	eventCalls int
	viewCalls  int
	base       domain.ContentHash
	offset     int
	limit      int
	view       string
}

func (backend *docVisibleLegacyBackend) ReadDocEvents(_ context.Context, _, _, base domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	backend.eventCalls++
	backend.base, backend.offset, backend.limit = base, offset, limit
	return domain.DocEventPage{Next: -1}, nil
}

func (backend *docVisibleLegacyBackend) ReadDocViewEvents(_ context.Context, _, _, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	backend.viewCalls++
	backend.base, backend.offset, backend.limit, backend.view = base, offset, limit, view
	return domain.DocViewPage{Next: -1}, nil
}

type docVisibleHTTPBackend struct {
	*docVisibleLegacyBackend
	calls   int
	repo    domain.ContentHash
	hash    domain.ContentHash
	request domain.DocVisiblePageRequest
	read    func(context.Context) (domain.DocVisiblePage, error)
}

func (backend *docVisibleHTTPBackend) ReadDocVisiblePage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocVisiblePageRequest) (domain.DocVisiblePage, error) {
	backend.calls++
	backend.repo, backend.hash, backend.request = repo, hash, request
	if backend.read != nil {
		return backend.read(ctx)
	}
	return domain.DocVisiblePage{Projection: domain.DocVisibleProjection, Kind: "units", Hash: hash, View: request.View}, nil
}

func docVisibleTestSelector(repo, hash domain.ContentHash, purpose string) domain.DocVisibleSelector {
	return domain.DocVisibleSelector{
		Version: 1, Purpose: purpose, Repo: repo, Hash: hash,
		Generation: domain.HashContent([]byte("visible generation")), View: "chat",
		Total: 12, Unit: 1, Next: 4, Limit: 2,
	}
}

func encodeDocVisibleTestSelector(t *testing.T, selector domain.DocVisibleSelector) string {
	t.Helper()
	encoded, err := selector.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func docVisibleTestRequest(repo, hash domain.ContentHash, query string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+string(repo)+"/docs/"+string(hash)+"/events?"+query, nil)
	request.SetPathValue("repoID", string(repo))
	request.SetPathValue("hash", string(hash))
	return request
}

func TestDocVisibleHTTPParserContract(t *testing.T) {
	repo, hash, base := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc")), domain.HashContent([]byte("base"))
	cursor := encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "units"))
	expand := encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "fold"))
	cases := []struct {
		query string
		want  domain.DocVisiblePageRequest
	}{
		{"view=all", domain.DocVisiblePageRequest{View: "all", Offset: -1, Limit: 20}},
		{"view=chat", domain.DocVisiblePageRequest{View: "chat", Offset: -1, Limit: 20}},
		{"view=prompts", domain.DocVisiblePageRequest{View: "prompts", Offset: -1, Limit: 20}},
		{"view=chat&offset=0&limit=1", domain.DocVisiblePageRequest{View: "chat", Offset: 0, Limit: 1}},
		{"view=all&offset=-1&limit=100&base=" + string(base), domain.DocVisiblePageRequest{View: "all", Base: base, Offset: -1, Limit: 100}},
		{"view=prompts&offset=123&limit=20", domain.DocVisiblePageRequest{View: "prompts", Offset: 123, Limit: 20}},
		{"cursor=" + cursor, domain.DocVisiblePageRequest{Cursor: cursor}},
		{"expand=" + expand, domain.DocVisiblePageRequest{Expand: expand}},
	}
	for _, test := range cases {
		t.Run(test.query, func(t *testing.T) {
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}
			writer := httptest.NewRecorder()
			(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, "projection=visible-v1&"+test.query))
			if writer.Code != http.StatusOK || backend.calls != 1 || backend.request != test.want || backend.repo != repo || backend.hash != hash {
				t.Fatalf("status=%d calls=%d request=%+v want=%+v body=%s", writer.Code, backend.calls, backend.request, test.want, writer.Body)
			}
			if backend.eventCalls != 0 || backend.viewCalls != 0 {
				t.Fatal("visible projection fell back to legacy reads")
			}
		})
	}
}

func TestDocVisibleHTTPRejectsInvalidQueryBeforeBackend(t *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	selector := docVisibleTestSelector(repo, hash, "units")
	cursor := encodeDocVisibleTestSelector(t, selector)
	expand := encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "fold"))
	queries := []string{
		"projection", "projection=", "projection=other&view=chat", "projection=visible-v2&view=chat",
		"projection=visible-v1", "projection=visible-v1&view=raw", "projection=visible-v1&view=CHAT",
		"projection=visible-v1&view=chat&unknown=value", "projection=visible-v1&view=chat&=value",
		"projection=visible-v1&view=chat&offset=-2", "projection=visible-v1&view=chat&limit=0", "projection=visible-v1&view=chat&limit=101",
		fmt.Sprintf("projection=visible-v1&view=chat&offset=%d", domain.MaxConversationDocumentBytes+1),
		"projection=visible-v1&view=chat&base=invalid", "projection=visible-v1&view=chat&base=" + strings.ToUpper(string(hash)),
		"projection=visible-v1&view=chat&cursor=" + cursor, "projection=visible-v1&cursor=" + cursor + "&expand=" + expand,
		"projection=visible-v1&view=%zz", "projection=visible-v1&view=chat;offset=0", "projection=%zz&view=chat",
		"projection=visible-v1&view=chat&%zz=value", "projection=visible-v1&cursor=not-a-selector", "projection=visible-v1&expand=not-a-selector",
		"projection=visible-v1&cursor=" + expand, "projection=visible-v1&expand=" + cursor,
	}
	initial := "projection=visible-v1&view=chat&offset=0&limit=20&base=" + string(hash)
	for _, key := range []string{"projection", "view", "offset", "limit", "base"} {
		values, err := url.ParseQuery(initial)
		if err != nil {
			t.Fatal(err)
		}
		values.Add(key, values.Get(key))
		queries = append(queries, values.Encode())
		values[key] = []string{""}
		queries = append(queries, values.Encode())
	}
	for _, token := range []struct{ key, value string }{{"cursor", cursor}, {"expand", expand}} {
		prefix := "projection=visible-v1&" + token.key + "="
		queries = append(queries, prefix, prefix+token.value+"&"+token.key+"="+token.value)
		for _, extra := range []string{"view=chat", "base=" + string(hash), "offset=0", "limit=20", "view=", "base=", "offset=", "limit=", "unknown=value"} {
			queries = append(queries, prefix+token.value+"&"+extra)
		}
		for _, malformed := range []string{token.value + "=", " " + token.value, strings.Repeat("a", 3000), "e30"} {
			queries = append(queries, prefix+url.QueryEscape(malformed))
		}
	}
	for _, key := range []string{"offset", "limit"} {
		for _, raw := range []string{"1.5", "1e1", "0x10", "+1", " 1", "1 ", "-0", "-01", "9999999999999999999999999"} {
			queries = append(queries, "projection=visible-v1&view=chat&"+key+"="+url.QueryEscape(raw))
		}
	}
	for _, purpose := range []string{"units", "fold"} {
		key := "cursor"
		if purpose == "fold" {
			key = "expand"
		}
		for _, mutate := range []func(*domain.DocVisibleSelector){
			func(value *domain.DocVisibleSelector) { value.Repo = domain.HashContent([]byte("other repo")) },
			func(value *domain.DocVisibleSelector) { value.Hash = domain.HashContent([]byte("other doc")) },
		} {
			mismatch := docVisibleTestSelector(repo, hash, purpose)
			mutate(&mismatch)
			queries = append(queries, "projection=visible-v1&"+key+"="+encodeDocVisibleTestSelector(t, mismatch))
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{
		string(raw) + " ", strings.Replace(string(raw), `"version":1`, `"version":2`, 1),
		strings.Replace(string(raw), `"purpose":"units"`, `"purpose":"other"`, 1),
		strings.Replace(string(raw), `"limit":2`, `"limit":101`, 1),
		strings.Replace(string(raw), `{`, `{"extra":true,`, 1),
		strings.Replace(string(raw), `{`, `{"version":1,`, 1),
	} {
		queries = append(queries, "projection=visible-v1&cursor="+base64.RawURLEncoding.EncodeToString([]byte(malformed)))
	}
	for index, query := range queries {
		t.Run(fmt.Sprintf("query-%d", index), func(t *testing.T) {
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}
			writer := httptest.NewRecorder()
			(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, query))
			if writer.Code != http.StatusUnprocessableEntity || !strings.Contains(writer.Body.String(), `"code":"validation"`) || backend.calls != 0 || backend.eventCalls != 0 || backend.viewCalls != 0 {
				t.Fatalf("query=%s status=%d calls=%d body=%s", query, writer.Code, backend.calls, writer.Body)
			}
		})
	}
}

func TestDocVisibleHTTPLegacyContractWithoutProjection(t *testing.T) {
	for _, test := range []struct {
		query, view   string
		offset, limit int
	}{
		{"", "", -1, 50},
		{"view=&offset=&limit=&base=&unknown=ignored&cursor=ignored&expand=ignored", "", -1, 50},
		{"view=chat&unknown=ignored", "chat", -1, 20},
		{"view=raw&offset=2&limit=1", "raw", 2, 1},
	} {
		t.Run(test.query, func(t *testing.T) {
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}
			server := configureDocumentViewTestServer(&Server{b: backend})
			if err := server.SetDocumentEventQueries(&documentEventHTTPQueries{read: backend.ReadDocEvents}, inbound.DocumentQueryCompatibility); err != nil {
				t.Fatal(err)
			}
			writer := httptest.NewRecorder()
			server.getDocEvents(writer, httptest.NewRequest(http.MethodGet, "/?"+test.query, nil))
			if writer.Code != http.StatusOK || backend.calls != 0 || backend.eventCalls+backend.viewCalls != 1 || backend.view != test.view || backend.offset != test.offset || backend.limit != test.limit {
				t.Fatalf("legacy contract changed: status=%d backend=%+v body=%s", writer.Code, backend.docVisibleLegacyBackend, writer.Body)
			}
		})
	}
}

func TestDocVisibleHTTPWireContract(t *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	cursor := encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "units"))
	expand := encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "fold"))
	event := domain.DocVisibleEvent{DocViewEvent: domain.DocViewEvent{
		Event: domain.CIREvent{Kind: domain.EventMessage, Seq: 3, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "answer"}}}, Index: 3, Bytes: 90,
	}, SeedSummary: true}
	deferred := domain.DocVisibleEvent{DocViewEvent: domain.DocViewEvent{
		Event: domain.CIREvent{Kind: domain.EventToolResult, Seq: 2, Output: "hidden body"}, Index: 2, Bytes: 100, Deferred: true,
	}, EmptyReasoning: true}
	for _, kind := range []string{"units", "fold"} {
		t.Run(kind, func(t *testing.T) {
			page := domain.DocVisiblePage{
				Projection: domain.DocVisibleProjection, Kind: kind, Hash: hash,
				Generation: domain.HashContent([]byte("visible generation")), Rules: 1, View: "chat",
				Envelope: domain.CIREnvelope{CIRVersion: "1"}, Total: 12, Start: 1, From: 1, To: 4,
				Units:  []domain.DocVisiblePageUnit{{From: 1, To: 4, TurnIndex: 0, Event: &event, Fold: &domain.DocVisibleFold{From: 1, To: 3, Rows: 2, Cursor: expand}}},
				Events: []domain.DocVisibleEvent{}, NextCursor: &cursor,
			}
			query := "projection=visible-v1&view=chat"
			if kind == "fold" {
				page.Units, page.Events, page.Fold, page.NextCursor = []domain.DocVisiblePageUnit{}, []domain.DocVisibleEvent{deferred}, &domain.DocVisibleFoldRange{From: 1, To: 3}, nil
				page.From, page.To = 2, 3
				query = "projection=visible-v1&expand=" + expand
			}
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}, read: func(context.Context) (domain.DocVisiblePage, error) { return page, nil }}
			writer := httptest.NewRecorder()
			(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, query))
			expected, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			if writer.Code != http.StatusOK || strings.TrimSpace(writer.Body.String()) != string(expected) || writer.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("wire contract changed: status=%d body=%s want=%s", writer.Code, writer.Body, expected)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(writer.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"projection", "kind", "hash", "base", "generation", "rules", "view", "envelope", "total", "inherited", "start", "from", "to", "units", "fold", "events", "next_cursor"} {
				if _, present := fields[field]; !present {
					t.Fatalf("missing wire field %s", field)
				}
			}
			for _, field := range []string{"offset", "next"} {
				if _, present := fields[field]; present {
					t.Fatalf("legacy field leaked: %s", field)
				}
			}
			if kind == "units" && (string(fields["fold"]) != "null" || !strings.Contains(string(fields["units"]), `"seed_summary":true`) || !strings.Contains(string(fields["units"]), `"empty_reasoning":false`)) {
				t.Fatalf("unit metadata lost: %s", writer.Body)
			}
			if kind == "fold" && (string(fields["next_cursor"]) != "null" || strings.Contains(writer.Body.String(), "hidden body") || !strings.Contains(string(fields["events"]), `"deferred":true`) || !strings.Contains(string(fields["events"]), `"seed_summary":false`) || !strings.Contains(string(fields["events"]), `"empty_reasoning":true`)) {
				t.Fatalf("fold/deferred metadata lost: %s", writer.Body)
			}
		})
	}
}

func TestDocVisibleHTTPErrorMappingAndUnsupportedBackend(t *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	for _, test := range []struct {
		err    error
		status int
		code   string
		retry  string
	}{
		{domain.ErrDocVisibleProjectionUnsupported, 409, "doc_visible_projection_unsupported", ""},
		{domain.ErrDocVisibleGenerationStale, 409, "doc_visible_generation_stale", ""},
		{domain.ErrValidation, 422, "validation", ""},
		{domain.ErrDocRangeIndexPending, 503, "doc_range_index_pending", "3"},
		{domain.ErrResourceOverloaded, 503, "resource_overloaded", "1"},
	} {
		t.Run(test.code, func(t *testing.T) {
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}, read: func(context.Context) (domain.DocVisiblePage, error) {
				return domain.DocVisiblePage{}, fmt.Errorf("visible read: %w", test.err)
			}}
			writer := httptest.NewRecorder()
			(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, "projection=visible-v1&view=chat"))
			if writer.Code != test.status || !strings.Contains(writer.Body.String(), `"code":"`+test.code+`"`) || writer.Header().Get("Retry-After") != test.retry || backend.eventCalls+backend.viewCalls != 0 {
				t.Fatalf("wrong error: status=%d body=%s", writer.Code, writer.Body)
			}
		})
	}
	backend := &docVisibleLegacyBackend{}
	server := configureDocumentViewTestServer(&Server{b: backend})
	for _, test := range []struct {
		query string
		code  int
	}{
		{"projection=visible-v1&view=chat", 409},
		{"projection=visible-v1&cursor=" + encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "units")), 409},
		{"projection=visible-v1&expand=" + encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "fold")), 409},
		{"projection=visible-v1", 422},
		{"projection=unsupported&view=chat", 422},
	} {
		writer := httptest.NewRecorder()
		server.getDocEvents(writer, docVisibleTestRequest(repo, hash, test.query))
		if writer.Code != test.code || backend.eventCalls+backend.viewCalls != 0 || test.code == 409 && !strings.Contains(writer.Body.String(), `"code":"doc_visible_projection_unsupported"`) {
			t.Fatalf("missing optional viewer: status=%d body=%s", writer.Code, writer.Body)
		}
	}
}

type docVisibleDeadlineWriter struct {
	*deadlineDocWriter
	readCalls, writeCalls int
}

func (writer *docVisibleDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.readCalls++
	return writer.deadlineDocWriter.SetReadDeadline(deadline)
}

func (writer *docVisibleDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	writer.writeCalls++
	return writer.deadlineDocWriter.SetWriteDeadline(deadline)
}

func TestDocVisibleHTTPDeadlineAndApplicationOwnedPermit(t *testing.T) {
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	queries := []string{"view=chat", "cursor=" + encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "units")), "expand=" + encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "fold"))}
	for index, query := range queries {
		for _, parentLimit := range []time.Duration{0, 30 * time.Second} {
			t.Run(fmt.Sprintf("mode-%d/parent-%s", index, parentLimit), func(t *testing.T) {
				parent := context.Background()
				if parentLimit != 0 {
					var cancel context.CancelFunc
					parent, cancel = context.WithTimeout(parent, parentLimit)
					defer cancel()
				}
				var observed context.Context
				backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}, read: func(ctx context.Context) (domain.DocVisiblePage, error) {
					observed = ctx
					probe, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					for _, class := range []app.ResourceClass{app.ResourceBulk, app.ResourceInteractive} {
						_, release, err := app.AcquireResources(probe, class)
						if err != nil {
							t.Fatal("HTTP retained an outer permit", err)
						}
						defer release()
					}
					return domain.DocVisiblePage{}, nil
				}}
				writer := &docVisibleDeadlineWriter{deadlineDocWriter: &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder()}}
				start := time.Now()
				(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, "projection=visible-v1&"+query).WithContext(parent))
				if observed == nil || writer.Code != http.StatusOK || writer.readCalls != 1 || writer.writeCalls != 1 {
					t.Fatalf("missing or duplicate deadline: status=%d read=%d write=%d", writer.Code, writer.readCalls, writer.writeCalls)
				}
				deadline, present := observed.Deadline()
				if !present || !writer.read.Equal(deadline) || !writer.write.Equal(deadline) {
					t.Fatal("transport and service deadlines differ")
				}
				if parentLimit == 0 {
					if deadline.Before(start.Add(resourceRequestLimit)) || deadline.After(time.Now().Add(resourceRequestLimit)) {
						t.Fatal("deadline not bounded")
					}
				} else if expected, _ := parent.Deadline(); !deadline.Equal(expected) {
					t.Fatal("parent deadline extended")
				}
				if !errors.Is(observed.Err(), context.Canceled) || parent.Err() != nil {
					t.Fatal("request cancellation missing or leaked to parent")
				}
			})
		}
	}
	backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{}}
	writer := &deadlineDocWriter{ResponseRecorder: httptest.NewRecorder(), failure: errors.New("deadline failed")}
	(configureDocumentViewTestServer(&Server{b: backend})).getDocEvents(writer, docVisibleTestRequest(repo, hash, "projection=visible-v1&view=chat"))
	if writer.Code != http.StatusInternalServerError || backend.calls != 0 {
		t.Fatal("unbounded service call after deadline failure", writer.Code, backend.calls)
	}
}

func TestDocVisibleHTTPContinuationRechecksCurrentAccess(t *testing.T) {
	for _, revoke := range []string{"membership", "credential"} {
		t.Run(revoke, func(t *testing.T) {
			ctx := systemTestContext()
			storage := store.NewFSStore(t.TempDir())
			identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
			owner, err := identity.Authenticate(ctx, "dev:visible-owner@example.test:Owner")
			if err != nil {
				t.Fatal(err)
			}
			viewer, session, err := identity.Login(ctx, "dev:visible-viewer@example.test:Viewer", "visible pages")
			if err != nil {
				t.Fatal(err)
			}
			record, err := identity.CreateRepository(ctx, owner, "visible-auth")
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: viewer.ID, Role: domain.RoleViewer}); err != nil {
				t.Fatal(err)
			}
			repo, hash := domain.HashContent([]byte(record.ID)), domain.HashContent([]byte("doc"))
			if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
				t.Fatal(err)
			}
			backend := &docVisibleHTTPBackend{docVisibleLegacyBackend: &docVisibleLegacyBackend{Backend: app.NewService(storage, storage, nil, nil, storage)}}
			handler := newDocumentViewTestServer(backend, identity).Handler()
			queries := []string{"view=chat", "cursor=" + encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "units")), "expand=" + encodeDocVisibleTestSelector(t, docVisibleTestSelector(repo, hash, "fold"))}
			requestPage := func(query, token string) *httptest.ResponseRecorder {
				t.Helper()
				request := docVisibleTestRequest(repo, hash, "projection=visible-v1&"+query)
				if token != "" {
					request.Header.Set("Authorization", "Bearer "+token)
				}
				writer := httptest.NewRecorder()
				handler.ServeHTTP(writer, request)
				return writer
			}
			for _, query := range queries {
				for _, caller := range []struct {
					token  string
					status int
				}{{"", 401}, {"dev:visible-outsider@example.test:Outsider", 403}} {
					before := backend.calls
					writer := requestPage(query, caller.token)
					if writer.Code != caller.status || backend.calls != before {
						t.Fatalf("selector bypassed auth: status=%d calls=%d body=%s", writer.Code, backend.calls, writer.Body)
					}
				}
				if writer := requestPage(query, session.Token); writer.Code != http.StatusOK {
					t.Fatalf("authorized control rejected: %d %s", writer.Code, writer.Body)
				}
			}
			want := http.StatusForbidden
			if revoke == "membership" {
				err = identity.RemoveMember(ctx, owner.ID, record.ID, viewer.ID)
			} else {
				err = identity.Logout(ctx, session.Token)
				want = http.StatusUnauthorized
			}
			if err != nil {
				t.Fatal(err)
			}
			before := backend.calls
			for _, query := range queries {
				writer := requestPage(query, session.Token)
				if writer.Code != want || backend.calls != before {
					t.Fatalf("revoked reader reached projection: status=%d calls=%d want=%d body=%s", writer.Code, backend.calls, want, writer.Body)
				}
			}
		})
	}
}

func TestDocVisibleOpenAPISchemaFieldDrift(t *testing.T) {
	const spec = "../../../../../schemas/openapi.yaml"
	for _, test := range []struct {
		name   string
		typeOf reflect.Type
	}{
		{"DocVisiblePage", reflect.TypeOf(domain.DocVisiblePage{})},
		{"DocVisiblePageUnit", reflect.TypeOf(domain.DocVisiblePageUnit{})},
		{"DocVisibleFold", reflect.TypeOf(domain.DocVisibleFold{})},
		{"DocVisibleFoldRange", reflect.TypeOf(domain.DocVisibleFoldRange{})},
		{"DocVisibleSelector", reflect.TypeOf(domain.DocVisibleSelector{})},
	} {
		t.Run(test.name, func(t *testing.T) {
			properties, fields := schemaProps(t, spec, test.name), jsonFields(test.typeOf)
			for _, field := range fields {
				if !properties[field] {
					t.Errorf("missing schema property %s", field)
				}
			}
			for property := range properties {
				if !contains(fields, property) {
					t.Errorf("phantom schema property %s", property)
				}
			}
		})
	}
	properties := schemaProps(t, spec, "DocVisibleEvent")
	for _, field := range []string{"kind", "seq", "event_index", "event_bytes", "deferred", "seed_summary", "empty_reasoning"} {
		if !properties[field] {
			t.Errorf("missing visible event property %s", field)
		}
	}
	raw, err := os.ReadFile(spec)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(raw), "  /repos/{repoID}/docs/{hash}/events:\n")
	end := strings.Index(string(raw), "  /repos/{repoID}/docs/{hash}/event-fragments:\n")
	if start < 0 || end <= start {
		t.Fatal("missing document events route")
	}
	route := string(raw[start:end])
	for _, contract := range []string{"name: projection", "name: cursor", "name: expand", "DocVisiblePage", `"409"`, "doc_visible_projection_unsupported", "doc_visible_generation_stale", "unsigned", "not capabilities", "duplicate", "unknown", "empty"} {
		if !strings.Contains(route, contract) {
			t.Errorf("missing visible route contract %q", contract)
		}
	}
}
