package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"strings"
	"time"
)

func (s *IdentityService) enterpriseStore() (outbound.EnterpriseStore, error) {
	st, ok := s.repositories.(outbound.EnterpriseStore)
	if !ok {
		return nil, domain.ErrForbidden
	}
	return st, nil
}
func (s *IdentityService) EnterpriseRoleOf(ctx context.Context, id, user string) (domain.EnterpriseRole, bool) {
	st, err := s.enterpriseStore()
	if err != nil || user == "" {
		return "", false
	}
	members, err := st.ListEnterpriseMembers(ctx, id)
	if err != nil {
		return "", false
	}
	for _, m := range members {
		if m.UserID == user && domain.ValidEnterpriseRole(m.Role) {
			return m.Role, true
		}
	}
	return "", false
}
func (s *IdentityService) canReadEnterprise(ctx context.Context, id, user string) bool {
	st, err := s.enterpriseStore()
	if err != nil || user == "" {
		return false
	}
	if _, ok := s.EnterpriseRoleOf(ctx, id, user); ok {
		return true
	}
	orgs, err := st.ListEnterpriseOrganizations(ctx, id)
	if err != nil {
		return false
	}
	for _, o := range orgs {
		if _, ok := s.OrganizationRoleOf(ctx, o.ID, user); ok {
			return true
		}
	}
	return false
}
func (s *IdentityService) enterpriseAudit(ctx context.Context, id, actor, action, target string) error {
	st, err := s.enterpriseStore()
	if err != nil {
		return err
	}
	return st.AppendEnterpriseAudit(ctx, domain.EnterpriseAuditEvent{ID: domain.NewID("aud_"), EnterpriseID: id, ActorID: actor, Action: action, TargetID: target, CreatedAt: time.Now().UTC()})
}
func (s *IdentityService) CreateEnterprise(ctx context.Context, user domain.User, name, slug string) (domain.Enterprise, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Enterprise, error) {
		st, err := s.enterpriseStore()
		if err != nil {
			return domain.Enterprise{}, err
		}
		name = strings.TrimSpace(name)
		slug = strings.ToLower(strings.TrimSpace(slug))
		if slug == "" {
			slug = domain.Slugify(name, "enterprise")
		}
		e := domain.Enterprise{ID: domain.NewID("ep_"), Name: name, Slug: slug, Policy: domain.DefaultEnterprisePolicy(), CreatedBy: user.ID, CreatedAt: time.Now().UTC()}
		if err = domain.ValidateEnterprise(e); err != nil {
			return domain.Enterprise{}, err
		}
		owner := domain.EnterpriseMembership{EnterpriseID: e.ID, UserID: user.ID, Role: domain.EnterpriseOwner, CreatedAt: e.CreatedAt}
		if err = st.CreateEnterprise(ctx, e, owner); err != nil {
			return domain.Enterprise{}, err
		}
		return e, s.enterpriseAudit(ctx, e.ID, user.ID, "enterprise.created", e.ID)
	})
}
func (s *IdentityService) ListEnterprises(ctx context.Context, user string) ([]domain.Enterprise, error) {
	st, err := s.enterpriseStore()
	if err != nil {
		return nil, err
	}
	return st.ListEnterprisesForUser(ctx, user)
}
func (s *IdentityService) GetEnterprise(ctx context.Context, user, slug string) (domain.Enterprise, error) {
	st, err := s.enterpriseStore()
	if err != nil {
		return domain.Enterprise{}, err
	}
	var e domain.Enterprise
	if domain.ValidateEnterpriseID(slug) == nil {
		e, err = st.GetEnterprise(ctx, slug)
	} else {
		e, err = st.GetEnterpriseBySlug(ctx, slug)
	}
	if err != nil {
		return e, err
	}
	if !s.canReadEnterprise(ctx, e.ID, user) {
		return domain.Enterprise{}, domain.ErrNotFound
	}
	return e, nil
}

type EnterprisePatch struct {
	Name   *string                  `json:"name"`
	Logo   *string                  `json:"logo"`
	Policy *domain.EnterprisePolicy `json:"policy"`
}

