package documentworker

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const findFieldsReadBytes = 128 << 10

type FindFieldsWorker struct {
	storage outbound.DocFindFieldsStore
	sources SourceReader
	scope   Scope
}

func NewFindFieldsWorker(storage outbound.DocFindFieldsStore, sources SourceReader, scope Scope) *FindFieldsWorker {
	return &FindFieldsWorker{storage: storage, sources: sources, scope: scope}
}

type findFieldsEventReader struct {
	source documentquery.Source
	entry  domain.DocEventIndex
	next   int
}

func (reader *findFieldsEventReader) Read(target []byte) (int, error) {
	if len(target) == 0 {
		return 0, nil
	}
	if reader.next == reader.entry.Length {
		return 0, io.EOF
	}
	length := min(len(target), findFieldsReadBytes, reader.entry.Length-reader.next)
	raw, err := reader.source.Read(reader.entry.Offset+reader.next, length)
	if err != nil {
		return 0, err
	}
	if len(raw) != length {
		return 0, domain.ErrIntegrity
	}
	reader.next += copy(target, raw)
	return length, nil
}

type findFieldsBatch struct {
	fields []domain.DocFindFields
	next   int
	total  int
}

func (worker *FindFieldsWorker) Build(ctx context.Context, job domain.DocRangeIndexJob) error {
	position := 0
	for {
		var batch findFieldsBatch
		err := worker.scope.WithinRepositoryRead(ctx, job.RepoID, func(read context.Context) error {
			if absent(worker.sources) {
				return domain.ErrCatalogUnsupported
			}
			source, err := worker.sources.OpenViewSource(read, job.RepoID, job.DocHash)
			if err != nil {
				return err
			}
			if !source.Root() || source.Binding().Validate() != nil {
				return domain.ErrIntegrity
			}
			batch = findFieldsBatch{next: min(position+domain.DocRangePageEvents, source.Total()), total: source.Total()}
			for index := position; index < batch.next; index++ {
				entry, err := source.Entry(index)
				if err != nil {
					return err
				}
				if entry.Kind != domain.EventReasoning && entry.Kind != domain.EventMessage || entry.EmptyReasoning || entry.Length <= findFieldsReadBytes {
					continue
				}
				_, err = worker.storage.ReadDocFindFields(read, job.RepoID, job.DocHash, entry.Hash, entry.Length)
				if err == nil {
					continue
				}
				if !errors.Is(err, domain.ErrNotFound) {
					return err
				}
				fields, err := domain.BuildDocFindFieldsReader(read, entry.Hash, entry.Length, &findFieldsEventReader{source: source, entry: entry.DocEventIndex})
				if err != nil {
					return err
				}
				batch.fields = append(batch.fields, fields)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, fields := range batch.fields {
			if err := worker.storage.PutDocFindFields(ctx, job, fields); err != nil {
				return err
			}
		}
		position = batch.next
		if position == batch.total {
			return nil
		}
	}
}

func (worker *FindFieldsWorker) RunJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	err := WithLease(ctx, Lease, func(renew context.Context) error {
		return worker.storage.RenewDocFindFields(renew, job, time.Now().UTC(), Lease)
	}, func(work context.Context) error {
		return worker.Build(work, job)
	})
	if ctx.Err() != nil {
		return err
	}

	now := time.Now().UTC()
	job.State, job.Reason, job.LeaseUntil = "completed", "", time.Time{}
	job.NextAttempt = now
	if err != nil {
		job.State, job.Reason = "retrying", "temporary_failure"
		job.NextAttempt = now.Add(time.Second << min(job.Attempts, 8))
		switch {
		case errors.Is(err, domain.ErrNotFound):
			job.State, job.Reason = "rejected", "source_unavailable"
		case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation):
			job.State, job.Reason = "rejected", "invalid_document"
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			job.Reason = "verification_interrupted"
		}
	}
	finish, release := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer release()
	finished := worker.storage.FinishDocFindFields(finish, job, now)
	if err == nil && errors.Is(finished, domain.ErrConflict) {
		job.State, job.Reason = "retrying", "source_changed"
		released := worker.storage.FinishDocFindFields(finish, job, now)
		if released == nil {
			_, refreshed := worker.storage.EnqueueDocFindFields(finish, job.RepoID, job.DocHash, now)
			return errors.Join(finished, refreshed)
		}
		return errors.Join(finished, released)
	}
	if finished == nil && errors.Is(err, domain.ErrConflict) {
		_, refreshed := worker.storage.EnqueueDocFindFields(finish, job.RepoID, job.DocHash, now)
		return errors.Join(err, refreshed)
	}
	return errors.Join(err, finished)
}

func (worker *FindFieldsWorker) Process(ctx context.Context, limit int) error {
	if worker.storage == nil {
		return nil
	}
	ctx = worker.scope.WorkerContext(ctx)
	for processed := 0; processed < limit; processed++ {
		claimed := false
		err := worker.scope.WithinWork(ctx, func(bound context.Context) error {
			job, err := worker.storage.ClaimDocFindFields(bound, time.Now().UTC(), Lease)
			if err != nil {
				return err
			}
			claimed = true
			return worker.RunJob(bound, job)
		})
		if !claimed && errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (worker *FindFieldsWorker) Run(ctx context.Context) {
	if worker.storage == nil {
		return
	}
	for {
		if err := worker.Process(ctx, 1); err != nil && ctx.Err() == nil {
			log.Printf("document find fields: %v", err)
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
