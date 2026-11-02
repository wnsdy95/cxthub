package documentworker

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type DisplayWorker struct {
	storage outbound.DocDisplayStore
	sources SourceReader
	scope   Scope
}

func NewDisplayWorker(storage outbound.DocDisplayStore, sources SourceReader, scope Scope) *DisplayWorker {
	return &DisplayWorker{storage: storage, sources: sources, scope: scope}
}

func (worker *DisplayWorker) Source(ctx context.Context, target domain.DocDisplayTarget) (documentquery.Source, error) {
	if target.Validate() != nil {
		return documentquery.Source{}, domain.ErrValidation
	}
	if absent(worker.sources) {
		return documentquery.Source{}, domain.ErrCatalogUnsupported
	}
	source, err := worker.sources.OpenSource(ctx, target.RepoID, target.DocHash)
	if err != nil {
		return documentquery.Source{}, err
	}
	if source.Binding() != target.Binding {
		return documentquery.Source{}, domain.ErrConflict
	}
	entry, err := source.Entry(target.Event.Index)
	if err != nil {
		return documentquery.Source{}, err
	}
	validKind := target.DisplayProfile() == domain.DocDisplayStringProfile && entry.Kind == domain.EventToolResult || target.DisplayProfile() == domain.DocDisplayProjectionProfile && (entry.Kind == domain.EventReasoning || entry.Kind == domain.EventMessage || entry.Kind == domain.EventToolCall || entry.Kind == domain.EventToolResult)
	if entry.DocEventIndex != target.Event || !validKind {
		return documentquery.Source{}, domain.ErrIntegrity
	}
	return source, nil
}

