package serverruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestWave59RuntimeAuthorityOwnersShareOriginalStoreAndCurrentMembership(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(runtime.Close)
	originalStore := runtime.Store
	if runtime.infrastructure == nil || originalStore != runtime.infrastructure.Store || originalStore != runtime.infrastructure.ownedStore {
		test.Fatal("API authority assembly replaced its one infrastructure store")
	}
	assets, actual := runtime.RepositoryAssets.(*app.RepositoryAssetCommands)
	if !actual || assets == nil {
		test.Fatalf("API authority assembly did not bind the actual asset owner: %T", runtime.RepositoryAssets)
	}
	publications, actual := runtime.DocumentPublications.(*app.DocumentPublicationCommands)
	if !actual || publications == nil {
		test.Fatalf("API authority assembly did not bind the actual publication owner: %T", runtime.DocumentPublications)
	}
	system := inbound.WithSystemActor(context.Background())
	owner := domain.User{ID: domain.NewID("u_"), Username: "authority-owner", Email: "authority-owner@example.test", Name: "Owner"}
	member := domain.User{ID: domain.NewID("u_"), Username: "authority-member", Email: "authority-member@example.test", Name: "Member"}
	for _, user := range []domain.User{owner, member} {
		if err := originalStore.UpsertUser(system, user); err != nil {
			test.Fatal(err)
		}
	}
	repository, err := runtime.Identity.CreateRepository(system, owner, "Authority")
	if err != nil {
		test.Fatal(err)
	}
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := originalStore.PutRepo(system, domain.Repo{ID: repo, RepositoryID: repository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	if err := originalStore.AddMember(system, domain.Membership{RepositoryID: repository.ID, UserID: member.ID, Role: domain.RoleMaintainer}); err != nil {
		test.Fatal(err)
	}
	runtime.Context = nil
	ctx := inbound.WithRepositoryActor(context.Background(), member.ID)
	if err := assets.UpdateAbout(ctx, repo, "runtime authority", "", nil); err != nil {
		test.Fatal("actual runtime asset owner lost its original authority", err)
	}
	chunk := inbound.ChunkObject{Data: []byte("runtime authority chunk")}
	chunk.Hash = domain.HashContent(chunk.Data)
	if output, err := publications.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{chunk}}); err != nil || output != (inbound.StoreChunksOutput{Stored: 1}) {
		test.Fatal("actual runtime publication owner lost its original authority", output, err)
	}
	if err := originalStore.RemoveMember(system, repository.ID, member.ID); err != nil {
		test.Fatal(err)
	}
	if err := assets.UpdateAbout(ctx, repo, "revoked write", "", nil); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("runtime asset owner cached pre-revocation authority", err)
	}
	if output, err := publications.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{chunk}}); !errors.Is(err, domain.ErrForbidden) || output != (inbound.StoreChunksOutput{}) {
		test.Fatal("runtime publication owner bypassed authority for a deduplicated chunk", output, err)
	}
	if output, err := publications.Commit(ctx, inbound.CommitInput{RepoID: repo}); !errors.Is(err, domain.ErrForbidden) || output != (inbound.CommitOutput{}) {
		test.Fatal("runtime publication owner bypassed authority for an empty commit", output, err)
	}
	current, err := originalStore.GetRepo(system, repo)
	if err != nil || current.Description != "runtime authority" {
		test.Fatal("runtime asset owner mutated a different store or accepted revoked work", current, err)
	}
	stored, err := originalStore.GetChunk(system, repo, chunk.Hash)
	if err != nil || !bytes.Equal(stored, chunk.Data) {
		test.Fatal("runtime publication owner did not retain its original store", err)
	}
}

func TestWave59RuntimeCommandConfigurationRejectsAuthorityBeforeIO(test *testing.T) {
	storage := &store.FSStore{}
	for _, scenario := range []struct {
		name         string
		repositories outbound.RepositoryStore
		want         error
	}{
		{name: "missing authority capabilities", repositories: struct{ outbound.RepositoryStore }{storage}, want: domain.ErrCatalogUnsupported},
		{name: "typed nil authority", repositories: (*store.FSStore)(nil), want: domain.ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			runtime := &Runtime{Context: app.NewService(storage, storage, nil, nil, scenario.repositories)}
			if err := runtime.ConfigureRepositoryAssetCommands(); !errors.Is(err, scenario.want) || runtime.RepositoryAssets != nil {
				test.Fatal("runtime published an asset owner without complete authority", err)
			}
			if err := runtime.ConfigureDocumentPublicationCommands(); !errors.Is(err, scenario.want) || runtime.DocumentPublications != nil {
				test.Fatal("runtime published a publication owner without complete authority", err)
			}
		})
	}
}

func TestWave59OpenMCPDoesNotRequireAuthorityCommandConstruction(test *testing.T) {
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	runtime, err := OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(runtime.Close)
	if runtime.Context != nil || runtime.RepositoryAssets != nil || runtime.DocumentPublications != nil {
		test.Fatal("OpenMCP assembled repository authority commands")
	}
	if runtime.infrastructure == nil || runtime.Store != runtime.infrastructure.Store || runtime.Store != runtime.infrastructure.ownedStore || runtime.DocumentQueries == nil || runtime.ContextQueries == nil {
		test.Fatal("MCP lost its original store or read owners while omitting authority commands")
	}
	if err := runtime.ConfigureRepositoryAssetCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.RepositoryAssets != nil {
		test.Fatal("MCP silently created an asset authority dependency", err)
	}
	if err := runtime.ConfigureDocumentPublicationCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.DocumentPublications != nil {
		test.Fatal("MCP silently created a publication authority dependency", err)
	}
}
