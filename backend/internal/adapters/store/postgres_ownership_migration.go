//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) migrateRepositoryOwnership(ctx context.Context, conn *pgxpool.Conn) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	var done bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ownership_migrations WHERE version='repository-ownership-v1')`).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE repositories, repos, memberships, invites, organization_break_glass_grants IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: tx, owner: s})
	rows, err := tx.Query(ctx, `SELECT id FROM repositories ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	var boundaries []domain.Repository
	var members []domain.Membership
	var invites []domain.Invite
	for _, id := range ids {
		w, e := s.GetRepository(ctx, id)
		if e != nil {
			return e
		}
		boundaries = append(boundaries, w)
		ms, e := s.ListMembers(ctx, id)
		if e != nil {
			return e
		}
		members = append(members, ms...)
		is, e := s.ListInvites(ctx, id)
		if e != nil {
			return e
		}
		invites = append(invites, is...)
	}
	repoRows, err := tx.Query(ctx, `SELECT id FROM repos ORDER BY id`)
	if err != nil {
		return err
	}
	var repoIDs []domain.ContentHash
	for repoRows.Next() {
		var id domain.ContentHash
		if err = repoRows.Scan(&id); err != nil {
			repoRows.Close()
			return err
		}
		repoIDs = append(repoIDs, id)
	}
	repoRows.Close()
	if err = repoRows.Err(); err != nil {
		return err
	}
	var repos []domain.Repo
	for _, id := range repoIDs {
		repo, err := s.GetRepo(ctx, id)
		if err != nil {
			return err
		}
		repos = append(repos, repo)
	}
	plan, err := domain.PlanRepositoryOwnership(boundaries, repos, members, invites)
	if err != nil {
		return fmt.Errorf("repository ownership migration: %w", err)
	}
	for _, r := range plan.Repositories {
		if err = s.CreateRepository(ctx, r); err != nil {
			return err
		}
		if !r.CreatedAt.IsZero() {
			if _, err = tx.Exec(ctx, `UPDATE repositories SET created_at=$2 WHERE id=$1`, r.ID, r.CreatedAt); err != nil {
				return err
			}
		}
	}
	for _, r := range plan.Repos {
		if _, err = tx.Exec(ctx, `UPDATE repos SET repository_id=NULLIF($2,'') WHERE id=$1`, string(r.ID), r.RepositoryID); err != nil {
			return err
		}
	}
	for _, m := range plan.Members {
		if err = s.AddMember(ctx, m); err != nil {
			return err
		}
		if !m.CreatedAt.IsZero() {
			if _, err = tx.Exec(ctx, `UPDATE memberships SET created_at=$3 WHERE repository_id=$1 AND user_id=$2`, m.RepositoryID, m.UserID, m.CreatedAt); err != nil {
				return err
			}
		}
	}
	for _, a := range plan.Aliases {
		key := a.NamespaceID
		if key == "" {
			key = "handle:" + a.Owner
		}
		if _, err = tx.Exec(ctx, `INSERT INTO repository_path_aliases(namespace_key,owner_handle,path,repository_id,context_repo_id) VALUES($1,$2,$3,$4,NULLIF($5,''))`, key, a.Owner, a.Path, a.RepositoryID, string(a.ContextRepoID)); err != nil {
			return err
		}
	}
	for _, target := range plan.InviteTargets {
		if _, err = tx.Exec(ctx, `INSERT INTO repository_invite_targets(token,repository_id) VALUES($1,$2)`, target.Token, target.RepositoryID); err != nil {
			return err
		}
	}
	for original, targets := range plan.Targets {
		for _, target := range targets {
			if original == target {
				continue
			}
			// Existing exceptional grants retain their exact scope and expiry.
			if _, err = tx.Exec(ctx, `INSERT INTO organization_break_glass_grants(id,organization_id,repository_id,user_id,reason,created_at,expires_at)
			 SELECT 'bg_'||md5(id||':'||$2),organization_id,$2,user_id,reason,created_at,expires_at FROM organization_break_glass_grants WHERE repository_id=$1`, original, target); err != nil {
				return err
			}
		}
	}
	if _, err = tx.Exec(ctx, `CREATE UNIQUE INDEX repos_single_repository_boundary ON repos(repository_id) WHERE repository_id IS NOT NULL`); err != nil {
		return err
	}
	report, err := json.Marshal(plan.Targets)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO ownership_migrations(version,report) VALUES('repository-ownership-v1',$1)`, report); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
