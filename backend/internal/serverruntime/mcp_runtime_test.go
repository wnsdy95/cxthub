package serverruntime

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMinimalMCPRuntimeHasActualQueryOwnersAndNoCommandService(test *testing.T) {
	test.Setenv("CXT_POSTGRES_DSN", "")
	test.Setenv("CXT_AUTH", "dev")
	test.Setenv("CXT_PUBLIC_URL", "https://cxthub.example")
	test.Setenv("CXT_MEMORY_READ_REPOS", "")
	test.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "")
	test.Setenv("CXT_LEGACY_BYTE_RANGE_PUBLICATION", "")
	test.Setenv("CXT_LEGACY_DERIVED_BYTE_PUBLICATION", "")
	runtime, err := OpenMCP(context.Background(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Context != nil || runtime.RepositoryAssets != nil {
		test.Fatal("MCP constructed a command service")
	}
	if runtime.Archive == nil || runtime.GitChanges == nil || runtime.GitScans == nil || runtime.CodeApplicability == nil || runtime.ConversationFind == nil || runtime.ContextQueries == nil {
		test.Fatal("MCP omitted a query dependency")
	}
	if _, actual := runtime.DocumentQueries.(*documentquery.Queries); !actual {
		test.Fatal("document reader is not the actual owner")
	}
	if _, actual := runtime.MemoryLive.(*memoryquery.LiveQueries); !actual {
		test.Fatal("memory reader is not the actual owner")
	}
	for _, query := range []any{runtime.Archive, runtime.DocumentQueries, runtime.ContextQueries, runtime.MemoryLive, runtime.PreparedMemory, runtime.MemorySnapshots, runtime.GitChanges, runtime.GitScans, runtime.CodeApplicability, runtime.ConversationFind} {
		if _, writable := query.(inbound.CommitSnapshot); writable {
			test.Fatal("MCP query retains a context command service")
		}
		if _, demand := query.(inbound.ConversationFindDemand); demand {
			test.Fatal("MCP query exposes local conversation preparation writes")
		}
	}
}
