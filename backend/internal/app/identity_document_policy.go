package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Call after authorization and inside the existing identity mutation. PG's
// identity lock excludes concurrent repository opt-in until commit. This is a
// compatibility check, never an actor or permission grant. Narrow legacy-only
// identity adapters have no context metadata or root opt-in capability.
func (s *IdentityService) checkRepositoryDocumentIdentities(ctx context.Context, ids ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	meta, ok := s.repositories.(interface {
		ListRepos(context.Context, string) ([]domain.Repo, error)
	})
	if !ok {
		return nil
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	repos, err := meta.ListRepos(ctx, "default")
	if err != nil {
		return err
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), binaryDocumentIdentitiesSupported())
	for _, repo := range repos {
		if wanted[repo.RepositoryID] {
			if err := outbound.CheckDocumentIdentityCompatibility(ctx, repo.RequiredDocIdentity); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *IdentityService) checkRepositoryRecordsDocumentIdentities(ctx context.Context, repos []domain.Repository) error {
	ids := make([]string, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	return s.checkRepositoryDocumentIdentities(ctx, ids...)
}
func (s *IdentityService) checkOrganizationDocumentIdentities(ctx context.Context, id string) error {
	org, err := s.organization.GetOrganization(ctx, id)
	if err != nil {
		return err
	}
	repos, err := s.organization.ListRepositoriesForNamespace(ctx, org.NamespaceID)
	if err != nil {
		return err
	}
	return s.checkRepositoryRecordsDocumentIdentities(ctx, repos)
}
func (s *IdentityService) checkTeamDocumentIdentities(ctx context.Context, id string) error {
	grants, err := s.teams.ListTeamRepositoryGrants(ctx, id)
	if err != nil {
		return err
	}
	ids := make([]string, len(grants))
	for i, g := range grants {
		ids[i] = g.RepositoryID
	}
	return s.checkRepositoryDocumentIdentities(ctx, ids...)
}
func (s *IdentityService) checkPersonalDocumentIdentities(ctx context.Context, user string) error {
	repos, err := s.repositories.ListRepositoriesForUser(ctx, user)
	if err != nil {
		return err
	}
	var ids []string
	for _, r := range repos {
		if r.OwnerID != user {
			continue
		}
		if r.OwnerNamespaceID != "" && s.organization != nil {
			ns, err := s.organization.GetNamespace(ctx, r.OwnerNamespaceID)
			if err != nil {
				return err
			}
			if ns.Kind != domain.NamespaceUser {
				continue
			}
		}
		ids = append(ids, r.ID)
	}
	return s.checkRepositoryDocumentIdentities(ctx, ids...)
}
