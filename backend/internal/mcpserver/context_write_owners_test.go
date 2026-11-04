package mcpserver

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
)

func TestMCPRejectsContextWriteOwners(test *testing.T) {
	var nilMemory *app.MemoryDigestCommands
	var nilGraft *app.SnapshotGraftCommands
	for _, injection := range []struct {
		name   string
		memory app.MemoryDigestWriter
		graft  app.SnapshotGraftWriter
	}{
		{name: "memory", memory: &app.MemoryDigestCommands{}},
		{name: "typed nil memory", memory: nilMemory},
		{name: "graft", graft: &app.SnapshotGraftCommands{}},
		{name: "typed nil graft", graft: nilGraft},
	} {
		for handlerName, construct := range wave58DocumentPublicationMCPHandlers() {
			test.Run(injection.name+"/"+handlerName, func(test *testing.T) {
				runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
				runtime.MemoryDigests, runtime.SnapshotGrafts = injection.memory, injection.graft
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("MCP accepted a context write owner", err)
				}
				runtime.MemoryDigests, runtime.SnapshotGrafts = nil, nil
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
			})
		}
	}
}
