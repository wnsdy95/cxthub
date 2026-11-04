package serverruntime

import (
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestContextBranchRuntimeActualOwnerFactoryAndCapturedFS(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	requirePRCoordinationRuntime(test, err == nil, err)
	defer runtime.Close()
	originalStore, actualFS := runtime.Store.(*store.FSStore)
	requirePRCoordinationRuntime(test, actualFS, "context branch fixture is not actual FS")
	opened, actual := runtime.ContextBranches.(*app.ContextBranchOperations)
	requirePRCoordinationRuntime(test, actual && opened != nil, "API omitted actual context branch owner")
	requirePRCoordinationRuntime(test, runtime.ConfigureContextBranchOperations() == nil, "configure context branches")
	configured, actual := runtime.ContextBranches.(*app.ContextBranchOperations)
	requirePRCoordinationRuntime(test, actual && configured != nil, "configuration returned a branch facade")
	service := runtime.Context
	factory, err := service.ContextBranchOperations()
	factoryOwner, actual := any(factory).(*app.ContextBranchOperations)
	requirePRCoordinationRuntime(test, err == nil && actual && factoryOwner != nil, "factory omitted actual owner", err)
	reflect.ValueOf(service).Elem().SetZero()
	runtime.Context = nil
	ctx := inbound.WithSystemActor(test.Context())
	for name, owner := range map[string]*app.ContextBranchOperations{"opened": opened, "configured": configured, "factory": factoryOwner} {
		repository := domain.Repository{ID: domain.NewID("ws_"), OwnerID: domain.NewID("user_"), Slug: name, Name: name}
		requirePRCoordinationRuntime(test, originalStore.CreateRepository(ctx, repository) == nil, "create original repository")
		repo := domain.HashContent([]byte(test.Name() + name))
		_, err := originalStore.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main"})
		requirePRCoordinationRuntime(test, err == nil, err)
		snapshots := map[string]domain.ContentHash{}
		for _, position := range []string{"root", "head", "source", "tip"} {
			document := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: name + position}}}}}}
			raw, err := domain.CanonicalBytes(document.CIR)
			requirePRCoordinationRuntime(test, err == nil, err)
			document.Hash = domain.HashContent(raw)
			_, err = originalStore.PutDoc(ctx, repo, document)
			requirePRCoordinationRuntime(test, err == nil, err)
			snapshots[position] = document.Hash
			var parents []domain.ContentHash
			if position != "root" {
				parents = []domain.ContentHash{snapshots["root"]}
			}
			if position == "tip" {
				parents = []domain.ContentHash{snapshots["source"]}
			}
			requirePRCoordinationRuntime(test, originalStore.PutSnapshot(ctx, domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash, Branch: "main", Parents: parents, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}) == nil, "seed original snapshot")
		}
		requirePRCoordinationRuntime(test, originalStore.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snapshots["head"]}, "") == nil, "seed main")
		requirePRCoordinationRuntime(test, originalStore.AddGraftParents(ctx, repo, snapshots["head"], []domain.ContentHash{snapshots["tip"]}) == nil, "attach session")
		preview, err := owner.PreviewJoin(ctx, inbound.JoinPreviewInput{ActorID: repository.OwnerID, RepoID: repo, Snapshot: snapshots["source"]})
		requirePRCoordinationRuntime(test, err == nil && preview.Reason == "" && preview.ExpectedHead == snapshots["head"] && preview.Tip == snapshots["tip"] && preview.Descendants == 1, "captured preview lost graph/access", preview, err)
		joined, err := owner.ConfirmJoin(ctx, inbound.ConfirmJoinInput{ActorID: repository.OwnerID, JoinInput: inbound.JoinInput{RepoID: repo, Snapshot: snapshots["source"], TargetBranch: preview.Branch, BranchID: preview.BranchID, ExpectedHead: preview.ExpectedHead, PlanRevision: preview.AllRevision, IncludeDescendants: true}})
		requirePRCoordinationRuntime(test, err == nil && joined.Head == snapshots["tip"], "captured join lost write/pending owner", joined, err)
		source, err := originalStore.GetSnapshot(ctx, repo, snapshots["source"])
		requirePRCoordinationRuntime(test, err == nil && reflect.DeepEqual(source.Parents, []domain.ContentHash{snapshots["root"]}) && reflect.DeepEqual(source.GraftParents, []domain.ContentHash{snapshots["head"]}), "join changed natural history", source, err)
		for _, branch := range []string{"legacy/fork", "protocol/fork"} {
			if branch == "protocol/fork" {
				requirePRCoordinationRuntime(test, owner.EnableContextProtocol(ctx, repo) == nil, "captured protocol port lost original work")
			}
			forked, err := owner.Fork(ctx, inbound.ForkInput{RepoID: repo, FromSnapshot: snapshots["tip"], NewBranch: branch})
			requirePRCoordinationRuntime(test, err == nil && forked.Branch == branch && forked.Head == snapshots["tip"], "captured fork failed", forked, err)
			ref, err := originalStore.GetRef(ctx, repo, domain.RefBranch, branch)
			requirePRCoordinationRuntime(test, err == nil && ref.Target == snapshots["tip"], "fork missed original FS store", ref, err)
		}
		events, err := owner.ListHistory(ctx, repo)
		stored, storeErr := originalStore.ListHistoryEvents(ctx, repo)
		requirePRCoordinationRuntime(test, err == nil && storeErr == nil && len(events) == 1 && reflect.DeepEqual(events, stored) && events[0].Kind == "birth" && events[0].Branch == "protocol/fork" && events[0].BranchID == events[0].ID, "captured recorder/history did not retain actual immutable birth", events, err, storeErr)
	}
	runtime.Context, runtime.ContextBranches = service, configured
	err = runtime.ConfigureContextBranchOperations()
	requirePRCoordinationRuntime(test, err != nil && runtime.ContextBranches == nil, "failed factory retained stale owner", err)
}

func TestContextBranchRuntimeMissingContextClearsStaleOwner(test *testing.T) {
	for _, runtime := range []*Runtime{nil, {}} {
		if runtime != nil {
			runtime.ContextBranches = &app.ContextBranchOperations{}
		}
		err := runtime.ConfigureContextBranchOperations()
		requirePRCoordinationRuntime(test, errors.Is(err, domain.ErrCatalogUnsupported) && (runtime == nil || runtime.ContextBranches == nil), "missing Context retained branch owner or lost current unsupported error", err)
	}
}

func TestContextBranchRuntimeMCPHasNoOwner(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	requirePRCoordinationRuntime(test, err == nil, err)
	defer runtime.Close()
	requirePRCoordinationRuntime(test, runtime.Context == nil && runtime.ContextBranches == nil, "MCP assembled context branch commands")
	for _, query := range []any{runtime.Archive, runtime.DocumentQueries, runtime.ContextQueries, runtime.MemoryLive, runtime.PreparedMemory, runtime.MemorySnapshots, runtime.GitChanges, runtime.GitScans, runtime.CodeApplicability, runtime.ConversationFind} {
		requirePRCoordinationRuntime(test, query != nil, "MCP omitted query owner")
		_, writable := query.(inbound.ContextBranchOperations)
		requirePRCoordinationRuntime(test, !writable, "MCP query exposes context branch operations")
	}
	runtime.ContextBranches = &app.ContextBranchOperations{}
	err = runtime.ConfigureContextBranchOperations()
	requirePRCoordinationRuntime(test, errors.Is(err, domain.ErrCatalogUnsupported) && runtime.ContextBranches == nil, "MCP retained writable branch owner", err)
}
