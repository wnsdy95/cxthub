package domain

import (
	"fmt"
	"strings"
	"time"
)

type NamespaceKind string

const (
	NamespaceUser       NamespaceKind = "user"
	NamespaceEnterprise NamespaceKind = "enterprise"
)

// Namespace owns the globally unique first URL segment. A namespace is
// either personal or enterprise-owned; it never grants Workspace access by
// itself.
type Namespace struct {
	ID           string        `json:"id"`
	Slug         string        `json:"slug"`
	Kind         NamespaceKind `json:"kind"`
	UserID       string        `json:"user_id,omitempty"`
	EnterpriseID string        `json:"enterprise_id,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
}

type EnterpriseRole string

const (
	EnterpriseMember EnterpriseRole = "member"
	EnterpriseAdmin  EnterpriseRole = "admin"
	EnterpriseOwner  EnterpriseRole = "owner"
)

func ValidEnterpriseRole(role EnterpriseRole) bool {
	switch role {
	case EnterpriseMember, EnterpriseAdmin, EnterpriseOwner:
		return true
	default:
		return false
	}
}

func (r EnterpriseRole) AtLeast(min EnterpriseRole) bool {
	rank := func(v EnterpriseRole) int {
		switch v {
		case EnterpriseMember:
			return 1
		case EnterpriseAdmin:
			return 2
		case EnterpriseOwner:
			return 3
		default:
			return 0
		}
	}
	return rank(r) >= rank(min) && rank(min) > 0
}

type Enterprise struct {
	ID          string    `json:"id"`
	NamespaceID string    `json:"namespace_id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Logo        string    `json:"logo,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type EnterpriseMembership struct {
	EnterpriseID string         `json:"enterprise_id"`
	UserID       string         `json:"user_id"`
	Role         EnterpriseRole `json:"role"`
	User         *User          `json:"user,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

type EnterpriseWorkspaceCreation string

const (
	EnterpriseWorkspaceAdmins  EnterpriseWorkspaceCreation = "admins"
	EnterpriseWorkspaceMembers EnterpriseWorkspaceCreation = "members"
)

// EnterprisePolicy contains only policies currently enforced by the service.
// Future SSO/SCIM/IP controls require their own enforcement engines and must
// not be represented as decorative booleans here.
type EnterprisePolicy struct {
	EnterpriseID               string                      `json:"enterprise_id"`
	WorkspaceCreation          EnterpriseWorkspaceCreation `json:"workspace_creation"`
	DefaultWorkspaceVisibility Visibility                  `json:"default_workspace_visibility"`
	AllowPublicWorkspaces      bool                        `json:"allow_public_workspaces"`
	BreakGlassEnabled          bool                        `json:"break_glass_enabled"`
	BreakGlassMaxMinutes       int                         `json:"break_glass_max_minutes"`
	UpdatedBy                  string                      `json:"updated_by,omitempty"`
	UpdatedAt                  time.Time                   `json:"updated_at"`
}

func DefaultEnterprisePolicy(enterpriseID string) EnterprisePolicy {
	return EnterprisePolicy{
		EnterpriseID:               enterpriseID,
		WorkspaceCreation:          EnterpriseWorkspaceAdmins,
		DefaultWorkspaceVisibility: VisibilityPrivate,
		AllowPublicWorkspaces:      true,
		BreakGlassEnabled:          true,
		BreakGlassMaxMinutes:       60,
	}
}

type EnterpriseAuditEvent struct {
	ID           string    `json:"id"`
	EnterpriseID string    `json:"enterprise_id"`
	ActorID      string    `json:"actor_id"`
	Action       string    `json:"action"`
	TargetType   string    `json:"target_type,omitempty"`
	TargetID     string    `json:"target_id,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// BreakGlassGrant is an exceptional, read-only Workspace access grant. It is
// owner-only, reason-bound, expires automatically, and is mirrored into the
// append-only enterprise audit log.
type BreakGlassGrant struct {
	ID           string    `json:"id"`
	EnterpriseID string    `json:"enterprise_id"`
	WorkspaceID  string    `json:"workspace_id"`
	UserID       string    `json:"user_id"`
	Reason       string    `json:"reason"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func ValidateNamespaceID(id string) error  { return validatePrefixedHexID(id, "ns_", 32) }
func ValidateEnterpriseID(id string) error { return validatePrefixedHexID(id, "ent_", 32) }
func ValidateAuditID(id string) error      { return validatePrefixedHexID(id, "aud_", 32) }
func ValidateBreakGlassID(id string) error { return validatePrefixedHexID(id, "bg_", 32) }

func ValidNamespaceSlug(slug string) bool {
	if slug == "" || len(slug) > 64 || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return false
	}
	for _, r := range slug {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func ValidateNamespaceRecord(ns Namespace) error {
	if err := ValidateNamespaceID(ns.ID); err != nil {
		return err
	}
	if !ValidNamespaceSlug(ns.Slug) {
		return fmt.Errorf("%w: invalid namespace slug", ErrValidation)
	}
	switch ns.Kind {
	case NamespaceUser:
		if ValidateExternalID(ns.UserID) != nil || ns.EnterpriseID != "" {
			return fmt.Errorf("%w: invalid personal namespace subject", ErrValidation)
		}
	case NamespaceEnterprise:
		if ValidateEnterpriseID(ns.EnterpriseID) != nil || ns.UserID != "" {
			return fmt.Errorf("%w: invalid enterprise namespace subject", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: invalid namespace kind", ErrValidation)
	}
	return nil
}

func ValidateEnterpriseRecord(ent Enterprise) error {
	if err := ValidateEnterpriseID(ent.ID); err != nil {
		return err
	}
	if err := ValidateNamespaceID(ent.NamespaceID); err != nil {
		return err
	}
	if !ValidNamespaceSlug(ent.Slug) || strings.TrimSpace(ent.Name) == "" || len(ent.Name) > 128 {
		return fmt.Errorf("%w: invalid enterprise name or slug", ErrValidation)
	}
	if err := ValidateExternalID(ent.CreatedBy); err != nil {
		return err
	}
	return ValidateAvatarDataURL(ent.Logo)
}

func ValidateEnterpriseMembershipRecord(m EnterpriseMembership) error {
	if err := ValidateEnterpriseID(m.EnterpriseID); err != nil {
		return err
	}
	if err := ValidateExternalID(m.UserID); err != nil {
		return err
	}
	if !ValidEnterpriseRole(m.Role) {
		return fmt.Errorf("%w: invalid enterprise role", ErrValidation)
	}
	return nil
}

func ValidateEnterprisePolicy(p EnterprisePolicy) error {
	if err := ValidateEnterpriseID(p.EnterpriseID); err != nil {
		return err
	}
	if p.WorkspaceCreation != EnterpriseWorkspaceAdmins && p.WorkspaceCreation != EnterpriseWorkspaceMembers {
		return fmt.Errorf("%w: invalid workspace creation policy", ErrValidation)
	}
	if p.DefaultWorkspaceVisibility != VisibilityPrivate && p.DefaultWorkspaceVisibility != VisibilityPublic {
		return fmt.Errorf("%w: invalid default workspace visibility", ErrValidation)
	}
	if !p.AllowPublicWorkspaces && p.DefaultWorkspaceVisibility == VisibilityPublic {
		return fmt.Errorf("%w: public default conflicts with public workspace policy", ErrValidation)
	}
	if p.BreakGlassMaxMinutes < 5 || p.BreakGlassMaxMinutes > 240 {
		return fmt.Errorf("%w: break-glass duration must be between 5 and 240 minutes", ErrValidation)
	}
	return nil
}

func ValidateEnterpriseAuditEvent(event EnterpriseAuditEvent) error {
	if err := ValidateAuditID(event.ID); err != nil {
		return err
	}
	if err := ValidateEnterpriseID(event.EnterpriseID); err != nil {
		return err
	}
	if err := ValidateExternalID(event.ActorID); err != nil {
		return err
	}
	if strings.TrimSpace(event.Action) == "" || len(event.Action) > 128 || len(event.Reason) > 1000 {
		return fmt.Errorf("%w: invalid enterprise audit event", ErrValidation)
	}
	return nil
}

func ValidateBreakGlassGrant(grant BreakGlassGrant) error {
	if err := ValidateBreakGlassID(grant.ID); err != nil {
		return err
	}
	if err := ValidateEnterpriseID(grant.EnterpriseID); err != nil {
		return err
	}
	if err := ValidateWorkspaceID(grant.WorkspaceID); err != nil {
		return err
	}
	if err := ValidateExternalID(grant.UserID); err != nil {
		return err
	}
	reason := strings.TrimSpace(grant.Reason)
	if len(reason) < 3 || len(reason) > 1000 || !grant.ExpiresAt.After(grant.CreatedAt) {
		return fmt.Errorf("%w: invalid break-glass grant", ErrValidation)
	}
	return nil
}
