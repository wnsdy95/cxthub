package preparation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestConversationFindPreparationBearerAndAudience(test *testing.T) {
	fixture := newConversationFindFixture(test)
	ctx := inbound.WithSystemActor(context.Background())
	wrongAudience, err := fixture.identity.IssueMCPTokenPair(ctx, fixture.reader.ID, "wrong-client", "https://app.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	cli, err := fixture.identity.CreateCLIToken(ctx, fixture.reader.ID, "CLI")
	if err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []struct {
		name   string
		bearer string
		cookie string
		extra  string
	}{
		{"missing", "", "", ""},
		{"wrong audience", "Bearer " + wrongAudience.AccessToken, "", ""},
		{"CLI token", "Bearer " + cli.Token, "", ""},
		{"refresh token", "Bearer " + wrongAudience.RefreshToken, "", ""},
		{"IDP token", "Bearer idp-token", "", ""},
		{"oversize", "Bearer " + strings.Repeat("x", 8193), "", ""},
		{"cookie only", "", fixture.token, ""},
		{"invalid bearer with valid cookie", "Bearer invalid", fixture.token, ""},
		{"wrong scheme", "Basic " + fixture.token, "", ""},
		{"extra field", "Bearer " + fixture.token + " extra", "", ""},
		{"duplicate bearer", "Bearer " + fixture.token, "", "Bearer " + fixture.token},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
				test.Fatal("non-MCP or wrong-audience bearer reached demand")
				return nil
			})
			request := fixture.request(test)
			request.Header.Del("Authorization")
			if candidate.bearer != "" {
				request.Header.Set("Authorization", candidate.bearer)
			}
			if candidate.cookie != "" {
				request.AddCookie(&http.Cookie{Name: "cxt_session", Value: candidate.cookie})
			}
			if candidate.extra != "" {
				request.Header.Add("Authorization", candidate.extra)
			}
			response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(response, request)
			assertConversationFindResponse(test, response.ResponseRecorder, http.StatusUnauthorized, "unauthenticated")
		})
	}
}

func TestConversationFindPreparationOriginAndContentType(test *testing.T) {
	fixture := newConversationFindFixture(test)
	for _, candidate := range []struct {
		name   string
		origin string
		typeOf string
		status int
		code   string
	}{
		{"foreign origin", "https://foreign.test", "application/json", 403, "forbidden"},
		{"origin path", "https://app.example.test/path", "application/json", 403, "forbidden"},
		{"null origin", "null", "application/json", 403, "forbidden"},
		{"missing type", "", "", 422, "validation"},
		{"text type", "", "text/plain", 422, "validation"},
		{"malformed type", "", "application/json; broken", 422, "validation"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
				test.Fatal("unsafe origin or body type reached demand")
				return nil
			})
			request := fixture.request(test)
			request.Header.Set("Origin", candidate.origin)
			request.Header.Set("Content-Type", candidate.typeOf)
			response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(response, request)
			assertConversationFindResponse(test, response.ResponseRecorder, candidate.status, candidate.code)
		})
	}
}

type conversationFindReadHook struct {
	io.ReadCloser
	after func()
}

func (body *conversationFindReadHook) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if body.after != nil {
		after := body.after
		body.after = nil
		after()
	}
	return count, err
}

