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

type repositoryRefHTTPCommands struct {
	calls   []string
	single  inbound.UpdateRefInput
	batch   inbound.UpdateRefsInput
	output  inbound.UpdateRefOutput
	applied int
	err     error
	observe func(context.Context, string)
}

func (commands *repositoryRefHTTPCommands) record(ctx context.Context, operation string) {
	commands.calls = append(commands.calls, operation)
	if commands.observe != nil {
		commands.observe(ctx, operation)
	}
}

func (commands *repositoryRefHTTPCommands) UpdateRef(ctx context.Context, input inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	commands.single = input
	commands.record(ctx, "ref")
	return commands.output, commands.err
}

func (commands *repositoryRefHTTPCommands) UpdateRefs(ctx context.Context, input inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error) {
	commands.batch = input
	commands.record(ctx, "refs")
	return inbound.UpdateRefsOutput{Applied: commands.applied}, commands.err
}

type repositoryRefHTTPBackend struct {
	Backend
	repo domain.Repo
}

func (backend *repositoryRefHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (*repositoryRefHTTPBackend) UpdateRef(context.Context, inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	panic("ref command reached the broad HTTP backend")
}

func (*repositoryRefHTTPBackend) UpdateRefs(context.Context, inbound.UpdateRefsInput) (inbound.UpdateRefsOutput, error) {
	panic("ref batch reached the broad HTTP backend")
}

type repositoryRefHTTPFixture struct {
	server   *Server
	backend  *repositoryRefHTTPBackend
	commands *repositoryRefHTTPCommands
	identity *branchPullHTTPIdentity
}

func newRepositoryRefHTTPFixture(test *testing.T) repositoryRefHTTPFixture {
	test.Helper()
	backend := &repositoryRefHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(test.Name())), RepositoryID: "ws_" + strings.Repeat("d", 32)}}
	identity := &branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: domain.RoleMember}
	target := domain.HashContent([]byte("owner response target"))
	fork := domain.Ref{RepoID: backend.repo.ID, Kind: domain.RefBranch, Name: "owner/fork", Target: target}
	commands := &repositoryRefHTTPCommands{
		applied: 2,
		output: inbound.UpdateRefOutput{
			Result: inbound.RefDivergedForked, Ref: fork, ForkedRef: &fork,
			RequestedTarget: domain.HashContent([]byte("owner requested target")),
			ServerTarget:    target, MergeBase: domain.HashContent([]byte("owner merge base")),
		},
	}
	server := NewServer(backend, identity)
	if err := server.SetRepositoryRefCommands(commands); err != nil {
		test.Fatal(err)
	}
	return repositoryRefHTTPFixture{server: server, backend: backend, commands: commands, identity: identity}
}

type repositoryRefHTTPRequest struct {
	name      string
	method    string
	path      string
	body      string
	operation string
	single    inbound.UpdateRefInput
	batch     inbound.UpdateRefsInput
}

func repositoryRefHTTPRequests(repo domain.ContentHash) []repositoryRefHTTPRequest {
	target := domain.HashContent([]byte("requested ref target"))
	previous := domain.HashContent([]byte("observed ref target"))
	foreign := domain.HashContent([]byte("untrusted body repository"))
	branch := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature/nested", BranchID: "branch-identity", Target: target}
	tag := domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "release/v1", Target: previous}
	revision := domain.RepositoryRevision{Graph: 9007199254740993, Evidence: 9007199254740995, Pending: 12}
	return []repositoryRefHTTPRequest{
		{
			name: "ordinary ref", method: http.MethodPut, path: "/refs/branch/feature/nested", operation: "ref",
			body:   fmt.Sprintf(`{"branch_id":"branch-identity","target":%q,"expected_target":%q,"force":true,"append":true}`, target, previous),
			single: inbound.UpdateRefInput{RepoID: repo, Ref: branch, ExpectedTarget: previous, Force: true, Append: true},
		},
		{
			name: "symbolic HEAD", method: http.MethodPut, path: "/refs/head/HEAD", operation: "ref",
			body:   `{"symbolic":"refs/heads/feature/nested"}`,
			single: inbound.UpdateRefInput{RepoID: repo, Ref: domain.Ref{RepoID: repo, Kind: domain.RefHead, Name: "HEAD", Symbolic: "refs/heads/feature/nested"}},
		},
		{
			name: "conditional append", method: http.MethodPost, path: "/conditional-appends/v1/feature/nested", operation: "ref",
			body:   fmt.Sprintf(`{"branch_id":"branch-identity","target":%q,"expected_target":%q,"expected_revision":{"graph":"9007199254740993","evidence":"9007199254740995","pending":"12"}}`, target, previous),
			single: inbound.UpdateRefInput{RepoID: repo, Ref: branch, ExpectedTarget: previous, ExpectedRevision: &revision, Append: true},
		},
		{
			name: "batch", method: http.MethodPost, path: "/refs/batch", operation: "refs",
			body:  fmt.Sprintf(`{"updates":[{"ref":{"repo_id":%q,"kind":"branch","name":"feature/nested","branch_id":"branch-identity","target":%q},"append":true},{"ref":{"repo_id":%q,"kind":"tag","name":"release/v1","target":%q},"force":true}]}`, foreign, target, foreign, previous),
			batch: inbound.UpdateRefsInput{RepoID: repo, Updates: []inbound.UpdateRefInput{{RepoID: repo, Ref: branch, Append: true}, {RepoID: repo, Ref: tag, Force: true}}},
		},
		{
			name: "empty batch retry", method: http.MethodPost, path: "/refs/batch", operation: "refs", body: `{"updates":[]}`,
			batch: inbound.UpdateRefsInput{RepoID: repo, Updates: []inbound.UpdateRefInput{}},
		},
	}
}