func (worker *DisplayWorker) Build(ctx context.Context, job domain.DocDisplayJob) (bool, error) {
	if job.Target.DisplayProfile() == domain.DocDisplayProjectionProfile {
		return worker.BuildEvent(ctx, job)
	}
	progress, err := worker.storage.LoadDocDisplayJob(ctx, job)
	if err != nil {
		return false, err
	}
	if progress.Source != nil && progress.Checkpoint == "" {
		progress, err = worker.storage.InitializeDocDisplay(ctx, job, *progress.Source)
		if err != nil {
			return false, err
		}
	}
	for processed := 0; processed < DisplayJobSteps; {
		if progress.Source == nil {
			progress, err = worker.AdvanceDiscovery(ctx, job, progress)
			if err != nil {
				return false, err
			}
			processed++
			continue
		}
		if progress.Source.EventHash != job.Target.Event.Hash || progress.Source.EventBytes != job.Target.Event.Length || progress.Checkpoint == "" || progress.Discovery != nil || progress.DiscoveryCheckpoint != "" {
			return false, domain.ErrIntegrity
		}
		if progress.State.Done {
			break
		}
		var steps [DisplayJobSteps]domain.DocDisplayStringStep
		prepared := 0
		var preparationError error
		err := worker.scope.WithinRepositoryRead(ctx, job.RepoID, func(read context.Context) error {
			opened, err := worker.Source(read, job.Target)
			if err != nil {
				return err
			}
			state := progress.State
			for prepared < DisplayJobSteps-processed && !state.Done {
				step, err := domain.StepDocDisplayString(read, *progress.Source, state, func(read context.Context, offset, length int) ([]byte, error) {
					if err := read.Err(); err != nil {
						return nil, err
					}
					return opened.Read(job.Target.Event.Offset+offset, length)
				})
				if err != nil {
					preparationError = err
					return nil
				}
				steps[prepared], state = step, step.State
				prepared++
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		for index := 0; index < prepared; index++ {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			progress, err = worker.storage.SaveDocDisplayStep(ctx, job, progress.Checkpoint, steps[index])
			steps[index] = domain.DocDisplayStringStep{}
			if err != nil {
				return false, err
			}
			processed++
		}
		if preparationError != nil {
			return false, preparationError
		}
	}
	if progress.Source == nil || !progress.State.Done {
		return false, nil
	}
	return true, worker.storage.CompleteDocDisplay(ctx, job, progress.Checkpoint, time.Now().UTC())
}

func (worker *DisplayWorker) AdvanceDiscovery(ctx context.Context, job domain.DocDisplayJob, progress domain.DocDisplayPreparation) (domain.DocDisplayPreparation, error) {
	if progress.Checkpoint != "" || (progress.Discovery == nil) != (progress.DiscoveryCheckpoint == "") {
		return domain.DocDisplayPreparation{}, domain.ErrIntegrity
	}
	state, err := domain.NewDocFieldDiscoveryState(job.Target.Event.Hash, job.Target.Event.Length)
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	if progress.Discovery != nil {
		state = *progress.Discovery
		if state.EventHash != job.Target.Event.Hash || state.EventBytes != job.Target.Event.Length {
			return domain.DocDisplayPreparation{}, domain.ErrIntegrity
		}
	}
	var step domain.DocFieldDiscoveryStep
	err = worker.scope.WithinRepositoryRead(ctx, job.RepoID, func(read context.Context) error {
		opened, err := worker.Source(read, job.Target)
		if err != nil {
			return err
		}
		length := min(domain.DocFieldDiscoveryStepBytes, state.EventBytes-state.Next)
		raw, err := opened.Read(job.Target.Event.Offset+state.Next, length)
		if err != nil {
			return err
		}
		if len(raw) != length {
			return domain.ErrIntegrity
		}
		step, err = domain.AdvanceDocFieldDiscovery(read, state, raw)
		return err
	})
	if err != nil {
		return domain.DocDisplayPreparation{}, err
	}
	return worker.storage.SaveDocDisplayDiscovery(ctx, job, step)
}

func (worker *DisplayWorker) RunJob(ctx context.Context, job domain.DocDisplayJob) error {
	completed := false
	err := WithLease(ctx, Lease, func(renew context.Context) error {
		return worker.storage.RenewDocDisplay(renew, job, time.Now().UTC(), Lease)
	}, func(work context.Context) error {
		var err error
		completed, err = worker.Build(work, job)
		return err
	})
	if completed && err == nil || ctx.Err() != nil {
		return err
	}

	now := time.Now().UTC()
	job.State, job.Reason, job.NextAttempt, job.LeaseUntil = "waiting", "", now, time.Time{}
	if err != nil {
		job.State, job.Reason = "retrying", "temporary_failure"
		job.NextAttempt = now.Add(time.Second << min(job.Attempts, 8))
		switch {
		case errors.Is(err, domain.ErrNotFound):
			job.State, job.Reason = "rejected", "source_unavailable"
		case errors.Is(err, domain.ErrDocDisplayIncarnationChanged):
			job.Reason = "source_incarnation_changed"
		case errors.Is(err, domain.ErrConflict):
			job.State, job.Reason = "rejected", "source_changed"
		case errors.Is(err, domain.ErrDocFindDisplayUnavailable):
			job.State, job.Reason = "rejected", "unsupported_display"
		case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation):
			job.State, job.Reason = "rejected", "invalid_document"
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			job.Reason = "verification_interrupted"
		}
	}
	finish, release := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer release()
	return errors.Join(err, worker.storage.FinishDocDisplay(finish, job, now))
}

func (worker *DisplayWorker) Process(ctx context.Context, limit int) (int, error) {
	if worker.storage == nil || limit <= 0 {
		return 0, nil
	}
	ctx = worker.scope.WorkerContext(ctx)
	for processed := 0; processed < limit; processed++ {
		claimed := false
		err := worker.scope.WithinWork(ctx, func(bound context.Context) error {
			job, err := worker.storage.ClaimDocDisplay(bound, time.Now().UTC(), Lease)
			if err != nil {
				return err
			}
			claimed = true
			return worker.RunJob(bound, job)
		})
		if !claimed && errors.Is(err, domain.ErrNotFound) {
			return processed, nil
		}
		if err != nil {
			return processed, err
		}
	}
	return limit, nil
}

func (worker *DisplayWorker) Run(ctx context.Context) {
	if worker.storage == nil {
		return
	}
	for {
		processed, err := worker.Process(ctx, 1)
		if err != nil && ctx.Err() == nil {
			log.Printf("document display preparation: %v", err)
		}
		if processed > 0 && err == nil {
			continue
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
