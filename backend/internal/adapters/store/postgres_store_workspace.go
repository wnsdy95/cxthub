//go:build postgres

package store

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// PostgresStore implementation of WorkspaceStore (0002 schema: users/workspaces/memberships/invites).

func (s *PostgresStore) UpsertUser(ctx context.Context, u domain.User) error {
	if err := domain.ValidateUserRecord(u); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, username, nickname, load_mode, avatar, locale) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (id) DO UPDATE SET email=EXCLUDED.email, name=EXCLUDED.name, username=EXCLUDED.username, nickname=EXCLUDED.nickname, load_mode=EXCLUDED.load_mode, avatar=EXCLUDED.avatar, locale=EXCLUDED.locale`,
		u.ID, u.Email, u.Name, u.Username, u.Nickname, u.LoadMode, u.Avatar, u.Locale)
	return mapPGConstraint(err)
}

func (s *PostgresStore) GetUser(ctx context.Context, id string) (domain.User, error) {
	if err := domain.ValidateExternalID(id); err != nil {
		return domain.User{}, err
	}
	var u domain.User
	err := s.pool.QueryRow(ctx, `SELECT id, email, name, COALESCE(username,''), COALESCE(nickname,''), COALESCE(load_mode,''), COALESCE(avatar,''), COALESCE(locale,''), created_at FROM users WHERE id=$1`, id).
		Scan(&u.ID, &u.Email, &u.Name, &u.Username, &u.Nickname, &u.LoadMode, &u.Avatar, &u.Locale, &u.CreatedAt)
	if err != nil {
		return domain.User{}, mapNoRows(err)
	}
	if u.ID != id {
		return domain.User{}, domain.ErrNotFound
	}
	if err := domain.ValidateUserRecord(u); err != nil {
		return domain.User{}, storedIdentityIntegrity(err)
	}
	return u, nil
}

// GetUserByUsername finds a user by handle (target column of partial unique index).
func (s *PostgresStore) GetUserByUsername(ctx context.Context, username string) (domain.User, error) {
	var u domain.User
	err := s.pool.QueryRow(ctx, `SELECT id, email, name, COALESCE(username,''), COALESCE(nickname,''), COALESCE(load_mode,''), COALESCE(avatar,''), COALESCE(locale,''), created_at FROM users WHERE username=$1`, username).
		Scan(&u.ID, &u.Email, &u.Name, &u.Username, &u.Nickname, &u.LoadMode, &u.Avatar, &u.Locale, &u.CreatedAt)
	if err != nil {
		return domain.User{}, mapNoRows(err)
	}
	if err := domain.ValidateUserRecord(u); err != nil {
		return domain.User{}, storedIdentityIntegrity(err)
	}
	return u, nil
}

func (s *PostgresStore) CreateWorkspace(ctx context.Context, ws domain.Workspace) error {
	if err := domain.ValidateWorkspaceRecord(ws); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, owner_id, slug, owner_username, owner_namespace_id, visibility, secrets_policy, settings_policy, gh_visibility_sync, gh_synced_at, archived, webhook_url, public_role)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, slug=EXCLUDED.slug, owner_username=EXCLUDED.owner_username,
		 owner_namespace_id=EXCLUDED.owner_namespace_id,
		 visibility=EXCLUDED.visibility, secrets_policy=EXCLUDED.secrets_policy, settings_policy=EXCLUDED.settings_policy,
		 owner_id=EXCLUDED.owner_id,
		 gh_visibility_sync=EXCLUDED.gh_visibility_sync, gh_synced_at=EXCLUDED.gh_synced_at,
		 archived=EXCLUDED.archived, webhook_url=EXCLUDED.webhook_url, public_role=EXCLUDED.public_role`,
		ws.ID, ws.Name, ws.OwnerID, ws.Slug, ws.OwnerUsername, pgNullableString(ws.OwnerNamespaceID), string(ws.Visibility), ws.SecretsPolicy, ws.SettingsPolicy, ws.GHVisibilitySync, ws.GHSyncedAt, ws.Archived, ws.WebhookURL, ws.PublicRole)
	return err
}

