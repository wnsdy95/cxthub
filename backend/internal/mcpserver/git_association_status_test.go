package mcpserver

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMCPRejectsMissingGitAssociationStatusQuery(test *testing.T) {
	for name, construct := range wave58DocumentPublicationMCPHandlers() {
		test.Run(name, func(test *testing.T) {
			runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
			wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
			if _, actual := runtime.GitAssociationStatus.(*app.GitAssociationStatusQueries); !actual {
				test.Fatalf("MCP did not bind the standalone status query: %T", runtime.GitAssociationStatus)
			}
			if _, writable := runtime.GitAssociationStatus.(inbound.CommitSnapshot); writable {
				test.Fatal("MCP retained a command service for status reads")
			}
			for _, missing := range []inbound.GitAssociationStatusQuery{nil, (*app.GitAssociationStatusQueries)(nil)} {
				runtime.GitAssociationStatus = missing
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("MCP accepted a missing Git association status query")
				}
			}
		})
	}
}
