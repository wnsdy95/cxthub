package memorypreparation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/preparation"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func preparationInput() domain.MemoryReadPreparationRequest {
	return domain.MemoryReadPreparationRequest{
		Version: 1,
		RepoID:  domain.HashContent([]byte("preparation repository")),
		Selection: domain.EffectiveMemorySelection{
			SnapshotID: domain.HashContent([]byte("preparation snapshot")),
			CodeCommit: strings.Repeat("a", 40),
		},
		Content:            "claims",
		Cursor:             "private-preparation-cursor",
		DocumentIdentities: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1},
	}
}

func preparationClient(test *testing.T, server *httptest.Server) *Client {
	test.Helper()
	client, err := NewClient(server.URL)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(client.http.CloseIdleConnections)
	return client
}

func assertPreparationError(test *testing.T, actual, expected error, secrets ...string) {
	test.Helper()
	if actual != expected {
		test.Fatalf("error = %v, want exact sentinel %v", actual, expected)
	}
	if actual != nil {
		for _, secret := range secrets {
			if secret != "" && strings.Contains(actual.Error(), secret) {
				test.Fatal("error exposed private request or response data")
			}
		}
	}
}

func TestNewClientOrigins(test *testing.T) {
	for _, origin := range []string{
		"https://preparation.example", "https://preparation.example:8443", "https://127.0.0.1",
		"https://[2001:db8::1]:443", "http://127.0.0.1", "http://127.1.2.3:8080",
		"http://localhost:8080", "http://LOCALHOST:8080", "http://[::1]:8080", "http://[::ffff:127.0.0.1]",
	} {
		test.Run(origin, func(test *testing.T) {
			client, err := NewClient(origin)
			if err != nil || client == nil {
				test.Fatalf("valid origin rejected: %v", err)
			}
			if client.endpoint != origin+domain.MemoryReadPreparationPath {
				test.Fatalf("endpoint changed: %s", client.endpoint)
			}
			if client.http == http.DefaultClient || client.http.Timeout != 5*time.Second || client.http.Jar != nil {
				test.Fatal("client must own its timeout and never retain cookies")
			}
			transport, valid := client.http.Transport.(*http.Transport)
			if !valid || transport == http.DefaultTransport || transport.Proxy != nil || !transport.DisableKeepAlives || !transport.DisableCompression {
				test.Fatal("transport must be private, direct, uncompressed, and non-reusing")
			}
			if transport.Protocols == nil || !transport.Protocols.HTTP1() || transport.Protocols.HTTP2() || transport.Protocols.UnencryptedHTTP2() {
				test.Fatal("transport permits automatic HTTP/2 replay")
			}
			if err := client.http.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
				test.Fatal("redirects are enabled")
			}
		})
	}
	for _, origin := range []string{
		"", " ", "preparation.example", "//preparation.example", "/relative", "ftp://preparation.example",
		"http://preparation.example", "http://localhost.example", "http://127.0.0.1.example",
		"http://10.0.0.1", "http://0.0.0.0", "http://[::]", "http://[2001:db8::1]", "http://127.1",
		"https://", "https:///host", "https:opaque", "https://user@preparation.example", "https://user:secret@preparation.example",
		"https://@preparation.example", "https://preparation.example/", "https://preparation.example/path",
		"https://preparation.example/%2f", "https://preparation.example?", "https://preparation.example?private=query",
		"https://preparation.example#", "https://preparation.example#private", "https://preparation.example:",
		"https://preparation.example:0", "https://preparation.example:65536", "https://preparation.example:-1",
		"https://preparation.example:invalid", "https://preparation.example:80:90", "https://:443",
		"https://[invalid]", "https://::1", "https://[::1", "http://[::1%25lo0]", "https://host%2fprivate",
		" https://preparation.example", "https://preparation.example ", "https://preparation.example\n",
		"https://bad host", "https://preparation.example\\other",
	} {
		test.Run("invalid/"+origin, func(test *testing.T) {
			client, err := NewClient(origin)
			if client != nil {
				test.Fatal("invalid origin created a client")
			}
			assertPreparationError(test, err, domain.ErrValidation, "private", "secret", "preparation.example")
		})
	}
}

