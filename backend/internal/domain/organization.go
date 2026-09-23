package domain

import (
	"fmt"
	"strings"
	"time"
)

type NamespaceKind string

const (
	NamespaceUser         NamespaceKind = "user"
	NamespaceOrganization NamespaceKind = "organization"
)

// Namespace owns the globally unique first URL segment. A namespace is
// either personal or organization-owned; it never grants Repository access by
// itself.
type Namespace struct {
	ID             string        `json:"id"`
	Slug           string        `json:"slug"`
	Kind           NamespaceKind `json:"kind"`
	UserID         string        `json:"user_id,omitempty"`
	OrganizationID string        `json:"organization_id,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

type OrganizationRole string

const (
	OrganizationMember OrganizationRole = "member"
	OrganizationAdmin  OrganizationRole = "admin"
	OrganizationOwner  OrganizationRole = "owner"
)

func ValidOrganizationRole(role OrganizationRole) bool {
	switch role {
	case OrganizationMember, OrganizationAdmin, OrganizationOwner:
		return true
	default:
		return false
	}
}

func (r OrganizationRole) AtLeast(min OrganizationRole) bool {
	rank := func(v OrganizationRole) int {
		switch v {
		case OrganizationMember:
			return 1
		case OrganizationAdmin:
			return 2
		case OrganizationOwner:
			return 3
		default:
			return 0
		}
	}
	return rank(r) >= rank(min) && rank(min) > 0
}

type Organization struct {
	ID          string    `json:"id"`
	NamespaceID string    `json:"namespace_id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Logo        string    `json:"logo,omitempty"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type OrganizationMembership struct {
	OrganizationID string           `json:"organization_id"`
	UserID         string           `json:"user_id"`
	Role           OrganizationRole `json:"role"`
	User           *User            `json:"user,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

type OrganizationRepositoryCreation string

const (
	OrganizationRepositoryAdmins  OrganizationRepositoryCreation = "admins"
	OrganizationRepositoryMembers OrganizationRepositoryCreation = "members"
)

// OrganizationPolicy contains only policies currently enforced by the service.
// Future SSO/SCIM/IP controls require their own enforcement engines and must
// not be represented as decorative booleans here.
type OrganizationPolicy struct {
	OrganizationID              string                         `json:"organization_id"`
	RepositoryCreation          OrganizationRepositoryCreation `json:"repository_creation"`
	DefaultRepositoryVisibility Visibility                     `json:"default_repository_visibility"`
	DefaultRepositoryRole       MemberRole                     `json:"default_repository_role"`
	AllowPublicRepositories     bool                           `json:"allow_public_repositories"`
	BreakGlassEnabled           bool                           `json:"break_glass_enabled"`
	BreakGlassMaxMinutes        int                            `json:"break_glass_max_minutes"`
	UpdatedBy                   string                         `json:"updated_by,omitempty"`
	UpdatedAt                   time.Time                      `json:"updated_at"`
}

func DefaultOrganizationPolicy(organizationID string) OrganizationPolicy {
	return OrganizationPolicy{
		OrganizationID:              organizationID,
		RepositoryCreation:          OrganizationRepositoryAdmins,
		DefaultRepositoryVisibility: VisibilityPrivate,
		AllowPublicRepositories:     true,
		BreakGlassEnabled:           true,
		BreakGlassMaxMinutes:        60,
	}
}

type OrganizationAuditEvent struct {
	CorrelationID  string    `json:"correlation_id,omitempty"`
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	ActorID        string    `json:"actor_id"`
	Action         string    `json:"action"`
	TargetType     string    `json:"target_type,omitempty"`
	TargetID       string    `json:"target_id,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// BreakGlassGrant is an exceptional, read-only Repository access grant. It is
// owner-only, reason-bound, expires automatically, and is mirrored into the
// append-only organization audit log.
type BreakGlassGrant struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	RepositoryID   string    `json:"repository_id"`
	UserID         string    `json:"user_id"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func ValidateNamespaceID(id string) error    { return validatePrefixedHexID(id, "ns_", 32) }
func ValidateOrganizationID(id string) error { return validatePrefixedHexID(id, "ent_", 32) }
func ValidateAuditID(id string) error        { return validatePrefixedHexID(id, "aud_", 32) }
func ValidateBreakGlassID(id string) error   { return validatePrefixedHexID(id, "bg_", 32) }

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
		if ValidateExternalID(ns.UserID) != nil || ns.OrganizationID != "" {
			return fmt.Errorf("%w: invalid personal namespace subject", ErrValidation)
		}
	case NamespaceOrganization:
		if ValidateOrganizationID(ns.OrganizationID) != nil || ns.UserID != "" {
			return fmt.Errorf("%w: invalid organization namespace subject", ErrValidation)
		}
	default:
		return fmt.Errorf("%w: invalid namespace kind", ErrValidation)
	}
	return nil
}

func ValidateOrganizationRecord(ent Organization) error {
	if err := ValidateOrganizationID(ent.ID); err != nil {
		return err
	}
	if err := ValidateNamespaceID(ent.NamespaceID); err != nil {
		return err
	}
	if !ValidNamespaceSlug(ent.Slug) || strings.TrimSpace(ent.Name) == "" || len(ent.Name) > 128 {
		return fmt.Errorf("%w: invalid organization name or slug", ErrValidation)
	}
	if err := ValidateExternalID(ent.CreatedBy); err != nil {
		return err
	}
	return ValidateAvatarDataURL(ent.Logo)
}

func ValidateOrganizationMembershipRecord(m OrganizationMembership) error {
	if err := ValidateOrganizationID(m.OrganizationID); err != nil {
		return err
	}
	if err := ValidateExternalID(m.UserID); err != nil {
		return err
	}
	if !ValidOrganizationRole(m.Role) {
		return fmt.Errorf("%w: invalid organization role", ErrValidation)
	}
	return nil
}

func ValidateOrganizationPolicy(p OrganizationPolicy) error {
	if p.DefaultRepositoryRole != "" && !ValidRole(p.DefaultRepositoryRole) {
		return fmt.Errorf("%w: invalid default repository role", ErrValidation)
	}
	if err := ValidateOrganizationID(p.OrganizationID); err != nil {
		return err
	}
	if p.RepositoryCreation != OrganizationRepositoryAdmins && p.RepositoryCreation != OrganizationRepositoryMembers {
		return fmt.Errorf("%w: invalid repository creation policy", ErrValidation)
	}
	if p.DefaultRepositoryVisibility != VisibilityPrivate && p.DefaultRepositoryVisibility != VisibilityPublic {
		return fmt.Errorf("%w: invalid default repository visibility", ErrValidation)
	}
	if !p.AllowPublicRepositories && p.DefaultRepositoryVisibility == VisibilityPublic {
		return fmt.Errorf("%w: public default conflicts with public repository policy", ErrValidation)
	}
	if p.BreakGlassMaxMinutes < 5 || p.BreakGlassMaxMinutes > 240 {
		return fmt.Errorf("%w: break-glass duration must be between 5 and 240 minutes", ErrValidation)
	}
	return nil
}

func ValidateOrganizationAuditEvent(event OrganizationAuditEvent) error {
	if err := ValidateAuditID(event.ID); err != nil {
		return err
	}
	if err := ValidateOrganizationID(event.OrganizationID); err != nil {
		return err
	}
	if err := ValidateExternalID(event.ActorID); err != nil {
		return err
	}
	if strings.TrimSpace(event.Action) == "" || len(event.Action) > 128 || len(event.Reason) > 1000 {
		return fmt.Errorf("%w: invalid organization audit event", ErrValidation)
	}
	return nil
}

func ValidateBreakGlassGrant(grant BreakGlassGrant) error {
	if err := ValidateBreakGlassID(grant.ID); err != nil {
		return err
	}
	if err := ValidateOrganizationID(grant.OrganizationID); err != nil {
		return err
	}
	if err := ValidateRepositoryID(grant.RepositoryID); err != nil {
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