func (fixture repositoryRefHTTPFixture) request(input repositoryRefHTTPRequest) *http.Request {
	request := httptest.NewRequest(input.method, "/api/v1/repos/"+string(fixture.backend.repo.ID)+input.path, strings.NewReader(input.body))
	request.Header.Set("Authorization", "Bearer ref-session")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
	return request
}

func TestRepositoryRefHTTPUsesInjectedOwnerAndExactWireInputs(test *testing.T) {
	for _, selected := range repositoryRefHTTPRequests("") {
		test.Run(selected.name, func(test *testing.T) {
			fixture := newRepositoryRefHTTPFixture(test)
			for _, input := range repositoryRefHTTPRequests(fixture.backend.repo.ID) {
				if input.name != selected.name {
					continue
				}
				requestKey := struct{ name string }{"ref command request"}
				parent, cancel := context.WithTimeout(context.WithValue(test.Context(), requestKey, "retained"), 30*time.Second)
				defer cancel()
				fixture.commands.observe = func(ctx context.Context, operation string) {
					deadline, present := ctx.Deadline()
					originalDeadline, _ := parent.Deadline()
					if operation != input.operation || ctx.Value(requestKey) != "retained" || ctx.Err() != nil || !present || !deadline.Equal(originalDeadline) {
						test.Fatal("ref command lost request values, deadline or operation", operation, deadline, ctx.Err())
					}
					if actor, system := inbound.RepositoryActor(ctx); actor != "actor" || system {
						test.Fatal("ref command lost authenticated actor", actor, system)
					}
					if identities := inbound.DocumentIdentities(ctx); !reflect.DeepEqual(identities, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}) {
						test.Fatal("ref command lost document identity declaration", identities)
					}
				}
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request(input).WithContext(parent))
				var actual, expected any
				expectedOutput := any(fixture.commands.output)
				if input.operation == "refs" {
					expectedOutput = inbound.UpdateRefsOutput{Applied: fixture.commands.applied}
				}
				expectedJSON, err := json.Marshal(expectedOutput)
				if err != nil {
					test.Fatal(err)
				}
				if err := json.Unmarshal(expectedJSON, &expected); err != nil {
					test.Fatal(err)
				}
				if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
					test.Fatal("ref response is not JSON", response.Code, response.Body, err)
				}
				if response.Code != http.StatusOK || !reflect.DeepEqual(actual, expected) || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
					test.Fatal("ref command changed response or headers", response.Code, response.Header(), response.Body)
				}
				if !reflect.DeepEqual(fixture.commands.calls, []string{input.operation}) || !reflect.DeepEqual(fixture.commands.single, input.single) || !reflect.DeepEqual(fixture.commands.batch, input.batch) {
					test.Fatal("request did not reach exactly one narrow owner with exact inputs", fixture.commands.calls, fixture.commands.single, fixture.commands.batch)
				}
			}
		})
	}
}

