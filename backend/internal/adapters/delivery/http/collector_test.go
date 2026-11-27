package http

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type collectorHTTPBackend struct {
	Backend
	repo    domain.Repo
	calls   int
	actor   string
	asked   domain.ContentHash
	report  domain.CollectorReport
	view    domain.PushOriginView
	err     error
	origins int
}

func (backend *collectorHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *collectorHTTPBackend) ReportCollector(ctx context.Context, repo domain.ContentHash, report domain.CollectorReport) error {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.asked, backend.report = repo, report
	return backend.err
}

func (backend *collectorHTTPBackend) PushOrigins(ctx context.Context, repo domain.ContentHash) (domain.PushOriginView, error) {
	backend.origins++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.asked = repo
	return backend.view, backend.err
}

func collectorHTTPFixture(t *testing.T, role domain.MemberRole) (*collectorHTTPBackend, http.Handler, string) {
	t.Helper()
	backend := &collectorHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("e", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: role}
	return backend, NewServer(backend, identity).Handler(), "/api/v1/repos/" + string(backend.repo.ID)
}

// A collector reports for the member it runs for: a member may, a viewer may
// not, and what is sent reaches the service as it was sent.
func TestCollectorReportHTTP(t *testing.T) {
	sha := strings.Repeat("a", 40)
	full := `{"machine":"laptop","cxt_version":"1.2.3","run_error":"the upload was refused","pushes":[{"branch":"main","sha":"` + sha + `","at":1700000000}]}`
	want := domain.CollectorReport{Machine: "laptop", Version: "1.2.3", RunError: "the upload was refused", Pushes: []domain.CollectorPush{{Branch: "main", SHA: sha, At: 1_700_000_000}}}
	for _, test := range []struct {
		name         string
		method       string
		role         domain.MemberRole
		authed       bool
		body         string
		status       int
		reached      bool
		serviceError error
	}{
		{"anonymous", http.MethodPut, domain.RoleOwner, false, full, 401, false, nil},
		{"viewer", http.MethodPut, domain.RoleViewer, true, full, 403, false, nil},
		{"puller", http.MethodPut, domain.RolePuller, true, full, 403, false, nil},
		{"member", http.MethodPut, domain.RoleMember, true, full, 200, true, nil},
		{"owner", http.MethodPut, domain.RoleOwner, true, full, 200, true, nil},
		{"refused_by_the_service", http.MethodPut, domain.RoleMember, true, full, 422, true, domain.ErrValidation},
		{"no_push_list", http.MethodPut, domain.RoleMember, true, `{"machine":"laptop"}`, 400, false, nil},
		{"an_unknown_field", http.MethodPut, domain.RoleMember, true, `{"pushes":[],"account":"member-b"}`, 400, false, nil},
		{"not_json", http.MethodPut, domain.RoleMember, true, `pushes`, 400, false, nil},
		{"too_large", http.MethodPut, domain.RoleMember, true, `{"pushes":[],"run_error":"` + strings.Repeat("e", collectorReportBodyLimit) + `"}`, 413, false, nil},
		{"posting_it", http.MethodPost, domain.RoleOwner, true, full, 405, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := collectorHTTPFixture(t, test.role)
			backend.err = test.serviceError
			response := bpHTTP(t, handler, test.method, base+"/collector", test.body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if !test.reached {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" || backend.asked != backend.repo.ID || !reflect.DeepEqual(backend.report, want) {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.status == 200 {
				var answer map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || !reflect.DeepEqual(answer, map[string]any{"heard": true}) {
					t.Fatalf("the answer: %s %v", response.Body.String(), err)
				}
			}
		})
	}
	// A report that names no push is a report: an empty list is sent, and reaches the service as one.
	backend, handler, base := collectorHTTPFixture(t, domain.RoleMember)
	if response := bpHTTP(t, handler, http.MethodPut, base+"/collector", `{"pushes":[]}`, true); response.Code != 200 || backend.calls != 1 || backend.report.Pushes == nil || len(backend.report.Pushes) != 0 {
		t.Fatalf("a report without pushes: %d %+v", response.Code, backend.report)
	}
}

// Where the pushes came from is read by whoever may view the repository.
func TestPushOriginsHTTP(t *testing.T) {
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	zero := 0
	view := domain.PushOriginView{
		Collectors: []domain.PushCollector{{By: "alice", Machine: "laptop", Version: "1.2.3", HeardAt: at, Failing: true, RunError: "the upload was refused", Own: true}},
		Pushes: []domain.PushOrigin{
			{Branch: "main", SHA: strings.Repeat("a", 40), At: at, State: domain.PushByFailingCollector, Collector: &zero, ClaimedAt: &at},
			{Branch: "topic", SHA: strings.Repeat("b", 40), At: at, State: domain.PushOriginNotKnown, Waiting: []int{0}},
		},
	}
	for _, test := range []struct {
		name         string
		method       string
		role         domain.MemberRole
		authed       bool
		status       int
		reached      bool
		serviceError error
	}{
		{"anonymous", http.MethodGet, domain.RoleOwner, false, 401, false, nil},
		{"viewer", http.MethodGet, domain.RoleViewer, true, 200, true, nil},
		{"member", http.MethodGet, domain.RoleMember, true, 200, true, nil},
		{"storage_unavailable", http.MethodGet, domain.RoleMember, true, 409, true, domain.ErrConflict},
		{"writing_it", http.MethodPost, domain.RoleOwner, true, 405, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := collectorHTTPFixture(t, test.role)
			backend.view, backend.err = view, test.serviceError
			body := ""
			if test.method == http.MethodPost {
				body = "{}"
			}
			response := bpHTTP(t, handler, test.method, base+"/push-origins", body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if !test.reached {
				if backend.origins != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.origins != 1 || backend.actor != "actor" || backend.asked != backend.repo.ID {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.status != 200 {
				if strings.Contains(response.Body.String(), "laptop") {
					t.Fatalf("a refusal carried the view: %s", response.Body.String())
				}
				return
			}
			var answer domain.PushOriginView
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || !reflect.DeepEqual(answer, view) {
				t.Fatalf("the answer: %s %v", response.Body.String(), err)
			}
			// The names a client reads.
			for _, name := range []string{`"collectors"`, `"pushes"`, `"heard_at"`, `"failing":true`, `"own":true`, `"state":"cxt_error"`, `"collector":0`, `"claimed_at"`, `"waiting":[0]`, `"cxt_version"`} {
				if !strings.Contains(response.Body.String(), name) {
					t.Errorf("the answer does not hold %s: %s", name, response.Body.String())
				}
			}
		})
	}
}
