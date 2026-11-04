package serverruntime

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestSnapshotGraftRuntimeActualOwner(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	storage, ok := runtime.Store.(*store.FSStore)
	if !ok {
		test.Fatalf("expected FS fixture, got %T", runtime.Store)
	}
	opened, ok := runtime.SnapshotGrafts.(*app.SnapshotGraftCommands)
	if !ok || opened == nil {
		test.Fatalf("runtime omitted actual owner: %T", runtime.SnapshotGrafts)
	}
	if err := runtime.ConfigureSnapshotGraftCommands(); err != nil {
		test.Fatal(err)
	}
	configured, ok := runtime.SnapshotGrafts.(*app.SnapshotGraftCommands)
	if !ok || configured == nil {
		test.Fatalf("runtime configured a facade: %T", runtime.SnapshotGrafts)
	}
	*runtime.Context = app.Service{}
	runtime.Context = nil
	ctx := inbound.WithSystemActor(test.Context())
	for name, commands := range map[string]*app.SnapshotGraftCommands{"opened": opened, "configured": configured} {
		repo := domain.HashContent([]byte(test.Name() + name))
		target, parent := domain.HashContent([]byte("target")), domain.HashContent([]byte("parent"))
		for _, id := range []domain.ContentHash{target, parent} {
			if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id, Branch: "main", Provider: "claude"}); err != nil {
				test.Fatal(err)
			}
		}
		if err := commands.GraftSnapshotParents(ctx, repo, target, []domain.ContentHash{parent}, 0); err != nil {
			test.Fatal("owner retained Service dependency", err)
		}
		actual, err := storage.GetSnapshot(ctx, repo, target)
		if err != nil || actual.GraftSeq != 1 || len(actual.GraftParents) != 1 || actual.GraftParents[0] != parent {
			test.Fatal(actual, err)
		}
	}
	if err := runtime.ConfigureSnapshotGraftCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.SnapshotGrafts != nil {
		test.Fatal("failed reconfiguration retained stale commands", err, runtime.SnapshotGrafts)
	}
}

func TestSnapshotGraftRuntimeMCPExcludesCommands(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Context != nil || runtime.SnapshotGrafts != nil {
		test.Fatal("MCP assembled graft writer")
	}
}
