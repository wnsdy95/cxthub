package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type contextBranchHTTPProbe struct {
	calls      []string
	calledRepo domain.ContentHash
	input      any
	err        error
	observe    func(context.Context)
	history    []domain.HistoryEvent
}

func (owner *contextBranchHTTPProbe) record(ctx context.Context, operation string, repo domain.ContentHash, input any) {
	owner.calls, owner.calledRepo, owner.input = append(owner.calls, operation), repo, input
	if owner.observe != nil {
		owner.observe(ctx)
	}
}
func (owner *contextBranchHTTPProbe) Fork(ctx context.Context, input inbound.ForkInput) (inbound.ForkOutput, error) {
	owner.record(ctx, "fork", input.RepoID, input)
	return inbound.ForkOutput{Branch: "owner/fork", Head: input.FromSnapshot}, owner.err
}
func (owner *contextBranchHTTPProbe) PreviewJoin(ctx context.Context, input inbound.JoinPreviewInput) (inbound.JoinPreviewOutput, error) {
	owner.record(ctx, "preview", input.RepoID, input)
	return inbound.JoinPreviewOutput{Snapshot: input.Snapshot, Branch: input.Branch, BranchID: "owner-identity", Branches: []inbound.JoinBranchOption{}, DropTargets: []domain.ContentHash{}}, owner.err
}
func (owner *contextBranchHTTPProbe) ConfirmJoin(ctx context.Context, input inbound.ConfirmJoinInput) (inbound.JoinOutput, error) {
	owner.record(ctx, "join", input.RepoID, input)
	return inbound.JoinOutput{Branch: input.TargetBranch, Head: input.Snapshot, ForkBranch: "session/remaining"}, owner.err
}
func (owner *contextBranchHTTPProbe) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	owner.record(ctx, "protocol", repo, nil)
	return owner.err
}
func (owner *contextBranchHTTPProbe) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	owner.record(ctx, "history", repo, nil)
	return owner.history, owner.err
}

type contextBranchHTTPBackend struct {
	*repositoryRefHTTPBackend
	*contextBranchHTTPProbe
}
type contextBranchHTTPRoute struct {
	operation, method, path, body string
	input                         any
	wire                          any
}

func contextBranchHTTPRoutes(repo domain.ContentHash) []contextBranchHTTPRoute {
	source, head, revision := domain.HashContent([]byte("branch source")), domain.HashContent([]byte("branch head")), domain.HashContent([]byte("branch revision"))
	author := domain.TeamIdentity{Name: "Attribution", Email: "author@example.test", Team: "team"}
	return []contextBranchHTTPRoute{
		{"fork", "POST", "/fork", fmt.Sprintf(`{"from":%q,"new_branch":"feature/nested","author":{"name":"Attribution","email":"author@example.test","team":"team"},"actor_id":"forged","repo_id":"foreign"}`, source), inbound.ForkInput{RepoID: repo, FromSnapshot: source, NewBranch: "feature/nested", Author: author}, inbound.ForkOutput{Branch: "owner/fork", Head: source}},
		{"preview", "GET", "/join/preview?snapshot=" + string(source) + "&branch=feature%2Fnested", "", inbound.JoinPreviewInput{ActorID: "actor", RepoID: repo, Snapshot: source, Branch: "feature/nested"}, inbound.JoinPreviewOutput{Snapshot: source, Branch: "feature/nested", BranchID: "owner-identity", Branches: []inbound.JoinBranchOption{}, DropTargets: []domain.ContentHash{}}},
		{"join", "POST", "/join", fmt.Sprintf(`{"snapshot":%q,"branch":"feature/nested","branch_id":"approved-identity","expected_head":%q,"plan_revision":%q,"include_descendants":true,"actor_id":"forged","repo_id":"foreign"}`, source, head, revision), inbound.ConfirmJoinInput{ActorID: "actor", JoinInput: inbound.JoinInput{RepoID: repo, Snapshot: source, TargetBranch: "feature/nested", BranchID: "approved-identity", ExpectedHead: head, PlanRevision: revision, IncludeDescendants: true}}, inbound.JoinOutput{Branch: "feature/nested", Head: source, ForkBranch: "session/remaining"}},
		{"protocol", "POST", "/context-protocol", `{"`, nil, map[string]int{"context_protocol": 1}},
		{"history", "GET", "/history", "", nil, []domain.HistoryEvent{}},
	}
}

