package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type auditOperationKey struct{}

func auditOperation(ctx context.Context, action string) context.Context {
	return context.WithValue(ctx, auditOperationKey{}, action)
}

// Only metadata is logged. Payloads, secret envelopes and webhook URLs are never copied.
func appendRepositoryAudit(ctx context.Context, repositories outbound.RepositoryStore, actor, id, action string) error {
	r, err := repositories.GetRepository(ctx, id)
	if err != nil {
		return err
	}
	if r.OwnerNamespaceID == "" {
		return nil
	}
	org, ok := repositories.(outbound.OrganizationStore)
	if !ok {
		return domain.ErrForbidden
	}
	ns, err := org.GetNamespace(ctx, r.OwnerNamespaceID)
	if err != nil {
		return err
	}
	if ns.Kind != domain.NamespaceOrganization {
		return nil
	}
	return org.AppendOrganizationAudit(ctx, organizationAudit(ctx, ns.OrganizationID, actor, action, "repository", id, "", time.Now().UTC()))
}
func (s *Service) auditRepositoryWrite(ctx context.Context, repo domain.ContentHash) error {
	actor, system := inbound.RepositoryActor(ctx)
	if system {
		return nil
	}
	record, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if record.RepositoryID == "" {
		return nil
	}
	action, _ := ctx.Value(auditOperationKey{}).(string)
	if action == "" {
		action = "context.updated"
	}
	return appendRepositoryAudit(ctx, s.repositories, actor, record.RepositoryID, action)
}
func (s *IdentityService) OrganizationAuditPage(ctx context.Context, actor, org, cursor string, limit int) (domain.OrganizationAuditPage, error) {
	if role, _ := s.OrganizationRoleOf(ctx, org, actor); !role.AtLeast(domain.OrganizationAdmin) {
		return domain.OrganizationAuditPage{}, domain.ErrForbidden
	}
	c, err := domain.DecodeAuditCursor(org, cursor)
	if err != nil {
		return domain.OrganizationAuditPage{}, err
	}
	if limit < 1 || limit > 500 {
		return domain.OrganizationAuditPage{}, domain.ErrValidation
	}
	reader, ok := s.organization.(outbound.OrganizationAuditReader)
	if !ok {
		return domain.OrganizationAuditPage{}, domain.ErrForbidden
	}
	events, err := reader.OrganizationAuditBefore(ctx, c, limit+1)
	if err != nil {
		return domain.OrganizationAuditPage{}, err
	}
	out := domain.OrganizationAuditPage{Events: events}
	if out.Events == nil {
		out.Events = []domain.OrganizationAuditEvent{}
	}
	if len(events) > limit {
		out.Events = events[:limit]
		out.NextCursor = domain.EncodeAuditCursor(events[limit-1])
	}
	return out, nil
}
