package mcpserver

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMCPRejectsContextBranchActualOwnerAndTypedNil(test *testing.T) {
	for _, injection := range []struct {
		name  string
		owner inbound.ContextBranchOperations
	}{
		{"actual owner", &app.ContextBranchOperations{}},
		{"typed nil owner", (*app.ContextBranchOperations)(nil)},
	} {
		for handlerName, construct := range wave58DocumentPublicationMCPHandlers() {
			test.Run(injection.name+"/"+handlerName, func(test *testing.T) {
				runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
				if runtime.ContextBranches != nil {
					test.Fatal("read-only positive control assembled branch commands")
				}
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
				runtime.ContextBranches = injection.owner
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("MCP accepted actual or typed-nil context branch owner", err)
				}
				runtime.ContextBranches = nil
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
			})
		}
	}
}
