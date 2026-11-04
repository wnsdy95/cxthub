package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type admissionReadBackend struct{ Backend }

func admissionReadResult[Result any](ctx context.Context) (Result, error) {
	var result Result
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		return result, err
	}
	defer release()
	return result, nil
}

func (admissionReadBackend) GetDoc(ctx context.Context, _, _ domain.ContentHash) (domain.SessionDoc, error) {
	return admissionReadResult[domain.SessionDoc](ctx)
}

func (admissionReadBackend) Search(ctx context.Context, _ inbound.SearchInput) (inbound.SearchOutput, error) {
	return admissionReadResult[inbound.SearchOutput](ctx)
}

func (admissionReadBackend) Send(ctx context.Context, _ inbound.PullSendInput) (inbound.PullSendOutput, error) {
	return admissionReadResult[inbound.PullSendOutput](ctx)
}

func (admissionReadBackend) PullChunks(ctx context.Context, _ inbound.PullChunksInput) (inbound.PullChunksOutput, error) {
	return admissionReadResult[inbound.PullChunksOutput](ctx)
}

func (admissionReadBackend) Diff(ctx context.Context, _ inbound.DiffInput) (inbound.DiffOutput, error) {
	return admissionReadResult[inbound.DiffOutput](ctx)
}

func (admissionReadBackend) ReadDocEvents(ctx context.Context, _, _, _ domain.ContentHash, _, _ int) (domain.DocEventPage, error) {
	return admissionReadResult[domain.DocEventPage](ctx)
}

func (admissionReadBackend) ReadAgentHistoryPage(ctx context.Context, _, _ domain.ContentHash, _ domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	return admissionReadResult[domain.AgentHistoryPage](ctx)
}

func (admissionReadBackend) GetPendingState(context.Context, domain.ContentHash) (domain.PendingStateView, error) {
	return domain.PendingStateView{Projection: "on-hold-v1", Pending: []domain.Pending{}, Snapshots: []domain.Snapshot{}, Hold: []domain.GraphHoldCluster{}, OrphanSessions: []string{}, HoldCounts: map[string]int{}}, nil
}

func (admissionReadBackend) ReadDocViewEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int, string) (domain.DocViewPage, error) {
	return domain.DocViewPage{Next: -1}, nil
}

func (admissionReadBackend) GetPendingView(ctx context.Context, _ domain.ContentHash) (domain.PendingView, error) {
	_, release, err := app.AcquireResources(ctx, app.ResourceInteractive)
	if err != nil {
		return domain.PendingView{}, err
	}
	defer release()
	return domain.PendingView{Graph: &domain.GraphState{Version: 1}}, nil
}

func (admissionReadBackend) QueryGraphState(ctx context.Context, _ domain.ContentHash, _ string) (domain.GraphState, error) {
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		return domain.GraphState{}, err
	}
	defer release()
	return domain.GraphState{Version: 1}, nil
}

func (admissionReadBackend) GetRepositoryView(ctx context.Context, _ domain.ContentHash) (domain.RepositoryView, error) {
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		return domain.RepositoryView{}, err
	}
	defer release()
	return domain.RepositoryView{Graph: &domain.GraphState{Version: 1}}, nil
}

type admissionUnreadBody struct{ reads int }

func (body *admissionUnreadBody) Read([]byte) (int, error) { body.reads++; return 0, io.EOF }
func (*admissionUnreadBody) Close() error                  { return nil }

func TestResourceAdmissionHTTPRejectsBeforeUploadDecode(t *testing.T) {
	_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	server := &Server{b: admissionReadBackend{}}
	if err := server.SetRepositoryGraphQueries(admissionReadBackend{}, inbound.RepositoryGraphCompatibility); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []http.HandlerFunc{server.pushObjects, server.pushChunks} {
		body := &admissionUnreadBody{}
		request := httptest.NewRequest(http.MethodPost, "/", body)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler(response, request)
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || !strings.Contains(response.Body.String(), "resource_overloaded") || body.reads != 0 {
			t.Fatalf("overload decoded request or lost retry contract: code=%d body=%s reads=%d", response.Code, response.Body, body.reads)
		}
	}
	response := httptest.NewRecorder()
	server.pendingView(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("OnHold blocked by bulk upload: %d %s", response.Code, response.Body)
	}
}

type admissionResponseWriter struct {
	*httptest.ResponseRecorder
	checked  bool
	err      error
	writeErr error
}

func (writer *admissionResponseWriter) Write(payload []byte) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if release != nil {
		release()
	}
	writer.checked, writer.err = true, err
	if writer.writeErr != nil {
		return 0, writer.writeErr
	}
	return writer.ResponseRecorder.Write(payload)
}

