//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pgStreamingRootPublication struct {
	*preparedStreamingRootDoc
	store *PostgresStore
}

func (storage *PostgresStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	prepared, err := prepareStreamingRootJob(ctx, job, &storage.docProofs.rootVerifier, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		var raw []byte
		err := storage.db(ctx).QueryRow(ctx, `SELECT substring(b.bytes FROM 1 FOR $3) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2`, job.RepoID, hash, maxStoredRangeChunkBytes+1).Scan(&raw)
		if err == nil && len(raw) > maxStoredRangeChunkBytes {
			return nil, domain.ErrIntegrity
		}
		return raw, mapNoRows(err)
	})
	if err != nil {
		return nil, err
	}
	return &pgStreamingRootPublication{preparedStreamingRootDoc: prepared, store: storage}, nil
}

func (publication *pgStreamingRootPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, _ time.Time) error {
	if err := publication.matches(job); err != nil {
		return err
	}
	storage := publication.store
	return storage.WithinRepository(ctx, job.RepoID, func(write context.Context) error {
		var raw []byte
		if err := storage.db(write).QueryRow(write, `SELECT payload FROM doc_finalization_jobs WHERE repo_id=$1 AND id=$2 FOR UPDATE`, job.RepoID, job.ID).Scan(&raw); err != nil {
			return mapNoRows(err)
		}
		var current domain.DocFinalizationJob
		if err := json.Unmarshal(raw, &current); err != nil {
			return err
		}
		if !current.Fences(job, time.Now().UTC()) {
			return domain.ErrConflict
		}
		if err := publication.matches(current); err != nil {
			return err
		}
		repo, err := storage.GetRepo(write, job.RepoID)
		if err != nil {
			return err
		}
		if err := checkDocJobPublicationPolicy(write, repo, domain.DocumentIdentityRootV1); err != nil {
			return err
		}
		if err := publication.publish(write); err != nil {
			return err
		}
		now := time.Now().UTC()
		if !current.Fences(job, now) {
			return domain.ErrConflict
		}
		if err := write.Err(); err != nil {
			return err
		}
		current.State = "completed"
		current.Reason = ""
		current.UpdatedAt = now
		current.LeaseUntil = time.Time{}
		return storage.writeDocJob(write, current)
	})
}

func (publication *pgStreamingRootPublication) publish(ctx context.Context) error {
	storage, job := publication.store, publication.job
	bound, ok := ctx.Value(repositoryTxKey{}).(*repositoryTx)
	if !ok || bound.owner != storage || bound.readOnly || bound.repo != job.RepoID {
		return domain.ErrConflict
	}
	if _, err := bound.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2) ON CONFLICT(hash) DO NOTHING`, job.DocHash, docCompress(publication.root.canonical)); err != nil {
		return err
	}
	var raw []byte
	if err := bound.QueryRow(ctx, `SELECT substring(bytes FROM 1 FOR $2) FROM blobs WHERE hash=$1 FOR UPDATE`, job.DocHash, maxStoredRangeRootBytes+1).Scan(&raw); err != nil {
		return mapNoRows(err)
	}
	if len(raw) > maxStoredRangeRootBytes {
		return domain.ErrIntegrity
	}
	if err := publication.root.matchesStored(ctx, raw); err != nil {
		return err
	}
	if err := publication.compareCurrent(ctx, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		var raw []byte
		err := bound.QueryRow(ctx, `SELECT substring(b.bytes FROM 1 FOR $3) FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2 FOR SHARE OF rb,b`, job.RepoID, hash, maxStoredRangeChunkBytes+1).Scan(&raw)
		if err == nil && len(raw) > maxStoredRangeChunkBytes {
			return nil, domain.ErrIntegrity
		}
		return raw, mapNoRows(err)
	}); err != nil {
		return err
	}
	if _, err := bound.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2) ON CONFLICT DO NOTHING`, job.RepoID, job.DocHash); err != nil {
		return err
	}
	return storage.putRangeIndex(ctx, job.RepoID, publication.root.ranges)
}

var _ outbound.StreamingRootJobStore = (*PostgresStore)(nil)