func TestConversationFindPreparationFreshRevocation(test *testing.T) {
	for _, phase := range []string{"body read", "admission", "snapshot", "job demand"} {
		for _, mutation := range []string{"token", "permission", "identity"} {
			test.Run(phase+"/"+mutation, func(test *testing.T) {
				fixture := newConversationFindFixture(test)
				mutate := func() {
					ctx := inbound.WithSystemActor(context.Background())
					switch mutation {
					case "token":
						if err := fixture.identity.RevokeMCPToken(ctx, fixture.token, "find-client"); err != nil {
							test.Fatal(err)
						}
					case "permission":
						if err := fixture.identity.RemoveMember(ctx, fixture.owner.ID, fixture.repository.ID, fixture.reader.ID); err != nil {
							test.Fatal(err)
						}
					case "identity":
						fixture.identity.swapUser = true
					}
				}
				completed := false
				handler := fixture.handler(test, func(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
					if len(fixture.identity.tokens) != 1 || fixture.identity.admissions != 0 {
						test.Fatal("transport performed application admission")
					}
					if phase == "admission" {
						mutate()
					}
					return fixture.backend.Service.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
						snapshot := inbound.DocumentSnapshotReadAuthorizer(bound)
						if snapshot == nil {
							test.Fatal("application admission did not receive a fresh snapshot callback")
						}
						if phase == "snapshot" {
							mutate()
						}
						bound = context.WithValue(bound, conversationFindSnapshotKey{}, true)
						if _, err := snapshot(bound, repo); err != nil {
							return err
						}
						if phase == "job demand" {
							mutate()
							if _, err := snapshot(bound, repo); err != nil {
								return err
							}
						}
						completed = true
						return nil
					})
				})
				request := fixture.request(test)
				if phase == "body read" {
					request.Body = &conversationFindReadHook{ReadCloser: request.Body, after: mutate}
				}
				response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
				handler.ServeHTTP(response, request)
				status, code := http.StatusUnauthorized, "unauthenticated"
				if mutation == "permission" {
					status, code = http.StatusForbidden, "forbidden"
				}
				assertConversationFindResponse(test, response.ResponseRecorder, status, code)
				if completed || len(fixture.identity.tokens) != 2 {
					test.Fatal("revoked authority reached document job demand")
				}
				if phase == "snapshot" && len(fixture.identity.snapshotTokens) != 1 || phase == "job demand" && len(fixture.identity.snapshotTokens) != 2 {
					test.Fatal("revocation was not checked in the application's fresh snapshot")
				}
			})
		}
	}
}

func TestConversationFindPreparationRepositoryBinding(test *testing.T) {
	for _, phase := range []string{"admission", "snapshot"} {
		for _, mutation := range []string{"wrong repo", "unowned repo", "changed owner", "nil permit", "wrong snapshot repo"} {
			test.Run(phase+"/"+mutation, func(test *testing.T) {
				fixture := newConversationFindFixture(test)
				fixture.identity.nilPermit = mutation == "nil permit"
				fixture.backend.get = func(ctx context.Context, requested domain.ContentHash) (domain.Repo, error) {
					if inbound.DocumentReadAuthorizer(ctx) != nil {
						test.Fatal("repository lookup recursively retained the admission callback")
					}
					repo, err := fixture.backend.Service.GetRepo(ctx, requested)
					if phase == "admission" || fixture.backend.lookups == 2 {
						switch mutation {
						case "wrong repo":
							repo.ID = domain.HashContent([]byte("another repo"))
						case "unowned repo":
							repo.RepositoryID = ""
						case "changed owner":
							repo.RepositoryID = "ws_" + strings.Repeat("0", 32)
						}
					}
					return repo, err
				}
				handler := fixture.handler(test, func(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
					bound, err := inbound.DocumentReadAuthorizer(ctx)(ctx, repo)
					if err != nil {
						return err
					}
					if mutation == "wrong snapshot repo" {
						repo = domain.HashContent([]byte("unrequested repo"))
					}
					_, err = inbound.DocumentSnapshotReadAuthorizer(bound)(context.WithValue(bound, conversationFindSnapshotKey{}, true), repo)
					return err
				})
				response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
				handler.ServeHTTP(response, fixture.request(test))
				status, code := http.StatusForbidden, "forbidden"
				if phase == "admission" && mutation == "changed owner" {
					status, code = http.StatusNotFound, "not_found"
				}
				assertConversationFindResponse(test, response.ResponseRecorder, status, code)
			})
		}
	}
}

