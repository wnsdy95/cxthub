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

func insertEnterpriseAuditTx(ctx context.Context, tx pgx.Tx, event domain.EnterpriseAuditEvent) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO enterprise_audit_events (id,enterprise_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		event.ID, event.EnterpriseID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt)
	return err
}

func (s *PostgresStore) CreateNamespace(ctx context.Context, ns domain.Namespace) error {
	if err := domain.ValidateNamespaceRecord(ns); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
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
		`INSERT INTO namespaces (id, slug, kind, user_id, enterprise_id, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (id) DO NOTHING`,
		ns.ID, ns.Slug, string(ns.Kind), pgNullableString(ns.UserID), pgNullableString(ns.EnterpriseID), ns.CreatedAt)
	if err != nil {
		return mapPGConstraint(err)
	}
	var stored domain.Namespace
	err = tx.QueryRow(ctx,
		`SELECT id,slug,kind,COALESCE(user_id,''),COALESCE(enterprise_id,''),created_at FROM namespaces WHERE id=$1`, ns.ID).
		Scan(&stored.ID, &stored.Slug, &stored.Kind, &stored.UserID, &stored.EnterpriseID, &stored.CreatedAt)
	if err != nil {
		return mapNoRows(err)
	}
	if stored.Slug != ns.Slug || stored.Kind != ns.Kind || stored.UserID != ns.UserID || stored.EnterpriseID != ns.EnterpriseID {
		return domain.ErrConflict
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) GetNamespace(ctx context.Context, id string) (domain.Namespace, error) {
	if err := domain.ValidateNamespaceID(id); err != nil {
		return domain.Namespace{}, err
	}
	var ns domain.Namespace
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, kind, COALESCE(user_id,''), COALESCE(enterprise_id,''), created_at FROM namespaces WHERE id=$1`, id).
		Scan(&ns.ID, &ns.Slug, &ns.Kind, &ns.UserID, &ns.EnterpriseID, &ns.CreatedAt)
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
	err := s.pool.QueryRow(ctx,
		`SELECT n.id,n.slug,n.kind,COALESCE(n.user_id,''),COALESCE(n.enterprise_id,''),n.created_at
		 FROM namespaces n WHERE n.slug=$1
		 UNION ALL
		 SELECT n.id,n.slug,n.kind,COALESCE(n.user_id,''),COALESCE(n.enterprise_id,''),n.created_at
		 FROM namespace_aliases a JOIN namespaces n ON n.id=a.namespace_id WHERE a.slug=$1
		 LIMIT 1`, slug).
		Scan(&ns.ID, &ns.Slug, &ns.Kind, &ns.UserID, &ns.EnterpriseID, &ns.CreatedAt)
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
	tx, err := s.pool.Begin(ctx)
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

func (s *PostgresStore) CreateEnterprise(
	ctx context.Context,
	ent domain.Enterprise,
	ns domain.Namespace,
	owner domain.EnterpriseMembership,
	policy domain.EnterprisePolicy,
	audit domain.EnterpriseAuditEvent,
) error {
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return err
	}
	if err := domain.ValidateNamespaceRecord(ns); err != nil || ns.ID != ent.NamespaceID || ns.EnterpriseID != ent.ID || ns.Slug != ent.Slug {
		return domain.ErrValidation
	}
	if err := domain.ValidateEnterpriseMembershipRecord(owner); err != nil || owner.EnterpriseID != ent.ID || owner.Role != domain.EnterpriseOwner {
		return domain.ErrValidation
	}
	if err := domain.ValidateEnterprisePolicy(policy); err != nil || policy.EnterpriseID != ent.ID {
		return domain.ErrValidation
	}
	if err := domain.ValidateEnterpriseAuditEvent(audit); err != nil || audit.EnterpriseID != ent.ID {
		return domain.ErrValidation
	}
	tx, err := s.pool.Begin(ctx)
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
		`INSERT INTO namespaces (id,slug,kind,enterprise_id,created_at) VALUES ($1,$2,'enterprise',$3,$4)`,
		ns.ID, ns.Slug, ent.ID, ns.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO enterprises (id,namespace_id,name,slug,logo,created_by,created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		ent.ID, ent.NamespaceID, ent.Name, ent.Slug, ent.Logo, ent.CreatedBy, ent.CreatedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO enterprise_memberships (enterprise_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)`,
		owner.EnterpriseID, owner.UserID, string(owner.Role), owner.CreatedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO enterprise_policies (enterprise_id,workspace_creation,default_workspace_visibility,allow_public_workspaces,break_glass_enabled,break_glass_max_minutes,updated_by,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		policy.EnterpriseID, string(policy.WorkspaceCreation), string(policy.DefaultWorkspaceVisibility), policy.AllowPublicWorkspaces,
		policy.BreakGlassEnabled, policy.BreakGlassMaxMinutes, policy.UpdatedBy, policy.UpdatedAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO enterprise_audit_events (id,enterprise_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		audit.ID, audit.EnterpriseID, audit.ActorID, audit.Action, audit.TargetType, audit.TargetID, audit.Reason, audit.CreatedAt); err != nil {
		return err
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func scanEnterprise(row interface{ Scan(...any) error }) (domain.Enterprise, error) {
	var ent domain.Enterprise
	err := row.Scan(&ent.ID, &ent.NamespaceID, &ent.Name, &ent.Slug, &ent.Logo, &ent.CreatedBy, &ent.CreatedAt)
	if err != nil {
		return domain.Enterprise{}, mapNoRows(err)
	}
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return domain.Enterprise{}, storedIdentityIntegrity(err)
	}
	return ent, nil
}

const enterpriseSelect = `SELECT id,namespace_id,name,slug,COALESCE(logo,''),created_by,created_at FROM enterprises`

func (s *PostgresStore) GetEnterprise(ctx context.Context, id string) (domain.Enterprise, error) {
	if err := domain.ValidateEnterpriseID(id); err != nil {
		return domain.Enterprise{}, err
	}
	return scanEnterprise(s.pool.QueryRow(ctx, enterpriseSelect+` WHERE id=$1`, id))
}

func (s *PostgresStore) GetEnterpriseBySlug(ctx context.Context, slug string) (domain.Enterprise, error) {
	if !domain.ValidNamespaceSlug(slug) {
		return domain.Enterprise{}, domain.ErrValidation
	}
	return scanEnterprise(s.pool.QueryRow(ctx,
		enterpriseSelect+` WHERE namespace_id=(
			SELECT id FROM namespaces WHERE slug=$1
			UNION ALL
			SELECT namespace_id FROM namespace_aliases WHERE slug=$1
			LIMIT 1
		)`, slug))
}

func (s *PostgresStore) UpdateEnterprise(ctx context.Context, ent domain.Enterprise) error {
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return err
	}
	result, err := s.pool.Exec(ctx,
		`UPDATE enterprises SET name=$1,logo=$2
		 WHERE id=$3 AND namespace_id=$4 AND slug=$5 AND created_by=$6 AND created_at=$7`,
		ent.Name, ent.Logo, ent.ID, ent.NamespaceID, ent.Slug, ent.CreatedBy, ent.CreatedAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	return nil
}

func (s *PostgresStore) UpdateEnterpriseWithAudit(ctx context.Context, ent domain.Enterprise, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterpriseRecord(ent); err != nil {
		return err
	}
	if err := validateEnterpriseMutationAudit(event, ent.ID, "enterprise.profile.updated", "enterprise", ent.ID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx,
		`UPDATE enterprises SET name=$1,logo=$2
		 WHERE id=$3 AND namespace_id=$4 AND slug=$5 AND created_by=$6 AND created_at=$7`,
		ent.Name, ent.Logo, ent.ID, ent.NamespaceID, ent.Slug, ent.CreatedBy, ent.CreatedAt)
	if err != nil {
		return mapPGConstraint(err)
	}
	if result.RowsAffected() != 1 {
		return domain.ErrConflict
	}
	if err := insertEnterpriseAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) ListEnterprisesForUser(ctx context.Context, userID string) ([]domain.Enterprise, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT e.id,e.namespace_id,e.name,e.slug,COALESCE(e.logo,''),e.created_by,e.created_at
		 FROM enterprises e JOIN enterprise_memberships m ON m.enterprise_id=e.id
		 WHERE m.user_id=$1 ORDER BY e.slug`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Enterprise
	for rows.Next() {
		ent, err := scanEnterprise(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ent)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AddEnterpriseMember(ctx context.Context, member domain.EnterpriseMembership) error {
	if err := domain.ValidateEnterpriseMembershipRecord(member); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO enterprise_memberships (enterprise_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (enterprise_id,user_id) DO UPDATE SET role=EXCLUDED.role`,
		member.EnterpriseID, member.UserID, string(member.Role), member.CreatedAt)
	return mapPGConstraint(err)
}

func (s *PostgresStore) AddEnterpriseMemberWithAudit(ctx context.Context, member domain.EnterpriseMembership, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterpriseMembershipRecord(member); err != nil {
		return err
	}
	if err := validateEnterpriseMutationAudit(event, member.EnterpriseID, "enterprise.member.updated", "user", member.UserID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO enterprise_memberships (enterprise_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (enterprise_id,user_id) DO UPDATE SET role=EXCLUDED.role`,
		member.EnterpriseID, member.UserID, string(member.Role), member.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if err := insertEnterpriseAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) RemoveEnterpriseMember(ctx context.Context, enterpriseID, userID string) error {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM enterprise_memberships WHERE enterprise_id=$1 AND user_id=$2`, enterpriseID, userID)
	return mapPGConstraint(err)
}

func (s *PostgresStore) RemoveEnterpriseMemberWithAudit(ctx context.Context, enterpriseID, userID string, event domain.EnterpriseAuditEvent) error {
	if err := validateEnterpriseMutationAudit(event, enterpriseID, "enterprise.member.removed", "user", userID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `DELETE FROM enterprise_memberships WHERE enterprise_id=$1 AND user_id=$2`, enterpriseID, userID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	if err := insertEnterpriseAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) GetEnterpriseMembership(ctx context.Context, enterpriseID, userID string) (domain.EnterpriseMembership, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return domain.EnterpriseMembership{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.EnterpriseMembership{}, err
	}
	var member domain.EnterpriseMembership
	err := s.pool.QueryRow(ctx,
		`SELECT enterprise_id,user_id,role,created_at FROM enterprise_memberships WHERE enterprise_id=$1 AND user_id=$2`,
		enterpriseID, userID).Scan(&member.EnterpriseID, &member.UserID, &member.Role, &member.CreatedAt)
	if err != nil {
		return domain.EnterpriseMembership{}, mapNoRows(err)
	}
	if err := domain.ValidateEnterpriseMembershipRecord(member); err != nil {
		return domain.EnterpriseMembership{}, storedIdentityIntegrity(err)
	}
	return member, nil
}

func (s *PostgresStore) ListEnterpriseMembers(ctx context.Context, enterpriseID string) ([]domain.EnterpriseMembership, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.enterprise_id,m.user_id,m.role,m.created_at,u.email,u.name,COALESCE(u.username,''),COALESCE(u.nickname,''),COALESCE(u.avatar,'')
		 FROM enterprise_memberships m JOIN users u ON u.id=m.user_id
		 WHERE m.enterprise_id=$1 ORDER BY m.created_at`, enterpriseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EnterpriseMembership
	for rows.Next() {
		var member domain.EnterpriseMembership
		var user domain.User
		if err := rows.Scan(&member.EnterpriseID, &member.UserID, &member.Role, &member.CreatedAt, &user.Email, &user.Name, &user.Username, &user.Nickname, &user.Avatar); err != nil {
			return nil, err
		}
		user.ID = member.UserID
		member.User = &user
		if err := domain.ValidateEnterpriseMembershipRecord(member); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		if err := domain.ValidateUserRecord(user); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, member)
	}
	return out, rows.Err()
}

func (s *PostgresStore) PutEnterprisePolicy(ctx context.Context, policy domain.EnterprisePolicy) error {
	if err := domain.ValidateEnterprisePolicy(policy); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO enterprise_policies (enterprise_id,workspace_creation,default_workspace_visibility,allow_public_workspaces,break_glass_enabled,break_glass_max_minutes,updated_by,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (enterprise_id) DO UPDATE SET workspace_creation=EXCLUDED.workspace_creation,
		 default_workspace_visibility=EXCLUDED.default_workspace_visibility,allow_public_workspaces=EXCLUDED.allow_public_workspaces,
		 break_glass_enabled=EXCLUDED.break_glass_enabled,break_glass_max_minutes=EXCLUDED.break_glass_max_minutes,
		 updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`,
		policy.EnterpriseID, string(policy.WorkspaceCreation), string(policy.DefaultWorkspaceVisibility), policy.AllowPublicWorkspaces,
		policy.BreakGlassEnabled, policy.BreakGlassMaxMinutes, pgNullableString(policy.UpdatedBy), policy.UpdatedAt)
	return err
}

func (s *PostgresStore) PutEnterprisePolicyWithAudit(ctx context.Context, policy domain.EnterprisePolicy, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterprisePolicy(policy); err != nil {
		return err
	}
	if err := validateEnterpriseMutationAudit(event, policy.EnterpriseID, "enterprise.policy.updated", "enterprise", policy.EnterpriseID); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO enterprise_policies (enterprise_id,workspace_creation,default_workspace_visibility,allow_public_workspaces,break_glass_enabled,break_glass_max_minutes,updated_by,updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (enterprise_id) DO UPDATE SET workspace_creation=EXCLUDED.workspace_creation,
		 default_workspace_visibility=EXCLUDED.default_workspace_visibility,allow_public_workspaces=EXCLUDED.allow_public_workspaces,
		 break_glass_enabled=EXCLUDED.break_glass_enabled,break_glass_max_minutes=EXCLUDED.break_glass_max_minutes,
		 updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`,
		policy.EnterpriseID, string(policy.WorkspaceCreation), string(policy.DefaultWorkspaceVisibility), policy.AllowPublicWorkspaces,
		policy.BreakGlassEnabled, policy.BreakGlassMaxMinutes, pgNullableString(policy.UpdatedBy), policy.UpdatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if err := insertEnterpriseAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) GetEnterprisePolicy(ctx context.Context, enterpriseID string) (domain.EnterprisePolicy, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return domain.EnterprisePolicy{}, err
	}
	var policy domain.EnterprisePolicy
	err := s.pool.QueryRow(ctx,
		`SELECT enterprise_id,workspace_creation,default_workspace_visibility,allow_public_workspaces,break_glass_enabled,break_glass_max_minutes,COALESCE(updated_by,''),updated_at
		 FROM enterprise_policies WHERE enterprise_id=$1`, enterpriseID).
		Scan(&policy.EnterpriseID, &policy.WorkspaceCreation, &policy.DefaultWorkspaceVisibility, &policy.AllowPublicWorkspaces,
			&policy.BreakGlassEnabled, &policy.BreakGlassMaxMinutes, &policy.UpdatedBy, &policy.UpdatedAt)
	if err != nil {
		return domain.EnterprisePolicy{}, mapNoRows(err)
	}
	if err := domain.ValidateEnterprisePolicy(policy); err != nil {
		return domain.EnterprisePolicy{}, storedIdentityIntegrity(err)
	}
	return policy, nil
}

func (s *PostgresStore) ListWorkspacesForNamespace(ctx context.Context, namespaceID string) ([]domain.Workspace, error) {
	if err := domain.ValidateNamespaceID(namespaceID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id,name,owner_id,COALESCE(slug,''),COALESCE(owner_username,''),COALESCE(owner_namespace_id,''),COALESCE(visibility,''),COALESCE(secrets_policy,''),COALESCE(settings_policy,''),COALESCE(gh_visibility_sync,false),gh_synced_at,COALESCE(archived,false),COALESCE(webhook_url,''),COALESCE(public_role,''),created_at
		 FROM workspaces WHERE owner_namespace_id=$1 ORDER BY created_at`, namespaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Workspace
	for rows.Next() {
		var workspace domain.Workspace
		if err := rows.Scan(&workspace.ID, &workspace.Name, &workspace.OwnerID, &workspace.Slug, &workspace.OwnerUsername,
			&workspace.OwnerNamespaceID, &workspace.Visibility, &workspace.SecretsPolicy, &workspace.SettingsPolicy,
			&workspace.GHVisibilitySync, &workspace.GHSyncedAt, &workspace.Archived, &workspace.WebhookURL,
			&workspace.PublicRole, &workspace.CreatedAt); err != nil {
			return nil, err
		}
		if err := domain.ValidateWorkspaceRecord(workspace); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, workspace)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateEnterpriseWorkspaceWithAudit(ctx context.Context, workspace domain.Workspace, owner domain.Membership, event domain.EnterpriseAuditEvent) error {
	if err := validateEnterpriseWorkspaceMutation(workspace, owner, event); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var enterpriseID string
	if err := tx.QueryRow(ctx,
		`SELECT enterprise_id FROM namespaces WHERE id=$1 AND kind='enterprise' FOR SHARE`, workspace.OwnerNamespaceID).
		Scan(&enterpriseID); err != nil {
		return mapNoRows(err)
	}
	if enterpriseID != event.EnterpriseID {
		return domain.ErrValidation
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO workspaces (id,name,owner_id,slug,owner_username,owner_namespace_id,visibility,secrets_policy,settings_policy,gh_visibility_sync,gh_synced_at,archived,webhook_url,public_role,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		workspace.ID, workspace.Name, workspace.OwnerID, workspace.Slug, workspace.OwnerUsername, workspace.OwnerNamespaceID,
		string(workspace.Visibility), workspace.SecretsPolicy, workspace.SettingsPolicy, workspace.GHVisibilitySync,
		workspace.GHSyncedAt, workspace.Archived, workspace.WebhookURL, workspace.PublicRole, workspace.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO memberships (workspace_id,user_id,role,created_at) VALUES ($1,$2,$3,$4)`,
		owner.WorkspaceID, owner.UserID, string(owner.Role), owner.CreatedAt); err != nil {
		return mapPGConstraint(err)
	}
	if err := insertEnterpriseAuditTx(ctx, tx, event); err != nil {
		return mapPGConstraint(err)
	}
	return mapPGConstraint(tx.Commit(ctx))
}

func (s *PostgresStore) AppendEnterpriseAudit(ctx context.Context, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateEnterpriseAuditEvent(event); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO enterprise_audit_events (id,enterprise_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (id) DO NOTHING`,
		event.ID, event.EnterpriseID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt)
	return err
}

func (s *PostgresStore) ListEnterpriseAudit(ctx context.Context, enterpriseID string, limit int) ([]domain.EnterpriseAuditEvent, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id,enterprise_id,actor_id,action,target_type,target_id,reason,created_at
		 FROM enterprise_audit_events WHERE enterprise_id=$1 ORDER BY created_at DESC LIMIT $2`, enterpriseID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EnterpriseAuditEvent
	for rows.Next() {
		var event domain.EnterpriseAuditEvent
		if err := rows.Scan(&event.ID, &event.EnterpriseID, &event.ActorID, &event.Action, &event.TargetType, &event.TargetID, &event.Reason, &event.CreatedAt); err != nil {
			return nil, err
		}
		if err := domain.ValidateEnterpriseAuditEvent(event); err != nil {
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
	_, err := s.pool.Exec(ctx,
		`INSERT INTO enterprise_break_glass_grants (id,enterprise_id,workspace_id,user_id,reason,created_at,expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		grant.ID, grant.EnterpriseID, grant.WorkspaceID, grant.UserID, grant.Reason, grant.CreatedAt, grant.ExpiresAt)
	return err
}

func (s *PostgresStore) CreateBreakGlassGrantWithAudit(ctx context.Context, grant domain.BreakGlassGrant, event domain.EnterpriseAuditEvent) error {
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return err
	}
	if err := domain.ValidateEnterpriseAuditEvent(event); err != nil || event.EnterpriseID != grant.EnterpriseID || event.ActorID != grant.UserID || event.Action != "enterprise.break_glass.created" {
		return domain.ErrValidation
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx,
		`INSERT INTO enterprise_break_glass_grants (id,enterprise_id,workspace_id,user_id,reason,created_at,expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		grant.ID, grant.EnterpriseID, grant.WorkspaceID, grant.UserID, grant.Reason, grant.CreatedAt, grant.ExpiresAt); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO enterprise_audit_events (id,enterprise_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		event.ID, event.EnterpriseID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) GetActiveBreakGlassGrant(ctx context.Context, enterpriseID, workspaceID, userID string, now time.Time) (domain.BreakGlassGrant, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	var grant domain.BreakGlassGrant
	err := s.pool.QueryRow(ctx,
		`SELECT id,enterprise_id,workspace_id,user_id,reason,created_at,expires_at
		 FROM enterprise_break_glass_grants
		 WHERE enterprise_id=$1 AND workspace_id=$2 AND user_id=$3 AND expires_at>$4
		 ORDER BY expires_at DESC LIMIT 1`, enterpriseID, workspaceID, userID, now).
		Scan(&grant.ID, &grant.EnterpriseID, &grant.WorkspaceID, &grant.UserID, &grant.Reason, &grant.CreatedAt, &grant.ExpiresAt)
	if err != nil {
		return domain.BreakGlassGrant{}, mapNoRows(err)
	}
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return domain.BreakGlassGrant{}, fmt.Errorf("%w: invalid stored break-glass grant", domain.ErrIntegrity)
	}
	return grant, nil
}

func (s *PostgresStore) UseActiveBreakGlassGrant(ctx context.Context, enterpriseID, workspaceID, userID string, now time.Time, event domain.EnterpriseAuditEvent) (domain.BreakGlassGrant, error) {
	if err := domain.ValidateEnterpriseID(enterpriseID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.BreakGlassGrant{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var grant domain.BreakGlassGrant
	err = tx.QueryRow(ctx,
		`SELECT id,enterprise_id,workspace_id,user_id,reason,created_at,expires_at
		 FROM enterprise_break_glass_grants
		 WHERE enterprise_id=$1 AND workspace_id=$2 AND user_id=$3 AND expires_at>$4
		 ORDER BY expires_at DESC LIMIT 1 FOR UPDATE`, enterpriseID, workspaceID, userID, now).
		Scan(&grant.ID, &grant.EnterpriseID, &grant.WorkspaceID, &grant.UserID, &grant.Reason, &grant.CreatedAt, &grant.ExpiresAt)
	if err != nil {
		return domain.BreakGlassGrant{}, mapNoRows(err)
	}
	if err := domain.ValidateBreakGlassGrant(grant); err != nil {
		return domain.BreakGlassGrant{}, fmt.Errorf("%w: invalid stored break-glass grant", domain.ErrIntegrity)
	}
	event.Reason = grant.Reason
	if err := domain.ValidateEnterpriseAuditEvent(event); err != nil || event.EnterpriseID != enterpriseID || event.ActorID != userID || event.Action != "enterprise.break_glass.used" || event.TargetID != workspaceID {
		return domain.BreakGlassGrant{}, domain.ErrValidation
	}
	if _, err = tx.Exec(ctx,
		`INSERT INTO enterprise_audit_events (id,enterprise_id,actor_id,action,target_type,target_id,reason,created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		event.ID, event.EnterpriseID, event.ActorID, event.Action, event.TargetType, event.TargetID, event.Reason, event.CreatedAt); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.BreakGlassGrant{}, err
	}
	return grant, nil
}
