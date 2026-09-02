package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *IdentityService) ensurePersonalNamespace(ctx context.Context, user domain.User) (domain.Namespace, error) {
	if s.enterprise == nil {
		return domain.Namespace{}, fmt.Errorf("%w: enterprise storage unavailable", domain.ErrForbidden)
	}
	if !domain.ValidNamespaceSlug(user.Username) {
		return domain.Namespace{}, domain.ErrValidation
	}
	if existing, err := s.enterprise.GetNamespaceBySlug(ctx, user.Username); err == nil {
		if existing.Kind != domain.NamespaceUser || existing.UserID != user.ID {
			return domain.Namespace{}, domain.ErrConflict
		}
		return existing, nil
	}
	ns := domain.Namespace{
		ID:        domain.NewID("ns_"),
		Slug:      user.Username,
		Kind:      domain.NamespaceUser,
		UserID:    user.ID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.enterprise.CreateNamespace(ctx, ns); err != nil {
		// Concurrent login may have created the same personal namespace.
		if existing, getErr := s.enterprise.GetNamespaceBySlug(ctx, user.Username); getErr == nil && existing.Kind == domain.NamespaceUser && existing.UserID == user.ID {
			return existing, nil
		}
		return domain.Namespace{}, err
	}
	return ns, nil
}

func enterpriseAudit(enterpriseID, actorID, action, targetType, targetID, reason string, now time.Time) domain.EnterpriseAuditEvent {
	return domain.EnterpriseAuditEvent{
		ID:           domain.NewID("aud_"),
		EnterpriseID: enterpriseID,
		ActorID:      actorID,
		Action:       action,
		TargetType:   targetType,
		TargetID:     targetID,
		Reason:       reason,
		CreatedAt:    now,
	}
}

func (s *IdentityService) CreateEnterprise(ctx context.Context, creator domain.User, name, requestedSlug string) (domain.Enterprise, error) {
	if s.enterprise == nil {
		return domain.Enterprise{}, fmt.Errorf("%w: enterprise storage unavailable", domain.ErrForbidden)
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return domain.Enterprise{}, domain.ErrValidation
	}
	slug := strings.ToLower(strings.TrimSpace(requestedSlug))
	if slug == "" {
		slug = domain.Slugify(name, "enterprise")
	}
	if !domain.ValidNamespaceSlug(slug) || reservedUsernames[slug] {
		return domain.Enterprise{}, domain.ErrValidation
	}
	if _, err := s.enterprise.GetNamespaceBySlug(ctx, slug); err == nil {
		return domain.Enterprise{}, domain.ErrConflict
	}
	// FS records created before namespace support are also checked. PostgreSQL
	// migration backfills them, but this closes the lazy/local compatibility gap.
	if user, err := s.ws.GetUserByUsername(ctx, slug); err == nil && user.ID != "" {
		return domain.Enterprise{}, domain.ErrConflict
	}
	now := time.Now().UTC()
	ent := domain.Enterprise{
		ID:          domain.NewID("ent_"),
		NamespaceID: domain.NewID("ns_"),
		Name:        name,
		Slug:        slug,
		CreatedBy:   creator.ID,
		CreatedAt:   now,
	}
	ns := domain.Namespace{ID: ent.NamespaceID, Slug: slug, Kind: domain.NamespaceEnterprise, EnterpriseID: ent.ID, CreatedAt: now}
	owner := domain.EnterpriseMembership{EnterpriseID: ent.ID, UserID: creator.ID, Role: domain.EnterpriseOwner, CreatedAt: now}
	policy := domain.DefaultEnterprisePolicy(ent.ID)
	policy.UpdatedBy, policy.UpdatedAt = creator.ID, now
	audit := enterpriseAudit(ent.ID, creator.ID, "enterprise.created", "enterprise", ent.ID, "", now)
	if err := s.enterprise.CreateEnterprise(ctx, ent, ns, owner, policy, audit); err != nil {
		return domain.Enterprise{}, err
	}
	return ent, nil
}

func (s *IdentityService) ListEnterprises(ctx context.Context, userID string) ([]domain.Enterprise, error) {
	if s.enterprise == nil {
		return []domain.Enterprise{}, nil
	}
	return s.enterprise.ListEnterprisesForUser(ctx, userID)
}

func (s *IdentityService) GetEnterprise(ctx context.Context, enterpriseID string) (domain.Enterprise, error) {
	if s.enterprise == nil {
		return domain.Enterprise{}, domain.ErrNotFound
	}
	return s.enterprise.GetEnterprise(ctx, enterpriseID)
}

// UpdateEnterpriseProfile changes display-only organization metadata. The
// namespace slug is intentionally excluded: renaming a namespace changes every
// canonical Workspace/Repository URL and requires a dedicated alias-aware flow.
func (s *IdentityService) UpdateEnterpriseProfile(ctx context.Context, actorID, enterpriseID string, name, logo *string) (domain.Enterprise, error) {
	role, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID)
	if !ok || !role.AtLeast(domain.EnterpriseAdmin) {
		return domain.Enterprise{}, domain.ErrForbidden
	}
	ent, err := s.enterprise.GetEnterprise(ctx, enterpriseID)
	if err != nil {
		return domain.Enterprise{}, err
	}
	if name != nil {
		ent.Name = strings.TrimSpace(*name)
	}
	if logo != nil {
		ent.Logo = strings.TrimSpace(*logo)
	}
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return domain.Enterprise{}, err
	}
	now := time.Now().UTC()
	audit := enterpriseAudit(enterpriseID, actorID, "enterprise.profile.updated", "enterprise", enterpriseID, "", now)
	if err := s.enterprise.UpdateEnterpriseWithAudit(ctx, ent, audit); err != nil {
		return domain.Enterprise{}, err
	}
	return ent, nil
}

