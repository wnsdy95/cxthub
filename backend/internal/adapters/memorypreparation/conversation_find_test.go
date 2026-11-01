package memorypreparation

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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
)

func conversationPreparationInput() domain.ConversationFindPreparationRequest {
	return domain.ConversationFindPreparationRequest{
		Version: 1,
		RepoID:  domain.HashContent([]byte("conversation preparation repository")),
		Hash:    domain.HashContent([]byte("conversation preparation document")),
		Find: domain.ConversationFindRequest{
			Query:  "private search \uac80\uc0c9",
			Cursor: "private-conversation-cursor",
		},
		DocumentIdentities: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1},
	}
}

func TestNewClientConversationEndpoint(test *testing.T) {
	for _, origin := range []string{"https://preparation.example", "https://preparation.example:8443", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		test.Run(origin, func(test *testing.T) {
			client, err := NewClient(origin)
			if err != nil {
				test.Fatal(err)
			}
			if client.conversationEndpoint != origin+domain.ConversationFindPreparationPath || client.endpoint != origin+domain.MemoryReadPreparationPath {
				test.Fatal("preparation endpoints did not preserve the fixed origin and paths")
			}
		})
	}
}

func TestRequestConversationFindPreparationCredentialsAndPayload(test *testing.T) {
	for _, secure := range []bool{false, true} {
		test.Run(fmt.Sprintf("tls=%t", secure), func(test *testing.T) {
			input := conversationPreparationInput()
			requests := make(chan *http.Request, 4)
			bodies := make(chan []byte, 4)
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
			var proxyCalls atomic.Int64
			proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				proxyCalls.Add(1)
				writer.WriteHeader(http.StatusBadGateway)
			}))
			defer proxy.Close()
			for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
				test.Setenv(name, proxy.URL)
			}
			test.Setenv("NO_PROXY", "")
			origin, _ := url.Parse(server.URL)
			for _, bearer := range []string{"first-exact-token", "second-exact-token", strings.Repeat("b", 8192)} {
				assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), bearer, input), nil)
				request, body := <-requests, <-bodies
				if request.Host != origin.Host || request.Method != http.MethodPost || request.RequestURI != domain.ConversationFindPreparationPath {
					test.Fatal("conversation request escaped the pinned endpoint")
				}
				if !reflect.DeepEqual(request.Header.Values("Authorization"), []string{"Bearer " + bearer}) || request.Header.Get("Cookie") != "" || request.Header.Get("Referer") != "" {
					test.Fatal("request changed bearer or retained cookies or referrer")
				}
				if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" || request.Header.Get("Accept-Encoding") != "" {
					test.Fatal("unexpected content negotiation")
				}
				var actual domain.ConversationFindPreparationRequest
				if json.Unmarshal(body, &actual) != nil || !reflect.DeepEqual(actual, input) || request.ContentLength != int64(len(body)) {
					test.Fatal("conversation request payload changed")
				}
			}
			assertPreparationError(test, client.RequestMemoryPreparation(context.Background(), "memory-exact-token", preparationInput()), nil)
			request, body := <-requests, <-bodies
			var actual domain.MemoryReadPreparationRequest
			if request.Host != origin.Host || request.RequestURI != domain.MemoryReadPreparationPath || request.Header.Get("Authorization") != "Bearer memory-exact-token" || request.Header.Get("Cookie") != "" ||
				json.Unmarshal(body, &actual) != nil || !reflect.DeepEqual(actual, preparationInput()) {
				test.Fatal("conversation calls changed memory endpoint, credentials, or payload")
			}
			if proxyCalls.Load() != 0 {
				test.Fatal("preparation forwarded credentials to an environment proxy")
			}
		})
	}
}