func TestSetRepositoryRefCommandsRejectsNilWithoutReplacingOwner(test *testing.T) {
	var typedNil *repositoryRefHTTPCommands
	for _, rejected := range []struct {
		name     string
		commands inbound.RepositoryRefCommands
	}{
		{name: "nil"},
		{name: "typed nil", commands: typedNil},
	} {
		test.Run(rejected.name, func(test *testing.T) {
			fixture := newRepositoryRefHTTPFixture(test)
			for _, server := range []*Server{NewServer(nil, nil), NewServer(fixture.backend, nil), fixture.server} {
				if err := server.SetRepositoryRefCommands(rejected.commands); !errors.Is(err, domain.ErrValidation) {
					test.Fatal("nil ref owner was not rejected", err)
				}
			}
			for _, input := range repositoryRefHTTPRequests(fixture.backend.repo.ID) {
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request(input))
				if response.Code != http.StatusOK {
					test.Fatal("rejected setter replaced the live owner", response.Code, response.Body)
				}
			}
			if !reflect.DeepEqual(fixture.commands.calls, []string{"ref", "ref", "ref", "refs", "refs"}) {
				test.Fatal("rejected setter changed the selected ref owner", fixture.commands.calls)
			}
		})
	}
}

func TestRepositoryRefHTTPPreservesOwnerErrors(test *testing.T) {
	for _, input := range repositoryRefHTTPRequests("") {
		for _, failure := range []struct {
			name   string
			err    error
			status int
			code   string
			retry  string
		}{
			{name: "ref CAS", err: domain.ErrRefConflict, status: http.StatusConflict, code: "ref_conflict"},
			{name: "non fast forward", err: domain.ErrNonFastForward, status: http.StatusConflict, code: "non_fast_forward"},
			{name: "archive", err: domain.ErrBranchArchived, status: http.StatusConflict, code: "branch_archived"},
			{name: "validation", err: domain.ErrValidation, status: http.StatusUnprocessableEntity, code: "validation"},
			{name: "integrity", err: domain.ErrIntegrity, status: http.StatusUnprocessableEntity, code: "integrity_violation"},
			{name: "forbidden", err: domain.ErrForbidden, status: http.StatusForbidden, code: "forbidden"},
			{name: "missing", err: domain.ErrNotFound, status: http.StatusNotFound, code: "not_found"},
			{name: "overload", err: domain.ErrResourceOverloaded, status: http.StatusServiceUnavailable, code: "resource_overloaded", retry: "1"},
			{name: "private failure", err: errors.New("private ref storage detail"), status: http.StatusInternalServerError, code: "internal"},
		} {
			test.Run(input.name+"/"+failure.name, func(test *testing.T) {
				fixture := newRepositoryRefHTTPFixture(test)
				fixture.commands.err = fmt.Errorf("ref owner: %w", failure.err)
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, fixture.request(input))
				message := fixture.commands.err.Error()
				if failure.code == "internal" {
					message = "internal server error"
				}
				var envelope any
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
					test.Fatal(err)
				}
				expected := map[string]any{"error": map[string]any{"code": failure.code, "message": message, "details": map[string]any{}}}
				if !reflect.DeepEqual(envelope, expected) {
					test.Fatal("ref command error did not preserve the exact public envelope", envelope, expected)
				}
				if response.Code != failure.status || !strings.Contains(response.Body.String(), `"code":"`+failure.code+`"`) || strings.Contains(response.Body.String(), "private ref storage detail") || strings.Contains(response.Body.String(), `"result"`) || strings.Contains(response.Body.String(), `"applied"`) || response.Header().Get("Retry-After") != failure.retry || response.Header().Get("Cache-Control") != "no-store" || !reflect.DeepEqual(fixture.commands.calls, []string{input.operation}) {
					test.Fatal("ref error changed mapping, sanitization or dispatch count", response.Code, response.Header(), response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestRepositoryRefHTTPPropagatesLiveCancellation(test *testing.T) {
	for _, input := range repositoryRefHTTPRequests("") {
		test.Run(input.name, func(test *testing.T) {
			fixture := newRepositoryRefHTTPFixture(test)
			parent, cancel := context.WithCancel(test.Context())
			defer cancel()
			fixture.commands.err = context.Canceled
			fixture.commands.observe = func(ctx context.Context, operation string) {
				cancel()
				if operation != input.operation || !errors.Is(ctx.Err(), context.Canceled) {
					test.Fatal("ref owner lost live request cancellation", operation, ctx.Err())
				}
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(input).WithContext(parent))
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"internal"`) || !reflect.DeepEqual(fixture.commands.calls, []string{input.operation}) {
				test.Fatal("canceled ref command was retried or remapped", response.Code, response.Body, fixture.commands.calls)
			}
		})
	}
}

func TestRepositoryRefHTTPRetainsAuthorizationAndDecodeGates(test *testing.T) {
	for _, input := range repositoryRefHTTPRequests("") {
		for _, gate := range []struct {
			name   string
			status int
		}{
			{name: "anonymous", status: http.StatusUnauthorized},
			{name: "viewer", status: http.StatusForbidden},
			{name: "puller", status: http.StatusForbidden},
			{name: "archived", status: http.StatusForbidden},
			{name: "break glass", status: http.StatusForbidden},
			{name: "csrf", status: http.StatusForbidden},
			{name: "content type", status: http.StatusUnsupportedMediaType},
			{name: "malformed JSON", status: http.StatusBadRequest},
		} {
			test.Run(input.name+"/"+gate.name, func(test *testing.T) {
				fixture := newRepositoryRefHTTPFixture(test)
				request := fixture.request(input)
				switch gate.name {
				case "anonymous":
					request.Header.Del("Authorization")
				case "viewer":
					fixture.identity.role = domain.RoleViewer
				case "puller":
					fixture.identity.role = domain.RolePuller
				case "archived":
					fixture.identity.repo.Archived = true
				case "break glass":
					fixture.identity.role, fixture.identity.breakGlass = "", true
				case "csrf":
					request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ref-session"})
				case "content type":
					request.Header.Set("Content-Type", "text/plain")
				case "malformed JSON":
					request.Body = io.NopCloser(strings.NewReader(`{"`))
				}
				response := httptest.NewRecorder()
				fixture.server.Handler().ServeHTTP(response, request)
				if response.Code != gate.status || len(fixture.commands.calls) != 0 {
					test.Fatal("transport gate reached ref owner", response.Code, response.Body, fixture.commands.calls)
				}
			})
		}
	}
}

func TestRepositoryRefHTTPConditionalAppendStrictDecodeBeforeOwner(test *testing.T) {
	for _, mode := range []string{"missing graph", "missing evidence", "numeric graph", "negative evidence", "missing expected target", "missing branch identity", "force", "not append", "unknown field", "trailing object", "body limit"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newRepositoryRefHTTPFixture(test)
			input := repositoryRefHTTPRequests(fixture.backend.repo.ID)[2]
			var body map[string]any
			if err := json.Unmarshal([]byte(input.body), &body); err != nil {
				test.Fatal(err)
			}
			revision := body["expected_revision"].(map[string]any)
			switch mode {
			case "missing graph":
				delete(revision, "graph")
			case "missing evidence":
				delete(revision, "evidence")
			case "numeric graph":
				revision["graph"] = 1
			case "negative evidence":
				revision["evidence"] = "-1"
			case "missing expected target":
				delete(body, "expected_target")
			case "missing branch identity":
				delete(body, "branch_id")
			case "force":
				body["force"] = true
			case "not append":
				body["append"] = false
			case "unknown field":
				body["symbolic"] = "refs/heads/other"
			}
			payload, err := json.Marshal(body)
			if err != nil {
				test.Fatal(err)
			}
			input.body = string(payload)
			status := http.StatusBadRequest
			if mode == "trailing object" {
				input.body += `{}`
			}
			if mode == "body limit" {
				input.body += strings.Repeat(" ", 16<<10)
				status = http.StatusRequestEntityTooLarge
			}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(input))
			if response.Code != status || len(fixture.commands.calls) != 0 {
				test.Fatal("conditional append decoded invalid preconditions into owner", response.Code, response.Body, fixture.commands.calls)
			}
		})
	}
}

func TestRepositoryRefHTTPBatchBoundsBeforeOwner(test *testing.T) {
	for _, mode := range []string{"maximum", "too many", "body limit", "missing updates"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newRepositoryRefHTTPFixture(test)
			input := repositoryRefHTTPRequests(fixture.backend.repo.ID)[3]
			count, status, calls := inbound.MaxRefBatchUpdates, http.StatusOK, 1
			if mode == "too many" {
				count, status, calls = count+1, http.StatusUnprocessableEntity, 0
			}
			entry := fmt.Sprintf(`{"ref":{"kind":"branch","name":"main","target":%q}}`, domain.HashContent([]byte("batch bound target")))
			input.body = `{"updates":[` + strings.TrimSuffix(strings.Repeat(entry+",", count), ",") + `]}`
			if mode == "body limit" {
				input.body = strings.Repeat(" ", inbound.MaxRefBatchJSONBody) + input.body
				status, calls = http.StatusRequestEntityTooLarge, 0
			}
			if mode == "missing updates" {
				input.body, count = `{}`, 0
			}
			fixture.commands.applied = count
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, fixture.request(input))
			if response.Code != status || len(fixture.commands.calls) != calls {
				test.Fatal("batch transport bound changed dispatch", response.Code, response.Body, fixture.commands.calls)
			}
			if calls == 1 && (fixture.commands.batch.RepoID != fixture.backend.repo.ID || len(fixture.commands.batch.Updates) != count || fixture.commands.batch.Updates == nil) {
				test.Fatal("bounded or empty batch lost its exact cardinality", fixture.commands.batch)
			}
		})
	}
}

