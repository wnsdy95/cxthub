package mcpserver

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestMCPRejectsPRCoordinationActualOwnerAndTypedNil(test *testing.T) {
	var typedNil *app.PRPromotionCommands
	for _, injection := range []struct {
		name     string
		commands inbound.PRPromotionCommands
	}{
		{"actual owner", &app.PRPromotionCommands{}},
		{"typed nil owner", typedNil},
	} {
		for handlerName, construct := range wave58DocumentPublicationMCPHandlers() {
			test.Run(injection.name+"/"+handlerName, func(test *testing.T) {
				runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
				if runtime.PRPromotions != nil {
					test.Fatal("read-only positive control assembled PR commands")
				}
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
				runtime.PRPromotions = injection.commands
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("MCP accepted PR command field", err)
				}
				runtime.PRPromotions = nil
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
			})
		}
	}
}