func (s *IdentityService) PublicEnterprise(ctx context.Context, slug string) (domain.Enterprise, []domain.Workspace, error) {
	if s.enterprise == nil {
		return domain.Enterprise{}, nil, domain.ErrNotFound
	}
	ent, err := s.enterprise.GetEnterpriseBySlug(ctx, slug)
	if err != nil {
		return domain.Enterprise{}, nil, err
	}
	workspaces, err := s.enterprise.ListWorkspacesForNamespace(ctx, ent.NamespaceID)
	if err != nil {
		return domain.Enterprise{}, nil, err
	}
	public := make([]domain.Workspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.IsPublic() {
			public = append(public, workspace)
		}
	}
	return ent, public, nil
}

func (s *IdentityService) EnterpriseRoleOf(ctx context.Context, enterpriseID, userID string) (domain.EnterpriseRole, bool) {
	if s.enterprise == nil || userID == "" {
		return "", false
	}
	member, err := s.enterprise.GetEnterpriseMembership(ctx, enterpriseID, userID)
	if err != nil || !domain.ValidEnterpriseRole(member.Role) {
		return "", false
	}
	return member.Role, true
}

func (s *IdentityService) ListEnterpriseMembers(ctx context.Context, actorID, enterpriseID string) ([]domain.EnterpriseMembership, error) {
	if role, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID); !ok || !role.AtLeast(domain.EnterpriseMember) {
		return nil, domain.ErrForbidden
	}
	return s.enterprise.ListEnterpriseMembers(ctx, enterpriseID)
}

func (s *IdentityService) UpdateEnterpriseMember(ctx context.Context, actorID, enterpriseID, targetID string, role domain.EnterpriseRole) error {
	if !domain.ValidEnterpriseRole(role) {
		return domain.ErrValidation
	}
	actorRole, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID)
	if !ok || !actorRole.AtLeast(domain.EnterpriseAdmin) {
		return domain.ErrForbidden
	}
	targetRole, targetExists := s.EnterpriseRoleOf(ctx, enterpriseID, targetID)
	if actorRole != domain.EnterpriseOwner {
		// Admins manage ordinary members only. Admin/Owner promotion,
		// demotion, and removal stay inside the Owner boundary.
		if role != domain.EnterpriseMember || targetExists && targetRole != domain.EnterpriseMember {
			return domain.ErrForbidden
		}
	}
	if targetExists && targetRole == domain.EnterpriseOwner && role != domain.EnterpriseOwner {
		members, err := s.enterprise.ListEnterpriseMembers(ctx, enterpriseID)
		if err != nil {
			return err
		}
		owners := 0
		for _, member := range members {
			if member.Role == domain.EnterpriseOwner {
				owners++
			}
		}
		if owners <= 1 {
			return domain.ErrConflict
		}
	}
	if _, err := s.ws.GetUser(ctx, targetID); err != nil {
		return err
	}
	now := time.Now().UTC()
	membership := domain.EnterpriseMembership{EnterpriseID: enterpriseID, UserID: targetID, Role: role, CreatedAt: now}
	audit := enterpriseAudit(enterpriseID, actorID, "enterprise.member.updated", "user", targetID, string(role), now)
	return s.enterprise.AddEnterpriseMemberWithAudit(ctx, membership, audit)
}

