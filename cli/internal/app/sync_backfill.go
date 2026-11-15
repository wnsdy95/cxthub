package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

var _ inbound.HistoricalSync = (*SyncRepoService)(nil)

// Background uploads of retained history share the server with live work.
// The server verifies every uploaded document in full while holding its single
// bulk slot, so a long upload is followed by an equal rest, and a busy server
// ends the round instead of being asked for the next snapshot at once.
const (
	historicalUploadRestFloor = time.Second
	historicalUploadRestCap   = 15 * time.Minute
	historicalBusyHold        = time.Minute
)

// SyncHistorical serializes only archival workers. The object-retention lease
// is released between jobs so capture collection can proceed for unpinned data.
// A failed job keeps its pin and backs off; later independent jobs can progress.
// A paused repository uploads nothing, and a hold delays the next upload; the
// queue entries keep their retention pins either way.
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
	control, err := queue.HistoricalUploadControl(ctx, repo)
	if err != nil {
		return out, err
	}
	hold := control.HoldUntil
	out.Paused = control.Paused()
	entered := false
	if !out.Paused {
		entered, err = queue.WithBackfillWorker(ctx, func() error {
			// Another process may have paused or held uploads since the read above.
			control, err := queue.HistoricalUploadControl(ctx, repo)
			if err != nil {
				return err
			}
			hold, out.Paused = control.HoldUntil, control.Paused()
			if out.Paused || hold.After(time.Now()) {
				return nil
			}
			jobs, err := queue.ListBackfills(ctx, repo)
			if err != nil {
				return err
			}
			due := jobs[:0]
			for _, job := range jobs {
				if !job.NextAttempt.After(time.Now()) {
					due = append(due, job)
				}
			}
			s.orderHistoricalUploads(ctx, due)
			for processed, job := range due {
				if err := ctx.Err(); err != nil {
					return err
				}
				if processed >= limit {
					break
				}
				started := time.Now()
				_, uploadErr := withRetainedObjects(ctx, s.store, func() (bool, error) {
					if err := s.pushHistoricalSnapshot(ctx, repo, job.Snapshot); err != nil {
						return false, err
					}
					return true, queue.UpdateBackfill(ctx, job, nil)
				})
				if uploadErr == nil {
					out.Completed++
					if rest := historicalUploadRest(time.Since(started)); rest > 0 {
						// The upload is already acknowledged; record its rest even if the
						// helper's lifetime ends right now.
						persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
						hold, err = holdHistoricalUploads(persist, queue, repo, time.Now().Add(rest))
						cancel()
						return err
					}
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
				if err == nil && historicalServerBusy(uploadErr) {
					hold, err = holdHistoricalUploads(persist, queue, repo, time.Now().Add(max(historicalBusyHold, time.Until(next.NextAttempt))))
				}
				cancel()
				if err != nil {
					return errors.Join(uploadErr, err)
				}
				if historicalServerBusy(uploadErr) {
					return nil
				}
			}
			return nil
		})
	}
	out.Busy = !out.Paused && !entered && err == nil
	if err != nil {
		return out, err
	}
	jobs, err := queue.ListBackfills(ctx, repo)
	if err != nil {
		return out, err
	}
	out.Pending = len(jobs)
	if out.Paused {
		return out, nil
	}
	for _, job := range jobs {
		if out.NextAttempt.IsZero() || job.NextAttempt.Before(out.NextAttempt) {
			out.NextAttempt = job.NextAttempt
		}
	}
	if out.Pending > 0 && hold.After(out.NextAttempt) {
		out.NextAttempt = hold
	}
	return out, nil
}

// historicalUploadRest is the pause after a successful upload: as long as the
// upload took, so background history uses at most about half of the server's
// time. Short uploads need no rest.
func historicalUploadRest(elapsed time.Duration) time.Duration {
	if elapsed < historicalUploadRestFloor {
		return 0
	}
	return min(elapsed, historicalUploadRestCap)
}

func holdHistoricalUploads(ctx context.Context, queue outbound.HistoricalBackfillStore, repo string, until time.Time) (time.Time, error) {
	control, err := queue.UpdateHistoricalUploadControl(ctx, repo, func(current domain.HistoricalUploadControl) domain.HistoricalUploadControl {
		if until.After(current.HoldUntil) {
			current.HoldUntil = until.UTC()
		}
		return current
	})
	return control.HoldUntil, err
}

// historicalServerBusy reports a rejection about the server rather than the
// snapshot: overload, rate limiting, or a gateway that never reached it.
func historicalServerBusy(err error) bool {
	var status statusCodeError
	if !errors.As(err, &status) {
		return false
	}
	switch status.StatusCode() {
	case 429, 502, 503, 504:
		return true
	}
	return false
}

// Retained snapshots of a session are usually cumulative captures, so capture
// order makes each upload extend the one before it and archives history oldest
// first. Snapshots that cannot be read keep queue order after the rest.
func (s *SyncRepoService) orderHistoricalUploads(ctx context.Context, jobs []domain.SnapshotBackfill) {
	created := make(map[domain.ContentHash]time.Time, len(jobs))
	for _, job := range jobs {
		if snap, err := s.store.GetSnapshot(ctx, job.Snapshot); err == nil && !snap.CreatedAt.IsZero() {
			created[job.Snapshot] = snap.CreatedAt
		}
	}
	sort.SliceStable(jobs, func(left, right int) bool {
		a, aKnown := created[jobs[left].Snapshot]
		b, bKnown := created[jobs[right].Snapshot]
		if aKnown != bKnown {
			return aKnown
		}
		return aKnown && a.Before(b)
	})
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