func TestConversationFindPreparationRateLimitsFailClosed(test *testing.T) {
	for _, phase := range []int{1, 2} {
		for _, storageFailure := range []bool{false, true} {
			test.Run(fmt.Sprintf("phase=%d/storage_failure=%t", phase, storageFailure), func(test *testing.T) {
				fixture := newConversationFindFixture(test)
				fixture.runtime.allow = func(context.Context, string, int, time.Duration, time.Time) (bool, error) {
					if len(fixture.runtime.calls) != phase {
						return true, nil
					}
					if storageFailure {
						return false, errors.New("private storage details " + fixture.token)
					}
					return false, nil
				}
				handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
					test.Fatal("rate-limited request reached demand")
					return nil
				})
				response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
				handler.ServeHTTP(response, fixture.request(test))
				status, code := http.StatusTooManyRequests, "resource_overloaded"
				if storageFailure {
					status, code = http.StatusServiceUnavailable, "conversation_find_preparation_unavailable"
				} else if response.Header().Get("Retry-After") != "60" {
					test.Fatal("rate limit missing Retry-After")
				}
				assertConversationFindResponse(test, response.ResponseRecorder, status, code)
				if len(fixture.runtime.calls) != phase || len(fixture.identity.tokens) != phase-1 || strings.Contains(response.Body.String(), fixture.token) {
					test.Fatal("rate limiting did not fail closed before demand")
				}
			})
		}
	}
}

func TestConversationFindPreparationSharedBoundedRateKeys(test *testing.T) {
	fixture := newConversationFindFixture(test)
	ctx := inbound.WithSystemActor(context.Background())
	rotated, err := fixture.identity.IssueMCPTokenPair(ctx, fixture.reader.ID, "find-client", "https://mcp.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	owner, err := fixture.identity.IssueMCPTokenPair(ctx, fixture.owner.ID, "owner-client", "https://mcp.example.test/mcp")
	if err != nil {
		test.Fatal(err)
	}
	var keys []string
	for index, token := range []string{fixture.token, rotated.AccessToken, fixture.token, owner.AccessToken} {
		if index == 1 {
			fixture.body.Hash = domain.HashContent([]byte("different document"))
			fixture.body.Find = domain.ConversationFindRequest{Query: "different query", Cursor: "different-cursor"}
		}
		if index == 2 {
			fixture.body.RepoID = domain.HashContent([]byte("different repository"))
		}
		handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
			return nil
		})
		request := fixture.request(test)
		request.Header.Set("Authorization", "Bearer "+token)
		response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		handler.ServeHTTP(response, request)
		assertConversationFindResponse(test, response.ResponseRecorder, http.StatusAccepted, "")
		keys = append(keys, fixture.runtime.calls[len(fixture.runtime.calls)-1].key)
	}
	if keys[0] != keys[1] || keys[0] == keys[2] || keys[2] == keys[3] {
		test.Fatal("allowance is not shared across tokens and isolated by user/repository", keys)
	}
	for index, allowance := range fixture.runtime.calls {
		if allowance.deadline.IsZero() || allowance.deadline.After(time.Now().Add(2*time.Second)) || !allowance.now.IsZero() || allowance.window != time.Minute ||
			strings.Contains(allowance.key, fixture.token) || strings.Contains(allowance.key, rotated.AccessToken) || strings.Contains(allowance.key, fixture.reader.ID) || len(allowance.key) > 128 {
			test.Fatal("unbounded or secret-bearing allowance", allowance)
		}
		if index%2 == 0 {
			if allowance.key != "mcp:conversation-find-preparation:global" || allowance.limit != 600 {
				test.Fatal("global allowance changed", allowance)
			}
		} else if allowance.limit != 30 || domain.ValidateContentHash(domain.ContentHash(strings.TrimPrefix(allowance.key, "mcp:conversation-find-preparation:scope:"))) != nil {
			test.Fatal("per-user/repository allowance is not bounded", allowance)
		}
	}
}