func TestRequestConversationFindPreparationRejectsInvalidInputBeforeSend(test *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := preparationClient(test, server)
	for _, bearer := range []string{"", " ", "\t", "\r\n", "private\rheader", "private\nheader", "private\x00token", "private\x1ftoken", "private\x7ftoken", " private", "private ", strings.Repeat("b", 8193)} {
		assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), bearer, conversationPreparationInput()), domain.ErrUnauthorized, "private", server.URL)
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*domain.ConversationFindPreparationRequest)
	}{
		{"version", func(input *domain.ConversationFindPreparationRequest) { input.Version = 2 }},
		{"repository", func(input *domain.ConversationFindPreparationRequest) { input.RepoID = "private-invalid-repository" }},
		{"hash", func(input *domain.ConversationFindPreparationRequest) { input.Hash = "private-invalid-document" }},
		{"missing query", func(input *domain.ConversationFindPreparationRequest) { input.Find.Query = "" }},
		{"invalid query", func(input *domain.ConversationFindPreparationRequest) { input.Find.Query = "\xff" }},
		{"query too long", func(input *domain.ConversationFindPreparationRequest) {
			input.Find.Query = strings.Repeat("\ud55c", domain.MaxDocFindQueryRunes+1)
		}},
		{"cursor too long", func(input *domain.ConversationFindPreparationRequest) {
			input.Find.Cursor = strings.Repeat("c", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes)+1)
		}},
		{"wire too long", func(input *domain.ConversationFindPreparationRequest) {
			input.Find.Cursor = strings.Repeat("\x00", base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes))
		}},
		{"identity", func(input *domain.ConversationFindPreparationRequest) {
			input.DocumentIdentities = []domain.DocumentIdentity{"private-invalid-identity"}
		}},
		{"duplicate identity", func(input *domain.ConversationFindPreparationRequest) {
			input.DocumentIdentities = []domain.DocumentIdentity{"", ""}
		}},
		{"too many identities", func(input *domain.ConversationFindPreparationRequest) {
			input.DocumentIdentities = []domain.DocumentIdentity{"", domain.DocumentIdentityRootV1, ""}
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			input := conversationPreparationInput()
			scenario.mutate(&input)
			assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), "private-token", input), domain.ErrValidation, "private", server.URL)
		})
	}
	if calls.Load() != 0 {
		test.Fatal("invalid credentials or conversation payload reached HTTP")
	}
}

func TestRequestConversationFindPreparationRequestSizeBoundary(test *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if len(body) > domain.MaxConversationFindPreparationBytes {
			test.Error("oversized conversation request reached HTTP")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(writer, `{"accepted":true}`)
	}))
	defer server.Close()
	client := preparationClient(test, server)
	for _, size := range []int{domain.MaxConversationFindPreparationBytes - 1, domain.MaxConversationFindPreparationBytes, domain.MaxConversationFindPreparationBytes + 1} {
		input := conversationPreparationInput()
		input.Find.Cursor = "x"
		base, err := json.Marshal(input)
		if err != nil {
			test.Fatal(err)
		}
		remaining := size - len(base) + 1
		input.Find.Cursor = strings.Repeat("\x00", remaining/6) + strings.Repeat("x", remaining%6)
		body, err := json.Marshal(input)
		if err != nil || len(body) != size || len(input.Find.Cursor) > base64.RawURLEncoding.EncodedLen(domain.MaxDocFindSelectorBytes) {
			test.Fatal("invalid conversation request size fixture")
		}
		var expected error
		if size > domain.MaxConversationFindPreparationBytes {
			expected = domain.ErrValidation
		}
		assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), "private-token", input), expected)
	}
	if calls.Load() != 2 {
		test.Fatalf("conversation requests = %d, want 2", calls.Load())
	}
}

