package serverruntime

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestDocumentPublicationRuntimeUsesActualIndependentOwnerAndOriginalStore(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if _, localFS := runtime.Store.(*store.FSStore); !localFS {
		test.Fatalf("publication fixture did not open actual local FS storage: %T", runtime.Store)
	}
	if owner, actual := runtime.DocumentPublications.(*app.DocumentPublicationCommands); !actual || owner == nil {
		test.Fatalf("API did not construct the actual document publication owner: %T", runtime.DocumentPublications)
	}
	runtime.DocumentPublications = nil
	if err := runtime.ConfigureDocumentPublicationCommands(); err != nil {
		test.Fatal("explicit document publication configuration failed", err)
	}
	owner, actual := runtime.DocumentPublications.(*app.DocumentPublicationCommands)
	if !actual || owner == nil {
		test.Fatalf("configuration published a facade instead of the actual owner: %T", runtime.DocumentPublications)
	}
	originalStore := runtime.Store
	ctx := inbound.WithSystemActor(context.Background())
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := originalStore.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: domain.NewID("ws_"), DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, GitBranch: "main"},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: "independent document publication runtime"}}}},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		test.Fatal(err)
	}
	document := domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir}
	snapshot := domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: "independent owner"}
	chunk := inbound.ChunkObject{Hash: domain.HashContent([]byte("independent staged chunk")), Data: []byte("independent staged chunk")}
	runtime.Context = nil
	chunkResult, err := owner.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{chunk}})
	if err != nil || chunkResult != (inbound.StoreChunksOutput{Stored: 1}) {
		test.Fatal("actual chunk command retained a parent Service dependency", chunkResult, err)
	}
	input := inbound.CommitInput{RepoID: repo, Docs: []domain.SessionDoc{document}, Snapshots: []domain.Snapshot{snapshot}}
	commitResult, err := owner.Commit(ctx, input)
	if err != nil || commitResult != (inbound.CommitOutput{StoredSnapshots: 1, StoredDocs: 1}) {
		test.Fatal("actual document command retained a parent Service dependency", commitResult, err)
	}
	storedDocument, err := originalStore.GetDoc(ctx, repo, document.Hash)
	if err != nil || !reflect.DeepEqual(storedDocument, document) {
		test.Fatal("document command did not use its original shared store", storedDocument, err)
	}
	storedSnapshot, err := originalStore.GetSnapshot(ctx, repo, snapshot.ID)
	if err != nil || storedSnapshot.ID != snapshot.ID || storedSnapshot.RepoID != repo || storedSnapshot.DocHash != document.Hash || storedSnapshot.Message != snapshot.Message {
		test.Fatal("document command did not publish its snapshot in the original store", storedSnapshot, err)
	}
	storedChunk, err := originalStore.GetChunk(ctx, repo, chunk.Hash)
	if err != nil || !bytes.Equal(storedChunk, chunk.Data) {
		test.Fatal("chunk command did not use its original shared store", storedChunk, err)
	}
	chunkResult, err = owner.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{chunk}})
	if err != nil || chunkResult != (inbound.StoreChunksOutput{Deduped: 1}) {
		test.Fatal("independent chunk command lost original-store deduplication", chunkResult, err)
	}
	commitResult, err = owner.Commit(ctx, input)
	if err != nil || commitResult != (inbound.CommitOutput{DedupedSnapshots: 1, DedupedDocs: 1}) {
		test.Fatal("independent document command lost original-store deduplication", commitResult, err)
	}
}

func TestDocumentPublicationRuntimeConfigurationClearsIncompleteBindings(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		runtime func() *Runtime
	}{
		{name: "nil runtime", runtime: func() *Runtime { return nil }},
		{name: "missing Context", runtime: func() *Runtime { return &Runtime{} }},
		{name: "incomplete Service", runtime: func() *Runtime { return &Runtime{Context: app.NewService(nil, nil, nil, nil, nil)} }},
	} {
		for _, previousBinding := range []bool{false, true} {
			name := fixture.name + "/unbound"
			if previousBinding {
				name = fixture.name + "/previous binding"
			}
			test.Run(name, func(test *testing.T) {
				runtime := fixture.runtime()
				if runtime != nil && previousBinding {
					runtime.DocumentPublications = &app.DocumentPublicationCommands{}
				}
				err := runtime.ConfigureDocumentPublicationCommands()
				if err == nil || runtime != nil && runtime.DocumentPublications != nil {
					test.Fatal("failed configuration retained or published a document command owner", err)
				}
				if (runtime == nil || runtime.Context == nil) && !errors.Is(err, domain.ErrCatalogUnsupported) {
					test.Fatal("missing command Context lost its unsupported error", err)
				}
			})
		}
	}
}

func TestDocumentPublicationRuntimeMCPNeverConstructsCommands(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Context != nil || runtime.DocumentPublications != nil || runtime.RepositoryAssets != nil {
		test.Fatal("MCP assembled a write-side dependency")
	}
	if runtime.Archive == nil || runtime.GitChanges == nil || runtime.GitScans == nil || runtime.CodeApplicability == nil || runtime.ConversationFind == nil || runtime.DocumentQueries == nil || runtime.ContextQueries == nil || runtime.MemoryLive == nil || runtime.PreparedMemory == nil || runtime.MemorySnapshots == nil {
		test.Fatal("MCP omitted a required query dependency")
	}
	for _, query := range []any{runtime.Archive, runtime.DocumentQueries, runtime.ContextQueries, runtime.MemoryLive, runtime.PreparedMemory, runtime.MemorySnapshots, runtime.GitChanges, runtime.GitScans, runtime.CodeApplicability, runtime.ConversationFind} {
		if _, writable := query.(inbound.DocumentPublicationCommands); writable {
			test.Fatalf("MCP query owner exposes document publication commands: %T", query)
		}
	}
	if err := runtime.ConfigureDocumentPublicationCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.DocumentPublications != nil {
		test.Fatal("query-only runtime obtained publication commands", err)
	}
}
