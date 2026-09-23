package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *IdentityService) ensurePersonalNamespace(ctx context.Context, user domain.User) (domain.Namespace, error) {
	if s.organization == nil {
		return domain.Namespace{}, fmt.Errorf("%w: organization storage unavailable", domain.ErrForbidden)
	}
	if !domain.ValidNamespaceSlug(user.Username) {
		return domain.Namespace{}, domain.ErrValidation
	}
	if existing, err := s.organization.GetNamespaceBySlug(ctx, user.Username); err == nil {
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
	if err := s.organization.CreateNamespace(ctx, ns); err != nil {
		// Concurrent login may have created the same personal namespace.
		if existing, getErr := s.organization.GetNamespaceBySlug(ctx, user.Username); getErr == nil && existing.Kind == domain.NamespaceUser && existing.UserID == user.ID {
			return existing, nil
		}
		return domain.Namespace{}, err
	}
	return ns, nil
}

func organizationAudit(ctx context.Context, organizationID, actorID, action, targetType, targetID, reason string, now time.Time) domain.OrganizationAuditEvent {
	return domain.OrganizationAuditEvent{
		ID:             domain.NewID("aud_"),
		CorrelationID:  inbound.Correlation(ctx),
		OrganizationID: organizationID,
		ActorID:        actorID,
		Action:         action,
		TargetType:     targetType,
		TargetID:       targetID,
		Reason:         reason,
		CreatedAt:      now,
	}
}

func (s *IdentityService) mutateCreateOrganization(ctx context.Context, creator domain.User, name, requestedSlug string) (domain.Organization, error) {
	if s.organization == nil {
		return domain.Organization{}, fmt.Errorf("%w: organization storage unavailable", domain.ErrForbidden)
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return domain.Organization{}, domain.ErrValidation
	}
	slug := strings.ToLower(strings.TrimSpace(requestedSlug))
	if slug == "" {
		slug = domain.Slugify(name, "organization")
	}
	if !domain.ValidNamespaceSlug(slug) || reservedUsernames[slug] {
		return domain.Organization{}, domain.ErrValidation
	}
	if _, err := s.organization.GetNamespaceBySlug(ctx, slug); err == nil {
		return domain.Organization{}, domain.ErrConflict
	}
	// FS records created before namespace support are also checked. PostgreSQL
	// migration backfills them, but this closes the lazy/local compatibility gap.
	if user, err := s.repositories.GetUserByUsername(ctx, slug); err == nil && user.ID != "" {
		return domain.Organization{}, domain.ErrConflict
	}
	now := time.Now().UTC()
	organizationRecord := domain.Organization{
		ID:          domain.NewID("ent_"),
		NamespaceID: domain.NewID("ns_"),
		Name:        name,
		Slug:        slug,
		CreatedBy:   creator.ID,
		CreatedAt:   now,
	}
	ns := domain.Namespace{ID: organizationRecord.NamespaceID, Slug: slug, Kind: domain.NamespaceOrganization, OrganizationID: organizationRecord.ID, CreatedAt: now}
	owner := domain.OrganizationMembership{OrganizationID: organizationRecord.ID, UserID: creator.ID, Role: domain.OrganizationOwner, CreatedAt: now}
	policy := domain.DefaultOrganizationPolicy(organizationRecord.ID)
	policy.UpdatedBy, policy.UpdatedAt = creator.ID, now
	audit := organizationAudit(ctx, organizationRecord.ID, creator.ID, "organization.created", "organization", organizationRecord.ID, "", now)
	if err := s.organization.CreateOrganization(ctx, organizationRecord, ns, owner, policy, audit); err != nil {
		return domain.Organization{}, err
	}
	return organizationRecord, nil
}

func (s *IdentityService) ListOrganizations(ctx context.Context, userID string) ([]domain.Organization, error) {
	if s.organization == nil {
		return []domain.Organization{}, nil
	}
	return s.organization.ListOrganizationsForUser(ctx, userID)
}

func (s *IdentityService) GetOrganization(ctx context.Context, organizationID string) (domain.Organization, error) {
	if s.organization == nil {
		return domain.Organization{}, domain.ErrNotFound
	}
	return s.organization.GetOrganization(ctx, organizationID)
}

// UpdateOrganizationProfile changes display-only organization metadata. The
// namespace slug is intentionally excluded: renaming a namespace changes every
// canonical repository URL and requires a dedicated alias-aware flow.
func (s *IdentityService) mutateUpdateOrganizationProfile(ctx context.Context, actorID, organizationID string, name, logo *string) (domain.Organization, error) {
	role, ok := s.OrganizationRoleOf(ctx, organizationID, actorID)
	if !ok || !role.AtLeast(domain.OrganizationAdmin) {
		return domain.Organization{}, domain.ErrForbidden
	}
	organizationRecord, err := s.organization.GetOrganization(ctx, organizationID)
	if err != nil {
		return domain.Organization{}, err
	}
	if name != nil {
		organizationRecord.Name = strings.TrimSpace(*name)
	}
	if logo != nil {
		organizationRecord.Logo = strings.TrimSpace(*logo)
	}
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return domain.Organization{}, err
	}
	now := time.Now().UTC()
	audit := organizationAudit(ctx, organizationID, actorID, "organization.profile.updated", "organization", organizationID, "", now)
	if err := s.organization.UpdateOrganizationWithAudit(ctx, organizationRecord, audit); err != nil {
		return domain.Organization{}, err
	}
	return organizationRecord, nil
}

