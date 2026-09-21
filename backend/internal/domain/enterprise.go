package domain

import (
	"strings"
	"time"
)

// Enterprise groups organizations. It owns no repositories and confers no
// implicit repository access on administrators or organization members.
type Enterprise struct {
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Slug      string           `json:"slug"`
	Logo      string           `json:"logo,omitempty"`
	Policy    EnterprisePolicy `json:"policy"`
	CreatedBy string           `json:"created_by"`
	CreatedAt time.Time        `json:"created_at"`
}
type EnterprisePolicy struct {
	RepositoryCreation      OrganizationRepositoryCreation `json:"repository_creation"`
	AllowPublicRepositories bool                           `json:"allow_public_repositories"`
	AllowBreakGlass         bool                           `json:"allow_break_glass"`
}

func DefaultEnterprisePolicy() EnterprisePolicy {
	return EnterprisePolicy{RepositoryCreation: OrganizationRepositoryMembers, AllowPublicRepositories: true, AllowBreakGlass: true}
}

type EnterpriseRole string

const (
	EnterpriseOwner  EnterpriseRole = "owner"
	EnterpriseAdmin  EnterpriseRole = "admin"
	EnterpriseMember EnterpriseRole = "member"
)

func (r EnterpriseRole) AtLeast(min EnterpriseRole) bool {
	return OrganizationRole(r).AtLeast(OrganizationRole(min))
}
func ValidEnterpriseRole(r EnterpriseRole) bool {
	return r == EnterpriseOwner || r == EnterpriseAdmin || r == EnterpriseMember
}

type EnterpriseMembership struct {
	User         *User          `json:"user,omitempty"`
	EnterpriseID string         `json:"enterprise_id"`
	UserID       string         `json:"user_id"`
	Role         EnterpriseRole `json:"role"`
	CreatedAt    time.Time      `json:"created_at"`
}
type EnterpriseAuditEvent struct {
	ID           string    `json:"id"`
	EnterpriseID string    `json:"enterprise_id"`
	ActorID      string    `json:"actor_id"`
	Action       string    `json:"action"`
	TargetID     string    `json:"target_id"`
	CreatedAt    time.Time `json:"created_at"`
}

func ValidateEnterpriseID(id string) error { return validatePrefixedHexID(id, "ep_", 32) }
func ValidateEnterprise(e Enterprise) error {
	if err := ValidateEnterpriseID(e.ID); err != nil {
		return err
	}
	if strings.TrimSpace(e.Name) == "" || len(e.Name) > 128 || !ValidNamespaceSlug(e.Slug) {
		return ErrValidation
	}
	if err := ValidateExternalID(e.CreatedBy); err != nil {
		return err
	}
	if err := ValidateAvatarDataURL(e.Logo); err != nil {
		return err
	}
	if e.Policy.RepositoryCreation != OrganizationRepositoryAdmins && e.Policy.RepositoryCreation != OrganizationRepositoryMembers {
		return ErrValidation
	}
	return nil
}
func ValidateEnterpriseMembership(m EnterpriseMembership) error {
	if err := ValidateEnterpriseID(m.EnterpriseID); err != nil {
		return err
	}
	if err := ValidateExternalID(m.UserID); err != nil {
		return err
	}
	if !ValidEnterpriseRole(m.Role) {
		return ErrValidation
	}
	return nil
}
func EffectiveOrganizationPolicy(p OrganizationPolicy, parent *EnterprisePolicy) OrganizationPolicy {
	if parent == nil {
		return p
	}
	if parent.RepositoryCreation == OrganizationRepositoryAdmins {
		p.RepositoryCreation = OrganizationRepositoryAdmins
	}
	p.AllowPublicRepositories = p.AllowPublicRepositories && parent.AllowPublicRepositories
	if !p.AllowPublicRepositories {
		p.DefaultRepositoryVisibility = VisibilityPrivate
	}
	p.BreakGlassEnabled = p.BreakGlassEnabled && parent.AllowBreakGlass
	return p
}
