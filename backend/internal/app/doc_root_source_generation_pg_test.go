//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRootSourceGenerationPGStore struct {
	*store.PostgresStore
	pool            *pgxpool.Pool
	repo            domain.ContentHash
	chunk           domain.ContentHash
	directory       string
	claims          []domain.DocFinalizationJob
	prepared        []*docRootSourceGenerationPGPublication
	completionCalls int
	updates         int
}

type docRootSourceGenerationPGPublication struct {
	outbound.BoundedRootDocPublication
	storage            *docRootSourceGenerationPGStore
	job                domain.DocFinalizationJob
	paths              []string
	initialResources   outbound.RootPublicationResources
	resourceCalls      int
	completionCalls    int
	completionErr      error
	closeCalls         int
	closedResources    outbound.RootPublicationResources
	closedResourcesErr error
}

func (storage *docRootSourceGenerationPGStore) observe(ctx context.Context) error {
	if _, system := inbound.RepositoryActor(ctx); !system {
		return domain.ErrUnauthorized
	}
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if permit == nil || !permit.live.Load() {
		return errors.New("root source retry bypassed live worker admission")
	}
	return outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1)
}

func (storage *docRootSourceGenerationPGStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	if err := storage.observe(ctx); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	job, err := storage.PostgresStore.ClaimDocJob(ctx, repo, now, lease)
	if err == nil {
		storage.claims = append(storage.claims, job)
	}
	return job, err
}

func (storage *docRootSourceGenerationPGStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	if err := storage.observe(ctx); err != nil {
		return nil, err
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return nil, errors.New("root source retry bypassed the actual preparation snapshot")
	}
	prepared, err := storage.PostgresStore.PrepareRootDocJob(ctx, job)
	if err != nil {
		return prepared, err
	}
	bounded, ok := prepared.(outbound.BoundedRootDocPublication)
	if !ok {
		return nil, errors.Join(errors.New("real root preparation lost resource accounting"), prepared.Close())
	}
	resources, err := bounded.RetainedResources()
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	paths, err := filepath.Glob(filepath.Join(storage.directory, "cxthub-doc-range-*"))
	if err != nil || len(paths) != 2 {
		return nil, errors.Join(fmt.Errorf("root preparation must retain original and derived spools: paths=%v error=%v", paths, err), prepared.Close())
	}
	var spoolBytes int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, errors.Join(err, prepared.Close())
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() <= 0 {
			return nil, errors.Join(errors.New("root preparation did not retain private publication spools"), prepared.Close())
		}
		spoolBytes += info.Size()
	}
	if spoolBytes != resources.SpoolBytes || resources.HeapBytes <= 0 {
		return nil, errors.Join(errors.New("root preparation did not account for both publication spools"), prepared.Close())
	}
	publication := &docRootSourceGenerationPGPublication{
		BoundedRootDocPublication: bounded,
		storage:                   storage,
		job:                       job,
		paths:                     paths,
		initialResources:          resources,
	}
	storage.prepared = append(storage.prepared, publication)
	return publication, nil
}

func (publication *docRootSourceGenerationPGPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	publication.resourceCalls++
	return publication.BoundedRootDocPublication.RetainedResources()
}

func (publication *docRootSourceGenerationPGPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	storage := publication.storage
	if err := storage.observe(ctx); err != nil {
		return err
	}
	if storage.InTransaction(ctx) || !reflect.DeepEqual(job, publication.job) {
		return errors.New("root completion did not leave its preparation snapshot with the exact claim")
	}
	current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil {
		return err
	}
	if !current.Fences(job, time.Now().UTC()) {
		return errors.New("root source mutation requires the actual current live lease")
	}
	storage.completionCalls++
	publication.completionCalls++
	if storage.completionCalls == 1 {
		updated, err := storage.pool.Exec(ctx, `UPDATE blobs source SET bytes=source.bytes
 WHERE source.hash=$2 AND EXISTS(SELECT 1 FROM repo_blobs owned
 WHERE owned.repo_id=$1 AND owned.kind='chunk' AND owned.hash=source.hash)`, storage.repo, storage.chunk)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return errors.New("identical-byte update did not change exactly one owned dependency")
		}
		storage.updates++
	}
	publication.completionErr = publication.BoundedRootDocPublication.Complete(ctx, job, now)
	return publication.completionErr
}

func (publication *docRootSourceGenerationPGPublication) Close() error {
	publication.closeCalls++
	err := publication.BoundedRootDocPublication.Close()
	publication.closedResources, publication.closedResourcesErr = publication.BoundedRootDocPublication.RetainedResources()
	return err
}
