package serverruntime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestRepositoryRefRuntimeUsesActualIndependentOwnerAndOriginalFSStore(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if _, actualFS := runtime.Store.(*store.FSStore); !actualFS {
		test.Fatalf("ref runtime fixture did not open actual FS storage: %T", runtime.Store)
	}
	if owner, actual := runtime.RepositoryRefs.(*app.RepositoryRefCommands); !actual || owner == nil {
		test.Fatalf("API did not construct the actual independent ref owner: %T", runtime.RepositoryRefs)
	}
	runtime.RepositoryRefs = nil
	if err := runtime.ConfigureRepositoryRefCommands(); err != nil {
		test.Fatal("explicit ref command configuration failed", err)
	}
	owner, actual := runtime.RepositoryRefs.(*app.RepositoryRefCommands)
	if !actual || owner == nil {
		test.Fatalf("configuration returned a facade instead of actual ref commands: %T", runtime.RepositoryRefs)
	}
	originalStore := runtime.Store
	ctx := inbound.WithSystemActor(context.Background())
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := originalStore.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	first, second := domain.HashContent([]byte("runtime first snapshot")), domain.HashContent([]byte("runtime second snapshot"))
	for _, snapshot := range []domain.Snapshot{
		{ID: first, RepoID: repo, DocHash: first, Branch: "main"},
		{ID: second, RepoID: repo, DocHash: second, Branch: "main", Parents: []domain.ContentHash{first}},
	} {
		if err := originalStore.PutSnapshot(ctx, snapshot); err != nil {
			test.Fatal(err)
		}
	}
	runtime.Context = nil
	initial := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: first}
	output, err := owner.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: initial})
	if err != nil || output.Result != inbound.RefFastForward || output.ServerTarget != first || output.RequestedTarget != first || !reflect.DeepEqual(output.Ref, initial) {
		test.Fatal("single actual ref command retained a parent Service dependency", output, err)
	}
	stored, err := originalStore.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || stored.Target != first {
		test.Fatal("single ref owner did not use its original FS store", stored, err)
	}
	next := initial
	next.Target = second
	tag := domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "release/runtime", Target: second}
	batch, err := owner.UpdateRefs(ctx, inbound.UpdateRefsInput{RepoID: repo, Updates: []inbound.UpdateRefInput{{Ref: next}, {Ref: tag}}})
	if err != nil || batch != (inbound.UpdateRefsOutput{Applied: 2}) {
		test.Fatal("batch actual ref command retained a parent Service dependency", batch, err)
	}
	for _, expected := range []domain.Ref{next, tag} {
		stored, err := originalStore.GetRef(ctx, repo, expected.Kind, expected.Name)
		if err != nil || stored.RepoID != repo || stored.Target != second {
			test.Fatal("batch ref owner did not mutate its original FS store", stored, err)
		}
	}
	output, err = owner.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: next, ExpectedTarget: second})
	if err != nil || output.Result != inbound.RefUpToDate {
		test.Fatal("actual ref owner lost same-store idempotency", output, err)
	}
	batch, err = owner.UpdateRefs(ctx, inbound.UpdateRefsInput{RepoID: repo})
	if err != nil || batch != (inbound.UpdateRefsOutput{}) {
		test.Fatal("actual ref owner lost empty-batch recovery", batch, err)
	}
}

func TestRepositoryRefRuntimeConfigurationClearsIncompleteBindings(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		runtime func() *Runtime
	}{
		{name: "nil runtime", runtime: func() *Runtime { return nil }},
		{name: "missing Context", runtime: func() *Runtime { return &Runtime{} }},
		{name: "incomplete Service", runtime: func() *Runtime { return &Runtime{Context: app.NewService(nil, nil, nil, nil, nil)} }},
	} {
		for _, stale := range []bool{false, true} {
			name := fixture.name + "/unbound"
			if stale {
				name = fixture.name + "/stale owner"
			}
			test.Run(name, func(test *testing.T) {
				runtime := fixture.runtime()
				if runtime != nil && stale {
					runtime.RepositoryRefs = &app.RepositoryRefCommands{}
				}
				err := runtime.ConfigureRepositoryRefCommands()
				if err == nil || runtime != nil && runtime.RepositoryRefs != nil {
					test.Fatal("failed runtime configuration retained or published a ref owner", err)
				}
				if (runtime == nil || runtime.Context == nil) && !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("missing runtime Context lost its unsupported error", err)
				}
			})
		}
	}
}

