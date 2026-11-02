//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pgLegacyStreamingPublication struct {
	store *PostgresStore
	job   domain.DocFinalizationJob
	doc   domain.VerifiedLegacyStream
	spool *legacyDocSpool
	read  preparedReadIndexPG
}

func (storage *PostgresStore) legacyJobChunk(ctx context.Context, repo, hash domain.ContentHash, lock bool) ([]byte, error) {
	query := `SELECT substring(b.bytes FROM 1 FOR $3) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2`
	if lock {
		query += ` FOR SHARE OF rb,b`
	}
	var raw []byte
	if err := storage.db(ctx).QueryRow(ctx, query, repo, hash, 2*maxLegacyReadBytes+1).Scan(&raw); err != nil {
		return nil, mapNoRows(err)
	}
	return boundedLegacyObject(ctx, raw, maxLegacyReadBytes)
}

func (storage *PostgresStore) PrepareLegacyDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	if err := job.Validate(); err != nil {
		return nil, err
	}
	if job.DocIdentity != domain.DocumentIdentityLegacy || job.Manifest.Format != domain.ChunkFormatV2 {
		return nil, domain.ErrUnsupportedDocumentIdentity
	}
	spool, err := newLegacyDocSpool()
	if err != nil {
		return nil, err
	}
	prepared := false
	defer func() {
		if !prepared {
			_ = spool.Close()
		}
	}()
	doc, err := storage.docProofs.rootVerifier.VerifyLegacyStream(ctx, job.DocHash, job.Manifest, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		return storage.legacyJobChunk(ctx, job.RepoID, hash, false)
	}, spool)
	if err != nil {
		return nil, err
	}
	plan, err := doc.PlanReadIndexContext(ctx)
	if err != nil {
		return nil, err
	}
	envelope, err := json.Marshal(plan.Envelope())
	if err != nil {
		return nil, err
	}
	publication := &pgLegacyStreamingPublication{store: storage, job: job, doc: doc, spool: spool,
		read: preparedReadIndexPG{hash: job.DocHash, blocks: plan.Blocks(), envelope: envelope, eventCount: plan.EventCount()}}
	prepared = true
	return publication, nil
}

func (publication *pgLegacyStreamingPublication) Close() error {
	return publication.spool.Close()
}

func (publication *pgLegacyStreamingPublication) matches(job domain.DocFinalizationJob) error {
	if err := job.Validate(); err != nil {
		return err
	}
	if !publication.doc.Valid() || publication.doc.Reference().DocumentRef() != job.DocumentRef() ||
		job.RepoID != publication.job.RepoID || job.RepositoryEpoch != publication.job.RepositoryEpoch || job.ID != publication.job.ID || job.Version != publication.job.Version ||
		!sameDocManifest(job.Manifest, publication.job.Manifest) {
		return domain.ErrConflict
	}
	_, err := publication.spool.file.Stat()
	return err
}

func (publication *pgLegacyStreamingPublication) current(ctx context.Context, tx pgx.Tx, job domain.DocFinalizationJob) (domain.DocFinalizationJob, error) {
	current, err := readLockedDocJobPG(ctx, tx, job.RepoID, job.ID)
	if err != nil {
		return current, err
	}
	if err := publication.matches(current); err != nil {
		return current, err
	}
	if !current.Fences(job, time.Now().UTC()) {
		return current, domain.ErrConflict
	}
	return current, nil
}

