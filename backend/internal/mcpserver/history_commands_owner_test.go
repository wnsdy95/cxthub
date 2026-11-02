package mcpserver

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMCPRejectsHistoryCommandsIncludingTypedNil(test *testing.T) {
	var typedNil *app.HistoryCommands
	for _, injection := range []struct {
		name     string
		commands inbound.HistoryCommands
	}{
		{name: "actual owner", commands: &app.HistoryCommands{}},
		{name: "typed nil owner", commands: typedNil},
	} {
		for handlerName, construct := range wave58DocumentPublicationMCPHandlers() {
			test.Run(injection.name+"/"+handlerName, func(test *testing.T) {
				runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
				if runtime.HistoryCommands != nil {
					test.Fatal("read-only positive control assembled history commands")
				}
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
				runtime.HistoryCommands = injection.commands
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("MCP accepted a nonnil history command field", err)
				}
				runtime.HistoryCommands = nil
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
			})
		}
	}
}
