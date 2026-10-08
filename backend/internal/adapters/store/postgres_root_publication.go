//go:build postgres

package store

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func prepareRootDocPG(ctx context.Context, doc domain.VerifiedSessionDoc) (preparedDocPG, error) {
	root, err := prepareRootPublication(ctx, doc)
	if err != nil {
		return preparedDocPG{}, err
	}
	// Root consumers derive their projection from current verified bytes. The
	// legacy persistent index is neither consumed nor a root readiness proof.
	return preparedDocPG{doc: doc, root: &root, payload: docCompress(root.canonical)}, ctx.Err()
}

// Only durable job completion calls this branch, inside its repository/job
// transaction. Root jobs never create chunk grants or repair missing blobs.
func (s *PostgresStore) putRootDocJob(ctx context.Context, repo domain.ContentHash, prepared preparedDocPG) error {
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != s || bound.readOnly || bound.repo != repo {
		return domain.ErrConflict
	}
	if prepared.root == nil || !prepared.doc.Valid() || prepared.doc.DocumentRef().Identity != domain.DocumentIdentityRootV1 {
		return domain.ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := bound.Tx
	if _, err := tx.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT(hash) DO NOTHING`, prepared.doc.Hash(), prepared.payload); err != nil {
		return err
	}
	var current []byte
	if err := tx.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1 FOR UPDATE`, prepared.doc.Hash()).Scan(&current); err != nil {
		return err
	}
	if err := prepared.root.matchesStored(ctx, current); err != nil {
		return err
	}
	if err := prepared.root.compareCurrent(ctx, prepared.doc, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		var raw []byte
		// SHARE, not KEY SHARE, prevents an in-place byte update as well as
		// deletion; both the repository grant and global body are retained.
		err := tx.QueryRow(ctx, `SELECT b.bytes FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2 FOR SHARE OF rb,b`, repo, hash).Scan(&raw)
		if err != nil {
			return nil, fmt.Errorf("root dependency: %w", mapNoRows(err))
		}
		return raw, nil
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2) ON CONFLICT DO NOTHING`, repo, prepared.doc.Hash()); err != nil {
		return err
	}
	return ctx.Err()
}
