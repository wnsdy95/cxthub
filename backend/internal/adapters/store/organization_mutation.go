package store

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func validateOrganizationMutationAudit(event domain.OrganizationAuditEvent, organizationID, action, targetType, targetID string) error {
	if err := domain.ValidateOrganizationAuditEvent(event); err != nil {
		return err
	}
	if event.OrganizationID != organizationID || event.Action != action || event.TargetType != targetType || event.TargetID != targetID {
		return domain.ErrValidation
	}
	return nil
}

func validateOrganizationRepositoryMutation(repository domain.Repository, owner domain.Membership, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateRepositoryRecord(repository); err != nil {
		return err
	}
	if err := domain.ValidateMembershipRecord(owner); err != nil {
		return err
	}
	if repository.OwnerNamespaceID == "" || owner.RepositoryID != repository.ID || owner.UserID != repository.OwnerID || owner.Role != domain.RoleOwner {
		return domain.ErrValidation
	}
	return validateOrganizationMutationAudit(event, event.OrganizationID, "organization.repository.created", "repository", repository.ID)
}