func (s *IdentityService) PublicOrganization(ctx context.Context, slug string) (domain.Organization, []domain.Repository, error) {
	if s.organization == nil {
		return domain.Organization{}, nil, domain.ErrNotFound
	}
	organizationRecord, err := s.organization.GetOrganizationBySlug(ctx, slug)
	if err != nil {
		return domain.Organization{}, nil, err
	}
	repositories, err := s.organization.ListRepositoriesForNamespace(ctx, organizationRecord.NamespaceID)
	if err != nil {
		return domain.Organization{}, nil, err
	}
	public := make([]domain.Repository, 0, len(repositories))
	for _, repository := range repositories {
		if repository.IsPublic() {
			public = append(public, repository)
		}
	}
	return organizationRecord, public, nil
}

func (s *IdentityService) OrganizationRoleOf(ctx context.Context, organizationID, userID string) (domain.OrganizationRole, bool) {
	if s.organization == nil || userID == "" {
		return "", false
	}
	member, err := s.organization.GetOrganizationMembership(ctx, organizationID, userID)
	if err != nil || !domain.ValidOrganizationRole(member.Role) {
		return "", false
	}
	return member.Role, true
}

func (s *IdentityService) ListOrganizationMembers(ctx context.Context, actorID, organizationID string) ([]domain.OrganizationMembership, error) {
	if role, ok := s.OrganizationRoleOf(ctx, organizationID, actorID); !ok || !role.AtLeast(domain.OrganizationMember) {
		return nil, domain.ErrForbidden
	}
	return s.organization.ListOrganizationMembers(ctx, organizationID)
}

func (s *IdentityService) mutateUpdateOrganizationMember(ctx context.Context, actorID, organizationID, targetID string, role domain.OrganizationRole) error {
	if !domain.ValidOrganizationRole(role) {
		return domain.ErrValidation
	}
	actorRole, ok := s.OrganizationRoleOf(ctx, organizationID, actorID)
	if !ok || !actorRole.AtLeast(domain.OrganizationAdmin) {
		return domain.ErrForbidden
	}
	resolved, err := s.memberSubject(ctx, targetID)
	if err != nil {
		return err
	}
	targetID = resolved
	targetRole, targetExists := s.OrganizationRoleOf(ctx, organizationID, targetID)
	if actorRole != domain.OrganizationOwner {
		// Admins manage ordinary members only. Admin/Owner promotion,
		// demotion, and removal stay inside the Owner boundary.
		if role != domain.OrganizationMember || targetExists && targetRole != domain.OrganizationMember {
			return domain.ErrForbidden
		}
	}
	if targetExists && targetRole == domain.OrganizationOwner && role != domain.OrganizationOwner {
		members, err := s.organization.ListOrganizationMembers(ctx, organizationID)
		if err != nil {
			return err
		}
		owners := 0
		for _, member := range members {
			if member.Role == domain.OrganizationOwner {
				owners++
			}
		}
		if owners <= 1 {
			return domain.ErrConflict
		}
	}
	now := time.Now().UTC()
	membership := domain.OrganizationMembership{OrganizationID: organizationID, UserID: targetID, Role: role, CreatedAt: now}
	audit := organizationAudit(ctx, organizationID, actorID, "organization.member.updated", "user", targetID, string(role), now)
	return s.organization.AddOrganizationMemberWithAudit(ctx, membership, audit)
}

