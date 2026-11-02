package mcpserver

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMCPRejectsRepositoryRefCommandsIncludingTypedNil(test *testing.T) {
	var typedNil *app.RepositoryRefCommands
	for _, injection := range []struct {
		name     string
		commands inbound.RepositoryRefCommands
	}{
		{name: "actual owner", commands: &app.RepositoryRefCommands{}},
		{name: "typed nil owner", commands: typedNil},
	} {
		for handlerName, construct := range wave58DocumentPublicationMCPHandlers() {
			test.Run(injection.name+"/"+handlerName, func(test *testing.T) {
				runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
				runtime.RepositoryRefs = injection.commands
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("MCP accepted a nonnil ref command field", err)
				}
				runtime.RepositoryRefs = nil
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
			})
		}
	}
}