func (s *IdentityService) RemoveEnterpriseMember(ctx context.Context, actorID, enterpriseID, targetID string) error {
	actorRole, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID)
	if !ok || !actorRole.AtLeast(domain.EnterpriseAdmin) {
		return domain.ErrForbidden
	}
	targetRole, exists := s.EnterpriseRoleOf(ctx, enterpriseID, targetID)
	if !exists {
		return domain.ErrNotFound
	}
	if actorRole != domain.EnterpriseOwner && targetRole != domain.EnterpriseMember {
		return domain.ErrForbidden
	}
	if targetRole == domain.EnterpriseOwner {
		members, err := s.enterprise.ListEnterpriseMembers(ctx, enterpriseID)
		if err != nil {
			return err
		}
		owners := 0
		for _, member := range members {
			if member.Role == domain.EnterpriseOwner {
				owners++
			}
		}
		if owners <= 1 {
			return domain.ErrConflict
		}
	}
	audit := enterpriseAudit(enterpriseID, actorID, "enterprise.member.removed", "user", targetID, "", time.Now().UTC())
	return s.enterprise.RemoveEnterpriseMemberWithAudit(ctx, enterpriseID, targetID, audit)
}

func (s *IdentityService) GetEnterprisePolicy(ctx context.Context, actorID, enterpriseID string) (domain.EnterprisePolicy, error) {
	if role, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID); !ok || !role.AtLeast(domain.EnterpriseMember) {
		return domain.EnterprisePolicy{}, domain.ErrForbidden
	}
	return s.enterprise.GetEnterprisePolicy(ctx, enterpriseID)
}

func (s *IdentityService) UpdateEnterprisePolicy(ctx context.Context, actorID string, policy domain.EnterprisePolicy) (domain.EnterprisePolicy, error) {
	role, ok := s.EnterpriseRoleOf(ctx, policy.EnterpriseID, actorID)
	if !ok || !role.AtLeast(domain.EnterpriseAdmin) {
		return domain.EnterprisePolicy{}, domain.ErrForbidden
	}
	policy.UpdatedBy, policy.UpdatedAt = actorID, time.Now().UTC()
	if err := domain.ValidateEnterprisePolicy(policy); err != nil {
		return domain.EnterprisePolicy{}, err
	}
	audit := enterpriseAudit(policy.EnterpriseID, actorID, "enterprise.policy.updated", "enterprise", policy.EnterpriseID, "", policy.UpdatedAt)
	if err := s.enterprise.PutEnterprisePolicyWithAudit(ctx, policy, audit); err != nil {
		return domain.EnterprisePolicy{}, err
	}
	return policy, nil
}

func (s *IdentityService) CreateEnterpriseWorkspace(ctx context.Context, actor domain.User, enterpriseID, name string) (domain.Workspace, error) {
	if s.enterprise == nil {
		return domain.Workspace{}, domain.ErrForbidden
	}
	role, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actor.ID)
	if !ok {
		return domain.Workspace{}, domain.ErrForbidden
	}
	policy, err := s.enterprise.GetEnterprisePolicy(ctx, enterpriseID)
	if err != nil {
		return domain.Workspace{}, err
	}
	required := domain.EnterpriseAdmin
	if policy.WorkspaceCreation == domain.EnterpriseWorkspaceMembers {
		required = domain.EnterpriseMember
	}
	if !role.AtLeast(required) {
		return domain.Workspace{}, domain.ErrForbidden
	}
	name = strings.TrimSpace(name)
	if !domain.ValidWorkspaceName(name) {
		return domain.Workspace{}, domain.ErrValidation
	}
	ent, err := s.enterprise.GetEnterprise(ctx, enterpriseID)
	if err != nil {
		return domain.Workspace{}, err
	}
	now := time.Now().UTC()
	workspace := domain.Workspace{
		ID:               domain.NewID("ws_"),
		Name:             name,
		OwnerID:          actor.ID,
		OwnerUsername:    ent.Slug,
		OwnerNamespaceID: ent.NamespaceID,
		Slug:             s.uniqueNamespaceWorkspaceSlug(ctx, ent.NamespaceID, name, ""),
		Visibility:       policy.DefaultWorkspaceVisibility,
		CreatedAt:        now,
	}
	owner := domain.Membership{WorkspaceID: workspace.ID, UserID: actor.ID, Role: domain.RoleOwner, CreatedAt: now}
	audit := enterpriseAudit(enterpriseID, actor.ID, "enterprise.workspace.created", "workspace", workspace.ID, "", now)
	if err := s.enterprise.CreateEnterpriseWorkspaceWithAudit(ctx, workspace, owner, audit); err != nil {
		return domain.Workspace{}, err
	}
	return workspace, nil
}