func (s *IdentityService) checkOrganizationRemoval(ctx context.Context, actorID, organizationID, targetID string) error {
	actorRole, ok := s.OrganizationRoleOf(ctx, organizationID, actorID)
	if !ok || !actorRole.AtLeast(domain.OrganizationAdmin) {
		return domain.ErrForbidden
	}
	targetRole, exists := s.OrganizationRoleOf(ctx, organizationID, targetID)
	if !exists {
		return domain.ErrNotFound
	}
	if actorRole != domain.OrganizationOwner && targetRole != domain.OrganizationMember {
		return domain.ErrForbidden
	}
	if targetRole == domain.OrganizationOwner {
		members, err := s.organization.ListOrganizationMembers(ctx, organizationID)
		if err != nil {
			return err
		}
		owners := 0
		for _, member := range members {
			if member.Role == domain.OrganizationOwner {
				owners++
			}
		}
		if owners <= 1 {
			return domain.ErrConflict
		}
	}
	return nil
}
func (s *IdentityService) mutateRemoveOrganizationMember(ctx context.Context, actorID, organizationID, targetID string) error {
	if err := s.checkOrganizationRemoval(ctx, actorID, organizationID, targetID); err != nil {
		return err
	}
	audit := organizationAudit(ctx, organizationID, actorID, "organization.member.removed", "user", targetID, "", time.Now().UTC())
	return s.organization.RemoveOrganizationMemberWithAudit(ctx, organizationID, targetID, audit)
}

func (s *IdentityService) GetOrganizationPolicy(ctx context.Context, actorID, organizationID string) (domain.OrganizationPolicy, error) {
	if role, ok := s.OrganizationRoleOf(ctx, organizationID, actorID); !ok || !role.AtLeast(domain.OrganizationMember) {
		return domain.OrganizationPolicy{}, domain.ErrForbidden
	}
	return s.organization.GetOrganizationPolicy(ctx, organizationID)
}

func (s *IdentityService) mutateUpdateOrganizationPolicy(ctx context.Context, actorID string, policy domain.OrganizationPolicy) (domain.OrganizationPolicy, error) {
	role, ok := s.OrganizationRoleOf(ctx, policy.OrganizationID, actorID)
	if !ok || !role.AtLeast(domain.OrganizationAdmin) {
		return domain.OrganizationPolicy{}, domain.ErrForbidden
	}
	current, err := s.organization.GetOrganizationPolicy(ctx, policy.OrganizationID)
	if err != nil {
		return domain.OrganizationPolicy{}, err
	}
	if current.DefaultRepositoryRole != policy.DefaultRepositoryRole && role != domain.OrganizationOwner {
		return domain.OrganizationPolicy{}, domain.ErrForbidden
	}
	if !policy.UpdatedAt.IsZero() && !policy.UpdatedAt.Equal(current.UpdatedAt) {
		return domain.OrganizationPolicy{}, domain.ErrConflict
	}
	// PostgreSQL persists microseconds. Return that exact precision so a fresh
	// response is a usable editing baseline on nanosecond-resolution hosts too.
	// Keep revisions distinct even if the clock repeats or moves backwards.
	revision := time.Now().UTC().Truncate(time.Microsecond)
	if !revision.After(current.UpdatedAt) {
		revision = current.UpdatedAt.UTC().Truncate(time.Microsecond).Add(time.Microsecond)
	}
	policy.UpdatedBy, policy.UpdatedAt = actorID, revision
	if err := domain.ValidateOrganizationPolicy(policy); err != nil {
		return domain.OrganizationPolicy{}, err
	}
	audit := organizationAudit(ctx, policy.OrganizationID, actorID, "organization.policy.updated", "organization", policy.OrganizationID, "", policy.UpdatedAt)
	if err := s.organization.PutOrganizationPolicyWithAudit(ctx, policy, audit); err != nil {
		return domain.OrganizationPolicy{}, err
	}
	return policy, nil
}

