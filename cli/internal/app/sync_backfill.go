package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ inbound.HistoricalSync = (*SyncRepoService)(nil)

// SyncHistorical serializes only archival workers. The object-retention lease
// is released between jobs so capture collection can proceed for unpinned data.
// A failed job keeps its pin and backs off; later independent jobs can progress.
func (s *SyncRepoService) SyncHistorical(ctx context.Context, in inbound.SyncInput, limit int) (out inbound.HistoricalSyncOutput, err error) {
	queue, ok := s.store.(outbound.HistoricalBackfillStore)
	if !ok {
		return out, nil
	}
	if _, ok := s.store.(outbound.ObjectRetention); !ok {
		return out, fmt.Errorf("historical sync requires coordinated object retention")
	}
	repo, err := s.repoID(ctx, in)
	if err != nil {
		return out, err
	}
	if limit < 1 {
		limit = 8
	}
	if limit > 32 {
		limit = 32
	}
	entered, err := queue.WithBackfillWorker(ctx, func() error {
		jobs, err := queue.ListBackfills(ctx, repo)
		if err != nil {
			return err
		}
		processed := 0
		for _, job := range jobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if job.NextAttempt.After(time.Now()) || processed >= limit {
				continue
			}
			processed++
			_, uploadErr := withRetainedObjects(ctx, s.store, func() (bool, error) {
				if err := s.pushHistoricalSnapshot(ctx, repo, job.Snapshot); err != nil {
					return false, err
				}
				return true, queue.UpdateBackfill(ctx, job, nil)
			})
			if uploadErr == nil {
				out.Completed++
				continue
			}
			out.Failed++
			next := job
			next.Version++
			if next.Attempts < 32 {
				next.Attempts++
			}
			next.NextAttempt = time.Now().UTC().Add(time.Second << min(next.Attempts, 8))
			next.Reason = "unavailable"
			if errors.Is(uploadErr, domain.ErrHashMismatch) || errors.Is(uploadErr, domain.ErrInvalidCIR) {
				next.Reason = "integrity"
			}
			if errors.Is(uploadErr, domain.ErrSyncConflict) {
				next.Reason = "conflict"
			}
			if errors.Is(uploadErr, context.Canceled) || errors.Is(uploadErr, context.DeadlineExceeded) {
				next.Reason = "cancelled"
			}
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := queue.UpdateBackfill(persist, job, &next)
			cancel()
			if err != nil {
				return errors.Join(uploadErr, err)
			}
		}
		return nil
	})
	out.Busy = !entered && err == nil
	if err != nil {
		return out, err
	}
	jobs, err := queue.ListBackfills(ctx, repo)
	if err != nil {
		return out, err
	}
	out.Pending = len(jobs)
	for _, job := range jobs {
		if out.NextAttempt.IsZero() || job.NextAttempt.Before(out.NextAttempt) {
			out.NextAttempt = job.NextAttempt
		}
	}
	return out, nil
}

func (s *SyncRepoService) pushHistoricalSnapshot(ctx context.Context, repo string, id domain.ContentHash) error {
	if _, err := s.store.GetSnapshot(ctx, id); err != nil {
		return err
	}
	man, _, err := s.readPushCatalog(ctx, repo)
	if err != nil {
		return err
	}
	// The queued object remains an archival obligation even if it was later
	// relabelled as a stash. This synthetic root is never sent to the server.
	man.Refs = append(man.Refs, domain.Ref{Kind: domain.RefTag, Target: id})
	all, err := s.collectSnapshots(ctx, repo, man)
	if err != nil {
		return err
	}
	attachments, err := s.remoteMemoryCatalog(ctx, repo)
	if err != nil {
		return err
	}
	selected, _, err := s.snapshotDependencyClosure(ctx, all, []domain.ContentHash{id}, nil, attachments)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return domain.ErrNotFound
	}
	snaps, docs, err := s.selectPushObjects(ctx, repo, selected)
	if err != nil {
		return err
	}
	if err := s.pushSettingsObjects(ctx, repo, selected); err != nil {
		return err
	}
	var memories []domain.Snapshot
	for _, snap := range selected {
		if snap.MemoryHash != "" {
			memories = append(memories, snap)
		}
	}
	plans, ahead, err := s.prepareMemoryPushPlans(ctx, repo, memories, attachments)
	if err != nil {
		return err
	}
	if err := s.pushSelectedObjects(ctx, repo, selected, snaps, docs); err != nil {
		return err
	}
	return s.sendMemoryPushPlans(ctx, repo, plans, attachments, ahead)
}
