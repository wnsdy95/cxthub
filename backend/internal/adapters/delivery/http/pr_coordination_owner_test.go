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

func requirePRCoordinationHTTP(test *testing.T, condition bool, details ...any) {
	test.Helper()
	if !condition {
		test.Fatal(details...)
	}
}

type prCoordinationHTTPCommands struct {
	calls      []string
	calledRepo domain.ContentHash
	request    domain.PullRequestMerge
	jobID      string
	job        domain.PRPromotionJob
	rows       []domain.PRPromotionJob
	output     inbound.UpdateRefOutput
	err        error
	observe    func(context.Context)
}

func (commands *prCoordinationHTTPCommands) record(ctx context.Context, operation string, repo domain.ContentHash) {
	commands.calls, commands.calledRepo = append(commands.calls, operation), repo
	if commands.observe != nil {
		commands.observe(ctx)
	}
}
func (commands *prCoordinationHTTPCommands) DeliverPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	commands.request = request
	commands.record(ctx, "deliver", repo)
	return commands.output, commands.err
}
func (commands *prCoordinationHTTPCommands) SubmitPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	commands.request = request
	commands.record(ctx, "submit", repo)
	return commands.job, commands.err
}
func (commands *prCoordinationHTTPCommands) ListPRPromotions(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	commands.record(ctx, "list", repo)
	return commands.rows, commands.err
}
func (commands *prCoordinationHTTPCommands) RetryPRPromotion(ctx context.Context, repo domain.ContentHash, jobID string) error {
	commands.jobID = jobID
	commands.record(ctx, "retry", repo)
	return commands.err
}
func (*prCoordinationHTTPCommands) PromoteRepositoryPR(context.Context, domain.ContentHash, domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	panic("HTTP must use typed Deliver, never synchronous Promote")
}

type prCoordinationHTTPBackend struct {
	*repositoryRefHTTPBackend
	*prCoordinationHTTPCommands
}
type prCoordinationHTTPRoute struct {
	operation, method, suffix string
}

var prCoordinationHTTPRoutes = []prCoordinationHTTPRoute{
	{"deliver", http.MethodPost, "/prs/promote"},
	{"submit", http.MethodPost, "/prs/promotions"},
	{"list", http.MethodGet, "/prs/promotions"},
	{"retry", http.MethodPost, "/prs/promotions/job-route/retry"},
}

func newPRCoordinationHTTPFixture(test *testing.T, route prCoordinationHTTPRoute) (*Server, *prCoordinationHTTPCommands, *http.Request) {
	test.Helper()
	backend := &prCoordinationHTTPBackend{repositoryRefHTTPBackend: &repositoryRefHTTPBackend{repo: domain.Repo{
		ID: domain.HashContent([]byte(test.Name())), RepositoryID: "ws_" + strings.Repeat("a", 32),
	}}, prCoordinationHTTPCommands: &prCoordinationHTTPCommands{observe: func(context.Context) { panic("PR reached broad backend or optional capability") }}}
	identity := &branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: domain.RoleMember}
	commands := &prCoordinationHTTPCommands{job: domain.PRPromotionJob{ID: "accepted-job", RepoID: backend.repo.ID, State: "waiting"},
		output: inbound.UpdateRefOutput{Result: inbound.RefUpToDate, Ref: domain.Ref{RepoID: backend.repo.ID, Name: "main", BranchID: "accepted-base"}}}
	commands.rows = []domain.PRPromotionJob{commands.job}
	server := NewServer(backend, identity)
	requirePRCoordinationHTTP(test, server.SetPRPromotionCommands(commands) == nil, "inject PR owner")
	request := httptest.NewRequest(route.method, "/api/v1/repos/"+string(backend.repo.ID)+route.suffix, strings.NewReader(`{"number":7,"base_branch":"main","head_branch":"feature/nested","head_sha":"`+strings.Repeat("b", 40)+`","merge_sha":"`+strings.Repeat("c", 40)+`"}`))
	request.Header.Set("Authorization", "Bearer pr-session")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
	return server, commands, request
}