func TestConversationFindPreparationSanitizedDemandFailures(test *testing.T) {
	fixture := newConversationFindFixture(test)
	for _, candidate := range []struct {
		cause     error
		status    int
		code      string
		retryable bool
	}{
		{domain.ErrUnauthorized, 401, "unauthenticated", false},
		{domain.ErrForbidden, 403, "forbidden", false},
		{domain.ErrValidation, 422, "validation", false},
		{domain.ErrNotFound, 404, "not_found", false},
		{domain.ErrConflict, 409, "conflict", false},
		{domain.ErrUnsupportedDocumentIdentity, 409, "unsupported_document_identity", false},
		{domain.ErrDocumentIdentityUpgradeRequired, 409, "document_identity_upgrade_required", false},
		{domain.ErrDocVisibleGenerationStale, 409, "doc_visible_generation_stale", false},
		{domain.ErrDocRangeIndexUnavailable, 503, "doc_range_index_unavailable", true},
		{domain.ErrDocRangeIndexPending, 503, "doc_range_index_pending", true},
		{domain.ErrDocFindFieldsUnavailable, 503, "doc_find_fields_unavailable", false},
		{domain.ErrDocFindFieldsPending, 503, "doc_find_fields_pending", true},
		{domain.ErrDocFindDisplayUnavailable, 503, "doc_find_display_unavailable", false},
		{domain.ErrConversationFindPreparationUnavailable, 503, "conversation_find_preparation_unavailable", true},
		{domain.ErrResourceOverloaded, 429, "resource_overloaded", true},
		{domain.ErrIntegrity, 503, "integrity_violation", false},
		{domain.ErrBreakGlassAuditUnavailable, 503, "audit_unavailable", false},
		{context.Canceled, 503, "canceled", false},
		{context.DeadlineExceeded, 503, "deadline_exceeded", true},
		{errors.New("private unknown failure"), 503, "unavailable", false},
		{errors.Join(domain.ErrDocRangeIndexPending, domain.ErrIntegrity), 503, "integrity_violation", false},
		{errors.Join(domain.ErrDocFindFieldsPending, domain.ErrUnauthorized), 401, "unauthenticated", false},
		{errors.Join(domain.ErrDocRangeIndexPending, domain.ErrDocFindDisplayUnavailable), 503, "doc_find_display_unavailable", false},
		{&domain.ConversationFindNotReady{Cursor: "private-cursor", Cause: domain.ErrDocRangeIndexUnavailable}, 503, "doc_range_index_unavailable", true},
	} {
		test.Run(candidate.code, func(test *testing.T) {
			handler := fixture.handler(test, func(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
				if fixture.backend.lookups != 0 || inbound.DocumentReadAuthorizer(ctx) == nil || repo != fixture.body.RepoID || hash != fixture.body.Hash {
					test.Fatal("transport did not delegate permission and current-source decisions")
				}
				return fmt.Errorf("private source %s %s %s: %w", fixture.token, hash, request.Query, candidate.cause)
			})
			response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(response, fixture.request(test))
			assertConversationFindResponse(test, response.ResponseRecorder, candidate.status, candidate.code)
			if !strings.Contains(response.Body.String(), fmt.Sprintf(`"retryable":%t`, candidate.retryable)) {
				test.Fatal("failure changed retryability", response.Body)
			}
			for _, secret := range []string{fixture.token, string(fixture.body.Hash), fixture.body.Find.Query, fixture.body.Find.Cursor, "private"} {
				if strings.Contains(response.Body.String(), secret) {
					test.Fatalf("failure leaked %q: %s", secret, response.Body)
				}
			}
		})
	}
}

