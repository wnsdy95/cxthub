//go:build postgres

package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ outbound.GitTreeStore = (*PostgresStore)(nil)

func (s *PostgresStore) GetGitCommitTree(ctx context.Context, repo domain.ContentHash, origin, commit string) (domain.GitCommitTree, error) {
	var b []byte
	var c domain.GitCommitTree
	err := s.db(ctx).QueryRow(ctx, `SELECT payload FROM git_commit_trees WHERE repo_id=$1 AND origin=$2 AND commit_oid=$3`, repo, origin, commit).Scan(&b)
	if err != nil {
		return c, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &c); err == nil {
		err = c.Validate()
		if c.Commit != commit {
			err = domain.ErrIntegrity
		}
	}
	return c, err
}
func (s *PostgresStore) GetGitTreeNode(ctx context.Context, repo domain.ContentHash, origin, oid string) (domain.GitTreeNode, error) {
	var b []byte
	var n domain.GitTreeNode
	err := s.db(ctx).QueryRow(ctx, `SELECT payload FROM git_tree_nodes WHERE repo_id=$1 AND origin=$2 AND oid=$3`, repo, origin, oid).Scan(&b)
	if err != nil {
		return n, mapNoRows(err)
	}
	if err = json.Unmarshal(b, &n); err == nil {
		err = n.Validate()
		if n.OID != oid {
			err = domain.ErrIntegrity
		}
	}
	return n, err
}
func writeGitTreePG(ctx context.Context, tx pgx.Tx, repo domain.ContentHash, origin string, evidence domain.GitTreeEvidence) error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	for _, n := range evidence.Nodes {
		b, err := json.Marshal(n)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, repo, origin, n.OID, b)
		if err != nil {
			return err
		}
		var same bool
		if err = tx.QueryRow(ctx, `SELECT payload=$4::jsonb FROM git_tree_nodes WHERE repo_id=$1 AND origin=$2 AND oid=$3`, repo, origin, n.OID, b).Scan(&same); err != nil {
			return err
		}
		if !same {
			return domain.ErrIntegrity
		}
	}
	c := evidence.Commit
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO git_commit_trees(repo_id,origin,commit_oid,payload) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, repo, origin, c.Commit, b)
	if err != nil {
		return err
	}
	var same bool
	if err = tx.QueryRow(ctx, `SELECT payload=$4::jsonb FROM git_commit_trees WHERE repo_id=$1 AND origin=$2 AND commit_oid=$3`, repo, origin, c.Commit, b).Scan(&same); err != nil {
		return err
	}
	if !same {
		return domain.ErrIntegrity
	}
	return nil
}
