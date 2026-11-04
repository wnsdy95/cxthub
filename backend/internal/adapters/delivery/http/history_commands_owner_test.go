package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

var historyCommandsHTTPRoutes = []string{"/history", "/history/memory-selection"}

type historyCommandsHTTPProbe struct {
	calls   []domain.HistoryEvent
	err     error
	observe func(context.Context)
}

func (commands *historyCommandsHTTPProbe) RecordHistory(ctx context.Context, event domain.HistoryEvent) error {
	commands.calls = append(commands.calls, event)
	if commands.observe != nil {
		commands.observe(ctx)
	}
	return commands.err
}

type historyCommandsHTTPBackend struct{ *repositoryRefHTTPBackend }

func (*historyCommandsHTTPBackend) RecordHistory(context.Context, domain.HistoryEvent) error {
	panic("history command reached the broad HTTP backend")
}

type historyCommandsHTTPFixture struct {
	server   *Server
	commands *historyCommandsHTTPProbe
	identity *branchPullHTTPIdentity
	event    domain.HistoryEvent
	request  *http.Request
}

func newHistoryCommandsHTTPFixture(test *testing.T, route string) historyCommandsHTTPFixture {
	test.Helper()
	backend := &historyCommandsHTTPBackend{&repositoryRefHTTPBackend{repo: domain.Repo{
		ID: domain.HashContent([]byte(test.Name())), RepositoryID: "ws_" + strings.Repeat("a", 32),
	}}}
	identity := &branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: domain.RoleMember}
	target := domain.HashContent([]byte("history target"))
	event := domain.HistoryEvent{ID: strings.Repeat("b", 32), RepoID: string(backend.repo.ID),
		BranchID: "context-identity", Branch: "feature/nested", Kind: "position", LocalBranch: "local/alias",
		Source: target, Target: target, SharedTarget: target, GitAfter: strings.Repeat("c", 40),
		WorktreeID: strings.Repeat("d", 32), CreatedAt: time.Unix(1720000000, 0).UTC()}
	if route == "/history/memory-selection" {
		event.MemorySelectionParent, event.MemoryPinned = strings.Repeat("e", 32), true
		event.MemorySource, event.MemoryHash = target, domain.HashContent([]byte("selected memory"))
	}
	payload, err := json.Marshal(event)
	if err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+event.RepoID+route, strings.NewReader(string(payload)))
	request.Header.Set("Authorization", "Bearer history-session")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
	commands := &historyCommandsHTTPProbe{}
	server := NewServer(backend, identity)
	if err := server.SetHistoryCommands(commands); err != nil {
		test.Fatal(err)
	}
	return historyCommandsHTTPFixture{server: server, commands: commands, identity: identity, event: event, request: request}
}

func TestHistoryCommandsHTTPExactDispatchAndRejectedSetters(test *testing.T) {
	for _, route := range historyCommandsHTTPRoutes {
		test.Run(route, func(test *testing.T) {
			fixture := newHistoryCommandsHTTPFixture(test, route)
			for _, rejected := range []inbound.HistoryCommands{nil, (*historyCommandsHTTPProbe)(nil)} {
				for _, server := range []*Server{NewServer(nil, nil), fixture.server} {
					if err := server.SetHistoryCommands(rejected); !errors.Is(err, domain.ErrValidation) {
						test.Fatal("nil history owner was not rejected", err)
					}
				}
			}
			key := struct{ name string }{"history request"}
			parent, cancel := context.WithTimeout(context.WithValue(test.Context(), key, "retained"), 30*time.Second)
			defer cancel()
			fixture.commands.observe = func(ctx context.Context) {
				deadline, present := ctx.Deadline()
				expected, _ := parent.Deadline()
				actor, system := inbound.RepositoryActor(ctx)
				if ctx.Value(key) != "retained" || ctx.Err() != nil || !present || !deadline.Equal(expected) || actor != "actor" || system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityRootV1}) {
					test.Fatal("history owner lost request context, actor or identities")
				}
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request.WithContext(parent))
			var output map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &output); err != nil {
				test.Fatal(err)
			}
			if response.Code != http.StatusOK || !reflect.DeepEqual(output, map[string]string{"id": fixture.event.ID}) || !reflect.DeepEqual(fixture.commands.calls, []domain.HistoryEvent{fixture.event}) || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				test.Fatal("history wire input, response or previous owner changed", response.Code, response.Body, fixture.commands.calls)
			}
		})
	}
}

