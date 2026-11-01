package mcpserver

import (
	"net/http"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/serverruntime"
)

func wave58DocumentPublicationValidReadOnlyRuntime(test *testing.T) *serverruntime.Runtime {
	test.Helper()
	for name, value := range map[string]string{
		"CXT_POSTGRES_DSN": "", "CXT_AUTH": "dev", "CXT_FIREBASE_PROJECT": "",
		"CXT_PUBLIC_URL": "https://cxthub.example", "CXT_MEMORY_READ_REPOS": "",
		"CXT_CONVERSATION_ROOT_PUBLICATION": "false", "CXT_LEGACY_BYTE_RANGE_PUBLICATION": "false",
		"CXT_LEGACY_DERIVED_BYTE_PUBLICATION": "false", "CXT_EXECUTION_PROFILE": "embedded",
	} {
		test.Setenv(name, value)
	}
	runtime, err := serverruntime.OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(runtime.Close)
	if runtime.Context != nil || runtime.RepositoryAssets != nil || runtime.DocumentPublications != nil || runtime.RepositoryRefs != nil {
		test.Fatal("positive control is not an independently assembled read-only runtime")
	}
	return runtime
}

func wave58DocumentPublicationMCPHandlers() map[string]func(*serverruntime.Runtime) (http.Handler, error) {
	return map[string]func(*serverruntime.Runtime) (http.Handler, error){
		"Handler": func(runtime *serverruntime.Runtime) (http.Handler, error) {
			return Handler(runtime, "https://mcp.example.test")
		},
		"HandlerWithMemoryPreparation": func(runtime *serverruntime.Runtime) (http.Handler, error) {
			return HandlerWithMemoryPreparation(runtime, "https://mcp.example.test", nil)
		},
		"HandlerWithReadPreparation": func(runtime *serverruntime.Runtime) (http.Handler, error) {
			return HandlerWithReadPreparation(runtime, "https://mcp.example.test", nil, nil)
		},
	}
}

func wave58DocumentPublicationValidReadOnlyHandler(test *testing.T, runtime *serverruntime.Runtime, construct func(*serverruntime.Runtime) (http.Handler, error)) {
	test.Helper()
	if handler, err := construct(runtime); err != nil || handler == nil {
		test.Fatal("real read-only runtime must be a valid handler before testing the guard", err)
	}
}

func TestMCPRejectsDocumentPublicationCommandsIncludingTypedNil(test *testing.T) {
	var typedNil *app.DocumentPublicationCommands
	for _, injection := range []struct {
		name     string
		commands inbound.DocumentPublicationCommands
	}{
		{name: "actual owner", commands: &app.DocumentPublicationCommands{}},
		{name: "typed nil owner", commands: typedNil},
	} {
		for handlerName, construct := range wave58DocumentPublicationMCPHandlers() {
			test.Run(injection.name+"/"+handlerName, func(test *testing.T) {
				runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
				runtime.DocumentPublications = injection.commands
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("MCP accepted a nonnil publication command field", err)
				}
				runtime.DocumentPublications = nil
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
			})
		}
	}
}

func TestMCPDocumentPublicationGuardDoesNotMaskOtherRequiredDependencies(test *testing.T) {
	for _, missing := range []struct {
		name  string
		apply func(*serverruntime.Runtime)
	}{
		{name: "archive", apply: func(runtime *serverruntime.Runtime) { runtime.Archive = nil }},
		{name: "git changes", apply: func(runtime *serverruntime.Runtime) { runtime.GitChanges = nil }},
		{name: "git scans", apply: func(runtime *serverruntime.Runtime) { runtime.GitScans = nil }},
		{name: "code applicability", apply: func(runtime *serverruntime.Runtime) { runtime.CodeApplicability = nil }},
		{name: "conversation find", apply: func(runtime *serverruntime.Runtime) { runtime.ConversationFind = nil }},
		{name: "Context command service", apply: func(runtime *serverruntime.Runtime) { runtime.Context = &app.Service{} }},
		{name: "repository asset commands", apply: func(runtime *serverruntime.Runtime) { runtime.RepositoryAssets = &app.RepositoryAssetCommands{} }},
	} {
		for handlerName, construct := range wave58DocumentPublicationMCPHandlers() {
			test.Run(missing.name+"/"+handlerName, func(test *testing.T) {
				runtime := wave58DocumentPublicationValidReadOnlyRuntime(test)
				wave58DocumentPublicationValidReadOnlyHandler(test, runtime, construct)
				missing.apply(runtime)
				if runtime.DocumentPublications != nil {
					test.Fatal("document publication rejection would mask the required dependency under test")
				}
				if handler, err := construct(runtime); err == nil || handler != nil {
					test.Fatal("document publication guard weakened another required MCP dependency", err)
				}
			})
		}
	}
}