func TestRequestMemoryPreparationCredentialsAndPayload(test *testing.T) {
	for _, secure := range []bool{false, true} {
		test.Run(fmt.Sprintf("tls=%t", secure), func(test *testing.T) {
			input := preparationInput()
			input.Selection.Branch = "feature/private-branch"
			requests := make(chan *http.Request, 3)
			bodies := make(chan []byte, 3)
			handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					test.Error(err)
				}
				requests <- request
				bodies <- body
				writer.Header().Set("Set-Cookie", "session=must-not-replay; Path=/")
				writer.Header().Set("Content-Type", "application/json; charset=utf-8")
				writer.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(writer, " { \"accepted\" : true }\n")
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			client := preparationClient(test, server)
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				client.http.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
			}
			for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
				test.Setenv(name, "http://127.0.0.1:1")
			}
			test.Setenv("NO_PROXY", "")
			origin, _ := url.Parse(server.URL)
			for _, bearer := range []string{"first-exact-token", "second-exact-token", strings.Repeat("b", 8192)} {
				if err := client.RequestMemoryPreparation(context.Background(), bearer, input); err != nil {
					test.Fatal(err)
				}
				request, body := <-requests, <-bodies
				if request.Host != origin.Host || request.Method != http.MethodPost || request.RequestURI != domain.MemoryReadPreparationPath {
					test.Fatalf("request escaped pinned endpoint: %s %s %s", request.Method, request.Host, request.RequestURI)
				}
				if !reflect.DeepEqual(request.Header.Values("Authorization"), []string{"Bearer " + bearer}) || request.Header.Get("Cookie") != "" {
					test.Fatal("request changed bearer or retained cookies")
				}
				if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" || request.Header.Get("Accept-Encoding") != "" {
					test.Fatal("unexpected content negotiation")
				}
				var actual domain.MemoryReadPreparationRequest
				if json.Unmarshal(body, &actual) != nil || !reflect.DeepEqual(actual, input) || request.ContentLength != int64(len(body)) {
					test.Fatal("request payload changed")
				}
			}
		})
	}
}

func TestRequestMemoryPreparationRejectsInvalidInputBeforeSend(test *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := preparationClient(test, server)
	for _, bearer := range []string{"", " ", "\t", "\r\n", "private\rheader", "private\nheader", "private\x00token", "private\x7ftoken", " private", "private ", strings.Repeat("b", 8193)} {
		assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), bearer, preparationInput()), domain.ErrUnauthorized, "private", server.URL)
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*domain.MemoryReadPreparationRequest)
	}{
		{"version", func(input *domain.MemoryReadPreparationRequest) { input.Version = 2 }},
		{"repository", func(input *domain.MemoryReadPreparationRequest) { input.RepoID = "private-invalid-repository" }},
		{"snapshot", func(input *domain.MemoryReadPreparationRequest) {
			input.Selection.SnapshotID = "private-invalid-snapshot"
		}},
		{"commit", func(input *domain.MemoryReadPreparationRequest) {
			input.Selection.CodeCommit = "private-invalid-commit"
		}},
		{"content", func(input *domain.MemoryReadPreparationRequest) { input.Content = "private-invalid-content" }},
		{"cursor missing", func(input *domain.MemoryReadPreparationRequest) { input.Cursor = "" }},
		{"cursor too long", func(input *domain.MemoryReadPreparationRequest) {
			input.Cursor = strings.Repeat("c", domain.MaxPreparedMemoryCursorBytes+1)
		}},
		{"wire too long", func(input *domain.MemoryReadPreparationRequest) {
			input.Cursor = strings.Repeat("\x00", domain.MaxPreparedMemoryCursorBytes)
		}},
		{"identity", func(input *domain.MemoryReadPreparationRequest) {
			input.DocumentIdentities = []domain.DocumentIdentity{"private-invalid-identity"}
		}},
		{"duplicate identity", func(input *domain.MemoryReadPreparationRequest) {
			input.DocumentIdentities = []domain.DocumentIdentity{"", ""}
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			input := preparationInput()
			scenario.mutate(&input)
			assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "private-token", input), domain.ErrValidation, "private", server.URL)
		})
	}
	if calls.Load() != 0 {
		test.Fatal("invalid credentials or payload reached HTTP")
	}
}

