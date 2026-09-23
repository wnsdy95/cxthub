package domain

import (
	"net/mail"
	"strings"
	"time"
)

// CollaborationInvite requires the authenticated recipient's verified account.
// Its ID is a locator, never a bearer credential or an implicit membership.
type CollaborationInvite struct {
	ID          string           `json:"id"`
	Kind        string           `json:"kind"`
	SpaceID     string           `json:"space_id"`
	Email       string           `json:"email"`
	RecipientID string           `json:"recipient_id,omitempty"`
	Role        OrganizationRole `json:"role"`
	Status      string           `json:"status"`
	CreatedBy   string           `json:"created_by"`
	AcceptedBy  string           `json:"accepted_by,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	ExpiresAt   time.Time        `json:"expires_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

func ValidateCollaborationInviteID(id string) error { return validatePrefixedHexID(id, "ci_", 32) }
func ValidateCollaborationScope(kind, id string) error {
	switch kind {
	case "organization":
		return ValidateOrganizationID(id)
	case "enterprise":
		return ValidateEnterpriseID(id)
	default:
		return ErrValidation
	}
}
func NormalizeInvitationEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 {
		return "", ErrValidation
	}
	return email, nil
}
func ValidateCollaborationInvite(i CollaborationInvite) error {
	if ValidateCollaborationInviteID(i.ID) != nil || ValidateCollaborationScope(i.Kind, i.SpaceID) != nil || !ValidOrganizationRole(i.Role) {
		return ErrValidation
	}
	email, err := NormalizeInvitationEmail(i.Email)
	if err != nil || email != i.Email || ValidateExternalID(i.CreatedBy) != nil {
		return ErrValidation
	}
	if i.RecipientID != "" && ValidateExternalID(i.RecipientID) != nil {
		return ErrValidation
	}
	if i.AcceptedBy != "" && ValidateExternalID(i.AcceptedBy) != nil {
		return ErrValidation
	}
	if i.Status != "pending" && i.Status != "accepted" && i.Status != "revoked" {
		return ErrValidation
	}
	if (i.Status == "accepted") != (i.AcceptedBy != "") || i.CreatedAt.IsZero() || !i.ExpiresAt.After(i.CreatedAt) || i.UpdatedAt.Before(i.CreatedAt) {
		return ErrValidation
	}
	return nil
}
