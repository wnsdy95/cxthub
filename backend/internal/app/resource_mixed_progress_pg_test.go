//go:build postgres

package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type progressObservation struct {
	Operation      string             `json:"operation"`
	Repo           string             `json:"repo"`
	ElapsedMS      float64            `json:"elapsed_ms"`
	QueueMS        float64            `json:"granted_queue_ms"`
	ReadTxMS       float64            `json:"owned_read_transaction_ms"`
	WriteTxMS      float64            `json:"owned_write_transaction_ms"`
	Outcome        string             `json:"outcome"`
	Detail         string             `json:"detail,omitempty"`
	StartMS        float64            `json:"start_ms"`
	EndMS          float64            `json:"end_ms"`
	Claims         []progressClaim    `json:"claims,omitempty"`
	Phases         map[string]float64 `json:"phases_ms,omitempty"`
	RootHeapBytes  int64              `json:"prepared_root_heap_bytes,omitempty"`
	RootSpoolBytes int64              `json:"prepared_root_spool_bytes,omitempty"`
	ResumeWaitMS   float64            `json:"root_resume_wait_ms,omitempty"`
	permits        map[*resourcePermit]bool
	origin         time.Time
}

type progressClaim struct {
	Repo    domain.ContentHash `json:"repo"`
	Phase   string             `json:"phase"`
	StartMS float64            `json:"start_ms"`
}

type progressObservationKey struct{}

func observeProgressPermit(ctx context.Context) *progressObservation {
	row, _ := ctx.Value(progressObservationKey{}).(*progressObservation)
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if row != nil && permit != nil && permit.live.Load() && !row.permits[permit] {
		row.permits[permit] = true
		row.QueueMS += float64(permit.wait) / float64(time.Millisecond)
	}
	return row
}

type progressStore struct{ *store.PostgresStore }

func observeProgressPhase(ctx context.Context, phase string, started time.Time) {
	if row, ok := ctx.Value(progressObservationKey{}).(*progressObservation); ok {
		if row.Phases == nil {
			row.Phases = make(map[string]float64)
		}
		row.Phases[phase] += float64(time.Since(started)) / float64(time.Millisecond)
	}
}

type progressRootPublication struct {
	outbound.PreparedRootDocPublication
	resources outbound.RootPublicationResources
}

func (publication *progressRootPublication) RetainedResources() (outbound.RootPublicationResources, error) {
	if bounded, ok := publication.PreparedRootDocPublication.(outbound.BoundedRootDocPublication); ok {
		resources, err := bounded.RetainedResources()
		publication.resources = resources
		return resources, err
	}
	return outbound.RootPublicationResources{}, nil
}

func (publication *progressRootPublication) Complete(ctx context.Context, job domain.DocFinalizationJob, now time.Time) error {
	if row, ok := ctx.Value(progressObservationKey{}).(*progressObservation); ok {
		row.RootHeapBytes, row.RootSpoolBytes = publication.resources.HeapBytes, publication.resources.SpoolBytes
		if permit, ok := ctx.Value(resourcePermitKey{}).(*resourcePermit); ok {
			row.ResumeWaitMS = float64(permit.resumeWait.Load()) / float64(time.Millisecond)
			row.QueueMS += row.ResumeWaitMS
		}
	}
	defer observeProgressPhase(ctx, "complete", time.Now())
	return publication.PreparedRootDocPublication.Complete(ctx, job, now)
}

type progressStagedRootPublication struct {
	*progressRootPublication
	stage outbound.StagedDocPublication
}

func (publication *progressStagedRootPublication) Stage(ctx context.Context, job domain.DocFinalizationJob, lease time.Duration) error {
	defer observeProgressPhase(ctx, "stage", time.Now())
	return publication.stage.Stage(ctx, job, lease)
}

func (storage *progressStore) PrepareRootDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	defer observeProgressPhase(ctx, "prepare", time.Now())
	prepared, err := storage.PostgresStore.PrepareRootDocJob(ctx, job)
	if prepared == nil {
		return nil, err
	}
	wrapped := &progressRootPublication{PreparedRootDocPublication: prepared}
	if staged, ok := prepared.(outbound.StagedDocPublication); ok {
		return &progressStagedRootPublication{progressRootPublication: wrapped, stage: staged}, err
	}
	return wrapped, err
}

func observeProgressClaim(ctx context.Context, repo domain.ContentHash, phase string, err error) {
	row := observeProgressPermit(ctx)
	if row != nil && err == nil {
		row.Claims = append(row.Claims, progressClaim{Repo: repo, Phase: phase, StartMS: float64(time.Since(row.origin)) / float64(time.Millisecond)})
	}
}

func (storage *progressStore) WithinReadSnapshot(ctx context.Context, operation func(context.Context) error) error {
	row := observeProgressPermit(ctx)
	owned := !storage.InTransaction(ctx)
	started := time.Now()
	err := storage.PostgresStore.WithinReadSnapshot(ctx, operation)
	if row != nil && owned {
		row.ReadTxMS += float64(time.Since(started)) / float64(time.Millisecond)
	}
	return err
}

func (storage *progressStore) WithinRepository(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	row := observeProgressPermit(ctx)
	owned := !storage.InTransaction(ctx)
	started := time.Now()
	err := storage.PostgresStore.WithinRepository(ctx, repo, operation)
	if row != nil && owned {
		row.WriteTxMS += float64(time.Since(started)) / float64(time.Millisecond)
	}
	return err
}

func (storage *progressStore) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	observeProgressPermit(ctx)
	job, err := storage.PostgresStore.ClaimDocJob(ctx, repo, now, lease)
	observeProgressClaim(ctx, job.RepoID, "publish", err)
	return job, err
}
