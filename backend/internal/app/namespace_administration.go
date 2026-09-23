package app

import (
	"context"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *IdentityService) RenameCollaborationSpace(ctx context.Context, actor, kind, id, expected, next string) (string, error) {
	return identityResult(ctx, s, func(ctx context.Context) (string, error) {
		if err := domain.ValidateCollaborationScope(kind, id); err != nil {
			return "", err
		}
		if s.collaborationRole(ctx, actor, kind, id) != domain.OrganizationOwner {
			return "", domain.ErrForbidden
		}
		next = strings.ToLower(strings.TrimSpace(next))
		if !domain.ValidNamespaceSlug(next) || kind == "organization" && reservedUsernames[next] {
			return "", domain.ErrValidation
		}
		st, ok := s.repositories.(outbound.NamespaceAdministration)
		if !ok {
			return "", domain.ErrForbidden
		}
		if kind == "enterprise" {
			es, err := s.enterpriseStore()
			if err != nil {
				return "", err
			}
			e, err := es.GetEnterprise(ctx, id)
			if err != nil {
				return "", err
			}
			if e.Slug != expected {
				return "", domain.ErrConflict
			}
			if next == expected {
				return "/enterprises/" + next, nil
			}
			if err = st.RenameEnterpriseSlug(ctx, id, next); err != nil {
				return "", err
			}
			if err = s.enterpriseAudit(ctx, id, actor, "enterprise.renamed", expected+" -> "+next); err != nil {
				return "", err
			}
			return "/enterprises/" + next, nil
		}
		org, err := s.organization.GetOrganization(ctx, id)
		if err != nil {
			return "", err
		}
		if org.Slug != expected {
			return "", domain.ErrConflict
		}
		if next == expected {
			return "/" + next, nil
		}
		repositories, err := s.organization.ListRepositoriesForNamespace(ctx, org.NamespaceID)
		if err != nil {
			return "", err
		}
		if err = st.RenameOrganizationNamespace(ctx, id, next); err != nil {
			return "", err
		}
		for _, repo := range repositories {
			repo.OwnerUsername = next
			if err = s.repositories.CreateRepository(ctx, repo); err != nil {
				return "", err
			}
		}
		err = s.organization.AppendOrganizationAudit(ctx, organizationAudit(ctx, id, actor, "organization.renamed", "organization", id, expected+" -> "+next, time.Now().UTC()))
		return "/" + next, err
	})
}