func (s *IdentityService) mutateCreateOrganizationRepository(ctx context.Context, actor domain.User, organizationID, name string) (domain.Repository, error) {
	if s.organization == nil {
		return domain.Repository{}, domain.ErrForbidden
	}
	role, ok := s.OrganizationRoleOf(ctx, organizationID, actor.ID)
	if !ok {
		return domain.Repository{}, domain.ErrForbidden
	}
	policy, err := s.effectiveOrganizationPolicy(ctx, organizationID)
	if err != nil {
		return domain.Repository{}, err
	}
	required := domain.OrganizationAdmin
	if policy.RepositoryCreation == domain.OrganizationRepositoryMembers {
		required = domain.OrganizationMember
	}
	if !role.AtLeast(required) {
		return domain.Repository{}, domain.ErrForbidden
	}
	name = strings.TrimSpace(name)
	if !domain.ValidRepositoryName(name) {
		return domain.Repository{}, domain.ErrValidation
	}
	organizationRecord, err := s.organization.GetOrganization(ctx, organizationID)
	if err != nil {
		return domain.Repository{}, err
	}
	slug, err := s.uniqueNamespaceRepositorySlug(ctx, organizationRecord.NamespaceID, name, "")
	if err != nil {
		return domain.Repository{}, err
	}
	now := time.Now().UTC()
	repository := domain.Repository{
		ID:               domain.NewID("ws_"),
		Name:             name,
		OwnerID:          actor.ID,
		OwnerUsername:    organizationRecord.Slug,
		OwnerNamespaceID: organizationRecord.NamespaceID,
		Slug:             slug,
		Visibility:       policy.DefaultRepositoryVisibility,
		CreatedAt:        now,
	}
	owner := domain.Membership{RepositoryID: repository.ID, UserID: actor.ID, Role: domain.RoleOwner, CreatedAt: now}
	audit := organizationAudit(ctx, organizationID, actor.ID, "organization.repository.created", "repository", repository.ID, "", now)
	if err := s.organization.CreateOrganizationRepositoryWithAudit(ctx, repository, owner, audit); err != nil {
		return domain.Repository{}, err
	}
	return repository, nil
}