func (publication *pgLegacyStreamingPublication) Stage(ctx context.Context, job domain.DocFinalizationJob, lease time.Duration) error {
	if err := publication.matches(job); err != nil {
		return err
	}
	if _, nested := ctx.Value(repositoryTxKey{}).(*repositoryTx); nested {
		return domain.ErrConflict
	}
	if lease < time.Minute {
		return domain.ErrValidation
	}
	blocks := append([]domain.DocReadBlock(nil), publication.read.blocks...)
	sort.Slice(blocks, func(left, right int) bool { return blocks[left].Hash < blocks[right].Hash })
	for start := 0; start < len(blocks); start += 4 {
		renew, finish := context.WithTimeout(ctx, 5*time.Second)
		err := publication.store.RenewDocJob(renew, job, time.Now().UTC(), lease)
		finish()
		if err != nil {
			return err
		}
		batch, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = publication.stageBatch(batch, job, blocks[start:min(start+4, len(blocks))])
		cancel()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (publication *pgLegacyStreamingPublication) stageBatch(ctx context.Context, job domain.DocFinalizationJob, blocks []domain.DocReadBlock) error {
	tx, err := publication.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(tx)
	current, err := publication.current(ctx, tx, job)
	if err != nil {
		return err
	}
	if err := putPreparedReadBlocksPG(ctx, tx, blocks); err != nil {
		return err
	}
	hashes := make([]domain.ContentHash, len(blocks))
	for index, block := range blocks {
		hashes[index] = block.Hash
	}
	if _, err := tx.Exec(ctx, `INSERT INTO doc_read_block_preparations_v3(repo_id,job_id,version,block_hash)
 SELECT $1,$2,$3,hash FROM unnest($4::text[]) AS x(hash) ORDER BY hash ON CONFLICT DO NOTHING`, job.RepoID, job.ID, job.Version, hashes); err != nil {
		return err
	}
	if !current.Fences(job, time.Now().UTC()) {
		return domain.ErrConflict
	}
	return tx.Commit(ctx)
}

func (publication *pgLegacyStreamingPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, _ time.Time) error {
	if err := publication.matches(job); err != nil {
		return err
	}
	storage := publication.store
	return storage.WithinRepository(ctx, job.RepoID, func(write context.Context) error {
		bound := write.Value(repositoryTxKey{}).(*repositoryTx)
		current, err := publication.current(write, bound, job)
		if err != nil {
			return err
		}
		repo, err := storage.GetRepo(write, job.RepoID)
		if err != nil {
			return err
		}
		if err := checkDocJobPublicationPolicy(write, repo, job.DocIdentity); err != nil {
			return err
		}
		if err := publication.publish(write, bound); err != nil {
			return err
		}
		now := time.Now().UTC()
		if !current.Fences(job, now) {
			return domain.ErrConflict
		}
		if err := write.Err(); err != nil {
			return err
		}
		current.State, current.Reason = "completed", ""
		current.UpdatedAt, current.LeaseUntil = now, time.Time{}
		return storage.writeDocJob(write, current)
	})
}

func (publication *pgLegacyStreamingPublication) publish(ctx context.Context, tx pgx.Tx) error {
	job := publication.job
	manifest := publication.doc.Manifest()
	payload, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, job.DocHash, docCompress(payload))
	if err != nil {
		return err
	}
	var stored []byte
	if err := tx.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $2) FROM blobs WHERE hash=$1 FOR UPDATE`, job.DocHash, 2*maxLegacyReadBytes+1).Scan(&stored); err != nil {
		return mapNoRows(err)
	}
	stored, err = boundedLegacyObject(ctx, stored, maxLegacyReadBytes)
	if err != nil {
		return err
	}
	current, ok := domain.ParseDocChunkManifest(stored)
	alternate := !ok || !sameDocManifest(current, manifest)
	if alternate {
		if err := publication.validateRepresentation(ctx, tx, stored, current, ok); err != nil {
			return err
		}
	}
	originals := append([]domain.ContentHash(nil), job.Manifest.Chunks...)
	sort.Slice(originals, func(left, right int) bool { return originals[left] < originals[right] })
	for index, hash := range originals {
		if index > 0 && originals[index-1] == hash {
			continue
		}
		body, err := publication.store.legacyJobChunk(ctx, job.RepoID, hash, true)
		if err != nil {
			return err
		}
		if domain.HashContent(body) != hash {
			return domain.ErrIntegrity
		}
	}
	ordinals := make(map[domain.ContentHash]int, len(manifest.Chunks))
	ordered := make([]domain.ContentHash, 0, len(manifest.Chunks))
	for ordinal, hash := range manifest.Chunks {
		if _, seen := ordinals[hash]; !seen {
			ordinals[hash] = ordinal
			ordered = append(ordered, hash)
		}
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
	for _, hash := range ordered {
		ordinal := ordinals[hash]
		body, err := publication.doc.Chunk(ctx, ordinal)
		if err != nil {
			return err
		}
		var existing []byte
		err = tx.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $2) FROM blobs WHERE hash=$1 FOR SHARE`, hash, 2*domain.ChunkTarget+1).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			if inserted.RowsAffected() == 0 && !alternate {
				return domain.ErrIntegrity
			}
			if _, err = tx.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, hash, docCompress(body)); err != nil {
				return err
			}
			err = tx.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $2) FROM blobs WHERE hash=$1 FOR SHARE`, hash, 2*domain.ChunkTarget+1).Scan(&existing)
		}
		if err != nil {
			return err
		}
		existing, err = boundedLegacyObject(ctx, existing, domain.ChunkTarget)
		if err != nil || !bytes.Equal(existing, body) {
			return domain.ErrIntegrity
		}
		if _, err := tx.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2) ON CONFLICT DO NOTHING`, job.RepoID, hash); err != nil {
			return err
		}
	}
	if alternate && ok {
		for _, hash := range current.Chunks {
			if _, err := tx.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2) ON CONFLICT DO NOTHING`, job.RepoID, hash); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2) ON CONFLICT DO NOTHING`, job.RepoID, job.DocHash); err != nil {
		return err
	}
	return putPreparedReadIndexPG(ctx, tx, publication.read)
}

func (publication *pgLegacyStreamingPublication) validateRepresentation(ctx context.Context, tx pgx.Tx, raw []byte, manifest domain.DocChunkManifest, chunked bool) error {
	if !chunked {
		_, err := domain.VerifyStoredDocBytes(publication.job.DocHash, raw)
		return err
	}
	read := func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $2) FROM blobs WHERE hash=$1 FOR SHARE`, hash, 2*maxLegacyReadBytes+1).Scan(&raw); err != nil {
			return nil, mapNoRows(err)
		}
		return boundedLegacyObject(ctx, raw, maxLegacyReadBytes)
	}
	if manifest.Format == domain.ChunkFormatV2 {
		spool, err := newLegacyDocSpool()
		if err != nil {
			return err
		}
		defer spool.Close()
		_, err = publication.store.docProofs.rootVerifier.VerifyLegacyStream(ctx, publication.job.DocHash, manifest, read, spool)
		return err
	}
	if len(manifest.Chunks) > domain.MaxDocJobChunks {
		return domain.ErrDocumentResourceLimit
	}
	var bodies [][]byte
	total := len(manifest.Envelope)
	for _, hash := range manifest.Chunks {
		body, err := read(ctx, hash)
		if err != nil {
			return err
		}
		total += len(body)
		if total > maxLegacyReadBytes {
			return domain.ErrDocumentResourceLimit
		}
		bodies = append(bodies, body)
	}
	canonical, err := domain.AssembleDocChunks(manifest, bodies, publication.job.DocHash)
	if err != nil {
		return err
	}
	_, err = domain.VerifyStoredDocBytes(publication.job.DocHash, canonical)
	return err
}

var _ outbound.StreamingLegacyJobStore = (*PostgresStore)(nil)
