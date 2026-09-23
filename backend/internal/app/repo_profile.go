package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) PatchRepoProfile(ctx context.Context, id domain.ContentHash, p inbound.RepoProfilePatch) (domain.Repo, error) {
	return repositoryWrite(writeAction(auditOperation(ctx, "repository.profile.updated"), "manage"), s, id, func(ctx context.Context) (domain.Repo, error) {
		cur, err := s.meta.GetRepo(ctx, id)
		if err != nil {
			return domain.Repo{}, err
		}
		if p.DefaultBranch != nil && *p.DefaultBranch != "" {
			if err := domain.ValidateBranchName(*p.DefaultBranch); err != nil {
				return domain.Repo{}, err
			}
		}
		if p.Description != nil {
			cur.Description = *p.Description
		}
		if p.Website != nil {
			cur.Website = *p.Website
		}
		if p.Topics != nil {
			cur.Topics = p.Topics
		}
		if _, err := normalizeAboutWebsite(cur.Website); err != nil {
			return domain.Repo{}, err
		}
		if p.DefaultBranch != nil || p.ProtectDefault != nil {
			if err := s.updateRepoConfigCommand(ctx, id, p.DefaultBranch, p.ProtectDefault); err != nil {
				return domain.Repo{}, err
			}
		}
		if p.Description != nil || p.Website != nil || p.Topics != nil {
			if err := s.updateAboutCommand(ctx, id, cur.Description, cur.Website, cur.Topics); err != nil {
				return domain.Repo{}, err
			}
		}
		return s.meta.GetRepo(ctx, id)
	})
}