func TestConversationFindPreparationBlockedBodyDeadline(test *testing.T) {
	fixture := newConversationFindFixture(test)
	raw, err := json.Marshal(fixture.body)
	if err != nil {
		test.Fatal(err)
	}
	for _, candidate := range []struct {
		name        string
		prefix      string
		readTimeout time.Duration
	}{
		{"partial body", `{"version":1`, 0},
		{"awaiting EOF", string(raw), 0},
		{"server read deadline", `{"version":1`, 25 * time.Millisecond},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			reader, writer := net.Pipe()
			defer reader.Close()
			defer writer.Close()
			written := make(chan error, 1)
			go func() {
				_, err := io.WriteString(writer, candidate.prefix)
				written <- err
			}()
			handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
				test.Fatal("stalled body reached demand")
				return nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			ctx = context.WithValue(ctx, http.ServerContextKey, &http.Server{ReadTimeout: candidate.readTimeout})
			response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), set: reader.SetReadDeadline}
			started := time.Now()
			handler.ServeHTTP(response, fixture.rawRequest(reader).WithContext(ctx))
			assertConversationFindResponse(test, response.ResponseRecorder, http.StatusServiceUnavailable, "deadline_exceeded")
			if time.Since(started) > time.Second {
				test.Fatal("blocked body exceeded its bounded deadline")
			}
			if err := <-written; err != nil {
				test.Fatal("could not deliver partial body", err)
			}
		})
	}
}

func TestConversationFindPreparationBodyDeadlineFailures(test *testing.T) {
	fixture := newConversationFindFixture(test)
	handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
		test.Fatal("unbounded body reached demand")
		return nil
	})
	for _, failAt := range []int{1, 2} {
		for _, failure := range []error{http.ErrNotSupported, errors.New("private connection error")} {
			response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			response.set = func(time.Time) error {
				if len(response.deadlines) == failAt {
					return failure
				}
				return nil
			}
			handler.ServeHTTP(response, fixture.request(test))
			assertConversationFindResponse(test, response.ResponseRecorder, http.StatusServiceUnavailable, "conversation_find_preparation_unavailable")
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, fixture.request(test))
	assertConversationFindResponse(test, response, http.StatusServiceUnavailable, "conversation_find_preparation_unavailable")
}

func TestConversationFindPreparationRestoresDeadlineAfterEOF(test *testing.T) {
	fixture := newConversationFindFixture(test)
	for _, readTimeout := range []time.Duration{0, 50 * time.Millisecond} {
		response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		ctx = context.WithValue(ctx, http.ServerContextKey, &http.Server{ReadTimeout: readTimeout})
		deadline, _ := ctx.Deadline()
		handler := fixture.handler(test, func(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error {
			if readTimeout == 0 {
				if len(response.deadlines) != 2 || !response.deadlines[0].Equal(deadline) || !response.deadlines[1].IsZero() {
					test.Fatal("body deadline not restored after EOF", response.deadlines)
				}
			} else if len(response.deadlines) != 1 || response.deadlines[0].IsZero() || response.deadlines[0].After(time.Now().Add(readTimeout)) {
				test.Fatal("server read timeout was disabled or extended", response.deadlines)
			}
			return nil
		})
		handler.ServeHTTP(response, fixture.request(test).WithContext(ctx))
		cancel()
		assertConversationFindResponse(test, response.ResponseRecorder, http.StatusAccepted, "")
	}
}

func TestConversationFindPreparationCancellation(test *testing.T) {
	for _, phase := range []string{"before request", "body read", "demand"} {
		test.Run(phase, func(test *testing.T) {
			fixture := newConversationFindFixture(test)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			if phase == "before request" {
				cancel()
			}
			calls := 0
			handler := fixture.handler(test, func(bound context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
				calls++
				if actual, _ := bound.Deadline(); !actual.Equal(deadline) {
					test.Fatal("handler extended caller's deadline")
				}
				cancel()
				return nil
			})
			request := fixture.request(test).WithContext(ctx)
			if phase == "body read" {
				request.Body = &conversationFindReadHook{ReadCloser: request.Body, after: cancel}
			}
			response := &conversationFindDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			handler.ServeHTTP(response, request)
			assertConversationFindResponse(test, response.ResponseRecorder, http.StatusServiceUnavailable, "canceled")
			if phase == "demand" && calls != 1 || phase != "demand" && calls != 0 {
				test.Fatal("canceled request reached demand or returned an accepted result", calls)
			}
		})
	}
}
