package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type snapshotGraftHTTPBackend struct{ *repositoryRefHTTPBackend }

func (*snapshotGraftHTTPBackend) GraftSnapshotParents(context.Context, domain.ContentHash, domain.ContentHash, []domain.ContentHash, uint64) error {
	panic("graft reached broad HTTP backend")
}

type snapshotGraftHTTPPorts struct {
	test     *testing.T
	bound    context.Context
	repo     domain.ContentHash
	target   domain.ContentHash
	parents  []domain.ContentHash
	expected uint64
	calls    int
	err      error
}

func (ports *snapshotGraftHTTPPorts) WithinSnapshotGraftWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	ports.test.Helper()
	if actor, system := inbound.RepositoryActor(ctx); actor != "actor" || system {
		ports.test.Fatal("graft lost authenticated actor", actor, system)
	}
	ports.bound = ctx
	defer func() { ports.bound = nil }()
	return write(ctx)
}

func (ports *snapshotGraftHTTPPorts) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	ports.test.Helper()
	if ctx != ports.bound {
		ports.test.Fatal("graft read escaped scope")
	}
	return domain.Snapshot{ID: id, RepoID: repo}, nil
}

func (ports *snapshotGraftHTTPPorts) AddGraftParentsCAS(ctx context.Context, repo, id domain.ContentHash, parents []domain.ContentHash, expected uint64) error {
	ports.test.Helper()
	if ctx != ports.bound {
		ports.test.Fatal("graft CAS escaped scope")
	}
	ports.repo, ports.target, ports.parents, ports.expected = repo, id, parents, expected
	ports.calls++
	return ports.err
}

func TestSnapshotGraftHTTPActualOwnerAndWireContract(test *testing.T) {
	repo := domain.HashContent([]byte(test.Name()))
	target := domain.HashContent([]byte("HTTP graft target"))
	parent := domain.HashContent([]byte("HTTP graft parent"))
	for _, scenario := range []struct {
		name   string
		body   string
		err    error
		status int
		calls  int
	}{
		{name: "exact sequence", body: fmt.Sprintf(`{"parents":[%q],"expected_seq":9007199254740993}`, parent), status: http.StatusOK, calls: 1},
		{name: "CAS conflict", body: fmt.Sprintf(`{"parents":[%q],"expected_seq":9007199254740993}`, parent), err: domain.ErrConflict, status: http.StatusConflict, calls: 1},
		{name: "missing sequence", body: fmt.Sprintf(`{"parents":[%q]}`, parent), status: http.StatusUnprocessableEntity},
		{name: "null sequence", body: fmt.Sprintf(`{"parents":[%q],"expected_seq":null}`, parent), status: http.StatusUnprocessableEntity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			backend := &snapshotGraftHTTPBackend{&repositoryRefHTTPBackend{repo: domain.Repo{ID: repo, RepositoryID: "ws_" + strings.Repeat("d", 32)}}}
			identity := &branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: domain.RoleMember}
			server := NewServer(backend, identity)
			ports := &snapshotGraftHTTPPorts{test: test, err: scenario.err}
			commands, err := app.NewSnapshotGraftCommands(app.SnapshotGraftCommandsConfig{Store: ports, Scope: ports})
			if err != nil {
				test.Fatal(err)
			}
			if err := server.SetSnapshotGraftCommands(commands); err != nil {
				test.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+string(repo)+"/snapshots/"+string(target)+"/graft", strings.NewReader(scenario.body))
			request.Header.Set("Authorization", "Bearer graft-session")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != scenario.status || ports.calls != scenario.calls {
				test.Fatalf("status=%d calls=%d body=%s", response.Code, ports.calls, response.Body.String())
			}
			if ports.calls != 0 && (ports.repo != repo || ports.target != target || ports.expected != 9007199254740993 || !reflect.DeepEqual(ports.parents, []domain.ContentHash{parent})) {
				test.Fatal("HTTP changed exact graft input", ports)
			}
		})
	}
}

func TestSnapshotGraftHTTPRejectsNilCommands(test *testing.T) {
	server := &Server{}
	for _, commands := range []app.SnapshotGraftWriter{nil, (*app.SnapshotGraftCommands)(nil)} {
		if err := server.SetSnapshotGraftCommands(commands); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("nil graft commands accepted", err)
		}
	}
}
