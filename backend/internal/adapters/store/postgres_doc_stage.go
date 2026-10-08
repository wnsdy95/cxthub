//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A stage owns only rebuildable derivatives, never a document/read-index or a
// repository grant. Durable job/version pins protect shared blocks from GC until
// completion, rejection, retry or reclaim. Nested publication uses the original
// atomic path: a savepoint cannot release staged block locks before doc locking.
func (p *pgDocPublication) stageReadBlocks(ctx context.Context, j domain.DocFinalizationJob) error {
	if p.doc.doc.DocumentRef().Identity != domain.DocumentIdentityLegacy {
		return domain.ErrUnsupportedDocumentIdentity
	}
	nested := false
	if prior, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx); ok {
		if prior.owner != p.store || prior.readOnly || prior.repo != j.RepoID {
			return domain.ErrConflict
		}
		nested = true
	}
	if err := j.Validate(); err != nil {
		return err
	}
	if !p.doc.doc.Valid() || p.doc.doc.Hash() != j.DocHash {
		return domain.ErrIntegrity
	}
	if nested {
		return nil // Preserve doc -> blocks order and enclosing rollback/quota.
	}
	tx, err := p.store.db(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, j.RepoID, j.ID).Scan(&raw); err != nil {
		return mapNoRows(err)
	}
	var old domain.DocFinalizationJob
	if err := json.Unmarshal(raw, &old); err != nil {
		return err
	}
	if !old.Fences(j, time.Now().UTC()) || old.DocHash != p.doc.doc.Hash() {
		return domain.ErrConflict
	}
	if err := putPreparedReadBlocksPG(ctx, tx, p.doc.read.blocks); err != nil {
		return err
	}
	// The job row stays locked before blocks/search rows, matching reclaim and
	// publication. Check elapsed time again: pausing during INSERT cannot turn an
	// expired stage into a valid ownership grant or leave committed stale pins.
	if !old.Fences(j, time.Now().UTC()) {
		return domain.ErrConflict
	}
	hashes := make([]domain.ContentHash, len(p.doc.read.blocks))
	for i, b := range p.doc.read.blocks {
		hashes[i] = b.Hash
	}
	if _, err := tx.Exec(ctx, `INSERT INTO doc_read_block_preparations_v3(repo_id,job_id,version,block_hash)
 SELECT $1,$2,$3,hash FROM unnest($4::text[]) AS x(hash)
 ORDER BY hash ON CONFLICT DO NOTHING`, j.RepoID, j.ID, j.Version, hashes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