func TestRequestMemoryPreparationRequestSizeBoundary(test *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if len(body) > domain.MaxMemoryReadPreparationBytes {
			test.Error("oversized request reached HTTP")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(writer, `{"accepted":true}`)
	}))
	defer server.Close()
	client := preparationClient(test, server)
	for _, size := range []int{domain.MaxMemoryReadPreparationBytes - 1, domain.MaxMemoryReadPreparationBytes, domain.MaxMemoryReadPreparationBytes + 1} {
		input := preparationInput()
		input.Cursor = ""
		base, err := json.Marshal(input)
		if err != nil {
			test.Fatal(err)
		}
		remaining := size - len(base)
		input.Cursor = strings.Repeat("\x00", remaining/6) + strings.Repeat("x", remaining%6)
		body, err := json.Marshal(input)
		if err != nil || len(body) != size || len(input.Cursor) > domain.MaxPreparedMemoryCursorBytes {
			test.Fatal("invalid request size fixture")
		}
		var expected error
		if size > domain.MaxMemoryReadPreparationBytes {
			expected = domain.ErrValidation
		}
		assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "private-token", input), expected)
	}
	if calls.Load() != 2 {
		test.Fatalf("requests = %d, want 2", calls.Load())
	}
}

func TestRequestMemoryPreparationResponseContract(test *testing.T) {
	accepted := `{"accepted":true}`
	for _, scenario := range []struct {
		name        string
		status      int
		contentType string
		body        string
		chunked     bool
		accepted    bool
	}{
		{name: "accepted", body: accepted, accepted: true},
		{name: "whitespace", body: "\n\t{\"accepted\" : true}\r\n", accepted: true},
		{name: "exact bound", body: accepted + strings.Repeat(" ", domain.MaxMemoryReadPreparationBytes-len(accepted)), accepted: true},
		{name: "charset", contentType: "application/json; charset=utf-8", body: accepted, accepted: true},
		{name: "empty"},
		{name: "whitespace only", body: " \n\t"},
		{name: "malformed", body: `{"accepted":`},
		{name: "missing", body: `{}`},
		{name: "false", body: `{"accepted":false}`},
		{name: "null accepted", body: `{"accepted":null}`},
		{name: "string accepted", body: `{"accepted":"true"}`},
		{name: "number accepted", body: `{"accepted":1}`},
		{name: "array", body: `[true]`},
		{name: "null", body: `null`},
		{name: "boolean", body: `true`},
		{name: "case mismatch", body: `{"Accepted":true}`},
		{name: "extra field", body: `{"accepted":true,"private":"response-body"}`},
		{name: "duplicate", body: `{"accepted":true,"accepted":true}`},
		{name: "duplicate escaped", body: `{"accepted":false,"\u0061ccepted":true}`},
		{name: "trailing object", body: accepted + `{}`},
		{name: "trailing scalar", body: accepted + `null`},
		{name: "trailing garbage", body: accepted + "private-response-body"},
		{name: "oversized", body: accepted + strings.Repeat(" ", domain.MaxMemoryReadPreparationBytes+1-len(accepted))},
		{name: "oversized chunked", body: accepted + strings.Repeat(" ", domain.MaxMemoryReadPreparationBytes+1-len(accepted)), chunked: true},
		{name: "plain text", contentType: "text/plain", body: accepted},
		{name: "missing content type", contentType: "missing", body: accepted},
		{name: "invalid content type", contentType: "application/json; broken", body: accepted},
		{name: "suffix JSON", contentType: "application/problem+json", body: accepted},
		{name: "200", status: http.StatusOK, body: accepted},
		{name: "201", status: http.StatusCreated, body: accepted},
		{name: "204", status: http.StatusNoContent, body: accepted},
		{name: "500", status: http.StatusInternalServerError, body: accepted},
		{name: "unknown rejection", status: http.StatusServiceUnavailable, body: `{"error":{"code":"private-unknown-code"}}`},
		{name: "wrong status", status: http.StatusForbidden, body: `{"error":{"code":"unauthenticated"}}`},
		{name: "wrong integrity status", status: http.StatusUnprocessableEntity, body: `{"error":{"code":"integrity_violation"}}`},
		{name: "wrong capture expiry status", status: http.StatusGone, body: `{"error":{"code":"memory_capture_expired"}}`},
		{name: "wrong overload status", status: http.StatusServiceUnavailable, body: `{"error":{"code":"resource_overloaded"}}`},
		{name: "known code on 200", status: http.StatusOK, body: `{"error":{"code":"forbidden"}}`},
		{name: "error on 202", body: `{"error":{"code":"forbidden"}}`},
		{name: "error malformed", status: http.StatusForbidden, body: `{"error":`},
		{name: "error missing", status: http.StatusForbidden, body: `{}`},
		{name: "error null", status: http.StatusForbidden, body: `{"error":null}`},
		{name: "error missing code", status: http.StatusForbidden, body: `{"error":{}}`},
		{name: "error non-string code", status: http.StatusForbidden, body: `{"error":{"code":42}}`},
		{name: "error null code", status: http.StatusForbidden, body: `{"error":{"code":null}}`},
		{name: "error empty code", status: http.StatusForbidden, body: `{"error":{"code":""}}`},
		{name: "error case mismatch", status: http.StatusForbidden, body: `{"Error":{"code":"forbidden"}}`},
		{name: "error code case mismatch", status: http.StatusForbidden, body: `{"error":{"Code":"forbidden"}}`},
		{name: "error duplicate", status: http.StatusForbidden, body: `{"error":{"code":"forbidden"},"error":{"code":"forbidden"}}`},
		{name: "error duplicate code", status: http.StatusForbidden, body: `{"error":{"code":"forbidden","code":"forbidden"}}`},
		{name: "error trailing", status: http.StatusForbidden, body: `{"error":{"code":"forbidden"}}{}`},
		{name: "error plain text", status: http.StatusForbidden, contentType: "text/plain", body: `{"error":{"code":"forbidden"}}`},
		{name: "error oversized", status: http.StatusForbidden, body: `{"error":{"code":"forbidden"}}` + strings.Repeat(" ", domain.MaxMemoryReadPreparationBytes)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				contentType := scenario.contentType
				if contentType == "" {
					contentType = "application/json"
				}
				if contentType == "missing" {
					writer.Header()["Content-Type"] = nil
				} else {
					writer.Header().Set("Content-Type", contentType)
				}
				status := scenario.status
				if status == 0 {
					status = http.StatusAccepted
				}
				writer.WriteHeader(status)
				if scenario.chunked {
					writer.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(writer, scenario.body)
			}))
			defer server.Close()
			client := preparationClient(test, server)
			expected := domain.ErrMemoryPreparationUnavailable
			if scenario.accepted {
				expected = nil
			}
			assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "private-token", preparationInput()), expected, server.URL, "private", "response-body")
		})
	}
}