func TestRequestConversationFindPreparationResponseContract(test *testing.T) {
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
		{name: "exact bound", body: accepted + strings.Repeat(" ", domain.MaxConversationFindPreparationBytes-len(accepted)), accepted: true},
		{name: "exact chunked bound", body: accepted + strings.Repeat(" ", domain.MaxConversationFindPreparationBytes-len(accepted)), chunked: true, accepted: true},
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
		{name: "unknown field", body: `{"accepted":true,"private":"response-body"}`},
		{name: "duplicate", body: `{"accepted":true,"accepted":true}`},
		{name: "duplicate escaped", body: `{"accepted":false,"\u0061ccepted":true}`},
		{name: "trailing object", body: accepted + `{}`},
		{name: "trailing scalar", body: accepted + `null`},
		{name: "trailing garbage", body: accepted + "private-response-body"},
		{name: "oversized", body: accepted + strings.Repeat(" ", domain.MaxConversationFindPreparationBytes+1-len(accepted))},
		{name: "oversized chunked", body: accepted + strings.Repeat(" ", domain.MaxConversationFindPreparationBytes+1-len(accepted)), chunked: true},
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
		{name: "wrong stale status", status: http.StatusGone, body: `{"error":{"code":"doc_visible_generation_stale"}}`},
		{name: "wrong overload status", status: http.StatusServiceUnavailable, body: `{"error":{"code":"resource_overloaded"}}`},
		{name: "wrong unavailable status", status: http.StatusForbidden, body: `{"error":{"code":"conversation_find_preparation_unavailable"}}`},
		{name: "known code on 200", status: http.StatusOK, body: `{"error":{"code":"forbidden"}}`},
		{name: "error on 202", body: `{"error":{"code":"forbidden"}}`},
		{name: "error malformed", status: http.StatusForbidden, body: `{"error":`},
		{name: "error missing", status: http.StatusForbidden, body: `{}`},
		{name: "error null", status: http.StatusForbidden, body: `{"error":null}`},
		{name: "error array", status: http.StatusForbidden, body: `{"error":[{"code":"forbidden"}]}`},
		{name: "error missing code", status: http.StatusForbidden, body: `{"error":{}}`},
		{name: "error non-string code", status: http.StatusForbidden, body: `{"error":{"code":42}}`},
		{name: "error null code", status: http.StatusForbidden, body: `{"error":{"code":null}}`},
		{name: "error empty code", status: http.StatusForbidden, body: `{"error":{"code":""}}`},
		{name: "error case mismatch", status: http.StatusForbidden, body: `{"Error":{"code":"forbidden"}}`},
		{name: "error code case mismatch", status: http.StatusForbidden, body: `{"error":{"Code":"forbidden"}}`},
		{name: "error duplicate", status: http.StatusForbidden, body: `{"error":{"code":"forbidden"},"error":{"code":"forbidden"}}`},
		{name: "error duplicate code", status: http.StatusForbidden, body: `{"error":{"code":"forbidden","code":"forbidden"}}`},
		{name: "error duplicate escaped code", status: http.StatusForbidden, body: `{"error":{"code":"forbidden","\u0063ode":"forbidden"}}`},
		{name: "error trailing", status: http.StatusForbidden, body: `{"error":{"code":"forbidden"}}{}`},
		{name: "error plain text", status: http.StatusForbidden, contentType: "text/plain", body: `{"error":{"code":"forbidden"}}`},
		{name: "error oversized", status: http.StatusForbidden, body: `{"error":{"code":"forbidden"}}` + strings.Repeat(" ", domain.MaxConversationFindPreparationBytes)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
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
			expected := domain.ErrConversationFindPreparationUnavailable
			if scenario.accepted {
				expected = nil
			}
			assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), "private-token", conversationPreparationInput()), expected, server.URL, "private", "response-body")
			if calls.Load() != 1 {
				test.Fatal("conversation response was retried")
			}
		})
	}
}

