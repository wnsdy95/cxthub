package store

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func validateEnterpriseMutationAudit(event domain.EnterpriseAuditEvent, enterpriseID, action, targetType, targetID string) error {
	if err := domain.ValidateEnterpriseAuditEvent(event); err != nil {
		return err
	}
	if event.EnterpriseID != enterpriseID || event.Action != action || event.TargetType != targetType || event.TargetID != targetID {
		return domain.ErrValidation
	}
	return nil
}

func validateEnterpriseWorkspaceMutation(workspace domain.Workspace, owner domain.Membership, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateWorkspaceRecord(workspace); err != nil {
		return err
	}
	if err := domain.ValidateMembershipRecord(owner); err != nil {
		return err
	}
	if workspace.OwnerNamespaceID == "" || owner.WorkspaceID != workspace.ID || owner.UserID != workspace.OwnerID || owner.Role != domain.RoleOwner {
		return domain.ErrValidation
	}
	return validateEnterpriseMutationAudit(event, event.EnterpriseID, "enterprise.workspace.created", "workspace", workspace.ID)
}