func (s *IdentityService) uniqueNamespaceWorkspaceSlug(ctx context.Context, namespaceID, name, selfID string) string {
	taken := map[string]bool{}
	if workspaces, err := s.enterprise.ListWorkspacesForNamespace(ctx, namespaceID); err == nil {
		for _, workspace := range workspaces {
			if workspace.ID != selfID && workspace.Slug != "" {
				taken[workspace.Slug] = true
			}
		}
	}
	base, candidate := domain.WorkspaceSlug(name), domain.WorkspaceSlug(name)
	for suffix := 2; taken[candidate]; suffix++ {
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
	return candidate
}

func (s *IdentityService) ListEnterpriseWorkspaces(ctx context.Context, actorID, enterpriseID string) ([]domain.Workspace, error) {
	if role, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID); !ok || !role.AtLeast(domain.EnterpriseMember) {
		return nil, domain.ErrForbidden
	}
	ent, err := s.enterprise.GetEnterprise(ctx, enterpriseID)
	if err != nil {
		return nil, err
	}
	return s.enterprise.ListWorkspacesForNamespace(ctx, ent.NamespaceID)
}

func (s *IdentityService) CreateBreakGlassGrant(ctx context.Context, actorID, enterpriseID, workspaceID, reason string, minutes int) (domain.BreakGlassGrant, error) {
	role, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID)
	if !ok || role != domain.EnterpriseOwner {
		return domain.BreakGlassGrant{}, domain.ErrForbidden
	}
	policy, err := s.enterprise.GetEnterprisePolicy(ctx, enterpriseID)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if !policy.BreakGlassEnabled || minutes < 1 || minutes > policy.BreakGlassMaxMinutes {
		return domain.BreakGlassGrant{}, domain.ErrForbidden
	}
	ent, err := s.enterprise.GetEnterprise(ctx, enterpriseID)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	workspace, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if workspace.OwnerNamespaceID != ent.NamespaceID {
		return domain.BreakGlassGrant{}, domain.ErrForbidden
	}
	now := time.Now().UTC()
	grant := domain.BreakGlassGrant{
		ID:           domain.NewID("bg_"),
		EnterpriseID: enterpriseID,
		WorkspaceID:  workspaceID,
		UserID:       actorID,
		Reason:       strings.TrimSpace(reason),
		CreatedAt:    now,
		ExpiresAt:    now.Add(time.Duration(minutes) * time.Minute),
	}
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	created := enterpriseAudit(enterpriseID, actorID, "enterprise.break_glass.created", "workspace", workspaceID, grant.Reason, now)
	if err := s.enterprise.CreateBreakGlassGrantWithAudit(ctx, grant, created); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	return grant, nil
}

func (s *IdentityService) HasBreakGlassAccess(ctx context.Context, workspaceID, userID string) (bool, error) {
	if s.enterprise == nil || workspaceID == "" || userID == "" {
		return false, nil
	}
	workspace, err := s.ws.GetWorkspace(ctx, workspaceID)
	if err != nil || workspace.OwnerNamespaceID == "" {
		return false, nil
	}
	ns, err := s.enterprise.GetNamespace(ctx, workspace.OwnerNamespaceID)
	if err != nil || ns.Kind != domain.NamespaceEnterprise {
		return false, nil
	}
	role, ok := s.EnterpriseRoleOf(ctx, ns.EnterpriseID, userID)
	if !ok || role != domain.EnterpriseOwner {
		return false, nil
	}
	now := time.Now().UTC()
	used := enterpriseAudit(ns.EnterpriseID, userID, "enterprise.break_glass.used", "workspace", workspaceID, "", now)
	_, err = s.enterprise.UseActiveBreakGlassGrant(ctx, ns.EnterpriseID, workspaceID, userID, now, used)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *IdentityService) ListEnterpriseAudit(ctx context.Context, actorID, enterpriseID string, limit int) ([]domain.EnterpriseAuditEvent, error) {
	if role, ok := s.EnterpriseRoleOf(ctx, enterpriseID, actorID); !ok || !role.AtLeast(domain.EnterpriseAdmin) {
		return nil, domain.ErrForbidden
	}
	return s.enterprise.ListEnterpriseAudit(ctx, enterpriseID, limit)
}
