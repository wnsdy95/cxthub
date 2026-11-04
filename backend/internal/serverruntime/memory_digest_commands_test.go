package serverruntime

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMemoryDigestRuntimeActualOwner(test *testing.T) {
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
	opened, ok := runtime.MemoryDigests.(*app.MemoryDigestCommands)
	if !ok || opened == nil {
		test.Fatalf("runtime omitted actual owner: %T", runtime.MemoryDigests)
	}
	if err := runtime.ConfigureMemoryDigestCommands(); err != nil {
		test.Fatal(err)
	}
	configured, ok := runtime.MemoryDigests.(*app.MemoryDigestCommands)
	if !ok || configured == nil {
		test.Fatalf("runtime configured a facade: %T", runtime.MemoryDigests)
	}
	*runtime.Context = app.Service{}
	runtime.Context = nil
	ctx := inbound.WithSystemActor(test.Context())
	for name, commands := range map[string]*app.MemoryDigestCommands{"opened": opened, "configured": configured} {
		repo, target := domain.HashContent([]byte(test.Name()+name)), domain.HashContent([]byte("memory target"))
		if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo, DocHash: target}); err != nil {
			test.Fatal(err)
		}
		hash, err := commands.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: target, Summary: "owned"})
		if err != nil || hash == "" {
			test.Fatal("owner retained Service dependency", hash, err)
		}
		actual, err := storage.GetSnapshot(ctx, repo, target)
		if err != nil || actual.MemoryHash != hash {
			test.Fatal(actual, err)
		}
	}
	if err := runtime.ConfigureMemoryDigestCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.MemoryDigests != nil {
		test.Fatal("failed reconfiguration retained stale commands", err, runtime.MemoryDigests)
	}
	if err := (*Runtime)(nil).ConfigureMemoryDigestCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("nil runtime accepted", err)
	}
}

func TestMemoryDigestRuntimeMCPExcludesCommands(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Context != nil || runtime.MemoryDigests != nil {
		test.Fatal("MCP assembled memory digest writer")
	}
	if err := runtime.ConfigureMemoryDigestCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.MemoryDigests != nil {
		test.Fatal("MCP accepted memory digest command configuration", err)
	}
}