func (s *PostgresStore) GetWorkspace(ctx context.Context, id string) (domain.Workspace, error) {
	if err := domain.ValidateWorkspaceID(id); err != nil {
		return domain.Workspace{}, err
	}
	var ws domain.Workspace
	err := s.pool.QueryRow(ctx, `SELECT id, name, owner_id, COALESCE(slug,''), COALESCE(owner_username,''), COALESCE(owner_namespace_id,''), COALESCE(visibility,''), COALESCE(secrets_policy,''), COALESCE(settings_policy,''), COALESCE(gh_visibility_sync,false), gh_synced_at, COALESCE(archived,false), COALESCE(webhook_url,''), COALESCE(public_role,''), created_at FROM workspaces WHERE id=$1`, id).
		Scan(&ws.ID, &ws.Name, &ws.OwnerID, &ws.Slug, &ws.OwnerUsername, &ws.OwnerNamespaceID, &ws.Visibility, &ws.SecretsPolicy, &ws.SettingsPolicy, &ws.GHVisibilitySync, &ws.GHSyncedAt, &ws.Archived, &ws.WebhookURL, &ws.PublicRole, &ws.CreatedAt)
	if err != nil {
		return domain.Workspace{}, mapNoRows(err)
	}
	if ws.ID != id {
		return domain.Workspace{}, domain.ErrNotFound
	}
	if err := domain.ValidateWorkspaceRecord(ws); err != nil {
		return domain.Workspace{}, storedIdentityIntegrity(err)
	}
	return ws, nil
}

// GetWorkspaceByPath finds a workspace by URL segments (owner_username, slug).
func (s *PostgresStore) GetWorkspaceByPath(ctx context.Context, ownerUsername, slug string) (domain.Workspace, error) {
	var ws domain.Workspace
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, owner_id, COALESCE(slug,''), COALESCE(owner_username,''), COALESCE(owner_namespace_id,''), COALESCE(visibility,''), COALESCE(secrets_policy,''), COALESCE(settings_policy,''), COALESCE(gh_visibility_sync,false), gh_synced_at, COALESCE(archived,false), COALESCE(webhook_url,''), COALESCE(public_role,''), created_at
		 FROM workspaces WHERE owner_username=$1 AND slug=$2`, ownerUsername, slug).
		Scan(&ws.ID, &ws.Name, &ws.OwnerID, &ws.Slug, &ws.OwnerUsername, &ws.OwnerNamespaceID, &ws.Visibility, &ws.SecretsPolicy, &ws.SettingsPolicy, &ws.GHVisibilitySync, &ws.GHSyncedAt, &ws.Archived, &ws.WebhookURL, &ws.PublicRole, &ws.CreatedAt)
	if err != nil {
		return domain.Workspace{}, mapNoRows(err)
	}
	if err := domain.ValidateWorkspaceRecord(ws); err != nil {
		return domain.Workspace{}, storedIdentityIntegrity(err)
	}
	return ws, nil
}

func (s *PostgresStore) GetWorkspaceByNamespacePath(ctx context.Context, namespaceID, slug string) (domain.Workspace, error) {
	if err := domain.ValidateNamespaceID(namespaceID); err != nil {
		return domain.Workspace{}, err
	}
	var ws domain.Workspace
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, owner_id, COALESCE(slug,''), COALESCE(owner_username,''), COALESCE(owner_namespace_id,''), COALESCE(visibility,''), COALESCE(secrets_policy,''), COALESCE(settings_policy,''), COALESCE(gh_visibility_sync,false), gh_synced_at, COALESCE(archived,false), COALESCE(webhook_url,''), COALESCE(public_role,''), created_at
		 FROM workspaces WHERE owner_namespace_id=$1 AND slug=$2`, namespaceID, slug).
		Scan(&ws.ID, &ws.Name, &ws.OwnerID, &ws.Slug, &ws.OwnerUsername, &ws.OwnerNamespaceID, &ws.Visibility, &ws.SecretsPolicy, &ws.SettingsPolicy, &ws.GHVisibilitySync, &ws.GHSyncedAt, &ws.Archived, &ws.WebhookURL, &ws.PublicRole, &ws.CreatedAt)
	if err != nil {
		return domain.Workspace{}, mapNoRows(err)
	}
	if err := domain.ValidateWorkspaceRecord(ws); err != nil {
		return domain.Workspace{}, storedIdentityIntegrity(err)
	}
	return ws, nil
}

