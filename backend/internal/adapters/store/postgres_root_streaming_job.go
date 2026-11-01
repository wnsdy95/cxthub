//go:build postgres

package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pgStreamingRootPublication struct {
	*preparedStreamingRootDoc
	store     *PostgresStore
	source    pgRootSourceWitness
	indexes   *pgPreparedRootIndexes
	closeOnce sync.Once
	closeErr  error
}

func (storage *PostgresStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	sources := make(map[domain.ContentHash]pgRootDependencySource)
	prepared, err := prepareStreamingRootJob(ctx, job, &storage.docProofs.rootVerifier, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		var raw []byte
		source := pgRootDependencySource{hash: hash}
		err := storage.db(ctx).QueryRow(ctx, `SELECT substring(b.bytes FROM 1 FOR $3),b.doc_range_source_version,incarnation.incarnation::text
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=b.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk' AND rb.hash=$2`, job.RepoID, hash, maxStoredRangeChunkBytes+1).Scan(&raw, &source.version, &source.incarnation)
		if err != nil {
			return nil, mapNoRows(err)
		}
		if len(raw) > maxStoredRangeChunkBytes {
			return nil, domain.ErrIntegrity
		}
		if previous, exists := sources[hash]; exists && previous != source {
			return nil, domain.ErrConflict
		}
		sources[hash] = source
		return raw, ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	if err := verifyMigrationRootSources(ctx, storage.db(ctx)); err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	source, err := newPGRootSourceWitness(prepared, sources)
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	indexes, err := preparePGRootIndexes(ctx, prepared)
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	return &pgStreamingRootPublication{preparedStreamingRootDoc: prepared, store: storage, source: source, indexes: indexes}, nil
}

func (publication *pgStreamingRootPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, _ time.Time) error {
	if err := publication.matches(job); err != nil {
		return err
	}
	storage := publication.store
	return storage.WithinRepository(ctx, job.RepoID, func(write context.Context) error {
		current, err := readLockedDocJobPG(write, storage.db(write), job.RepoID, job.ID)
		if err != nil {
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
	if err := publication.source.matches(job, publication.root.canonical); err != nil {
		return err
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
	read, current, err := storage.retainedRootChunkReaderWithSource(ctx, job.RepoID, conversationChunkOrder(publication.root.manifest), &publication.source)
	if err != nil {
		return err
	}
	if !current {
		if err := publication.compareCurrent(ctx, read); err != nil {
			return err
		}
		return domain.ErrConflict
	}
	if _, err := bound.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2) ON CONFLICT DO NOTHING`, job.RepoID, job.DocHash); err != nil {
		return err
	}
	return storage.putPreparedRootIndexes(ctx, job.RepoID, publication.preparedStreamingRootDoc, publication.indexes)
}

var _ outbound.StreamingRootJobStore = (*PostgresStore)(nil)