func TestRequestMemoryPreparationRejections(test *testing.T) {
	for _, scenario := range []struct {
		status int
		code   string
		cause  error
	}{
		{401, "unauthenticated", domain.ErrUnauthorized},
		{403, "forbidden", domain.ErrForbidden},
		{404, "not_found", domain.ErrNotFound},
		{422, "validation", domain.ErrValidation},
		{503, "integrity_violation", domain.ErrIntegrity},
		{422, "memory_projection_limit", domain.ErrMemoryProjectionLimit},
		{409, "conflict", domain.ErrConflict},
		{409, "catalog_reset_required", domain.ErrCatalogResetRequired},
		{409, "memory_cursor_stale", domain.ErrEffectiveMemoryCursorStale},
		{409, "unsupported_document_identity", domain.ErrUnsupportedDocumentIdentity},
		{409, "document_identity_upgrade_required", domain.ErrDocumentIdentityUpgradeRequired},
		{409, "memory_capture_expired", domain.ErrMemoryCaptureExpired},
		{503, "audit_unavailable", domain.ErrBreakGlassAuditUnavailable},
		{503, "memory_read_needs_attention", domain.ErrMemoryReadNeedsAttention},
		{503, "prepared_memory_unavailable", domain.ErrCatalogUnsupported},
		{503, "memory_preparation_unavailable", domain.ErrMemoryPreparationUnavailable},
		{429, "resource_overloaded", domain.ErrResourceOverloaded},
		{503, "memory_read_not_ready", domain.ErrMemoryReadNotReady},
		{503, "deadline_exceeded", context.DeadlineExceeded},
		{503, "canceled", context.Canceled},
		{503, "unavailable", errMemoryPreparationFailed},
		{405, "method_not_allowed", domain.ErrMemoryPreparationUnavailable},
	} {
		test.Run(scenario.code, func(test *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", "application/json; charset=utf-8")
				writer.Header().Set("Retry-After", "1")
				writer.WriteHeader(scenario.status)
				_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{
					"code": scenario.code, "message": "private-response-body", "details": map[string]any{"retry_cursor": "private-cursor"},
				}})
			}))
			defer server.Close()
			client := preparationClient(test, server)
			assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "private-token", preparationInput()), scenario.cause, server.URL, "private")
			if calls.Load() != 1 {
				test.Fatal("rejection was retried")
			}
		})
	}
}

type preparationHandlerBackend struct {
	inbound.SnapshotReadIdentity
	outbound.RuntimeStore
	cause error
	calls atomic.Int64
}