func TestHistoryCommandsHTTPGatesPrecedeOwner(test *testing.T) {
	for _, route := range historyCommandsHTTPRoutes {
		for _, gate := range []struct {
			name   string
			status int
		}{
			{"anonymous", 401}, {"viewer", 403}, {"puller", 403}, {"archived", 403}, {"break glass", 403},
			{"csrf", 403}, {"content type", 415}, {"malformed JSON", 400}, {"oversized JSON", 413},
			{"foreign repository", 422}, {"missing repository", 422}, {"missing selection parent", 422},
		} {
			if gate.name == "missing selection parent" && route == "/history" {
				continue
			}
			test.Run(route+"/"+gate.name, func(test *testing.T) {
				fixture := newHistoryCommandsHTTPFixture(test, route)
				payload := ""
				switch gate.name {
				case "anonymous":
					fixture.request.Header.Del("Authorization")
				case "viewer":
					fixture.identity.role = domain.RoleViewer
				case "puller":
					fixture.identity.role = domain.RolePuller
				case "archived":
					fixture.identity.repo.Archived = true
				case "break glass":
					fixture.identity.role, fixture.identity.breakGlass = "", true
				case "csrf":
					fixture.request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "history-session"})
				case "content type":
					fixture.request.Header.Set("Content-Type", "text/plain")
				case "malformed JSON":
					payload = `{"`
				case "oversized JSON":
					payload = `{"repo_id":"` + strings.Repeat("x", 32<<10) + `"}`
				default:
					if gate.name == "foreign repository" {
						fixture.event.RepoID = string(domain.HashContent([]byte("foreign")))
					} else if gate.name == "missing repository" {
						fixture.event.RepoID = ""
					} else {
						fixture.event.MemorySelectionParent = ""
					}
					raw, err := json.Marshal(fixture.event)
					if err != nil {
						test.Fatal(err)
					}
					payload = string(raw)
				}
				if payload != "" {
					fixture.request.Body = io.NopCloser(strings.NewReader(payload))
				}
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request)
				if response.Code != gate.status || len(fixture.commands.calls) != 0 {
					test.Fatal("history transport gate reached owner", response.Code, response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestHistoryCommandsHTTPOwnerErrors(test *testing.T) {
	for _, route := range historyCommandsHTTPRoutes {
		for _, failure := range []struct {
			err    error
			status int
			code   string
		}{
			{domain.ErrRefConflict, 409, "ref_conflict"}, {domain.ErrConflict, 409, "conflict"},
			{domain.ErrForbidden, 403, "forbidden"}, {domain.ErrValidation, 422, "validation"},
			{domain.ErrIntegrity, 422, "integrity_violation"},
			{domain.ErrResourceOverloaded, 503, "resource_overloaded"},
			{errors.New("private history failure"), 500, "internal"},
		} {
			test.Run(route+"/"+failure.code, func(test *testing.T) {
				fixture := newHistoryCommandsHTTPFixture(test, route)
				fixture.commands.err = failure.err
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request)
				if response.Code != failure.status || !strings.Contains(response.Body.String(), `"code":"`+failure.code+`"`) || strings.Contains(response.Body.String(), "private history failure") || strings.Contains(response.Body.String(), `"id":`) || !reflect.DeepEqual(fixture.commands.calls, []domain.HistoryEvent{fixture.event}) || failure.code == "resource_overloaded" && response.Header().Get("Retry-After") != "1" {
					test.Fatal("history owner error lost public mapping or exact dispatch", response.Code, response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestHistoryCommandsHTTPAdmissionBeforeDecode(test *testing.T) {
	for _, route := range historyCommandsHTTPRoutes {
		test.Run(route, func(test *testing.T) {
			fixture := newHistoryCommandsHTTPFixture(test, route)
			_, release, err := app.AcquireResources(test.Context(), app.ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			body := &admissionUnreadBody{}
			fixture.request.Body = body
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request)
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || !strings.Contains(response.Body.String(), `"code":"resource_overloaded"`) || body.reads != 0 || len(fixture.commands.calls) != 0 {
				test.Fatal("history decoded or dispatched before bulk admission", response.Code, response.Body, body.reads)
			}
		})
	}
}

func TestHistoryCommandsHTTPResourcesThroughResponseAndCancellation(test *testing.T) {
	for _, route := range historyCommandsHTTPRoutes {
		for _, outcome := range []string{"success", "owner failure", "decode failure", "owner cancellation", "response cancellation", "write failure"} {
			test.Run(route+"/"+outcome, func(test *testing.T) {
				fixture := newHistoryCommandsHTTPFixture(test, route)
				parent, cancel := context.WithCancel(test.Context())
				defer cancel()
				status, calls, reads := http.StatusOK, 1, 0
				reader := fixture.request.Body
				if outcome == "decode failure" {
					reader, status, calls = io.NopCloser(strings.NewReader(`{"`)), http.StatusBadRequest, 0
				}
				if outcome == "owner failure" {
					fixture.commands.err, status = domain.ErrIntegrity, http.StatusUnprocessableEntity
				}
				if outcome == "owner cancellation" {
					fixture.commands.err, status = context.Canceled, http.StatusInternalServerError
				}
				fixture.request.Body = &wave58DocumentPublicationHTTPBody{Reader: reader, observe: func() {
					reads++
					wave58DocumentPublicationHTTPAssertBulkHeld(test)
				}}
				fixture.commands.observe = func(ctx context.Context) {
					wave58DocumentPublicationHTTPAssertBulkHeld(test)
					_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
					if err != nil {
						test.Fatal("history could not reuse transport admission", err)
					}
					release()
					if outcome == "owner cancellation" {
						cancel()
						if !errors.Is(ctx.Err(), context.Canceled) {
							test.Fatal("history lost live request cancellation", ctx.Err())
						}
					}
				}
				writer := &wave58DocumentPublicationHTTPWriter{ResponseRecorder: httptest.NewRecorder(), observe: func() {
					wave58DocumentPublicationHTTPAssertBulkHeld(test)
					if outcome == "response cancellation" {
						cancel()
					}
				}}
				if outcome == "write failure" {
					writer.err = io.ErrClosedPipe
				}
				fixture.server.Handler().ServeHTTP(writer, fixture.request.WithContext(parent))
				if writer.Code != status || writer.writes != 1 || reads == 0 || len(fixture.commands.calls) != calls {
					test.Fatal("history lifetime changed decode, dispatch or response", writer.Code, writer.Body, reads, writer.writes, fixture.commands.calls)
				}
				probe, stop := context.WithTimeout(test.Context(), time.Second)
				defer stop()
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if err != nil {
					test.Fatal("history completion leaked bulk admission", err)
				}
				release()
			})
		}
	}
}