func (s *PostgresStore) ListWorkspacesForUser(ctx context.Context, userID string) ([]domain.Workspace, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT w.id, w.name, w.owner_id, COALESCE(w.slug,''), COALESCE(w.owner_username,''), COALESCE(w.owner_namespace_id,''), COALESCE(w.visibility,''), COALESCE(w.secrets_policy,''), COALESCE(w.settings_policy,''), COALESCE(w.gh_visibility_sync,false), w.gh_synced_at, COALESCE(w.archived,false), COALESCE(w.webhook_url,''), COALESCE(w.public_role,''), w.created_at FROM workspaces w
		 JOIN memberships m ON m.workspace_id = w.id WHERE m.user_id=$1 ORDER BY w.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Workspace
	for rows.Next() {
		var ws domain.Workspace
		if err := rows.Scan(&ws.ID, &ws.Name, &ws.OwnerID, &ws.Slug, &ws.OwnerUsername, &ws.OwnerNamespaceID, &ws.Visibility, &ws.SecretsPolicy, &ws.SettingsPolicy, &ws.GHVisibilitySync, &ws.GHSyncedAt, &ws.Archived, &ws.WebhookURL, &ws.PublicRole, &ws.CreatedAt); err != nil {
			return nil, err
		}
		if err := domain.ValidateWorkspaceRecord(ws); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

func (s *PostgresStore) AddMember(ctx context.Context, m domain.Membership) error {
	if err := domain.ValidateMembershipRecord(m); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1,$2,$3)
		 ON CONFLICT (workspace_id, user_id) DO UPDATE SET role=EXCLUDED.role`, // upsert updates the role
		m.WorkspaceID, m.UserID, string(m.Role))
	return err
}

func (s *PostgresStore) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM memberships WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID)
	return err
}

func (s *PostgresStore) IsMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return false, err
	}
	if err := domain.ValidateExternalID(userID); err != nil {
		return false, err
	}
	var one int
	err := s.pool.QueryRow(ctx,
		`SELECT 1 FROM memberships WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID).Scan(&one)
	if err != nil {
		if mapNoRows(err) == domain.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *PostgresStore) ListMembers(ctx context.Context, workspaceID string) ([]domain.Membership, error) {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT m.workspace_id, m.user_id, m.role, m.created_at, u.email, u.name, COALESCE(u.nickname,'')
		 FROM memberships m LEFT JOIN users u ON u.id = m.user_id
		 WHERE m.workspace_id=$1 ORDER BY m.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Membership
	for rows.Next() {
		var m domain.Membership
		var role, email, name, nickname string
		if err := rows.Scan(&m.WorkspaceID, &m.UserID, &role, &m.CreatedAt, &email, &name, &nickname); err != nil {
			return nil, err
		}
		m.Role = domain.MemberRole(role)
		m.User = &domain.User{ID: m.UserID, Email: email, Name: name, Nickname: nickname}
		if err := domain.ValidateMembershipRecord(m); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		if err := domain.ValidateUserRecord(*m.User); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CreateInvite(ctx context.Context, inv domain.Invite) error {
	if err := domain.ValidateInviteRecord(inv); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO invites (token, workspace_id, email, role, status, created_by, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		inv.Token, inv.WorkspaceID, inv.Email, string(inv.Role), string(inv.Status), inv.CreatedBy, inv.ExpiresAt)
	return err
}

func (s *PostgresStore) GetInvite(ctx context.Context, token string) (domain.Invite, error) {
	if err := domain.ValidateInviteToken(token); err != nil {
		return domain.Invite{}, err
	}
	var inv domain.Invite
	var role, status string
	err := s.pool.QueryRow(ctx,
		`SELECT token, workspace_id, email, role, status, created_by, created_at, expires_at FROM invites WHERE token=$1`, token).
		Scan(&inv.Token, &inv.WorkspaceID, &inv.Email, &role, &status, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt)
	if err != nil {
		return domain.Invite{}, mapNoRows(err)
	}
	inv.Role = domain.MemberRole(role)
	inv.Status = domain.InviteStatus(status)
	if inv.Token != token {
		return domain.Invite{}, domain.ErrNotFound
	}
	if err := domain.ValidateInviteRecord(inv); err != nil {
		return domain.Invite{}, storedIdentityIntegrity(err)
	}
	return inv, nil
}

func (s *PostgresStore) UpdateInviteStatus(ctx context.Context, token string, status domain.InviteStatus) error {
	if err := domain.ValidateInviteToken(token); err != nil {
		return err
	}
	if !domain.ValidInviteStatus(status) {
		return domain.ErrValidation
	}
	_, err := s.pool.Exec(ctx, `UPDATE invites SET status=$1 WHERE token=$2`, string(status), token)
	return err
}

func (s *PostgresStore) CreateSession(ctx context.Context, sess domain.Session) error {
	if err := domain.ValidateSessionRecord(sess); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sessions (token, user_id, expires_at, hint, kind, label) VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (token) DO UPDATE SET hint=EXCLUDED.hint, kind=EXCLUDED.kind, label=EXCLUDED.label`,
		sess.Token, sess.UserID, sess.ExpiresAt, sess.Hint, sess.Kind, sess.Label)
	return err
}

func (s *PostgresStore) GetSession(ctx context.Context, token string) (domain.Session, error) {
	if err := domain.ValidateStoredSessionToken(token); err != nil {
		return domain.Session{}, err
	}
	var sess domain.Session
	err := s.pool.QueryRow(ctx,
		`SELECT token, user_id, created_at, expires_at, COALESCE(hint,''), COALESCE(kind,''), COALESCE(label,'') FROM sessions WHERE token=$1`, token).
		Scan(&sess.Token, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.Hint, &sess.Kind, &sess.Label)
	if err != nil {
		return domain.Session{}, mapNoRows(err)
	}
	if sess.Token != token {
		return domain.Session{}, domain.ErrNotFound
	}
	if err := domain.ValidateSessionRecord(sess); err != nil {
		return domain.Session{}, storedIdentityIntegrity(err)
	}
	return sess, nil
}

func (s *PostgresStore) ConsumeSession(ctx context.Context, token, kind, label string) (domain.Session, error) {
	if err := domain.ValidateStoredSessionToken(token); err != nil {
		return domain.Session{}, err
	}
	var sess domain.Session
	err := s.pool.QueryRow(ctx,
		`DELETE FROM sessions WHERE token=$1 AND kind=$2 AND label=$3
		 RETURNING token, user_id, created_at, expires_at, COALESCE(hint,''), COALESCE(kind,''), COALESCE(label,'')`,
		token, kind, label).
		Scan(&sess.Token, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.Hint, &sess.Kind, &sess.Label)
	if err != nil {
		return domain.Session{}, mapNoRows(err)
	}
	if err := domain.ValidateSessionRecord(sess); err != nil {
		return domain.Session{}, storedIdentityIntegrity(err)
	}
	return sess, nil
}

func (s *PostgresStore) DeleteSession(ctx context.Context, token string) error {
	if err := domain.ValidateStoredSessionToken(token); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token=$1`, token)
	return err
}

func (s *PostgresStore) ListSessionsForUser(ctx context.Context, userID string) ([]domain.Session, error) {
	if err := domain.ValidateExternalID(userID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT token, user_id, created_at, expires_at, COALESCE(hint,''), COALESCE(kind,''), COALESCE(label,'') FROM sessions WHERE user_id=$1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Session
	for rows.Next() {
		var sess domain.Session
		if err := rows.Scan(&sess.Token, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.Hint, &sess.Kind, &sess.Label); err != nil {
			return nil, err
		}
		if err := domain.ValidateSessionRecord(sess); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListInvites(ctx context.Context, workspaceID string) ([]domain.Invite, error) {
	if err := domain.ValidateWorkspaceID(workspaceID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT token, workspace_id, email, role, status, created_by, created_at, expires_at
		 FROM invites WHERE workspace_id=$1 ORDER BY created_at DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Invite
	for rows.Next() {
		var inv domain.Invite
		var role, status string
		if err := rows.Scan(&inv.Token, &inv.WorkspaceID, &inv.Email, &role, &status, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return nil, err
		}
		inv.Role = domain.MemberRole(role)
		inv.Status = domain.InviteStatus(status)
		if err := domain.ValidateInviteRecord(inv); err != nil {
			return nil, storedIdentityIntegrity(err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}
