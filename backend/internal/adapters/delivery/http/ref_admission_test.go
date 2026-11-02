package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type admittedRefScope struct {
	app.RepositoryRefWriteScope
	calls int
}

func (scope *admittedRefScope) WithinRefWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.UpdateRefOutput, error)) (inbound.UpdateRefOutput, error) {
	scope.calls++
	_, release, err := app.AcquireResources(ctx, app.ResourceInteractive)
	if err != nil {
		return inbound.UpdateRefOutput{}, err
	}
	defer release()
	return write(ctx)
}

func (*admittedRefScope) NotifyRefUpdate(context.Context, domain.ContentHash, domain.Ref, bool, bool) error {
	return nil
}

type admittedRefStore struct {
	app.RepositoryRefStore
	target  domain.ContentHash
	lookups int
	swaps   int
}

func (*admittedRefStore) GetRepo(_ context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{ID: repo}, nil
}

func (*admittedRefStore) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	panic("interactive tag admission materialized a snapshot")
}

func (storage *admittedRefStore) HasOrdinaryTagTarget(_ context.Context, repo, target domain.ContentHash) (bool, error) {
	storage.lookups++
	return target == storage.target, nil
}

func (*admittedRefStore) GetRef(context.Context, domain.ContentHash, domain.RefKind, string) (domain.Ref, error) {
	return domain.Ref{}, domain.ErrNotFound
}

func (storage *admittedRefStore) CompareAndSwapRef(_ context.Context, repo domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	if expected != "" || ref.Kind != domain.RefTag || ref.Target != storage.target {
		return domain.ErrRefConflict
	}
	storage.swaps++
	return nil
}

func TestResourceAdmissionHTTPTagWritesDoNotWaitForBulk(test *testing.T) {
	_, release, err := app.AcquireResources(test.Context(), app.ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	repo, target := domain.HashContent([]byte("admitted-repo")), domain.HashContent([]byte("admitted-target"))
	body := fmt.Sprintf(`{"target":%q}`, target)
	for _, scenario := range []struct {
		name       string
		kind       domain.RefKind
		ref        string
		body       string
		filesystem bool
		status     int
		calls      int
	}{
		{name: "ordinary", kind: domain.RefTag, ref: "release", body: body, status: http.StatusOK, calls: 1},
		{name: "append", kind: domain.RefTag, ref: "release", body: fmt.Sprintf(`{"target":%q,"append":true}`, target), status: http.StatusOK, calls: 1},
		{name: "force", kind: domain.RefTag, ref: "release", body: fmt.Sprintf(`{"target":%q,"force":true}`, target), status: http.StatusOK, calls: 1},
		{name: "branch", kind: domain.RefBranch, ref: "main", body: body, status: http.StatusServiceUnavailable},
		{name: "lifecycle", kind: domain.RefTag, ref: domain.BranchLifecycleTagPrefix + "reserved", body: body, status: http.StatusServiceUnavailable},
		{name: "history", kind: domain.RefTag, ref: "cxt/history/v1/reserved", body: body, status: http.StatusServiceUnavailable},
		{name: "filesystem", kind: domain.RefTag, ref: "release", body: body, filesystem: true, status: http.StatusServiceUnavailable},
		{name: "oversize", kind: domain.RefTag, ref: "release", body: `{"symbolic":"` + strings.Repeat("x", 16<<10) + `"}`, status: http.StatusRequestEntityTooLarge},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage := &admittedRefStore{target: target}
			scope := &admittedRefScope{}
			config := app.RepositoryRefCommandsConfig{Profile: app.RepositoryRefCompatibility, Store: storage, Scope: scope, Notifications: scope}
			if scenario.filesystem {
				config.Store = store.NewFSStore(test.TempDir())
			}
			commands, err := app.NewRepositoryRefCommands(config)
			if err != nil {
				test.Fatal(err)
			}
			if commands.SupportsBoundedTagWrites() == scenario.filesystem {
				test.Fatal("owner did not preserve the store's bounded tag capability")
			}
			server := &Server{repositoryRefs: commands}
			request := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(scenario.body))
			request.Header.Set("Content-Type", "application/json")
			request.SetPathValue("repoID", string(repo))
			request.SetPathValue("kind", string(scenario.kind))
			request.SetPathValue("name", scenario.ref)
			response := httptest.NewRecorder()
			server.putRef(response, request)
			if response.Code != scenario.status || scope.calls != scenario.calls || storage.lookups != scenario.calls || storage.swaps != scenario.calls {
				test.Fatalf("tag admission: status=%d scope=%d presence=%d cas=%d body=%s", response.Code, scope.calls, storage.lookups, storage.swaps, response.Body)
			}
		})
	}
}