type repositoryRefRuntimeNoIO struct {
	app.RepositoryRefStore
	app.RepositoryRefEngine
	app.RepositoryRefWriteScope
	app.RepositoryRefHistory
	app.RepositoryRefAppend
	app.RepositoryRefRevisions
	app.RepositoryRefNotifications
}

func repositoryRefRuntimeCompleteConfig() app.RepositoryRefCommandsConfig {
	dependencies := &repositoryRefRuntimeNoIO{}
	return app.RepositoryRefCommandsConfig{
		Profile: app.RepositoryRefComplete, Store: dependencies, Engine: dependencies, Scope: dependencies,
		History: dependencies, Append: dependencies, Revisions: dependencies, Notifications: dependencies,
		Transactional: true,
	}
}

func TestRepositoryRefCompleteConfigurationRejectsMissingCapabilitiesBeforeIO(test *testing.T) {
	for _, missing := range []string{"Store", "Scope", "Engine", "History", "Append", "Revisions", "Notifications"} {
		test.Run(missing, func(test *testing.T) {
			config := repositoryRefRuntimeCompleteConfig()
			reflect.ValueOf(&config).Elem().FieldByName(missing).SetZero()
			owner, err := app.NewRepositoryRefCommands(config)
			expected := domain.ErrCatalogUnsupported
			if missing == "Store" || missing == "Scope" {
				expected = domain.ErrValidation
			}
			if owner != nil || !errors.Is(err, expected) {
				test.Fatal("incomplete ref owner did not fail closed before IO", owner, err)
			}
		})
	}
	for _, profile := range []app.RepositoryRefCommandsConfig{{Profile: app.RepositoryRefCompatibility}, {Profile: app.RepositoryRefComplete}} {
		for _, dependency := range []string{"Store", "Scope", "Engine", "History", "Append", "Revisions", "Notifications"} {
			test.Run(string(profile.Profile)+"/typed nil "+dependency, func(test *testing.T) {
				config := repositoryRefRuntimeCompleteConfig()
				config.Profile = profile.Profile
				var typedNil *repositoryRefRuntimeNoIO
				reflect.ValueOf(&config).Elem().FieldByName(dependency).Set(reflect.ValueOf(typedNil))
				owner, err := app.NewRepositoryRefCommands(config)
				if owner != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("typed-nil capability was treated as supported", owner, err)
				}
			})
		}
	}
	config := repositoryRefRuntimeCompleteConfig()
	config.Profile = "unknown"
	if owner, err := app.NewRepositoryRefCommands(config); owner != nil || !errors.Is(err, domain.ErrValidation) {
		test.Fatal("unknown ref command profile was accepted", owner, err)
	}
	if owner, err := app.NewRepositoryRefCommands(repositoryRefRuntimeCompleteConfig()); err != nil || owner == nil {
		test.Fatal("complete ref constructor required IO to assemble valid capabilities", owner, err)
	}
}

func TestRepositoryRefRuntimeMCPNeverConstructsCommands(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Context != nil || runtime.RepositoryRefs != nil {
		test.Fatal("MCP assembled a reference command dependency")
	}
	for _, query := range []any{runtime.Archive, runtime.DocumentQueries, runtime.ContextQueries, runtime.MemoryLive, runtime.PreparedMemory, runtime.MemorySnapshots, runtime.GitChanges, runtime.GitScans, runtime.CodeApplicability, runtime.ConversationFind} {
		if query == nil {
			test.Fatal("MCP omitted a required query dependency")
		}
		if _, writable := query.(inbound.RepositoryRefCommands); writable {
			test.Fatalf("MCP query owner exposes ref commands: %T", query)
		}
	}
	for _, query := range []any{runtime.RepositoryStates, runtime.RepositoryGraphs} {
		if _, writable := query.(inbound.RepositoryRefCommands); writable {
			test.Fatalf("MCP repository query owner exposes ref commands: %T", query)
		}
	}
	for _, stale := range []bool{false, true} {
		if stale {
			runtime.RepositoryRefs = &app.RepositoryRefCommands{}
		}
		if err := runtime.ConfigureRepositoryRefCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.RepositoryRefs != nil {
			test.Fatal("query-only runtime retained or obtained ref commands", err)
		}
	}
}
