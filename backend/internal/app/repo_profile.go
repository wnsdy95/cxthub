package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (service *Service) PatchRepoProfile(ctx context.Context, repo domain.ContentHash, patch inbound.RepoProfilePatch) (domain.Repo, error) {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return domain.Repo{}, err
	}
	return commands.PatchRepoProfile(ctx, repo, patch)
}