func (*preparationHandlerBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, domain.ErrForbidden
}

func (*preparationHandlerBackend) ResolveMCPUser(_ context.Context, bearer string, resources ...string) (domain.User, error) {
	if bearer != "private-token" || !reflect.DeepEqual(resources, []string{"https://preparation.example/mcp"}) {
		return domain.User{}, domain.ErrUnauthorized
	}
	return domain.User{ID: "preparation-user"}, nil
}

func (*preparationHandlerBackend) AllowRequest(context.Context, string, int, time.Duration, time.Time) (bool, error) {
	return true, nil
}

func (backend *preparationHandlerBackend) RequestPreparedMemory(context.Context, domain.ContentHash, domain.EffectiveMemoryRequest) error {
	backend.calls.Add(1)
	if backend.cause != nil {
		return fmt.Errorf("private-backend-failure: %w", backend.cause)
	}
	return nil
}

type preparationRecordedResponse struct {
	http.ResponseWriter
	record *httptest.ResponseRecorder
}

func (writer preparationRecordedResponse) WriteHeader(status int) {
	writer.record.WriteHeader(status)
	writer.ResponseWriter.WriteHeader(status)
}

func (writer preparationRecordedResponse) Write(body []byte) (int, error) {
	_, _ = writer.record.Write(body)
	return writer.ResponseWriter.Write(body)
}

func (writer preparationRecordedResponse) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func TestRequestMemoryPreparationHandlerRoundTrip(test *testing.T) {
	for _, scenario := range []struct {
		status    int
		code      string
		cause     error
		retryable bool
	}{
		{202, "accepted", nil, false},
		{401, "unauthenticated", domain.ErrUnauthorized, false},
		{403, "forbidden", domain.ErrForbidden, false},
		{404, "not_found", domain.ErrNotFound, false},
		{422, "validation", domain.ErrValidation, false},
		{422, "memory_projection_limit", domain.ErrMemoryProjectionLimit, false},
		{409, "conflict", domain.ErrConflict, false},
		{409, "catalog_reset_required", domain.ErrCatalogResetRequired, false},
		{409, "memory_cursor_stale", domain.ErrEffectiveMemoryCursorStale, false},
		{409, "document_identity_upgrade_required", domain.ErrDocumentIdentityUpgradeRequired, false},
		{409, "memory_capture_expired", domain.ErrMemoryCaptureExpired, false},
		{429, "resource_overloaded", domain.ErrResourceOverloaded, true},
		{503, "integrity_violation", domain.ErrIntegrity, false},
		{503, "audit_unavailable", domain.ErrBreakGlassAuditUnavailable, false},
		{503, "memory_read_needs_attention", domain.ErrMemoryReadNeedsAttention, false},
		{503, "prepared_memory_unavailable", domain.ErrCatalogUnsupported, false},
		{503, "memory_preparation_unavailable", domain.ErrMemoryPreparationUnavailable, true},
		{503, "memory_read_not_ready", domain.ErrMemoryReadNotReady, true},
		{503, "deadline_exceeded", context.DeadlineExceeded, true},
		{503, "canceled", context.Canceled, false},
		{503, "unavailable", errors.New("private-unknown-failure"), false},
		{405, "method_not_allowed", nil, false},
	} {
		test.Run(scenario.code, func(test *testing.T) {
			backend := &preparationHandlerBackend{cause: scenario.cause}
			handler, err := preparation.NewMemoryPreparationHandler(backend, backend, backend, backend, "https://preparation.example", "https://preparation.example")
			if err != nil {
				test.Fatal(err)
			}
			responses := make(chan *httptest.ResponseRecorder, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if scenario.code == "method_not_allowed" {
					request = request.Clone(request.Context())
					request.Method = http.MethodGet
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(preparationRecordedResponse{ResponseWriter: writer, record: response}, request)
				responses <- response
			}))
			defer server.Close()
			client := preparationClient(test, server)
			expected := scenario.cause
			if scenario.code == "unavailable" {
				expected = errMemoryPreparationFailed
			}
			if scenario.code == "method_not_allowed" {
				expected = domain.ErrMemoryPreparationUnavailable
			}
			assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "private-token", preparationInput()), expected, "private", server.URL)
			response := <-responses
			if response.Code != scenario.status {
				test.Fatalf("handler status = %d, want %d", response.Code, scenario.status)
			}
			if scenario.code != "accepted" {
				var envelope struct {
					Error struct {
						Code      string `json:"code"`
						Retryable bool   `json:"retryable"`
					} `json:"error"`
				}
				if json.Unmarshal(response.Body.Bytes(), &envelope) != nil || envelope.Error.Code != scenario.code || envelope.Error.Retryable != scenario.retryable {
					test.Fatal("handler code or retryability disagrees with client contract")
				}
			}
			if scenario.code != "method_not_allowed" && backend.calls.Load() != 1 {
				test.Fatal("preparation was skipped or retried")
			}
		})
	}
}

