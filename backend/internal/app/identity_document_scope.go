package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func checkRepositoryDocumentIdentitiesFromStores(ctx context.Context, repositories outbound.RepositoryStore, ids ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	meta, ok := repositories.(interface {
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

func checkTeamDocumentIdentitiesFromStores(ctx context.Context, repositories outbound.RepositoryStore, teams outbound.TeamStore, id string) error {
	grants, err := teams.ListTeamRepositoryGrants(ctx, id)
	if err != nil {
		return err
	}
	ids := make([]string, len(grants))
	for i, g := range grants {
		ids[i] = g.RepositoryID
	}
	return checkRepositoryDocumentIdentitiesFromStores(ctx, repositories, ids...)
}