func (s *IdentityService) uniqueNamespaceRepositorySlug(ctx context.Context, namespaceID, name, selfID string) (string, error) {
	base := domain.RepositorySlug(name)
	candidate := base
	for suffix := 2; ; suffix++ {
		existing, err := s.repositories.GetRepositoryByNamespacePath(ctx, namespaceID, candidate)
		if errors.Is(err, domain.ErrNotFound) || err == nil && existing.ID == selfID {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
		tail := fmt.Sprintf("-%d", suffix)
		candidate = base[:min(len(base), 64-len(tail))] + tail
	}
}

func (s *IdentityService) ListOrganizationRepositories(ctx context.Context, actorID, organizationID string) ([]domain.Repository, error) {
	if role, ok := s.OrganizationRoleOf(ctx, organizationID, actorID); !ok || !role.AtLeast(domain.OrganizationMember) {
		return nil, domain.ErrForbidden
	}
	organizationRecord, err := s.organization.GetOrganization(ctx, organizationID)
	if err != nil {
		return nil, err
	}
	return s.organization.ListRepositoriesForNamespace(ctx, organizationRecord.NamespaceID)
}

func (s *IdentityService) mutateCreateBreakGlassGrant(ctx context.Context, actorID, organizationID, repositoryID, reason string, minutes int) (domain.BreakGlassGrant, error) {
	role, ok := s.OrganizationRoleOf(ctx, organizationID, actorID)
	if !ok || role != domain.OrganizationOwner {
		return domain.BreakGlassGrant{}, domain.ErrForbidden
	}
	policy, err := s.effectiveOrganizationPolicy(ctx, organizationID)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if !policy.BreakGlassEnabled || minutes < 1 || minutes > policy.BreakGlassMaxMinutes {
		return domain.BreakGlassGrant{}, domain.ErrForbidden
	}
	organizationRecord, err := s.organization.GetOrganization(ctx, organizationID)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	repository, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if repository.OwnerNamespaceID != organizationRecord.NamespaceID {
		return domain.BreakGlassGrant{}, domain.ErrForbidden
	}
	now := time.Now().UTC()
	grant := domain.BreakGlassGrant{
		ID:             domain.NewID("bg_"),
		OrganizationID: organizationID,
		RepositoryID:   repositoryID,
		UserID:         actorID,
		Reason:         strings.TrimSpace(reason),
		CreatedAt:      now,
		ExpiresAt:      now.Add(time.Duration(minutes) * time.Minute),
	}
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	created := organizationAudit(ctx, organizationID, actorID, "organization.break_glass.created", "repository", repositoryID, grant.Reason, now)
	if err := s.organization.CreateBreakGlassGrantWithAudit(ctx, grant, created); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	return grant, nil
}

func (s *IdentityService) HasBreakGlassAccess(ctx context.Context, repositoryID, userID string) (bool, error) {
	if s.organization == nil || repositoryID == "" || userID == "" {
		return false, nil
	}
	repository, err := s.repositories.GetRepository(ctx, repositoryID)
	if err != nil || repository.OwnerNamespaceID == "" {
		return false, nil
	}
	ns, err := s.organization.GetNamespace(ctx, repository.OwnerNamespaceID)
	if err != nil || ns.Kind != domain.NamespaceOrganization {
		return false, nil
	}
	role, ok := s.OrganizationRoleOf(ctx, ns.OrganizationID, userID)
	if !ok || role != domain.OrganizationOwner {
		return false, nil
	}
	policy, err := s.effectiveOrganizationPolicy(ctx, ns.OrganizationID)
	if err != nil {
		return false, err
	}
	if !policy.BreakGlassEnabled {
		return false, nil
	}
	now := time.Now().UTC()
	used := organizationAudit(ctx, ns.OrganizationID, userID, "organization.break_glass.used", "repository", repositoryID, "", now)
	_, err = s.organization.UseActiveBreakGlassGrant(ctx, ns.OrganizationID, repositoryID, userID, now, used)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *IdentityService) ListOrganizationAudit(ctx context.Context, actorID, organizationID string, limit int) ([]domain.OrganizationAuditEvent, error) {
	if role, ok := s.OrganizationRoleOf(ctx, organizationID, actorID); !ok || !role.AtLeast(domain.OrganizationAdmin) {
		return nil, domain.ErrForbidden
	}
	return s.organization.ListOrganizationAudit(ctx, organizationID, limit)
}

// OffboardOrganizationMember requires the administrator to choose whether direct
// collaborator grants survive as outside access. Team access is always removed.
func (s *IdentityService) OffboardOrganizationMember(ctx context.Context, actor, org, target, access string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error {
		if access != "revoke" && access != "retain" {
			return fmt.Errorf("%w: choose revoke or retain for direct repository access", domain.ErrValidation)
		}
		if err := s.checkOrganizationRemoval(ctx, actor, org, target); err != nil {
			return err
		}
		var direct []string
		if access == "revoke" {
			organization, err := s.organization.GetOrganization(ctx, org)
			if err != nil {
				return err
			}
			repositories, err := s.organization.ListRepositoriesForNamespace(ctx, organization.NamespaceID)
			if err != nil {
				return err
			}
			for _, repository := range repositories {
				if repository.OwnerID == target {
					return fmt.Errorf("%w: transfer repository %s to another owner before revoking access", domain.ErrConflict, repository.Slug)
				}
				members, err := s.repositories.ListMembers(ctx, repository.ID)
				if err != nil {
					return err
				}
				for _, member := range members {
					if member.UserID == target {
						direct = append(direct, repository.ID)
					}
				}
			}
		}
		// The production identity transaction commits membership, team cascade,
		// direct grants, and audit together, including every affected repository.
		for _, id := range direct {
			if err := s.repositories.RemoveMember(ctx, id, target); err != nil {
				return err
			}
		}
		audit := organizationAudit(ctx, org, actor, "organization.member.removed", "user", target, "repository_access="+access, time.Now().UTC())
		return s.organization.RemoveOrganizationMemberWithAudit(ctx, org, target, audit)
	})
}
