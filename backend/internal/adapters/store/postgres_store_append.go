//go:build postgres

package store

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *PostgresStore) AppendRef(ctx context.Context, repoID domain.ContentHash, ref domain.Ref, expected domain.ContentHash) error {
	ref.RepoID = repoID
	if err := domain.ValidateRef(ref); err != nil {
		return err
	}
	if ref.Kind != domain.RefBranch {
		return domain.ErrValidation
	}
	if err := domain.ValidateContentHash(expected); err != nil {
		return err
	}
	tx, err := s.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRepoGraph(ctx, tx, repoID); err != nil {
		return err
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(target,'') FROM refs WHERE repo_id=$1 AND kind='branch' AND name=$2 FOR UPDATE`, string(repoID), ref.Name).Scan(&current); err != nil {
		return mapNoRows(err)
	}
	if domain.ContentHash(current) != expected {
		return domain.ErrRefConflict
	}
	if err := validateContextWritePG(ctx, tx, repoID, ref); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT `+snapCols+` FROM snapshots WHERE repo_id=$1`, string(repoID))
	if err != nil {
		return err
	}
	var snaps []domain.Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			rows.Close()
			return err
		}
		snaps = append(snaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	patches, err := appendPlan(snaps, expected, ref.Target)
	if err != nil {
		return err
	}
	for _, patch := range patches {
		ct, err := tx.Exec(ctx, `UPDATE snapshots SET graft_parents=$3,grafted=true,graft_seq=graft_seq+1 WHERE repo_id=$1 AND id=$2 AND graft_seq=$4`, string(repoID), string(patch.SnapshotID), strs(patch.Parents), patch.ExpectedSeq)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return domain.ErrConflict
		}
	}
	if err := ensureNoReachabilityCycle(ctx, tx, repoID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE refs SET target=$3,version=version+1,updated_at=now() WHERE repo_id=$1 AND kind='branch' AND name=$2`, string(repoID), ref.Name, string(ref.Target)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reflog(repo_id,kind,name,old,new) VALUES($1,'branch',$2,$3,$4)`, string(repoID), ref.Name, string(expected), string(ref.Target)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