func TestResourceAdmissionHTTPWholeDocumentPermitThroughResponse(t *testing.T) {
	ctx := systemTestContext()
	storage := store.NewFSStore(t.TempDir())
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	const token = "dev:admission@example.test:Owner"
	owner, err := identity.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	record, err := identity.CreateRepository(ctx, owner, "admission")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(record.ID))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
		t.Fatal(err)
	}
	service := app.NewService(storage, storage, nil, nil, storage)
	backend := admissionReadBackend{Backend: service}
	server := NewServer(backend, identity)
	if err := server.SetDocumentEventQueries(&documentEventHTTPQueries{read: backend.ReadDocEvents}, inbound.DocumentQueryCompatibility); err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/docs/" + string(repo)},
		{http.MethodGet, "/docs/" + string(repo) + "/events"},
		{http.MethodGet, "/docs/" + string(repo) + "/turns"},
		{http.MethodGet, "/search?q=term"},
		{http.MethodPost, "/pull/objects"},
		{http.MethodPost, "/pull/chunks"},
		{http.MethodPost, "/diff"},
	} {
		for _, writeErr := range []error{nil, io.ErrClosedPipe} {
			t.Run(fmt.Sprintf("%s/%v", route.path, writeErr), func(t *testing.T) {
				request := httptest.NewRequest(route.method, "/api/v1/repos/"+string(repo)+route.path, strings.NewReader(`{}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+token)
				writer := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: writeErr}
				handler.ServeHTTP(writer, request)
				if writer.Code != http.StatusOK || !writer.checked || !errors.Is(writer.err, context.DeadlineExceeded) {
					t.Fatalf("response escaped permit: code=%d checked=%t err=%v body=%s", writer.Code, writer.checked, writer.err, writer.Body)
				}
				check, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				_, release, err := app.AcquireResources(check, app.ResourceBulk)
				if err != nil {
					t.Fatal("response leaked permit", err)
				}
				release()
			})
		}
	}
}

func TestResourceAdmissionHTTPHeldThroughGraphEncoding(t *testing.T) {
	server := &Server{b: admissionReadBackend{}}
	if err := server.SetRepositoryGraphQueries(admissionReadBackend{}, inbound.RepositoryGraphCompatibility); err != nil {
		t.Fatal(err)
	}
	writer := &admissionResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	server.graphState(writer, httptest.NewRequest(http.MethodGet, "/?graph_encoding=indexed-v2", nil))
	if writer.Code != http.StatusOK || !writer.checked || !errors.Is(writer.err, context.DeadlineExceeded) {
		t.Fatalf("graph encoding escaped permit: status=%d checked=%t err=%v", writer.Code, writer.checked, writer.err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		t.Fatal("handler leaked permit", err)
	}
	release()
}

func TestResourceAdmissionHTTPRetryableErrors(t *testing.T) {
	for _, sentinel := range []error{domain.ErrResourceOverloaded, domain.ErrDocRangeIndexUnavailable} {
		response := httptest.NewRecorder()
		(&Server{}).respond(response, nil, fmt.Errorf("wrapped: %w", sentinel))
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
			t.Fatalf("not retryable: %v code=%d headers=%v", sentinel, response.Code, response.Header())
		}
	}
	response := httptest.NewRecorder()
	(&Server{}).respond(response, nil, fmt.Errorf("wrapped: %w", domain.ErrDocRangeIndexPending))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "3" || !strings.Contains(response.Body.String(), "doc_range_index_pending") {
		t.Fatalf("pending range retry contract: code=%d headers=%v body=%s", response.Code, response.Header(), response.Body)
	}
}

func TestPendingStateHTTPOptInNeverMasqueradesAsCompleteGraph(t *testing.T) {
	server := &Server{b: admissionReadBackend{}}
	if err := server.SetRepositoryGraphQueries(admissionReadBackend{}, inbound.RepositoryGraphCompatibility); err != nil {
		t.Fatal(err)
	}
	queries := &repositoryStateHTTPQueries{pending: domain.PendingStateView{Projection: "on-hold-v1", Pending: []domain.Pending{}, Snapshots: []domain.Snapshot{}, Hold: []domain.GraphHoldCluster{}, OrphanSessions: []string{}, HoldCounts: map[string]int{}}}
	if err := server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
		t.Fatal(err)
	}
	for _, projection := range []string{"", "on-hold-v1", "unknown"} {
		response := httptest.NewRecorder()
		server.pendingView(response, httptest.NewRequest(http.MethodGet, "/?projection="+projection, nil))
		if projection == "unknown" {
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("unknown projection accepted: %d", response.Code)
			}
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil || response.Code != http.StatusOK {
			t.Fatalf("projection response: %d %v", response.Code, err)
		}
		_, completeGraph := fields["graph"]
		if completeGraph != (projection == "") || projection != "" && string(fields["projection"]) != `"on-hold-v1"` {
			t.Fatalf("legacy/lightweight contracts conflated: %s", response.Body)
		}
	}
	for _, query := range []string{"projection=on-hold-v1&projection=on-hold-v1", "projection=&projection=on-hold-v1", "projection=%zz", "projection=on-hold-v1;graph_encoding=indexed-v2"} {
		response := httptest.NewRecorder()
		server.pendingView(response, httptest.NewRequest(http.MethodGet, "/?"+query, nil))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("ambiguous projection %q accepted: %d", query, response.Code)
		}
	}
}

func TestResourceAdmissionHTTPStrictEventsBulkButViewIndependent(t *testing.T) {
	_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	server := &Server{b: admissionReadBackend{}}
	configureDocumentViewTestServer(server)
	if err := server.SetDocumentEventQueries(&documentEventHTTPQueries{read: admissionReadBackend{}.ReadDocEvents}, inbound.DocumentQueryCompatibility); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.getDocEvents(response, httptest.NewRequest(http.MethodGet, "/?view=chat", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("fast view acquired bulk: %d %s", response.Code, response.Body)
	}
	response = httptest.NewRecorder()
	server.getDocEvents(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("strict events escaped bulk: %d %s", response.Code, response.Body)
	}
}