func (s *IdentityService) UpdateEnterprise(ctx context.Context, user, id string, patch EnterprisePatch) (domain.Enterprise, error) {
	return identityResult(ctx, s, func(ctx context.Context) (domain.Enterprise, error) {
		role, ok := s.EnterpriseRoleOf(ctx, id, user)
		if !ok || !role.AtLeast(domain.EnterpriseAdmin) {
			return domain.Enterprise{}, domain.ErrForbidden
		}
		st, _ := s.enterpriseStore()
		e, err := st.GetEnterprise(ctx, id)
		if err != nil {
			return e, err
		}
		if patch.Name != nil {
			e.Name = strings.TrimSpace(*patch.Name)
		}
		if patch.Logo != nil {
			e.Logo = strings.TrimSpace(*patch.Logo)
		}
		if patch.Policy != nil {
			// Public records cannot silently become private as a side effect of parent
			// policy. Owners must explicitly change their repositories before tightening.
			if !patch.Policy.AllowPublicRepositories {
				orgs, err := st.ListEnterpriseOrganizations(ctx, id)
				if err != nil {
					return e, err
				}
				for _, o := range orgs {
					if err = s.checkEnterpriseOrganizationPolicy(ctx, o, *patch.Policy); err != nil {
						return e, err
					}
				}
			}
			e.Policy = *patch.Policy
		}
		if err = domain.ValidateEnterprise(e); err != nil {
			return domain.Enterprise{}, err
		}
		if err = st.UpdateEnterprise(ctx, e); err != nil {
			return e, err
		}
		return e, s.enterpriseAudit(ctx, id, user, "enterprise.updated", id)
	})
}
func (s *IdentityService) ListEnterpriseMembers(ctx context.Context, user, id string) ([]domain.EnterpriseMembership, error) {
	if !s.canReadEnterprise(ctx, id, user) {
		return nil, domain.ErrForbidden
	}
	st, err := s.enterpriseStore()
	if err != nil {
		return nil, err
	}
	members, err := st.ListEnterpriseMembers(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range members {
		user, err := s.repositories.GetUser(ctx, members[i].UserID)
		if err != nil {
			return nil, err
		}
		members[i].User = &domain.User{ID: user.ID, Username: user.Username, Name: user.Name, Nickname: user.Nickname}
	}
	return members, nil
}
func (s *IdentityService) UpdateEnterpriseMember(ctx context.Context, user, id, target string, next domain.EnterpriseRole) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if !domain.ValidEnterpriseRole(next) {
			return domain.ErrValidation
		}
		actor, ok := s.EnterpriseRoleOf(ctx, id, user)
		if !ok || !actor.AtLeast(domain.EnterpriseAdmin) {
			return domain.ErrForbidden
		}
		resolved, err := s.memberSubject(ctx, target)
		if err != nil {
			return err
		}
		target = resolved
		old, exists := s.EnterpriseRoleOf(ctx, id, target)
		if actor != domain.EnterpriseOwner && (next != domain.EnterpriseMember || exists && old != domain.EnterpriseMember) {
			return domain.ErrForbidden
		}
		st, _ := s.enterpriseStore()
		members, err := st.ListEnterpriseMembers(ctx, id)
		if err != nil {
			return err
		}
		if old == domain.EnterpriseOwner && next != domain.EnterpriseOwner {
			owners := 0
			for _, m := range members {
				if m.Role == domain.EnterpriseOwner {
					owners++
				}
			}
			if owners <= 1 {
				return domain.ErrConflict
			}
		}
		if _, err = s.repositories.GetUser(ctx, target); err != nil {
			return err
		}
		if err = st.PutEnterpriseMember(ctx, domain.EnterpriseMembership{EnterpriseID: id, UserID: target, Role: next, CreatedAt: time.Now().UTC()}); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, user, "enterprise.member.updated", target)
	})
}
func (s *IdentityService) RemoveEnterpriseMember(ctx context.Context, user, id, target string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		actor, ok := s.EnterpriseRoleOf(ctx, id, user)
		if !ok || !actor.AtLeast(domain.EnterpriseAdmin) {
			return domain.ErrForbidden
		}
		old, exists := s.EnterpriseRoleOf(ctx, id, target)
		if !exists {
			return domain.ErrNotFound
		}
		if actor != domain.EnterpriseOwner && old != domain.EnterpriseMember {
			return domain.ErrForbidden
		}
		st, _ := s.enterpriseStore()
		members, err := st.ListEnterpriseMembers(ctx, id)
		if err != nil {
			return err
		}
		if old == domain.EnterpriseOwner {
			owners := 0
			for _, m := range members {
				if m.Role == domain.EnterpriseOwner {
					owners++
				}
			}
			if owners <= 1 {
				return domain.ErrConflict
			}
		}
		if err = st.RemoveEnterpriseMember(ctx, id, target); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, user, "enterprise.member.removed", target)
	})
}
func (s *IdentityService) ListEnterpriseOrganizations(ctx context.Context, user, id string) ([]domain.Organization, error) {
	if !s.canReadEnterprise(ctx, id, user) {
		return nil, domain.ErrForbidden
	}
	st, err := s.enterpriseStore()
	if err != nil {
		return nil, err
	}
	return st.ListEnterpriseOrganizations(ctx, id)
}
func (s *IdentityService) checkEnterpriseOrganizationPolicy(ctx context.Context, o domain.Organization, p domain.EnterprisePolicy) error {
	if p.AllowPublicRepositories {
		return nil
	}
	repositories, err := s.organization.ListRepositoriesForNamespace(ctx, o.NamespaceID)
	if err != nil {
		return err
	}
	for _, r := range repositories {
		if r.IsPublic() {
			return domain.ErrConflict
		}
	}
	return nil
}
func (s *IdentityService) LinkEnterpriseOrganization(ctx context.Context, user, id, org string, link bool) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if role, ok := s.EnterpriseRoleOf(ctx, id, user); !ok || role != domain.EnterpriseOwner {
			return domain.ErrForbidden
		}
		if role, ok := s.OrganizationRoleOf(ctx, org, user); !ok || role != domain.OrganizationOwner {
			return domain.ErrForbidden
		}
		st, _ := s.enterpriseStore()
		enterprise, err := st.GetEnterprise(ctx, id)
		if err != nil {
			return err
		}
		prior, err := st.OrganizationEnterprise(ctx, org)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err == nil && prior.ID != id {
			return domain.ErrConflict
		}
		target, action := "", "enterprise.organization.removed"
		if link {
			organization, err := s.organization.GetOrganization(ctx, org)
			if err != nil {
				return err
			}
			if err = s.checkEnterpriseOrganizationPolicy(ctx, organization, enterprise.Policy); err != nil {
				return err
			}
			target, action = id, "enterprise.organization.added"
		}
		if err = st.SetOrganizationEnterprise(ctx, org, target); err != nil {
			return err
		}
		return s.enterpriseAudit(ctx, id, user, action, org)
	})
}
func (s *IdentityService) ListEnterpriseAudit(ctx context.Context, user, id string) ([]domain.EnterpriseAuditEvent, error) {
	if role, ok := s.EnterpriseRoleOf(ctx, id, user); !ok || !role.AtLeast(domain.EnterpriseAdmin) {
		return nil, domain.ErrForbidden
	}
	st, _ := s.enterpriseStore()
	return st.ListEnterpriseAudit(ctx, id, 100)
}
func (s *IdentityService) effectiveOrganizationPolicy(ctx context.Context, org string) (domain.OrganizationPolicy, error) {
	policy, err := s.organization.GetOrganizationPolicy(ctx, org)
	if err != nil {
		return policy, err
	}
	st, ok := s.repositories.(outbound.EnterpriseStore)
	if !ok {
		return policy, nil
	}
	parent, err := st.OrganizationEnterprise(ctx, org)
	if errors.Is(err, domain.ErrNotFound) {
		return policy, nil
	}
	if err != nil {
		return policy, err
	}
	return domain.EffectiveOrganizationPolicy(policy, &parent.Policy), nil
}
func (s *IdentityService) EffectiveOrganizationPolicy(ctx context.Context, user, org string) (domain.OrganizationPolicy, error) {
	if _, ok := s.OrganizationRoleOf(ctx, org, user); !ok {
		return domain.OrganizationPolicy{}, domain.ErrForbidden
	}
	return s.effectiveOrganizationPolicy(ctx, org)
}