func TestRequestMemoryPreparationNeverFollowsRedirects(test *testing.T) {
	var destinationCalls atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		destinationCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(writer, `{"accepted":true}`)
	}))
	defer destination.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, location := range []string{destination.URL + "/private-redirect", "/private-redirect", strings.TrimPrefix(destination.URL, "http:") + "/private-redirect"} {
			test.Run(fmt.Sprintf("%d/%s", status, location), func(test *testing.T) {
				var sourceCalls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					sourceCalls.Add(1)
					writer.Header().Set("Location", location)
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(status)
					_, _ = io.WriteString(writer, `{"accepted":true}`)
				}))
				defer server.Close()
				client := preparationClient(test, server)
				assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "private-token", preparationInput()), domain.ErrMemoryPreparationUnavailable, "private", server.URL, destination.URL)
				if sourceCalls.Load() != 1 || destinationCalls.Load() != 0 {
					test.Fatal("redirect replayed credentials or request")
				}
			})
		}
	}
}

func TestRequestMemoryPreparationContext(test *testing.T) {
	test.Run("already canceled", func(test *testing.T) {
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
		defer server.Close()
		client := preparationClient(test, server)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assertPreparationError(test, client.RequestMemoryPreparation(ctx, "", domain.MemoryReadPreparationRequest{}), context.Canceled)
		ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		assertPreparationError(test, client.RequestMemoryPreparation(ctx, "", domain.MemoryReadPreparationRequest{}), context.DeadlineExceeded)
		if calls.Load() != 0 {
			test.Fatal("canceled request reached HTTP")
		}
	})
	for _, mode := range []string{"canceled", "context deadline", "client timeout"} {
		for _, headers := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/headers=%t", mode, headers), func(test *testing.T) {
				started := make(chan struct{})
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					_, _ = io.Copy(io.Discard, request.Body)
					if headers {
						writer.Header().Set("Content-Type", "application/json")
						writer.WriteHeader(http.StatusAccepted)
						_, _ = io.WriteString(writer, `{"accepted":`)
						writer.(http.Flusher).Flush()
					}
					close(started)
					select {
					case <-request.Context().Done():
					case <-release:
					}
				}))
				defer server.Close()
				defer close(release)
				client := preparationClient(test, server)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				expected := context.Canceled
				if mode == "context deadline" {
					ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
					defer cancel()
					expected = context.DeadlineExceeded
				}
				if mode == "client timeout" {
					client.http.Timeout = 200 * time.Millisecond
					expected = context.DeadlineExceeded
				}
				result := make(chan error, 1)
				go func() { result <- client.RequestMemoryPreparation(ctx, "private-token", preparationInput()) }()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					test.Fatal("request did not reach loopback server")
				}
				if mode == "canceled" {
					cancel()
				}
				select {
				case err := <-result:
					assertPreparationError(test, err, expected, "private", server.URL)
				case <-time.After(2 * time.Second):
					test.Fatal("client ignored cancellation or timeout")
				}
			})
		}
	}
}

func TestRequestMemoryPreparationTransportFailures(test *testing.T) {
	for _, mode := range []string{"connection closed", "truncated body", "oversized content length"} {
		test.Run(mode, func(test *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, request.Body)
				if mode == "connection closed" {
					connection, _, err := writer.(http.Hijacker).Hijack()
					if err != nil {
						test.Error(err)
						return
					}
					_ = connection.Close()
					return
				}
				body := `{"accepted":true}`
				length := len(body) + 1
				if mode == "oversized content length" {
					length = domain.MaxMemoryReadPreparationBytes + 1
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Content-Length", strconv.Itoa(length))
				writer.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(writer, body)
			}))
			defer server.Close()
			client := preparationClient(test, server)
			assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "private-token", preparationInput()), domain.ErrMemoryPreparationUnavailable, "private", server.URL)
			if calls.Load() != 1 {
				test.Fatal("transport failure was retried")
			}
		})
	}
}