func TestRequestConversationFindPreparationRejections(test *testing.T) {
	for _, scenario := range []struct {
		status int
		code   string
		cause  error
	}{
		{401, "unauthenticated", domain.ErrUnauthorized},
		{403, "forbidden", domain.ErrForbidden},
		{404, "not_found", domain.ErrNotFound},
		{422, "validation", domain.ErrValidation},
		{409, "conflict", domain.ErrConflict},
		{409, "catalog_reset_required", domain.ErrCatalogResetRequired},
		{409, "unsupported_document_identity", domain.ErrUnsupportedDocumentIdentity},
		{409, "document_identity_upgrade_required", domain.ErrDocumentIdentityUpgradeRequired},
		{409, "doc_visible_generation_stale", domain.ErrDocVisibleGenerationStale},
		{503, "integrity_violation", domain.ErrIntegrity},
		{503, "audit_unavailable", domain.ErrBreakGlassAuditUnavailable},
		{503, "prepared_memory_unavailable", domain.ErrCatalogUnsupported},
		{503, "doc_find_fields_unavailable", domain.ErrDocFindFieldsUnavailable},
		{503, "doc_find_display_unavailable", domain.ErrDocFindDisplayUnavailable},
		{503, "doc_range_index_unavailable", domain.ErrDocRangeIndexUnavailable},
		{503, "doc_range_index_pending", domain.ErrDocRangeIndexPending},
		{503, "doc_find_fields_pending", domain.ErrDocFindFieldsPending},
		{503, "conversation_find_preparation_unavailable", domain.ErrConversationFindPreparationUnavailable},
		{429, "resource_overloaded", domain.ErrResourceOverloaded},
		{503, "deadline_exceeded", context.DeadlineExceeded},
		{503, "canceled", context.Canceled},
		{503, "unavailable", domain.ErrConversationFindPreparationUnavailable},
		{503, "memory_preparation_unavailable", domain.ErrConversationFindPreparationUnavailable},
		{503, "private-unknown-code", domain.ErrConversationFindPreparationUnavailable},
		{405, "method_not_allowed", domain.ErrConversationFindPreparationUnavailable},
	} {
		test.Run(scenario.code, func(test *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.Header().Set("Content-Type", "application/json; charset=utf-8")
				writer.Header().Set("Retry-After", "1")
				writer.WriteHeader(scenario.status)
				_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]any{
					"code": scenario.code, "message": "private-response-body", "details": map[string]any{"cursor": "private-cursor"},
				}})
			}))
			defer server.Close()
			client := preparationClient(test, server)
			assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), "private-token", conversationPreparationInput()), scenario.cause, server.URL, "private")
			if calls.Load() != 1 {
				test.Fatal("conversation rejection was retried")
			}
		})
	}
}

type conversationPreparationHandlerBackend struct {
	preparationHandlerBackend
	expected domain.ConversationFindPreparationRequest
}

func (backend *conversationPreparationHandlerBackend) RequestConversationFind(ctx context.Context, repo, hash domain.ContentHash, find domain.ConversationFindRequest) error {
	if repo != backend.expected.RepoID || hash != backend.expected.Hash || !reflect.DeepEqual(find, backend.expected.Find) {
		return domain.ErrValidation
	}
	return backend.RequestPreparedMemory(ctx, repo, domain.EffectiveMemoryRequest{})
}

