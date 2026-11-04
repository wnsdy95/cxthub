package serverruntime

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestHistoryCommandsRuntimeActualFactoryAndOriginalFSStore(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	originalStore, actualFS := runtime.Store.(*store.FSStore)
	if !actualFS {
		test.Fatalf("history fixture did not open original FS storage: %T", runtime.Store)
	}
	if owner, actual := runtime.HistoryCommands.(*app.HistoryCommands); !actual || owner == nil {
		test.Fatalf("API did not assemble the actual history owner: %T", runtime.HistoryCommands)
	}
	runtime.HistoryCommands = nil
	if err := runtime.ConfigureHistoryCommands(); err != nil {
		test.Fatal(err)
	}
	owner, actual := runtime.HistoryCommands.(*app.HistoryCommands)
	if !actual || owner == nil {
		test.Fatalf("configuration returned a history facade: %T", runtime.HistoryCommands)
	}
	service := runtime.Context
	factoryCommands, err := service.HistoryCommands()
	factoryOwner, actual := any(factoryCommands).(*app.HistoryCommands)
	if err != nil || !actual || factoryOwner == nil {
		test.Fatalf("Service factory did not return the actual history owner: %T %v", factoryCommands, err)
	}
	ctx := inbound.WithSystemActor(test.Context())
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := originalStore.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events:   []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "independent history"}}}},
	}}
	raw, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(raw)
	if _, err := originalStore.PutDoc(ctx, repo, document); err != nil {
		test.Fatal(err)
	}
	if err := originalStore.PutSnapshot(ctx, domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	event := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "main-identity", Branch: "main",
		Kind: "position", Source: document.Hash, Target: document.Hash, SharedTarget: document.Hash, CreatedAt: time.Unix(1720000000, 0).UTC()}
	*service = app.Service{}
	runtime.Context = nil
	for _, commands := range []*app.HistoryCommands{owner, factoryOwner} {
		if err := commands.RecordHistory(ctx, event); err != nil {
			test.Fatal("independent owner retained a Service or lost FS replay", err)
		}
	}
	changed := event
	changed.LocalBranch = "different"
	if err := owner.RecordHistory(ctx, changed); !errors.Is(err, domain.ErrRefConflict) {
		test.Fatal("actual owner lost immutable event conflict", err)
	}
	rows, err := originalStore.ListHistoryEvents(ctx, repo)
	if err != nil || !reflect.DeepEqual(rows, []domain.HistoryEvent{event}) {
		test.Fatal("actual history owner did not retain original FS state", rows, err)
	}
}

func TestHistoryCommandsRuntimeConfigurationClearsStaleOwners(test *testing.T) {
	for name, runtime := range map[string]*Runtime{
		"nil runtime": nil, "missing Context": {}, "incomplete Service": {Context: app.NewService(nil, nil, nil, nil, nil)},
	} {
		test.Run(name, func(test *testing.T) {
			for _, stale := range []bool{false, true} {
				if runtime != nil && stale {
					runtime.HistoryCommands = &app.HistoryCommands{}
				}
				err := runtime.ConfigureHistoryCommands()
				if err == nil || runtime != nil && runtime.HistoryCommands != nil {
					test.Fatal("failed configuration retained or published history commands", err)
				}
				if (runtime == nil || runtime.Context == nil) && !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("missing command Context lost unsupported error", err)
				}
			}
		})
	}
}

func TestHistoryCommandsRuntimeMCPExcludesCommands(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Context != nil || runtime.HistoryCommands != nil {
		test.Fatal("MCP assembled history commands")
	}
	for _, query := range []any{runtime.Archive, runtime.DocumentQueries, runtime.ContextQueries, runtime.MemoryLive, runtime.PreparedMemory, runtime.MemorySnapshots, runtime.GitChanges, runtime.GitScans, runtime.CodeApplicability, runtime.ConversationFind} {
		if query == nil {
			test.Fatal("MCP omitted a required query owner")
		}
		if _, writable := query.(inbound.HistoryCommands); writable {
			test.Fatalf("MCP query exposes history commands: %T", query)
		}
	}
	runtime.HistoryCommands = &app.HistoryCommands{}
	if err := runtime.ConfigureHistoryCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.HistoryCommands != nil {
		test.Fatal("read-only runtime retained or obtained history commands", err)
	}
}
