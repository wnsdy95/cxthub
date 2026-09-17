//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func protocolHistoryPG(ctx context.Context, tx pgx.Tx, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	rows, err := tx.Query(ctx, `SELECT event FROM context_history WHERE repo_id=$1`, string(repo))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.HistoryEvent
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e domain.HistoryEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func validateContextWritePG(ctx context.Context, tx pgx.Tx, repo domain.ContentHash, next domain.Ref) error {
	var protocol int
	if err := tx.QueryRow(ctx, `SELECT context_protocol FROM repos WHERE id=$1`, string(repo)).Scan(&protocol); err != nil {
		return mapNoRows(err)
	}
	if protocol == 0 {
		return nil
	}
	events, err := protocolHistoryPG(ctx, tx, repo)
	if err != nil {
		return err
	}
	var current *domain.Ref
	ref := domain.Ref{RepoID: repo, Kind: next.Kind, Name: next.Name}
	err = tx.QueryRow(ctx, `SELECT COALESCE(target,''),branch_id FROM refs WHERE repo_id=$1 AND kind=$2 AND name=$3`, string(repo), string(next.Kind), next.Name).Scan(&ref.Target, &ref.BranchID)
	if err == nil {
		current = &ref
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return domain.ValidateContextRefWrite(protocol, events, current, next)
}

func (s *PostgresStore) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	if err := domain.ValidateContentHash(repo); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRepoGraph(ctx, tx, repo); err != nil {
		return err
	}
	var protocol int
	if err := tx.QueryRow(ctx, `SELECT context_protocol FROM repos WHERE id=$1 FOR UPDATE`, string(repo)).Scan(&protocol); err != nil {
		return mapNoRows(err)
	}
	if protocol == 1 {
		return nil
	}
	events, err := protocolHistoryPG(ctx, tx, repo)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT kind,name,COALESCE(target,''),COALESCE(symbolic,''),branch_id FROM refs WHERE repo_id=$1`, string(repo))
	if err != nil {
		return err
	}
	var refs []domain.Ref
	for rows.Next() {
		ref := domain.Ref{RepoID: repo}
		if err := rows.Scan(&ref.Kind, &ref.Name, &ref.Target, &ref.Symbolic, &ref.BranchID); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	next, err := domain.ContextProtocolRefs(repo, refs, events)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, ref := range next {
		live[ref.Name] = true
	}
	for _, ref := range refs {
		if ref.Kind == domain.RefBranch && !live[ref.Name] {
			if _, err := tx.Exec(ctx, `DELETE FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2`, string(repo), ref.Name); err != nil {
				return err
			}
		}
	}
	for _, ref := range next {
		if _, err := tx.Exec(ctx, `UPDATE refs SET branch_id=$3 WHERE repo_id=$1 AND kind='branch' AND name=$2`, string(repo), ref.Name, ref.BranchID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE repos SET context_protocol=1 WHERE id=$1`, string(repo)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func protocolHistoryWritePG(ctx context.Context, tx pgx.Tx, e domain.HistoryEvent, apply bool) error {
	var protocol int
	if err := tx.QueryRow(ctx, `SELECT context_protocol FROM repos WHERE id=$1`, e.RepoID).Scan(&protocol); err != nil {
		return mapNoRows(err)
	}
	if protocol == 0 {
		return nil
	}
	repo := domain.ContentHash(e.RepoID)
	if !apply {
		name := e.Branch
		if e.Kind == "rename" {
			name = e.PreviousBranch
		}
		var identity string
		err := tx.QueryRow(ctx, `SELECT branch_id FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2`, e.RepoID, name).Scan(&identity)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if (e.Kind == "birth" || e.Kind == "orphan") && err == nil {
			return domain.ErrRefConflict
		}
		if e.Kind == "rename" || e.Kind == "archive" {
			if err == nil && identity != e.BranchID {
				return domain.ErrRefConflict
			}
			if errors.Is(err, pgx.ErrNoRows) {
				events, err := protocolHistoryPG(ctx, tx, repo)
				if err != nil {
					return err
				}
				p, err := domain.ProjectContextBranches(events)
				if err != nil {
					return err
				}
				if p.Active[name].ID != e.BranchID {
					return domain.ErrRefConflict
				}
			}
			if e.Kind == "rename" {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2)`, e.RepoID, e.Branch).Scan(&exists); err != nil {
					return err
				}
				if exists {
					return domain.ErrRefConflict
				}
			}
		}
		if e.Kind == "advance" {
			return validateContextWritePG(ctx, tx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: e.Branch, BranchID: e.BranchID, Target: e.Target})
		}
		return nil
	}
	switch e.Kind {
	case "birth":
		if e.Target != "" {
			_, err := tx.Exec(ctx, `INSERT INTO refs(repo_id,kind,name,target,branch_id) VALUES($1,'branch',$2,$3,$4)`, e.RepoID, e.Branch, string(e.Target), e.BranchID)
			return err
		}
	case "rename":
		if e.Source != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO refs(repo_id,kind,name,target,branch_id) VALUES($1,'branch',$2,$3,$4)`, e.RepoID, e.Branch, string(e.Source), e.BranchID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2`, e.RepoID, e.PreviousBranch); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE refs SET symbolic=$3 WHERE repo_id=$1 AND kind='head' AND symbolic IN ($2,'refs/heads/' || $2)`, e.RepoID, e.PreviousBranch, e.Branch)
		return err
	case "archive":
		if e.Source != "" {
			if _, err := tx.Exec(ctx, `UPDATE refs SET symbolic='',target=$3 WHERE repo_id=$1 AND kind='head' AND symbolic IN ($2,'refs/heads/' || $2)`, e.RepoID, e.Branch, string(e.Source)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2`, e.RepoID, e.Branch)
		return err
	}
	return nil
}
