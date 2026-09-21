//go:build postgres

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func pgNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func insertOrganizationAuditTx(ctx context.Context, tx pgx.Tx, event domain.OrganizationAuditEvent) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO organization_audit_events (id,organization_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		event.ID, event.OrganizationID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt)
	return err
}

func (s *PostgresStore) CreateNamespace(ctx context.Context, ns domain.Namespace) error {
	if err := domain.ValidateNamespaceRecord(ns); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return mapPGConstraint(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ns.Slug); err != nil {
		return err
	}
	var aliasID string
	aliasErr := tx.QueryRow(ctx, `SELECT namespace_id FROM namespace_aliases WHERE slug=$1`, ns.Slug).Scan(&aliasID)
	if aliasErr == nil && aliasID != ns.ID {
		return domain.ErrConflict
	}
	if aliasErr != nil && mapNoRows(aliasErr) != domain.ErrNotFound {
		return aliasErr
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO namespaces (id, slug, kind, user_id, organization_id, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`,
		ns.ID, ns.Slug, string(ns.Kind), pgNullableString(ns.UserID), pgNullableString(ns.OrganizationID), ns.CreatedAt)
	if err != nil {
		return mapPGConstraint(err)
	}
	var stored domain.Namespace
	err = tx.QueryRow(ctx,
		`SELECT id,slug,kind,COALESCE(user_id,''),COALESCE(organization_id,''),created_at FROM namespaces WHERE id=$1`, ns.ID).
		Scan(&stored.ID, &stored.Slug, &stored.Kind, &stored.UserID, &stored.OrganizationID, &stored.CreatedAt)
	if err != nil {
		return mapNoRows(err)
	}
	if stored.Slug != ns.Slug || stored.Kind != ns.Kind || stored.UserID != ns.UserID || stored.OrganizationID != ns.OrganizationID {
		return domain.ErrConflict
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) GetNamespace(ctx context.Context, id string) (domain.Namespace, error) {
	if err := domain.ValidateNamespaceID(id); err != nil {
		return domain.Namespace{}, err
	}
	var ns domain.Namespace
	err := s.db(ctx).QueryRow(ctx,
		`SELECT id, slug, kind, COALESCE(user_id,''), COALESCE(organization_id,''), created_at FROM namespaces WHERE id=$1`, id).
		Scan(&ns.ID, &ns.Slug, &ns.Kind, &ns.UserID, &ns.OrganizationID, &ns.CreatedAt)
	if err != nil {
		return domain.Namespace{}, mapNoRows(err)
	}
	if err := domain.ValidateNamespaceRecord(ns); err != nil {
		return domain.Namespace{}, storedIdentityIntegrity(err)
	}
	return ns, nil
}

func (s *PostgresStore) GetNamespaceBySlug(ctx context.Context, slug string) (domain.Namespace, error) {
	if !domain.ValidNamespaceSlug(slug) {
		return domain.Namespace{}, domain.ErrValidation
	}
	var ns domain.Namespace
	err := s.db(ctx).QueryRow(ctx,
		`SELECT n.id,n.slug,n.kind,COALESCE(n.user_id,''),COALESCE(n.organization_id,''),n.created_at
		 FROM namespaces n WHERE n.slug=$1
		 UNION ALL
		 SELECT n.id,n.slug,n.kind,COALESCE(n.user_id,''),COALESCE(n.organization_id,''),n.created_at
		 FROM namespace_aliases a JOIN namespaces n ON n.id=a.namespace_id WHERE a.slug=$1
		 LIMIT 1`, slug).
		Scan(&ns.ID, &ns.Slug, &ns.Kind, &ns.UserID, &ns.OrganizationID, &ns.CreatedAt)
	if err != nil {
		return domain.Namespace{}, mapNoRows(err)
	}
	if err := domain.ValidateNamespaceRecord(ns); err != nil {
		return domain.Namespace{}, storedIdentityIntegrity(err)
	}
	return ns, nil
}

func (s *PostgresStore) RenameNamespace(ctx context.Context, id, nextSlug string) error {
	if err := domain.ValidateNamespaceID(id); err != nil {
		return err
	}
	if !domain.ValidNamespaceSlug(nextSlug) {
		return domain.ErrValidation
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, nextSlug); err != nil {
		return err
	}
	var currentSlug string
	if err := tx.QueryRow(ctx, `SELECT slug FROM namespaces WHERE id=$1 FOR UPDATE`, id).Scan(&currentSlug); err != nil {
		return mapNoRows(err)
	}
	if currentSlug == nextSlug {
		return tx.Commit(ctx)
	}
	var claimedID string
	claimErr := tx.QueryRow(ctx,
		`SELECT id FROM namespaces WHERE slug=$1
		 UNION ALL SELECT namespace_id FROM namespace_aliases WHERE slug=$1 LIMIT 1`, nextSlug).Scan(&claimedID)
	if claimErr == nil && claimedID != id {
		return domain.ErrConflict
	}
	if claimErr != nil && mapNoRows(claimErr) != domain.ErrNotFound {
		return claimErr
	}
	if _, err := tx.Exec(ctx, `DELETE FROM namespace_aliases WHERE slug=$1 AND namespace_id=$2`, nextSlug, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO namespace_aliases (slug,namespace_id) VALUES ($1,$2)
		 ON CONFLICT (slug) DO UPDATE SET namespace_id=EXCLUDED.namespace_id`, currentSlug, id); err != nil {
		return mapPGConstraint(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE namespaces SET slug=$1 WHERE id=$2`, nextSlug, id); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) CreateOrganization(
	ctx context.Context,
	organizationRecord domain.Organization,
	ns domain.Namespace,
	owner domain.OrganizationMembership,
	policy domain.OrganizationPolicy,
	audit domain.OrganizationAuditEvent,
) error {
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return err
	}
	if err := domain.ValidateNamespaceRecord(ns); err != nil || ns.ID != organizationRecord.NamespaceID || ns.OrganizationID != organizationRecord.ID || ns.Slug != organizationRecord.Slug {
		return domain.ErrValidation
	}
	if err := domain.ValidateOrganizationMembershipRecord(owner); err != nil || owner.OrganizationID != organizationRecord.ID || owner.Role != domain.OrganizationOwner {
		return domain.ErrValidation
	}
	if err := domain.ValidateOrganizationPolicy(policy); err != nil || policy.OrganizationID != organizationRecord.ID {
		return domain.ErrValidation
	}
	if err := domain.ValidateOrganizationAuditEvent(audit); err != nil || audit.OrganizationID != organizationRecord.ID {
		return domain.ErrValidation
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, ns.Slug); err != nil {
		return err
	}
	var aliasID string
	aliasErr := tx.QueryRow(ctx, `SELECT namespace_id FROM namespace_aliases WHERE slug=$1`, ns.Slug).Scan(&aliasID)
	if aliasErr == nil {
		return domain.ErrConflict
	}
	if mapNoRows(aliasErr) != domain.ErrNotFound {
		return aliasErr
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO namespaces (id,slug,kind,organization_id,created_at) VALUES ($1,$2,'organization',$3,$4)`,
		ns.ID, ns.Slug, organizationRecord.ID, ns.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO organizations (id,namespace_id,name,slug,logo,created_by,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		organizationRecord.ID, organizationRecord.NamespaceID, organizationRecord.Name, organizationRecord.Slug, organizationRecord.Logo, organizationRecord.CreatedBy, organizationRecord.CreatedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO organization_memberships (organization_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)`,
		owner.OrganizationID, owner.UserID, string(owner.Role), owner.CreatedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO organization_policies (organization_id,repository_creation,default_repository_visibility,allow_public_repositories,break_glass_enabled,break_glass_max_minutes,updated_by,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		policy.OrganizationID, string(policy.RepositoryCreation), string(policy.DefaultRepositoryVisibility), policy.AllowPublicRepositories,
		policy.BreakGlassEnabled, policy.BreakGlassMaxMinutes, policy.UpdatedBy, policy.UpdatedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO organization_audit_events (id,organization_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		audit.ID, audit.OrganizationID, audit.ActorID, audit.Action, audit.TargetType, audit.TargetID, audit.Reason, audit.CreatedAt); err != nil {
		return err
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func scanOrganization(row interface{ Scan(...any) error }) (domain.Organization, error) {
	var organizationRecord domain.Organization
	err := row.Scan(&organizationRecord.ID, &organizationRecord.NamespaceID, &organizationRecord.Name, &organizationRecord.Slug, &organizationRecord.Logo, &organizationRecord.CreatedBy, &organizationRecord.CreatedAt)
	if err != nil {
		return domain.Organization{}, mapNoRows(err)
	}
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return domain.Organization{}, storedIdentityIntegrity(err)
	}
	return organizationRecord, nil
}

const organizationSelect = `SELECT id,namespace_id,name,slug,COALESCE(logo,''),created_by,created_at FROM organizations`

func (s *PostgresStore) GetOrganization(ctx context.Context, id string) (domain.Organization, error) {
	if err := domain.ValidateOrganizationID(id); err != nil {
		return domain.Organization{}, err
	}
	return scanOrganization(s.db(ctx).QueryRow(ctx, organizationSelect+` WHERE id=$1`, id))
}

func (s *PostgresStore) GetOrganizationBySlug(ctx context.Context, slug string) (domain.Organization, error) {
	if !domain.ValidNamespaceSlug(slug) {
		return domain.Organization{}, domain.ErrValidation
	}
	return scanOrganization(s.db(ctx).QueryRow(ctx,
		organizationSelect+` WHERE namespace_id=(
			SELECT id FROM namespaces WHERE slug=$1
			UNION ALL
			SELECT namespace_id FROM namespace_aliases WHERE slug=$1
			LIMIT 1
		)`, slug))
}

func (s *PostgresStore) UpdateOrganization(ctx context.Context, organizationRecord domain.Organization) error {
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return err
	}
	result, err := s.db(ctx).Exec(ctx,
		`UPDATE organizations SET name=$1,logo=$2
		 WHERE id=$3 AND namespace_id=$4 AND slug=$5 AND created_by=$6 AND created_at=$7`,
		organizationRecord.Name, organizationRecord.Logo, organizationRecord.ID, organizationRecord.NamespaceID, organizationRecord.Slug, organizationRecord.CreatedBy, organizationRecord.CreatedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}

func (s *PostgresStore) UpdateOrganizationWithAudit(ctx context.Context, organizationRecord domain.Organization, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationRecord(organizationRecord); err != nil {
		return err
	}
	if err := validateOrganizationMutationAudit(event, organizationRecord.ID, "organization.profile.updated", "organization", organizationRecord.ID); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx,
		`UPDATE organizations SET name=$1,logo=$2
		 WHERE id=$3 AND namespace_id=$4 AND slug=$5 AND created_by=$6 AND created_at=$7`,
		organizationRecord.Name, organizationRecord.Logo, organizationRecord.ID, organizationRecord.NamespaceID, organizationRecord.Slug, organizationRecord.CreatedBy, organizationRecord.CreatedAt)
	if err != nil {
		return mapPGConstraint(err)
	}
	if result.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	if err := insertOrganizationAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) ListOrganizationsForUser(ctx context.Context, userID string) ([]domain.Organization, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx,
		`SELECT e.id,e.namespace_id,e.name,e.slug,COALESCE(e.logo,''),e.created_by,e.created_at
		 FROM organizations e JOIN organization_memberships m ON m.organization_id=e.id
		 WHERE m.user_id=$1 ORDER BY e.slug`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Organization
	for rows.Next() {
		organizationRecord, err := scanOrganization(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, organizationRecord)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AddOrganizationMember(ctx context.Context, member domain.OrganizationMembership) error {
	if err := domain.ValidateOrganizationMembershipRecord(member); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx,
		`INSERT INTO organization_memberships (organization_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (organization_id,user_id) DO UPDATE SET role=EXCLUDED.role`,
		member.OrganizationID, member.UserID, string(member.Role), member.CreatedAt)
	return mapPGConstraint(err)
}

func (s *PostgresStore) AddOrganizationMemberWithAudit(ctx context.Context, member domain.OrganizationMembership, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationMembershipRecord(member); err != nil {
		return err
	}
	if err := validateOrganizationMutationAudit(event, member.OrganizationID, "organization.member.updated", "user", member.UserID); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO organization_memberships (organization_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (organization_id,user_id) DO UPDATE SET role=EXCLUDED.role`,
		member.OrganizationID, member.UserID, string(member.Role), member.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if err := insertOrganizationAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) RemoveOrganizationMember(ctx context.Context, organizationID, userID string) error {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `DELETE FROM organization_memberships WHERE organization_id=$1 AND user_id=$2`, organizationID, userID)
	return mapPGConstraint(err)
}

func (s *PostgresStore) RemoveOrganizationMemberWithAudit(ctx context.Context, organizationID, userID string, event domain.OrganizationAuditEvent) error {
	if err := validateOrganizationMutationAudit(event, organizationID, "organization.member.removed", "user", userID); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `DELETE FROM organization_memberships WHERE organization_id=$1 AND user_id=$2`, organizationID, userID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	if err := insertOrganizationAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) GetOrganizationMembership(ctx context.Context, organizationID, userID string) (domain.OrganizationMembership, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return domain.OrganizationMembership{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.OrganizationMembership{}, err
	}
	var member domain.OrganizationMembership
	err := s.db(ctx).QueryRow(ctx,
		`SELECT organization_id,user_id,role,created_at FROM organization_memberships WHERE organization_id=$1 AND user_id=$2`,
		organizationID, userID).Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt)
	if err != nil {
		return domain.OrganizationMembership{}, mapNoRows(err)
	}
	if err := domain.ValidateOrganizationMembershipRecord(member); err != nil {
		return domain.OrganizationMembership{}, storedIdentityIntegrity(err)
	}
	return member, nil
}

func (s *PostgresStore) ListOrganizationMembers(ctx context.Context, organizationID string) ([]domain.OrganizationMembership, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx,
		`SELECT m.organization_id,m.user_id,m.role,m.created_at,u.email,u.name,COALESCE(u.username,''),COALESCE(u.nickname,''),COALESCE(u.avatar,'')
		 FROM organization_memberships m JOIN users u ON u.id=m.user_id
		 WHERE m.organization_id=$1 ORDER BY m.created_at`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OrganizationMembership
	for rows.Next() {
		var member domain.OrganizationMembership
		var user domain.User
		if err := rows.Scan(&member.OrganizationID, &member.UserID, &member.Role, &member.CreatedAt, &user.Email, &user.Name, &user.Username, &user.Nickname, &user.Avatar); err != nil {
			return nil, err
		}
		user.ID = member.UserID
		member.User = &user
		if err := domain.ValidateOrganizationMembershipRecord(member); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		if err := domain.ValidateUserRecord(user); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, member)
	}
	return out, rows.Err()
}

func (s *PostgresStore) PutOrganizationPolicy(ctx context.Context, policy domain.OrganizationPolicy) error {
	if err := domain.ValidateOrganizationPolicy(policy); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx,
		`INSERT INTO organization_policies (organization_id,repository_creation,default_repository_visibility,allow_public_repositories,break_glass_enabled,break_glass_max_minutes,updated_by,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (organization_id) DO UPDATE SET repository_creation=EXCLUDED.repository_creation,
		 default_repository_visibility=EXCLUDED.default_repository_visibility,allow_public_repositories=EXCLUDED.allow_public_repositories,
		 break_glass_enabled=EXCLUDED.break_glass_enabled,break_glass_max_minutes=EXCLUDED.break_glass_max_minutes,
		 updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`,
		policy.OrganizationID, string(policy.RepositoryCreation), string(policy.DefaultRepositoryVisibility), policy.AllowPublicRepositories,
		policy.BreakGlassEnabled, policy.BreakGlassMaxMinutes, pgNullableString(policy.UpdatedBy), policy.UpdatedAt)
	return err
}

func (s *PostgresStore) PutOrganizationPolicyWithAudit(ctx context.Context, policy domain.OrganizationPolicy, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationPolicy(policy); err != nil {
		return err
	}
	if err := validateOrganizationMutationAudit(event, policy.OrganizationID, "organization.policy.updated", "organization", policy.OrganizationID); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO organization_policies (organization_id,repository_creation,default_repository_visibility,allow_public_repositories,break_glass_enabled,break_glass_max_minutes,updated_by,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (organization_id) DO UPDATE SET repository_creation=EXCLUDED.repository_creation,
		 default_repository_visibility=EXCLUDED.default_repository_visibility,allow_public_repositories=EXCLUDED.allow_public_repositories,
		 break_glass_enabled=EXCLUDED.break_glass_enabled,break_glass_max_minutes=EXCLUDED.break_glass_max_minutes,
		 updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`,
		policy.OrganizationID, string(policy.RepositoryCreation), string(policy.DefaultRepositoryVisibility), policy.AllowPublicRepositories,
		policy.BreakGlassEnabled, policy.BreakGlassMaxMinutes, pgNullableString(policy.UpdatedBy), policy.UpdatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if err := insertOrganizationAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) GetOrganizationPolicy(ctx context.Context, organizationID string) (domain.OrganizationPolicy, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return domain.OrganizationPolicy{}, err
	}
	var policy domain.OrganizationPolicy
	err := s.db(ctx).QueryRow(ctx,
		`SELECT organization_id,repository_creation,default_repository_visibility,allow_public_repositories,break_glass_enabled,break_glass_max_minutes,COALESCE(updated_by,''),updated_at
		 FROM organization_policies WHERE organization_id=$1`, organizationID).
		Scan(&policy.OrganizationID, &policy.RepositoryCreation, &policy.DefaultRepositoryVisibility, &policy.AllowPublicRepositories,
			&policy.BreakGlassEnabled, &policy.BreakGlassMaxMinutes, &policy.UpdatedBy, &policy.UpdatedAt)
	if err != nil {
		return domain.OrganizationPolicy{}, mapNoRows(err)
	}
	if err := domain.ValidateOrganizationPolicy(policy); err != nil {
		return domain.OrganizationPolicy{}, storedIdentityIntegrity(err)
	}
	return policy, nil
}

func (s *PostgresStore) ListRepositoriesForNamespace(ctx context.Context, namespaceID string) ([]domain.Repository, error) {
	if err := domain.ValidateNamespaceID(namespaceID); err != nil {
		return nil, err
	}
	rows, err := s.db(ctx).Query(ctx,
		`SELECT id,name,owner_id,COALESCE(slug,''),COALESCE(owner_username,''),COALESCE(owner_namespace_id,''),COALESCE(visibility,''),COALESCE(secrets_policy,''),COALESCE(settings_policy,''),COALESCE(gh_visibility_sync,false),gh_synced_at,COALESCE(archived,false),COALESCE(webhook_url,''),COALESCE(public_role,''),created_at
		 FROM repositories WHERE owner_namespace_id=$1 ORDER BY created_at`, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Repository
	for rows.Next() {
		var repository domain.Repository
		if err := rows.Scan(&repository.ID, &repository.Name, &repository.OwnerID, &repository.Slug, &repository.OwnerUsername,
			&repository.OwnerNamespaceID, &repository.Visibility, &repository.SecretsPolicy, &repository.SettingsPolicy,
			&repository.GHVisibilitySync, &repository.GHSyncedAt, &repository.Archived, &repository.WebhookURL,
			&repository.PublicRole, &repository.CreatedAt); err != nil {
			return nil, err
		}
		if err := domain.ValidateRepositoryRecord(repository); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, repository)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateOrganizationRepositoryWithAudit(ctx context.Context, repository domain.Repository, owner domain.Membership, event domain.OrganizationAuditEvent) error {
	if err := validateOrganizationRepositoryMutation(repository, owner, event); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var organizationID string
	if err := tx.QueryRow(ctx,
		`SELECT organization_id FROM namespaces WHERE id=$1 AND kind='organization' FOR SHARE`, repository.OwnerNamespaceID).
		Scan(&organizationID); err != nil {
		return mapNoRows(err)
	}
	if organizationID != event.OrganizationID {
		return domain.ErrValidation
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO repositories (id,name,owner_id,slug,owner_username,owner_namespace_id,visibility,secrets_policy,settings_policy,gh_visibility_sync,gh_synced_at,archived,webhook_url,public_role,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		repository.ID, repository.Name, repository.OwnerID, repository.Slug, repository.OwnerUsername, repository.OwnerNamespaceID,
		string(repository.Visibility), repository.SecretsPolicy, repository.SettingsPolicy, repository.GHVisibilitySync,
		repository.GHSyncedAt, repository.Archived, repository.WebhookURL, repository.PublicRole, repository.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO memberships (repository_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)`,
		owner.RepositoryID, owner.UserID, string(owner.Role), owner.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if err := insertOrganizationAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateOrganizationAuditEvent(event); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx,
		`INSERT INTO organization_audit_events (id,organization_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (id) DO NOTHING`,
		event.ID, event.OrganizationID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt)
	return err
}

func (s *PostgresStore) ListOrganizationAudit(ctx context.Context, organizationID string, limit int) ([]domain.OrganizationAuditEvent, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db(ctx).Query(ctx,
		`SELECT id,organization_id,actor_id,action,target_type,target_id,reason,created_at
		 FROM organization_audit_events WHERE organization_id=$1 ORDER BY created_at DESC LIMIT $2`, organizationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OrganizationAuditEvent
	for rows.Next() {
		var event domain.OrganizationAuditEvent
		if err := rows.Scan(&event.ID, &event.OrganizationID, &event.ActorID, &event.Action, &event.TargetType, &event.TargetID, &event.Reason, &event.CreatedAt); err != nil {
			return nil, err
		}
		if err := domain.ValidateOrganizationAuditEvent(event); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateBreakGlassGrant(ctx context.Context, grant domain.BreakGlassGrant) error {
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx,
		`INSERT INTO organization_break_glass_grants (id,organization_id,repository_id,user_id,reason,created_at,expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		grant.ID, grant.OrganizationID, grant.RepositoryID, grant.UserID, grant.Reason, grant.CreatedAt, grant.ExpiresAt)
	return err
}

func (s *PostgresStore) CreateBreakGlassGrantWithAudit(ctx context.Context, grant domain.BreakGlassGrant, event domain.OrganizationAuditEvent) error {
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return err
	}
	if err := domain.ValidateOrganizationAuditEvent(event); err != nil || event.OrganizationID != grant.OrganizationID || event.ActorID != grant.UserID || event.Action != "organization.break_glass.created" {
		return domain.ErrValidation
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx,
		`INSERT INTO organization_break_glass_grants (id,organization_id,repository_id,user_id,reason,created_at,expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		grant.ID, grant.OrganizationID, grant.RepositoryID, grant.UserID, grant.Reason, grant.CreatedAt, grant.ExpiresAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO organization_audit_events (id,organization_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		event.ID, event.OrganizationID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetActiveBreakGlassGrant(ctx context.Context, organizationID, repositoryID, userID string, now time.Time) (domain.BreakGlassGrant, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateRepositoryID(repositoryID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	var grant domain.BreakGlassGrant
	err := s.db(ctx).QueryRow(ctx,
		`SELECT id,organization_id,repository_id,user_id,reason,created_at,expires_at
		 FROM organization_break_glass_grants
		 WHERE organization_id=$1 AND repository_id=$2 AND user_id=$3 AND expires_at>$4
		 ORDER BY expires_at DESC LIMIT 1`, organizationID, repositoryID, userID, now).
		Scan(&grant.ID, &grant.OrganizationID, &grant.RepositoryID, &grant.UserID, &grant.Reason, &grant.CreatedAt, &grant.ExpiresAt)
	if err != nil {
		return domain.BreakGlassGrant{}, mapNoRows(err)
	}
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return domain.BreakGlassGrant{}, fmt.Errorf("%w: invalid stored break-glass grant", domain.ErrIntegrity)
	}
	return grant, nil
}

func (s *PostgresStore) UseActiveBreakGlassGrant(ctx context.Context, organizationID, repositoryID, userID string, now time.Time, event domain.OrganizationAuditEvent) (domain.BreakGlassGrant, error) {
	if err := domain.ValidateOrganizationID(organizationID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateRepositoryID(repositoryID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var grant domain.BreakGlassGrant
	err = tx.QueryRow(ctx,
		`SELECT id,organization_id,repository_id,user_id,reason,created_at,expires_at
		 FROM organization_break_glass_grants
		 WHERE organization_id=$1 AND repository_id=$2 AND user_id=$3 AND expires_at>$4
		 ORDER BY expires_at DESC LIMIT 1 FOR UPDATE`, organizationID, repositoryID, userID, now).
		Scan(&grant.ID, &grant.OrganizationID, &grant.RepositoryID, &grant.UserID, &grant.Reason, &grant.CreatedAt, &grant.ExpiresAt)
	if err != nil {
		return domain.BreakGlassGrant{}, mapNoRows(err)
	}
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return domain.BreakGlassGrant{}, fmt.Errorf("%w: invalid stored break-glass grant", domain.ErrIntegrity)
	}
	event.Reason = grant.Reason
	if err := domain.ValidateOrganizationAuditEvent(event); err != nil || event.OrganizationID != organizationID || event.ActorID != userID || event.Action != "organization.break_glass.used" || event.TargetID != repositoryID {
		return domain.BreakGlassGrant{}, domain.ErrValidation
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO organization_audit_events (id,organization_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		event.ID, event.OrganizationID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	return grant, nil
}