type repositoryRefHTTPBody struct {
	*strings.Reader
	observe func()
}

func (body *repositoryRefHTTPBody) Read(destination []byte) (int, error) {
	body.observe()
	return body.Reader.Read(destination)
}

func (*repositoryRefHTTPBody) Close() error { return nil }

type repositoryRefHTTPWriter struct {
	*httptest.ResponseRecorder
	observe func()
	writes  int
	err     error
}

func (writer *repositoryRefHTTPWriter) Write(body []byte) (int, error) {
	writer.writes++
	writer.observe()
	if writer.err != nil {
		return 0, writer.err
	}
	return writer.ResponseRecorder.Write(body)
}

func repositoryRefHTTPAssertBulkHeld(test *testing.T) {
	test.Helper()
	probe, cancel := context.WithTimeout(test.Context(), 20*time.Millisecond)
	defer cancel()
	_, release, err := app.AcquireResources(probe, app.ResourceBulk)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		test.Fatal("ref HTTP response escaped transport resource admission", err)
	}
}

func TestRepositoryRefHTTPRetainsResourcesThroughDecodeOwnerAndResponse(test *testing.T) {
	for _, input := range repositoryRefHTTPRequests("") {
		for _, outcome := range []string{"success", "decode failure", "owner failure", "response cancellation", "write failure"} {
			test.Run(input.name+"/"+outcome, func(test *testing.T) {
				fixture := newRepositoryRefHTTPFixture(test)
				parent, cancel := context.WithCancel(test.Context())
				defer cancel()
				request := fixture.request(input).WithContext(parent)
				payload, status, calls := input.body, http.StatusOK, 1
				if outcome == "decode failure" {
					payload, status, calls = `{"`, http.StatusBadRequest, 0
				}
				if outcome == "owner failure" {
					fixture.commands.err, status = domain.ErrRefConflict, http.StatusConflict
				}
				reads := 0
				request.Body = &repositoryRefHTTPBody{Reader: strings.NewReader(payload), observe: func() {
					reads++
					repositoryRefHTTPAssertBulkHeld(test)
				}}
				fixture.commands.observe = func(ctx context.Context, operation string) {
					repositoryRefHTTPAssertBulkHeld(test)
					_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
					if err != nil {
						test.Fatal("ref owner could not reuse transport resource admission", operation, err)
					}
					release()
				}
				writer := &repositoryRefHTTPWriter{ResponseRecorder: httptest.NewRecorder(), observe: func() {
					repositoryRefHTTPAssertBulkHeld(test)
					if outcome == "response cancellation" {
						cancel()
					}
				}}
				if outcome == "write failure" {
					writer.err = io.ErrClosedPipe
				}
				fixture.server.Handler().ServeHTTP(writer, request)
				if writer.Code != status || writer.writes != 1 || reads == 0 || len(fixture.commands.calls) != calls {
					test.Fatal("ref response resource scope changed decode or dispatch", writer.Code, writer.Body, reads, writer.writes, fixture.commands.calls)
				}
				probe, stop := context.WithTimeout(test.Context(), time.Second)
				defer stop()
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if err != nil {
					test.Fatal("ref response completion leaked resource admission", err)
				}
				release()
			})
		}
	}
}

func TestRepositoryRefHTTPCanceledAdmissionPrecedesDecodeAndDispatch(test *testing.T) {
	for _, input := range repositoryRefHTTPRequests("") {
		test.Run(input.name, func(test *testing.T) {
			fixture := newRepositoryRefHTTPFixture(test)
			parent, cancel := context.WithCancel(test.Context())
			cancel()
			request := fixture.request(input).WithContext(parent)
			reads := 0
			request.Body = &repositoryRefHTTPBody{Reader: strings.NewReader(input.body), observe: func() { reads++ }}
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"internal"`) || reads != 0 || len(fixture.commands.calls) != 0 {
				test.Fatal("canceled ref request decoded or reached a command", response.Code, response.Body, reads, fixture.commands.calls)
			}
		})
	}
}