func TestPRCoordinationHTTPExactTypedDispatchAndNilSetters(test *testing.T) {
	for _, route := range prCoordinationHTTPRoutes {
		test.Run(route.operation, func(test *testing.T) {
			server, commands, request := newPRCoordinationHTTPFixture(test, route)
			for _, rejected := range []inbound.PRPromotionCommands{nil, (*prCoordinationHTTPCommands)(nil), (*app.PRPromotionCommands)(nil)} {
				for _, target := range []*Server{NewServer(nil, nil), server} {
					requirePRCoordinationHTTP(test, errors.Is(target.SetPRPromotionCommands(rejected), domain.ErrValidation), "nil setter")
				}
			}
			key := struct{ name string }{"PR request"}
			parent, cancel := context.WithTimeout(context.WithValue(test.Context(), key, "retained"), 30*time.Second)
			defer cancel()
			commands.observe = func(ctx context.Context) {
				deadline, present := ctx.Deadline()
				expected, _ := parent.Deadline()
				actor, system := inbound.RepositoryActor(ctx)
				requirePRCoordinationHTTP(test, !(ctx.Value(key) != "retained" || ctx.Err() != nil || !present || !deadline.Equal(expected) || actor != "actor" || system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})), "PR request context lost")
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request.WithContext(parent))
			expected := any(commands.output)
			switch route.operation {
			case "submit":
				expected = commands.job
			case "list":
				expected = commands.rows
			case "retry":
				expected = map[string]bool{"queued": true}
			}
			raw, err := json.Marshal(expected)
			requirePRCoordinationHTTP(test, err == nil, err)
			requirePRCoordinationHTTP(test, !(response.Code != 200 || strings.TrimSpace(response.Body.String()) != string(raw) || response.Header().Get("Content-Type") != "application/json; charset=utf-8" || response.Header().Get("Cache-Control") != "no-store" || !reflect.DeepEqual(commands.calls, []string{route.operation}) || commands.calledRepo != server.b.(*prCoordinationHTTPBackend).repo.ID), "typed dispatch or wire result", response.Code, response.Body, commands.calls)
			if route.operation == "deliver" || route.operation == "submit" {
				requirePRCoordinationHTTP(test, !(commands.request != (domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature/nested", HeadSHA: strings.Repeat("b", 40), MergeSHA: strings.Repeat("c", 40)})), "PR payload changed", commands.request)
			}
			requirePRCoordinationHTTP(test, !(route.operation == "retry" && commands.jobID != "job-route"), "retry route identity changed", commands.jobID)
			if route.operation == "list" {
				commands.rows = nil
				empty := httptest.NewRecorder()
				server.Handler().ServeHTTP(empty, request.WithContext(parent))
				requirePRCoordinationHTTP(test, !(empty.Code != 200 || strings.TrimSpace(empty.Body.String()) != "[]"), "nil list wire shape", empty.Code, empty.Body)
			}
		})
	}
}

func TestPRCoordinationHTTPGatesAndOwnerErrorPrecedence(test *testing.T) {
	for _, route := range prCoordinationHTTPRoutes {
		for _, failure := range []struct {
			name   string
			status int
			err    error
		}{
			{"anonymous", 401, nil}, {"content type", 415, nil}, {"malformed", 400, nil}, {"oversized", 413, nil},
			{"ordering_evidence_pending", 409, domain.ErrPROrderingPending},
			{"conflict", 409, domain.ErrConflict}, {"forbidden", 403, domain.ErrForbidden}, {"validation", 422, domain.ErrValidation},
			{"resource_overloaded", 503, domain.ErrResourceOverloaded}, {"internal", 500, errors.New("private PR failure")},
		} {
			if failure.name == "content type" && route.operation == "list" || (failure.name == "malformed" || failure.name == "oversized") && route.operation != "deliver" && route.operation != "submit" {
				continue
			}
			test.Run(route.operation+"/"+failure.name, func(test *testing.T) {
				server, commands, request := newPRCoordinationHTTPFixture(test, route)
				commands.err = failure.err
				switch failure.name {
				case "anonymous":
					request.Header.Del("Authorization")
				case "content type":
					request.Header.Set("Content-Type", "text/plain")
				case "malformed":
					request.Body = io.NopCloser(strings.NewReader(`{"`))
				case "oversized":
					request.Body = io.NopCloser(strings.NewReader(`{"head_branch":"` + strings.Repeat("x", 32<<10) + `"}`))
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				calls := 0
				if failure.err != nil {
					calls = 1
				}
				requirePRCoordinationHTTP(test, !(response.Code != failure.status || len(commands.calls) != calls || strings.Contains(response.Body.String(), `"queued":`) || strings.Contains(response.Body.String(), `"accepted-job"`) || strings.Contains(response.Body.String(), "private PR failure") || failure.err != nil && !strings.Contains(response.Body.String(), `"code":"`+failure.name+`"`) || failure.err == domain.ErrResourceOverloaded && response.Header().Get("Retry-After") != "1"), "PR error precedence", response.Code, response.Body, commands.calls)
			})
		}
	}
}

type prCoordinationHTTPLegacy struct {
	*repositoryRefHTTPBackend
	commands *prCoordinationHTTPCommands
}

func (backend *prCoordinationHTTPLegacy) PromoteRepositoryPR(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	backend.commands.record(ctx, "legacy", repo)
	return backend.commands.output, nil
}

type prCoordinationHTTPDurable struct{ *prCoordinationHTTPLegacy }

func (backend *prCoordinationHTTPDurable) DeliverPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	backend.commands.record(ctx, "durable", repo)
	return backend.commands.output, nil
}

func TestPRCoordinationHTTPCompositionCompatibility(test *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, route := range prCoordinationHTTPRoutes {
			for _, jsonBody := range []bool{false, true} {
				server, commands, request := newPRCoordinationHTTPFixture(test, route)
				legacy := &prCoordinationHTTPLegacy{server.b.(*prCoordinationHTTPBackend).repositoryRefHTTPBackend, commands}
				var backend Backend = legacy
				operation := "legacy"
				if durable {
					backend, operation = &prCoordinationHTTPDurable{legacy}, "durable"
				}
				server = NewServer(backend, server.id)
				request.Body = io.NopCloser(strings.NewReader(`{"`))
				if !jsonBody {
					request.Header.Set("Content-Type", "text/plain")
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				status := 503
				if route.operation == "deliver" {
					status = 400
					if !jsonBody {
						status = 415
					}
				}
				if route.operation == "retry" && !jsonBody {
					status = 415
				}
				requirePRCoordinationHTTP(test, !(response.Code != status || len(commands.calls) != 0 || status == 503 && !strings.Contains(response.Body.String(), `"message":"PR queue unavailable"`)), "legacy queue/JSON precedence", response.Code, response.Body)
				if route.operation == "deliver" && jsonBody {
					_, _, valid := newPRCoordinationHTTPFixture(test, route)
					accepted := httptest.NewRecorder()
					server.Handler().ServeHTTP(accepted, valid)
					requirePRCoordinationHTTP(test, !(accepted.Code != 200 || !reflect.DeepEqual(commands.calls, []string{operation})), "legacy durable preference", accepted.Code, accepted.Body, commands.calls)
				}
			}
		}
	}
}

func TestPRCoordinationHTTPInteractiveAdmissionLifetime(test *testing.T) {
	for _, route := range prCoordinationHTTPRoutes {
		for _, outcome := range []string{"success", "owner failure", "decode failure", "response cancellation", "write failure"} {
			if outcome == "decode failure" && route.operation != "deliver" && route.operation != "submit" {
				continue
			}
			test.Run(route.operation+"/"+outcome, func(test *testing.T) {
				server, commands, request := newPRCoordinationHTTPFixture(test, route)
				_, releaseBulk, err := app.AcquireResources(test.Context(), app.ResourceBulk)
				requirePRCoordinationHTTP(test, err == nil, err)
				defer releaseBulk()
				parent, cancel := context.WithCancel(test.Context())
				defer cancel()
				assertInteractive := func() {
					probe, stop := context.WithTimeout(test.Context(), 20*time.Millisecond)
					defer stop()
					_, release, err := app.AcquireResources(probe, app.ResourceInteractive)
					if release != nil {
						release()
					}
					requirePRCoordinationHTTP(test, errors.Is(err, context.DeadlineExceeded), "PR escaped Interactive scope", err)
				}
				status, calls, reads := 200, 1, 0
				if outcome == "owner failure" {
					commands.err, status = domain.ErrConflict, 409
				}
				if outcome == "decode failure" {
					request.Body, status, calls = io.NopCloser(strings.NewReader(`{"`)), 400, 0
				}
				request.Body = &wave58DocumentPublicationHTTPBody{Reader: request.Body, observe: func() {
					reads++
					assertInteractive()
				}}
				commands.observe = func(ctx context.Context) {
					assertInteractive()
					_, release, err := app.AcquireResources(ctx, app.ResourceBulk)
					if release != nil {
						release()
					}
					requirePRCoordinationHTTP(test, errors.Is(err, domain.ErrResourceOverloaded), "HTTP acquired Bulk before durable acceptance", err)
				}
				writer := &wave58DocumentPublicationHTTPWriter{ResponseRecorder: httptest.NewRecorder(), observe: func() {
					assertInteractive()
					if outcome == "response cancellation" {
						cancel()
					}
				}}
				if outcome == "write failure" {
					writer.err = io.ErrClosedPipe
				}
				server.Handler().ServeHTTP(writer, request.WithContext(parent))
				requirePRCoordinationHTTP(test, !(writer.Code != status || writer.writes != 1 || len(commands.calls) != calls || (route.operation == "deliver" || route.operation == "submit") && reads == 0), "PR admission lifetime", writer.Code, writer.Body, reads, commands.calls)
				probe, stop := context.WithTimeout(test.Context(), time.Second)
				defer stop()
				_, release, err := app.AcquireResources(probe, app.ResourceInteractive)
				requirePRCoordinationHTTP(test, err == nil, "PR leaked Interactive admission", err)
				release()
			})
		}
	}
}
