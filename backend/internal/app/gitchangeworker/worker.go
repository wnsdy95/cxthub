package gitchangeworker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type Worker struct {
	scope      Scope
	jobs       outbound.GitChangeStore
	metadata   RepositoryReader
	reader     outbound.GitEvidenceReader
	cache      evidencequery.GitDeltaReader
	authorizer outbound.GitSourceAuthorizer
}

func New(config Config) (*Worker, error) {
	if config.Profile != Compatibility && config.Profile != Complete {
		return nil, fmt.Errorf("%w: Git change worker profile is required", domain.ErrValidation)
	}
	if absent(config.Scope) {
		return nil, fmt.Errorf("%w: Git change worker scope is required", domain.ErrValidation)
	}
	for _, dependency := range []struct {
		name     string
		value    any
		required bool
	}{
		{name: "jobs", value: config.Jobs, required: true},
		{name: "metadata", value: config.Metadata, required: true},
		{name: "reader", value: config.Reader, required: true},
		{name: "cache", value: config.Cache},
		{name: "authorizer", value: config.Authorizer},
	} {
		if (dependency.value != nil || config.Profile == Complete && dependency.required) && absent(dependency.value) {
			return nil, fmt.Errorf("%w: Git change worker %s is required", domain.ErrValidation, dependency.name)
		}
	}
	return &Worker{
		scope: config.Scope, jobs: config.Jobs, metadata: config.Metadata,
		reader: config.Reader, cache: config.Cache, authorizer: config.Authorizer,
	}, nil
}

func absent(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

type repositoryEvidence struct {
	repo   domain.ContentHash
	reader outbound.GitEvidenceReader
}

func RepositoryEvidence(repo domain.ContentHash, reader outbound.GitEvidenceReader) evidencequery.ReversalReader {
	return repositoryEvidence{repo: repo, reader: reader}
}

func (reader repositoryEvidence) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	return reader.reader.ReadCommitDelta(outbound.WithGitRepository(ctx, reader.repo), origin, commit, parent)
}

func (reader repositoryEvidence) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	return reader.reader.IsGitAncestor(outbound.WithGitRepository(ctx, reader.repo), origin, ancestor, descendant)
}

func (worker *Worker) RunLease(ctx context.Context, job domain.GitChangeJob) error {
	if worker.jobs == nil || worker.metadata == nil || worker.reader == nil {
		return fmt.Errorf("durable Git change verification unavailable")
	}
	work, cancel := context.WithTimeout(outbound.WithGitRepository(ctx, job.RepoID), 90*time.Second)
	defer cancel()

	repository, err := worker.scope.ReadRepository(work, job.RepoID)
	if err == nil && repository.GitRemoteURL != job.GitOrigin {
		err = domain.ErrConflict
	}
	var fence outbound.GitReadFence
	if err == nil {
		if worker.authorizer == nil {
			fence = func(context.Context) error { return nil }
		} else {
			fence, err = worker.authorizer.AuthorizeGitRead(work, job.RepoID)
			if err == nil && fence == nil {
				err = domain.ErrIntegrity
			}
		}
	}
	var proof domain.GitReversalEvidence
	if err == nil {
		reader := RepositoryEvidence(job.RepoID, worker.reader)
		if worker.cache != nil {
			reader, err = evidencequery.NewCachedReversalReader(evidencequery.CachedReversalConfig{
				RepoID: job.RepoID, Cache: worker.cache, Provider: reader,
			})
		}
		if err == nil {
			proof, err = evidencequery.VerifyReversal(work, reader, job.GitOrigin, job.Request.Target, job.Request.TargetParent, job.Request.Commit, job.Request.Parent)
		}
	}

	now := time.Now().UTC()
	next := job
	next.UpdatedAt = now
	next.LeaseUntil = time.Time{}
	if err == nil {
		next.State = "completed"
		next.Result = &proof
		next.Reason = proof.Reason
		err = worker.scope.WithinEvidenceWrite(work, job.RepoID, func(tx context.Context) error {
			current, readErr := worker.metadata.GetRepo(tx, job.RepoID)
			if readErr != nil {
				return readErr
			}
			if current.GitRemoteURL != job.GitOrigin {
				return domain.ErrConflict
			}
			if fenceErr := fence(tx); fenceErr != nil {
				return fenceErr
			}
			return worker.jobs.FinishGitChange(tx, next)
		})
		if err == nil {
			return nil
		}
	}

	next.Result = nil
	next.State = "retrying"
	next.Reason = "temporary_provider_or_storage_failure"
	switch {
	case errors.Is(err, domain.ErrIntegrity):
		next.State = "attention"
		next.Reason = "integrity_check_failed"
	case errors.Is(err, domain.ErrValidation):
		next.State = "attention"
		next.Reason = "invalid_or_ambiguous_git_evidence"
	case errors.Is(err, domain.ErrConflict):
		next.State = "attention"
		next.Reason = "origin_or_lease_changed"
	case errors.Is(err, domain.ErrNotFound):
		next.State = "attention"
		next.Reason = "repository_missing"
	}
	if next.State == "retrying" && next.Attempts >= 20 {
		next.State = "attention"
		next.Reason = "retry_limit_reached"
	}
	next.NextAttempt = now.Add(time.Second << min(next.Attempts, 8))
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	finishErr := worker.scope.WithinEvidenceWrite(finish, job.RepoID, func(tx context.Context) error {
		return worker.jobs.FinishGitChange(tx, next)
	})
	return errors.Join(err, finishErr)
}

func (worker *Worker) Process(ctx context.Context, limit int) error {
	ctx = worker.scope.WorkerContext(inbound.WithSystemActor(ctx))
	if limit <= 0 {
		return nil
	}
	for processed := 0; processed < limit; processed++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if worker.jobs == nil {
			return fmt.Errorf("durable Git change verification unavailable")
		}
		err := worker.scope.WithinWork(ctx, func(bound context.Context) error {
			job, err := worker.jobs.ClaimGitChange(bound, "", "", time.Now().UTC(), 2*time.Minute)
			if err != nil {
				return err
			}
			if err := worker.RunLease(bound, job); err != nil {
				log.Printf("Git change verification %s attempt %d deferred", job.ID, job.Attempts)
			}
			return nil
		})
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (worker *Worker) Run(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		if err := worker.Process(ctx, 4); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Git change worker deferred")
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