func newContextBranchHTTPFixture(test *testing.T, route contextBranchHTTPRoute) (*Server, *contextBranchHTTPProbe, *http.Request) {
	test.Helper()
	backend := &contextBranchHTTPBackend{&repositoryRefHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("branch route")), RepositoryID: "ws_" + strings.Repeat("a", 32)}}, &contextBranchHTTPProbe{observe: func(context.Context) { panic("context branch operation reached broad Backend") }}}
	role := domain.RoleMember
	if route.operation == "protocol" {
		role = domain.RoleMaintainer
	}
	if route.operation == "history" {
		role = domain.RoleViewer
	}
	identity := &branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: role}
	server, owner := NewServer(backend, identity), &contextBranchHTTPProbe{}
	requirePRCoordinationHTTP(test, server.SetContextBranchOperations(owner) == nil, "inject typed context branch owner")
	request := httptest.NewRequest(route.method, "/api/v1/repos/"+string(backend.repo.ID)+route.path, strings.NewReader(route.body))
	request.Header.Set("Authorization", "Bearer branch-session")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
	return server, owner, request
}

func TestContextBranchHTTPExactDispatchSettersAndLegacyCapture(test *testing.T) {
	for _, route := range contextBranchHTTPRoutes(domain.HashContent([]byte("branch route"))) {
		test.Run(route.operation, func(test *testing.T) {
			server, owner, request := newContextBranchHTTPFixture(test, route)
			for _, rejected := range []inbound.ContextBranchOperations{nil, (*contextBranchHTTPProbe)(nil), (*app.ContextBranchOperations)(nil)} {
				for _, target := range []*Server{NewServer(nil, nil), server} {
					requirePRCoordinationHTTP(test, errors.Is(target.SetContextBranchOperations(rejected), domain.ErrValidation), "nil setter changed owner")
				}
			}
			key := struct{ name string }{"branch request"}
			parent, cancel := context.WithTimeout(context.WithValue(test.Context(), key, "retained"), 30*time.Second)
			defer cancel()
			owner.observe = func(ctx context.Context) {
				deadline, present := ctx.Deadline()
				expected, _ := parent.Deadline()
				actor, system := inbound.RepositoryActor(ctx)
				requirePRCoordinationHTTP(test, ctx.Value(key) == "retained" && ctx.Err() == nil && present && deadline.Equal(expected) && actor == "actor" && !system && reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityRootV1}), "branch request context lost")
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request.WithContext(parent))
			raw, err := json.Marshal(route.wire)
			requirePRCoordinationHTTP(test, err == nil, err)
			requirePRCoordinationHTTP(test, response.Code == 200 && strings.TrimSpace(response.Body.String()) == string(raw) && response.Header().Get("Content-Type") == "application/json; charset=utf-8" && response.Header().Get("Cache-Control") == "no-store" && reflect.DeepEqual(owner.calls, []string{route.operation}) && reflect.DeepEqual(owner.input, route.input) && owner.calledRepo == domain.HashContent([]byte("branch route")), "typed dispatch or wire changed", response.Code, response.Body, owner.input)
			if route.operation == "history" {
				owner.history = []domain.HistoryEvent{{ID: strings.Repeat("b", 32), RepoID: string(owner.calledRepo), BranchID: "history-identity", Branch: "feature/nested", Kind: "position", Target: domain.HashContent([]byte("history target")), CreatedAt: time.Unix(1720000000, 0).UTC()}}
				owner.calls = nil
				response = httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request.WithContext(parent))
				raw, err = json.Marshal(owner.history)
				requirePRCoordinationHTTP(test, err == nil && response.Code == 200 && strings.TrimSpace(response.Body.String()) == string(raw) && reflect.DeepEqual(owner.calls, []string{"history"}), "history wire fields were dropped", response.Code, response.Body, err)
			}
			legacy := &contextBranchHTTPBackend{server.b.(*contextBranchHTTPBackend).repositoryRefHTTPBackend, &contextBranchHTTPProbe{}}
			captured := NewServer(legacy, server.id)
			captured.b = server.b
			_, _, fresh := newContextBranchHTTPFixture(test, route)
			response = httptest.NewRecorder()
			captured.Handler().ServeHTTP(response, fresh)
			requirePRCoordinationHTTP(test, response.Code == 200 && reflect.DeepEqual(legacy.calls, []string{route.operation}) && reflect.DeepEqual(legacy.input, route.input), "legacy narrow view was not captured at construction", response.Code, response.Body)
		})
	}
}