func TestRequestConversationFindPreparationHandlerRoundTrip(test *testing.T) {
	for _, scenario := range []struct {
		code  string
		cause error
	}{
		{"accepted", nil},
		{"unauthenticated", domain.ErrUnauthorized},
		{"forbidden", domain.ErrForbidden},
		{"not_found", domain.ErrNotFound},
		{"validation", domain.ErrValidation},
		{"conflict", domain.ErrConflict},
		{"catalog_reset_required", domain.ErrCatalogResetRequired},
		{"document_identity_upgrade_required", domain.ErrDocumentIdentityUpgradeRequired},
		{"doc_visible_generation_stale", domain.ErrDocVisibleGenerationStale},
		{"integrity_violation", domain.ErrIntegrity},
		{"audit_unavailable", domain.ErrBreakGlassAuditUnavailable},
		{"prepared_memory_unavailable", domain.ErrCatalogUnsupported},
		{"doc_find_fields_unavailable", domain.ErrDocFindFieldsUnavailable},
		{"doc_find_display_unavailable", domain.ErrDocFindDisplayUnavailable},
		{"doc_range_index_unavailable", domain.ErrDocRangeIndexUnavailable},
		{"doc_range_index_pending", domain.ErrDocRangeIndexPending},
		{"doc_find_fields_pending", domain.ErrDocFindFieldsPending},
		{"conversation_find_preparation_unavailable", domain.ErrConversationFindPreparationUnavailable},
		{"resource_overloaded", domain.ErrResourceOverloaded},
		{"deadline_exceeded", context.DeadlineExceeded},
		{"canceled", context.Canceled},
		{"unavailable", errors.New("private-backend-failure")},
	} {
		test.Run(scenario.code, func(test *testing.T) {
			input := conversationPreparationInput()
			backend := &conversationPreparationHandlerBackend{
				preparationHandlerBackend: preparationHandlerBackend{cause: scenario.cause}, expected: input,
			}
			handler, err := preparation.NewConversationFindPreparationHandler(backend, backend, backend, backend, "https://preparation.example", "https://preparation.example")
			if err != nil {
				test.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			client := preparationClient(test, server)
			expected := scenario.cause
			if scenario.code == "unavailable" {
				expected = domain.ErrConversationFindPreparationUnavailable
			}
			assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), "private-token", input), expected, "private", server.URL)
			if backend.calls.Load() != 1 {
				test.Fatal("conversation preparation was skipped or retried")
			}
		})
	}
}

func TestRequestConversationFindPreparationNeverFollowsRedirects(test *testing.T) {
	var destinationCalls atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		destinationCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(writer, `{"accepted":true}`)
	}))
	defer destination.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		for _, location := range []string{destination.URL + "/private-redirect", "/private-redirect", strings.TrimPrefix(destination.URL, "http:") + "/private-redirect", domain.MemoryReadPreparationPath} {
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
				assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), "private-token", conversationPreparationInput()), domain.ErrConversationFindPreparationUnavailable, "private", server.URL, destination.URL)
				if sourceCalls.Load() != 1 || destinationCalls.Load() != 0 {
					test.Fatal("conversation redirect replayed credentials or request")
				}
			})
		}
	}
}

func TestRequestConversationFindPreparationContext(test *testing.T) {
	test.Run("already canceled", func(test *testing.T) {
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
		defer server.Close()
		client := preparationClient(test, server)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assertPreparationError(test, client.RequestConversationFindPreparation(ctx, "", domain.ConversationFindPreparationRequest{}), context.Canceled)
		ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		assertPreparationError(test, client.RequestConversationFindPreparation(ctx, "", domain.ConversationFindPreparationRequest{}), context.DeadlineExceeded)
		if calls.Load() != 0 {
			test.Fatal("canceled conversation request reached HTTP")
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
				go func() {
					result <- client.RequestConversationFindPreparation(ctx, "private-token", conversationPreparationInput())
				}()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					test.Fatal("conversation request did not reach loopback server")
				}
				if mode == "canceled" {
					cancel()
				}
				select {
				case err := <-result:
					assertPreparationError(test, err, expected, "private", server.URL)
				case <-time.After(2 * time.Second):
					test.Fatal("conversation client ignored cancellation or timeout")
				}
			})
		}
	}
}

func TestRequestConversationFindPreparationTransportFailures(test *testing.T) {
	for _, mode := range []string{"connection closed", "truncated body", "oversized content length", "oversized header"} {
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
					length = domain.MaxConversationFindPreparationBytes + 1
				}
				if mode == "oversized header" {
					length = len(body)
					writer.Header().Set("X-Private-Header", strings.Repeat("x", 2*domain.MaxConversationFindPreparationBytes))
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Content-Length", strconv.Itoa(length))
				writer.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(writer, body)
			}))
			defer server.Close()
			client := preparationClient(test, server)
			assertPreparationError(test, client.RequestConversationFindPreparation(context.Background(), "private-token", conversationPreparationInput()), domain.ErrConversationFindPreparationUnavailable, "private", server.URL)
			if calls.Load() != 1 {
				test.Fatal("conversation transport failure was retried")
			}
		})
	}
}
