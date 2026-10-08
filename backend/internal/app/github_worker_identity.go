package app

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Called inside the existing identity transaction, which excludes opt-in and
// binding/grant edits until the connection or delivery transition commits.
func (g *GitHubConnections) checkConnectionWorkerPolicy(ctx context.Context, c domain.GitHubConnection) error {
	for _, binding := range c.Bindings {
		if err := g.core.checkDocumentIdentity(ctx, binding.ContextRepoID, false); err != nil {
			return err
		}
	}
	for _, mapping := range c.Mappings {
		if mapping.SyncMembers {
			if err := g.id.checkTeamDocumentIdentities(ctx, mapping.TeamID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *GitHubConnections) checkDeliveryWorkerPolicy(ctx context.Context, j domain.GitHubDelivery) error {
	// Other deliveries only acknowledge already-applied revocation invalidation.
	if j.Kind != "push" && j.Kind != "pull_request" {
		return nil
	}
	if githubHash(string(j.Body)) != j.BodyHash {
		return domain.ErrIntegrity
	}
	var p struct {
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
	}
	if json.Unmarshal(j.Body, &p) != nil || p.Installation.ID <= 0 || p.Repository.ID <= 0 {
		return domain.ErrValidation
	}
	connections, err := g.store.ListGitHubConnections(ctx)
	if err != nil {
		return err
	}
	for _, c := range connections {
		if !c.Enabled || c.Installation.ID != p.Installation.ID {
			continue
		}
		for _, binding := range c.Bindings {
			if binding.ExternalID == p.Repository.ID {
				if err := g.core.checkDocumentIdentity(ctx, binding.ContextRepoID, false); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
