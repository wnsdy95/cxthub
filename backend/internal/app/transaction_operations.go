package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) Commit(ctx context.Context, in inbound.CommitInput) (inbound.CommitOutput, error) {
	commands, err := s.documentPublications(DocumentPublicationCompatibility)
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	return commands.Commit(ctx, in)
}

func (s *Service) EnsureRepo(ctx context.Context, actor string, repo domain.Repo) (domain.Repo, error) {
	if repo.RequiredDocIdentity != domain.DocumentIdentityLegacy {
		return domain.Repo{}, domain.ErrValidation
	}
	ctx = writeAction(inbound.WithRepositoryActor(ctx, actor), "register")
	return repositoryWrite(context.WithValue(ctx, revisionScopeKey{}, "none"), s, repo.ID, func(ctx context.Context) (domain.Repo, error) { return s.ensureRepo(ctx, actor, repo) })
}

func (s *Service) StoreChunks(ctx context.Context, in inbound.StoreChunksInput) (inbound.StoreChunksOutput, error) {
	commands, err := s.documentPublications(DocumentPublicationCompatibility)
	if err != nil {
		return inbound.StoreChunksOutput{}, err
	}
	return commands.StoreChunks(ctx, in)
}
func (service *Service) UpdateAbout(ctx context.Context, repo domain.ContentHash, description, website string, topics []string) error {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return err
	}
	return commands.UpdateAbout(ctx, repo, description, website, topics)
}
func (service *Service) UpdateRepoConfig(ctx context.Context, repo domain.ContentHash, branch *string, protected *bool) error {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return err
	}
	return commands.UpdateRepoConfig(ctx, repo, branch, protected)
}
func (service *Service) PutSettings(ctx context.Context, repo domain.ContentHash, bundle domain.SettingsBundle) error {
	commands, err := service.repositoryAssets(RepositoryAssetsCompatibility)
	if err != nil {
		return err
	}
	return commands.PutSettings(ctx, repo, bundle)
}
