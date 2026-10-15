package store

import (
	"context"
	"errors"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type fsStreamingRootPublication struct {
	store    *FSStore
	prepared *preparedStreamingRootDoc
}

func (store *FSStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	prepared, err := prepareStreamingRootJob(ctx, job, &store.docProofs.rootVerifier, store.streamingRootChunkReader(job.RepoID))
	if err != nil {
		return nil, err
	}
	return &fsStreamingRootPublication{store: store, prepared: prepared}, nil
}

func (store *FSStore) streamingRootChunkReader(repo domain.ContentHash) func(context.Context, domain.ContentHash) ([]byte, error) {
	return func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		if err := validateHashes(repo, hash); err != nil {
			return nil, err
		}
		return readRangeFile(ctx, store.chunkPath(repo, hash), maxStoredRangeChunkBytes)
	}
}

func (publication *fsStreamingRootPublication) Close() error {
	return publication.prepared.Close()
}

func (publication *fsStreamingRootPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	if err := publication.prepared.matches(job); err != nil {
		return err
	}
	store := publication.store
	queue := store.docQueue()
	queue.Lock()
	defer queue.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	old, err := store.readDocJob(job.RepoID, job.ID)
	if err != nil {
		return err
	}
	if !old.Fences(job, time.Now().UTC()) {
		return domain.ErrConflict
	}
	if err := publication.prepared.matches(old); err != nil {
		return err
	}
	retention := store.oauthLock()
	retention.Lock()
	defer retention.Unlock()
	metadata := store.refLock(job.RepoID, domain.RefBranch, "")
	metadata.Lock()
	defer metadata.Unlock()
	repo, err := store.GetRepo(ctx, job.RepoID)
	if err != nil {
		return err
	}
	if err := checkDocJobPublicationPolicy(ctx, repo, job.DocIdentity); err != nil {
		return err
	}
	fence := func() error {
		if !old.Fences(job, time.Now().UTC()) {
			return domain.ErrConflict
		}
		return ctx.Err()
	}
	if err := fence(); err != nil {
		return err
	}
	path := store.docPath(job.RepoID, job.DocHash)
	current, err := readRangeFile(ctx, path, maxStoredRangeRootBytes)
	missing := errors.Is(err, domain.ErrNotFound)
	if err != nil && !missing {
		return err
	}
	root := publication.prepared.root
	if !missing {
		if err := root.matchesStored(ctx, current); err != nil {
			return err
		}
	}
	if err := publication.prepared.compareCurrent(ctx, store.streamingRootChunkReader(job.RepoID)); err != nil {
		return err
	}
	if err := fence(); err != nil {
		return err
	}
	if err := store.putRangeIndex(ctx, job.RepoID, root.ranges, func() error {
		if err := fence(); err != nil {
			return err
		}
		current, err := readRangeFile(ctx, path, maxStoredRangeRootBytes)
		if errors.Is(err, domain.ErrNotFound) && missing {
			return writeAtomic(path, docCompress(root.canonical))
		}
		if err != nil {
			return err
		}
		return root.matchesStored(ctx, current)
	}); err != nil {
		return err
	}
	now = time.Now().UTC()
	if !old.Fences(job, now) {
		return domain.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	old.State = "completed"
	old.Reason = ""
	old.UpdatedAt = now
	old.LeaseUntil = time.Time{}
	return store.writeDocJob(old)
}

var _ outbound.StreamingRootJobStore = (*FSStore)(nil)
var _ outbound.PreparedRootDocPublication = (*fsStreamingRootPublication)(nil)
