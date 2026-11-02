package serverruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestRepositoryAssetRuntimeActualOwnerAndMCPIsolation(test *testing.T) {
	for _, queryOnly := range []bool{false, true} {
		test.Run(map[bool]string{false: "API", true: "MCP"}[queryOnly], func(test *testing.T) {
			infrastructureRuntimeEnvironment(test)
			var runtime *Runtime
			var err error
			if queryOnly {
				runtime, err = OpenMCP(test.Context(), "127.0.0.1:0", test.TempDir(), false)
			} else {
				runtime, err = Open(test.Context(), "127.0.0.1:0", test.TempDir(), false)
			}
			if err != nil {
				test.Fatal(err)
			}
			defer runtime.Close()
			if queryOnly {
				if runtime.Context != nil || runtime.RepositoryAssets != nil {
					test.Fatal("MCP assembled a repository command dependency")
				}
				if err := runtime.ConfigureRepositoryAssetCommands(); !errors.Is(err, domain.ErrCatalogUnsupported) || runtime.RepositoryAssets != nil {
					test.Fatal("query-only runtime silently obtained repository commands", err)
				}
				return
			}
			owner, actual := runtime.RepositoryAssets.(*app.RepositoryAssetCommands)
			if !actual || owner == nil {
				test.Fatal("API does not expose the independent repository command owner")
			}
			repo := domain.HashContent([]byte(test.Name()))
			ctx := inbound.WithSystemActor(context.Background())
			if _, err := runtime.Store.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
				test.Fatal(err)
			}
			runtime.Context = nil
			if err := owner.UpdateAbout(ctx, repo, " independent owner ", "example.test", []string{"context"}); err != nil {
				test.Fatal("actual command depends on the parent Service", err)
			}
			current, err := runtime.Store.GetRepo(ctx, repo)
			if err != nil || current.Description != "independent owner" || current.Website != "https://example.test" {
				test.Fatal("actual command did not mutate its original shared store", current, err)
			}
		})
	}
}

func TestRepositoryAssetRuntimeConfigurationRejectsIncompleteDependencies(test *testing.T) {
	for _, runtime := range []*Runtime{nil, {}, {Context: app.NewService(nil, nil, nil, nil, nil)}} {
		if err := runtime.ConfigureRepositoryAssetCommands(); err == nil {
			test.Fatal("incomplete runtime silently configured repository commands")
		}
		if runtime != nil && runtime.RepositoryAssets != nil {
			test.Fatal("failed configuration published a partial owner")
		}
	}
}
