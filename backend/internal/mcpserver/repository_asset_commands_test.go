package mcpserver

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

func TestMCPRejectsRuntimeContainingRepositoryAssetCommands(test *testing.T) {
	for name, value := range map[string]string{
		"CXT_POSTGRES_DSN": "", "CXT_AUTH": "dev", "CXT_PUBLIC_URL": "https://cxthub.example",
		"CXT_MEMORY_READ_REPOS": "", "CXT_CONVERSATION_ROOT_PUBLICATION": "false",
		"CXT_LEGACY_BYTE_RANGE_PUBLICATION": "false", "CXT_LEGACY_DERIVED_BYTE_PUBLICATION": "false",
	} {
		test.Setenv(name, value)
	}
	runtime, err := serverruntime.OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if handler, err := Handler(runtime, "https://mcp.example.test"); err != nil || handler == nil {
		test.Fatal("the independent query runtime must be a working positive control", err)
	}
	runtime.RepositoryAssets = &app.RepositoryAssetCommands{}
	if handler, err := Handler(runtime, "https://mcp.example.test"); err == nil || handler != nil {
		test.Fatal("MCP retained a write-side repository asset dependency")
	}
}