func TestContextBranchHTTPGuardsJSONAndPublicErrors(test *testing.T) {
	for _, route := range contextBranchHTTPRoutes("") {
		for _, failure := range []struct {
			name, code string
			status     int
			err        error
		}{
			{"anonymous", "unauthenticated", 401, nil}, {"role", "forbidden", 403, nil}, {"archived", "forbidden", 403, nil},
			{"content type", "bad_request", 415, nil}, {"malformed", "bad_request", 400, nil},
			{"owner", "join_preview_changed", 409, domain.ErrJoinPreviewChanged}, {"owner", "ref_conflict", 409, domain.ErrRefConflict},
			{"owner", "forbidden", 403, domain.ErrForbidden}, {"owner", "validation", 422, domain.ErrValidation},
			{"owner", "resource_overloaded", 503, domain.ErrResourceOverloaded}, {"owner", "internal", 500, errors.New("private branch failure")},
		} {
			if failure.name == "archived" && route.operation == "history" || failure.name == "content type" && route.method == "GET" || failure.name == "malformed" && route.operation != "fork" && route.operation != "join" {
				continue
			}
			test.Run(route.operation+"/"+failure.name+"/"+failure.code, func(test *testing.T) {
				server, owner, request := newContextBranchHTTPFixture(test, route)
				owner.err = failure.err
				switch failure.name {
				case "anonymous":
					request.Header.Del("Authorization")
				case "role":
					server.id.(*branchPullHTTPIdentity).role = ""
					if route.operation != "history" {
						server.id.(*branchPullHTTPIdentity).role = domain.RolePuller
					}
					if route.operation == "protocol" {
						server.id.(*branchPullHTTPIdentity).role = domain.RoleMember
					}
				case "archived":
					server.id.(*branchPullHTTPIdentity).repo.Archived = true
				case "content type":
					request.Header.Set("Content-Type", "text/plain")
				case "malformed":
					request.Body = io.NopCloser(strings.NewReader(`{"`))
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				calls := 0
				if failure.err != nil {
					calls = 1
				}
				requirePRCoordinationHTTP(test, response.Code == failure.status && len(owner.calls) == calls && strings.Contains(response.Body.String(), `"code":"`+failure.code+`"`) && !strings.Contains(response.Body.String(), "private branch failure") && !strings.Contains(response.Body.String(), `"head":`) && !strings.Contains(response.Body.String(), `"context_protocol":`) && (failure.status != 503 || response.Header().Get("Retry-After") == "1"), "branch guard/error precedence", response.Code, response.Body, owner.calls)
			})
		}
	}
}

func TestContextBranchHTTPAdmissionPrecedesPayloadButProtocolJSONWins(test *testing.T) {
	for _, route := range contextBranchHTTPRoutes("") {
		test.Run(route.operation, func(test *testing.T) {
			server, owner, request := newContextBranchHTTPFixture(test, route)
			_, release, err := app.AcquireResources(test.Context(), app.ResourceBulk)
			requirePRCoordinationHTTP(test, err == nil, err)
			defer release()
			if route.operation == "protocol" {
				_, releaseInteractive, err := app.AcquireResources(test.Context(), app.ResourceInteractive)
				requirePRCoordinationHTTP(test, err == nil, err)
				defer releaseInteractive()
			}
			body := &admissionUnreadBody{}
			request.Body = body
			request.Header.Set("Content-Type", "text/plain")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if route.operation == "protocol" {
				requirePRCoordinationHTTP(test, response.Code == 415 && body.reads == 0 && len(owner.calls) == 0, "protocol admitted before JSON guard", response.Code, response.Body)
				request.Header.Set("Content-Type", "application/json")
				response = httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
			}
			requirePRCoordinationHTTP(test, response.Code == 503 && response.Header().Get("Retry-After") == "1" && strings.Contains(response.Body.String(), `"code":"resource_overloaded"`) && body.reads == 0 && len(owner.calls) == 0, "branch decoded or dispatched before shared admission", response.Code, response.Body)
		})
	}
}

func TestContextBranchHTTPAdmissionThroughResponseAndRelease(test *testing.T) {
	for _, route := range contextBranchHTTPRoutes("") {
		for _, outcome := range []string{"success", "owner failure", "decode failure", "owner cancellation", "response cancellation", "write failure"} {
			if outcome == "decode failure" && route.operation != "fork" && route.operation != "join" {
				continue
			}
			test.Run(route.operation+"/"+outcome, func(test *testing.T) {
				server, owner, request := newContextBranchHTTPFixture(test, route)
				class := app.ResourceBulk
				if route.operation == "protocol" {
					class = app.ResourceInteractive
					_, releaseBulk, err := app.AcquireResources(test.Context(), app.ResourceBulk)
					requirePRCoordinationHTTP(test, err == nil, err)
					defer releaseBulk()
				}
				assertHeld := func() {
					probe, stop := context.WithTimeout(test.Context(), 20*time.Millisecond)
					defer stop()
					_, release, err := app.AcquireResources(probe, class)
					if release != nil {
						release()
					}
					requirePRCoordinationHTTP(test, errors.Is(err, context.DeadlineExceeded), "branch escaped shared resource scope", err)
				}
				parent, cancel := context.WithCancel(test.Context())
				defer cancel()
				status, calls, reads := 200, 1, 0
				if outcome == "decode failure" {
					request.Body, status, calls = io.NopCloser(strings.NewReader(`{"`)), 400, 0
				}
				if outcome == "owner failure" {
					owner.err, status = domain.ErrConflict, 409
				}
				request.Body = &wave58DocumentPublicationHTTPBody{Reader: request.Body, observe: func() { reads++; assertHeld() }}
				owner.observe = func(ctx context.Context) {
					assertHeld()
					_, release, err := app.AcquireResources(ctx, class)
					requirePRCoordinationHTTP(test, err == nil, "branch owner could not reuse admission", err)
					release()
					if outcome == "owner cancellation" {
						cancel()
						owner.err, status = context.Canceled, 500
						requirePRCoordinationHTTP(test, errors.Is(ctx.Err(), context.Canceled), "branch lost live cancellation")
					}
				}
				writer := &wave58DocumentPublicationHTTPWriter{ResponseRecorder: httptest.NewRecorder(), observe: func() {
					assertHeld()
					if outcome == "response cancellation" {
						cancel()
					}
				}}
				if outcome == "write failure" {
					writer.err = io.ErrClosedPipe
				}
				server.Handler().ServeHTTP(writer, request.WithContext(parent))
				decoded := route.operation == "fork" || route.operation == "join"
				requirePRCoordinationHTTP(test, writer.Code == status && writer.writes == 1 && len(owner.calls) == calls && (decoded && reads > 0 || !decoded && reads == 0), "branch admission lifetime", writer.Code, writer.Body, reads, owner.calls)
				probe, stop := context.WithTimeout(test.Context(), time.Second)
				defer stop()
				_, release, err := app.AcquireResources(probe, class)
				requirePRCoordinationHTTP(test, err == nil, "branch leaked admission", err)
				release()
			})
		}
	}
}
