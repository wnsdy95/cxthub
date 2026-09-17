//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) ApplyHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	repoID := domain.ContentHash(e.RepoID)
	if err := lockRepoGraph(ctx, tx, repoID); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT event FROM context_history WHERE repo_id=$1 AND id=$2`, e.RepoID, e.ID).Scan(&raw)
	if err == nil {
		var old domain.HistoryEvent
		if json.Unmarshal(raw, &old) != nil || !reflect.DeepEqual(old, e) {
			return domain.ErrRefConflict
		}
		return nil // Replay never repositions a branch that has since advanced.
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if domain.IsBranchBindingEvent(e) || e.Kind == "advance" || e.Kind == "attach" {
		rows, err := tx.Query(ctx, `SELECT event FROM context_history WHERE repo_id=$1 AND event->>'kind' IN ('birth','orphan','rename','archive')`, e.RepoID)
		if err != nil {
			return err
		}
		var events []domain.HistoryEvent
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			var prior domain.HistoryEvent
			if err := json.Unmarshal(raw, &prior); err != nil {
				rows.Close()
				return err
			}
			events = append(events, prior)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := domain.ValidateHistoryBranch(events, e); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrRefConflict, err)
		}
	}
	if e.Kind == "advance" || e.Kind == "rename" || e.Kind == "archive" {
		var protected bool
		var defaultBranch string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(protect_default,false),default_branch FROM repos WHERE id=$1 FOR SHARE`, e.RepoID).Scan(&protected, &defaultBranch); err != nil {
			return err
		}
		name := e.Branch
		if e.Kind == "rename" {
			name = e.PreviousBranch
		}
		if protected && name == defaultBranch {
			return domain.ErrForbidden
		}
	}
	if err := protocolHistoryWritePG(ctx, tx, e, false); err != nil {
		return err
	}
	for _, ref := range historyRoots(e) {
		ct, err := tx.Exec(ctx, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag',$2,$3)
   ON CONFLICT(repo_id,kind,name) DO UPDATE SET target=refs.target WHERE refs.target=EXCLUDED.target`, e.RepoID, ref.Name, string(ref.Target))
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return domain.ErrRefConflict
		}
	}
	if e.Kind == "rename" || e.Kind == "archive" {
		name := e.Branch
		if e.Kind == "rename" {
			name = e.PreviousBranch
		}
		var target string
		err := tx.QueryRow(ctx, `SELECT COALESCE(target,'') FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2`, e.RepoID, name).Scan(&target)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && domain.ContentHash(target) != e.Source {
			return domain.ErrRefConflict
		}
	}
	if e.Kind == "advance" {
		var protocol int
		if err := tx.QueryRow(ctx, `SELECT context_protocol FROM repos WHERE id=$1`, e.RepoID).Scan(&protocol); err != nil {
			return err
		}
		if protocol == 0 {
			refs, err := listBranchLifecycleRefs(ctx, tx, repoID, e.Branch)
			if err != nil {
				return err
			}
			last, ok, err := domain.LatestBranchLifecycle(refs, e.Branch)
			if err != nil {
				return err
			}
			if ok && last.State == domain.BranchArchived {
				return domain.ErrBranchArchived
			}
		}
		ct, err := tx.Exec(ctx, `UPDATE refs SET target=$1,version=version+1,updated_at=now() WHERE repo_id=$2 AND kind='branch' AND name=$3 AND target=$4`, string(e.Target), e.RepoID, e.Branch, string(e.Source))
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return domain.ErrRefConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO reflog(repo_id,kind,name,old,new) VALUES($1,'branch',$2,$3,$4)`, e.RepoID, e.Branch, string(e.Source), string(e.Target)); err != nil {
			return err
		}
	}
	raw, err = json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO context_history(repo_id,id,event) VALUES($1,$2,$3)`, e.RepoID, e.ID, raw); err != nil {
		return err
	}
	if err := protocolHistoryWritePG(ctx, tx, e, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresStore) ListHistoryEvents(ctx context.Context, repoID domain.ContentHash) ([]domain.HistoryEvent, error) {
	rows, err := s.db(ctx).Query(ctx, `SELECT event FROM context_history WHERE repo_id=$1 ORDER BY received_at,id`, string(repoID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.HistoryEvent{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var e domain.HistoryEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
