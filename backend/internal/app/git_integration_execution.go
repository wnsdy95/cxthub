package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (workers *GitIntegrationWorkers) run(ctx context.Context, job domain.GitScanJob) error {
	work, cancel := context.WithTimeout(outbound.WithGitRepository(ctx, job.RepoID), 90*time.Second)
	defer cancel()
	planned := domain.GitScanFinish{Job: job}
	planned.Job.LeaseUntil = time.Time{}
	planned.Job.UpdatedAt = time.Now().UTC()
	planned.Job.NextAttempt = planned.Job.UpdatedAt
	planned.Job.State = "waiting"
	repo, err := workers.workerRepository(work, job.RepoID)
	if err == nil && repo.GitRemoteURL != job.GitOrigin {
		err = domain.ErrConflict
	}
	var fence GitReadFence
	if err == nil {
		if workers.sourceAccess != nil && !rootAdapterPresent(workers.sourceAccess) {
			err = domain.ErrValidation
		} else {
			fence, err = authorizeGitRead(work, workers.sourceAccess, job.RepoID)
		}
	}
	if err == nil && !job.TreeIndexed {
		var evidence domain.GitTreeEvidence
		treeReader, supported := workers.reader.(outbound.GitTreeReader)
		if !supported || !rootAdapterPresent(treeReader) {
			err = domain.ErrValidation
		} else {
			evidence, err = treeReader.ReadCommitTree(work, job.GitOrigin, job.Commit)
		}
		if err == nil {
			planned.Tree = &evidence
			planned.Job.TreeIndexed = true
			err = planned.Validate()
		}
	} else if err == nil && !job.Indexed {
		var deltas []domain.GitCommitDelta
		if !rootAdapterPresent(workers.reader) {
			err = domain.ErrValidation
		} else {
			deltas, err = workers.reader.ReadCommitDeltas(work, job.GitOrigin, job.Commit)
		}
		if err == nil {
			err = planGitIndex(&planned, deltas)
		}
		if err == nil {
			err = workers.validateIndexTree(work, job, deltas)
		}
	} else if err == nil {
		var candidates []domain.GitInverseCandidate
		candidates, err = workers.store.FindGitInverses(work, job.RepoID, job.GitOrigin, job.Commit, job.Cursor, 51)
		if err == nil {
			if len(candidates) <= 50 {
				planned.Job.State = "completed"
			} else {
				candidates = candidates[:50]
			}
			for _, candidate := range candidates {
				planned.Job.Cursor = candidate.Cursor
				// Arrival order is unrelated to Git ancestry. Both orientations are
				// verified; at most the ancestor -> descendant orientation can prove true.
				reverse := domain.GitChangeRequest{Target: candidate.Request.Commit, TargetParent: candidate.Request.Parent, Commit: candidate.Request.Target, Parent: candidate.Request.TargetParent}
				for _, current := range []domain.GitChangeRequest{candidate.Request, reverse} {
					now := planned.Job.UpdatedAt
					planned.Changes = append(planned.Changes, domain.GitChangeJob{ID: domain.GitChangeID(job.RepoID, job.GitOrigin, current), RepoID: job.RepoID, GitOrigin: job.GitOrigin, Request: current, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now})
				}
			}
		}
	}
	if err == nil {
		err = repositoryWriteErrorWithWork(evidenceWriteContext(work), workers.work, job.RepoID, func(tx context.Context) error {
			current, operationErr := workers.work.meta.GetRepo(tx, job.RepoID)
			if operationErr != nil {
				return operationErr
			}
			if current.GitRemoteURL != job.GitOrigin {
				return domain.ErrConflict
			}
			if operationErr := fence(tx); operationErr != nil {
				return operationErr
			}
			return workers.store.FinishGitScan(tx, planned)
		})
		if err == nil {
			return nil
		}
	}
	// No candidate/cursor publication on failure. Immutable job identity and
	// version fence prevent a canceled worker from replacing a newer result.
	fail := domain.GitScanFinish{Job: job}
	fail.Job.State = "retrying"
	fail.Job.Reason = "temporary_provider_or_storage_failure"
	fail.Job.UpdatedAt = time.Now().UTC()
	fail.Job.LeaseUntil = time.Time{}
	fail.Job.NextAttempt = fail.Job.UpdatedAt.Add(time.Second << min(job.Attempts, 9))
	switch {
	case errors.Is(err, domain.ErrIntegrity):
		fail.Job.State = "attention"
		fail.Job.Reason = "integrity_check_failed"
	case errors.Is(err, domain.ErrValidation):
		fail.Job.State = "attention"
		fail.Job.Reason = "invalid_or_ambiguous_git_evidence"
	case errors.Is(err, domain.ErrConflict):
		fail.Job.State = "attention"
		fail.Job.Reason = "origin_or_lease_changed"
	}
	finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	operationErr := repositoryWriteErrorWithWork(evidenceWriteContext(finish), workers.work, job.RepoID, func(tx context.Context) error { return workers.store.FinishGitScan(tx, fail) })
	return errors.Join(err, operationErr)
}

func (workers *GitIntegrationWorkers) Process(ctx context.Context, limit int) error {
	ctx = workers.workerDocumentIdentityContext(inbound.WithSystemActor(ctx))
	for count := 0; count < limit; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := admittedJob(ctx, func(bound context.Context) (domain.GitScanJob, error) {
			if !rootAdapterPresent(workers.store) {
				return domain.GitScanJob{}, domain.ErrValidation
			}
			return workers.store.ClaimGitScan(bound, "", time.Now().UTC(), 2*time.Minute)
		}, func(bound context.Context, job domain.GitScanJob) error {
			if err := workers.run(bound, job); err != nil {
				log.Printf("Git observation %s attempt %d deferred", job.ID, job.Attempts)
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

func (workers *GitIntegrationWorkers) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := workers.Process(ctx, 2); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Git observation worker: %v", err)
		}
		if err := workers.ProcessContributions(ctx, 2); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Git contribution worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Reconcile observes one durable page per repository on each pass. Failed or
// canceled reads retain their page; other repositories continue independently.
func (workers *GitIntegrationWorkers) Reconcile(ctx context.Context) error {
	_, err := workers.reconcile(ctx)
	return err
}

func (workers *GitIntegrationWorkers) reconcile(ctx context.Context) (bool, error) {
	ctx = workers.workerDocumentIdentityContext(inbound.WithSystemActor(ctx))
	store, ok := workers.work.meta.(outbound.GitHeadScanStore)
	if !ok || !rootAdapterPresent(store) {
		return false, domain.ErrValidation
	}
	repos, err := workers.work.meta.ListRepos(ctx, "default")
	if err != nil {
		return false, err
	}
	progress := false
	var claimErrors []error
	for _, repo := range repos {
		if err = ctx.Err(); err != nil {
			return progress, errors.Join(append(claimErrors, err)...)
		}
		if repo.GitRemoteURL == "" {
			continue
		}
		admissionErr := resourceWork(ctx, ResourceBulk, func(ctx context.Context) error {
			advanced, discoveryErr := workers.discoverGitAssociations(ctx, repo)
			progress = progress || advanced
			if discoveryErr != nil && !errors.Is(discoveryErr, domain.ErrForbidden) && !errors.Is(discoveryErr, domain.ErrConflict) && !errors.Is(discoveryErr, domain.ErrDocumentIdentityUpgradeRequired) {
				log.Printf("Git association discovery %s deferred", repo.ID)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			job, operationErr := store.ClaimGitHeadScan(ctx, repo.ID, repo.GitRemoteURL, time.Now().UTC(), time.Minute)
			if errors.Is(operationErr, domain.ErrNotFound) || errors.Is(operationErr, domain.ErrDocumentIdentityUpgradeRequired) {
				return nil
			}
			if operationErr != nil {
				return fmt.Errorf("Git head reconciliation %s claim: %w", repo.ID, operationErr)
			}
			work, cancel := context.WithTimeout(outbound.WithGitRepository(ctx, job.RepoID), 30*time.Second)
			_, operationErr = workers.workerRepository(work, job.RepoID)
			var fence GitReadFence
			var identity *domain.GitProviderObservation
			if operationErr == nil {
				if workers.sourceAccess != nil && !rootAdapterPresent(workers.sourceAccess) {
					operationErr = domain.ErrValidation
				} else {
					identity, fence, operationErr = authorizeGitObservation(work, workers.sourceAccess, job.RepoID)
				}
			}
			var frozenAdmissions *frozenGitAdmissionBatch
			if operationErr == nil {
				var admissionErr error
				frozenAdmissions, admissionErr = workers.freezeGitAdmissionBatch(work, job, identity, fence)
				if admissionErr != nil {
					log.Printf("Git admission snapshot %s deferred", job.RepoID)
				}
			}
			var heads []outbound.GitHead
			var frozenContext *gitAdmissionSnapshot
			if operationErr == nil {
				var contextErr error
				frozenContext, contextErr = workers.freezeGitHeadContext(work, job, identity, frozenAdmissions)
				if contextErr != nil {
					log.Printf("Git context head snapshot %s deferred", job.RepoID)
				}
			}
			var more bool
			var sourceHeads []domain.GitRefObservation
			if operationErr == nil {
				sourceHeads, operationErr = workers.readGitAdmissionSources(work, frozenAdmissions)
			}
			if operationErr == nil {
				if !rootAdapterPresent(workers.reader) {
					operationErr = domain.ErrValidation
				} else {
					heads, more, operationErr = workers.reader.ListGitHeads(work, job.GitOrigin, job.Page)
				}
			}
			if operationErr == nil {
				observations := []domain.GitRefObservation{}
				for _, head := range heads {
					observations = append(observations, domain.GitRefObservation{RepoID: job.RepoID, GitOrigin: job.GitOrigin, Source: "reconciliation", Ref: head.Ref, After: head.Commit, Provider: identity}.WithID())
				}
				var preparedAdmissions *preparedGitAdmissionBatch
				var boundHeads []domain.GitContextHead
				receipt, receiptErr := domain.NewGitHeadReceiptWithSources(job, observations, sourceHeads, more)
				if receiptErr == nil {
					var contextErr error
					boundHeads, contextErr = prepareGitContextHeads(frozenContext, receipt)
					if contextErr != nil {
						log.Printf("Git context head preparation %s deferred", job.RepoID)
					}
				}
				if frozenAdmissions != nil {
					admissionErr := receiptErr
					if admissionErr == nil {
						preparedAdmissions, admissionErr = workers.prepareGitAdmissionBatch(work, frozenAdmissions, receipt)
					}
					if admissionErr != nil {
						log.Printf("Git admission preparation %s deferred", job.RepoID)
					}
				}
				operationErr = repositoryWriteErrorWithWork(evidenceWriteContext(work), workers.work, job.RepoID, func(tx context.Context) error {
					current, operationErr := workers.work.meta.GetRepo(tx, job.RepoID)
					if operationErr != nil {
						return operationErr
					}
					if current.GitRemoteURL != job.GitOrigin {
						return domain.ErrConflict
					}
					if operationErr := fence(tx); operationErr != nil {
						return operationErr
					}
					var validatedSnapshot *gitAdmissionSnapshot
					if len(boundHeads) > 0 || (preparedAdmissions != nil && len(preparedAdmissions.admissions) > 0) {
						var err error
						basis := frozenContext
						if preparedAdmissions != nil && preparedAdmissions.frozen.snapshot != nil {
							basis = preparedAdmissions.frozen.snapshot
						}
						validatedSnapshot, err = workers.reuseGitAdmissionSnapshot(tx, job.RepoID, basis)
						if errors.Is(err, domain.ErrConflict) {
							boundHeads = nil
							if preparedAdmissions != nil {
								preparedAdmissions.admissions = nil
							}
							err = nil
						}
						if err != nil {
							return err
						}
					}
					var finishErr error
					if len(sourceHeads) > 0 {
						sourceStore, supported := workers.work.meta.(outbound.GitHeadSourceScanStore)
						if !supported || !rootAdapterPresent(sourceStore) {
							return domain.ErrValidation
						}
						finishErr = sourceStore.FinishGitHeadScanWithSources(tx, job, observations, sourceHeads, more, time.Now().UTC())
					} else {
						finishErr = store.FinishGitHeadScan(tx, job, observations, more, time.Now().UTC())
					}
					if finishErr != nil {
						return finishErr
					}
					if err := workers.persistGitContextHeads(tx, receipt, boundHeads, validatedSnapshot); err != nil {
						return err
					}
					return workers.persistGitAdmissionBatch(tx, preparedAdmissions, validatedSnapshot)
				})
			}
			cancel()
			if operationErr != nil {
				finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_ = repositoryWriteErrorWithWork(evidenceWriteContext(finish), workers.work, job.RepoID, func(tx context.Context) error { return store.FailGitHeadScan(tx, job, time.Now().UTC()) })
				done()
				log.Printf("Git head reconciliation %s page %d deferred", job.RepoID, job.Page)
			}
			return nil
		})
		if admissionErr != nil {
			claimErrors = append(claimErrors, admissionErr)
			if errors.Is(admissionErr, domain.ErrResourceOverloaded) || ctx.Err() != nil {
				return progress, errors.Join(append(claimErrors, ctx.Err())...)
			}
		}
	}
	return progress, errors.Join(append(claimErrors, ctx.Err())...)
}

func (workers *GitIntegrationWorkers) RunReconciler(ctx context.Context) {
	for {
		progress, err := workers.reconcile(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Git reconciliation: %v", err)
		}
		delay := time.Minute
		if progress {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Index and tree come from separate bounded reads. Verify their common
// immutable identity before marking the index complete.
func (workers *GitIntegrationWorkers) validateIndexTree(ctx context.Context, job domain.GitScanJob, deltas []domain.GitCommitDelta) error {
	store, supported := workers.work.meta.(outbound.GitTreeStore)
	if !supported || !rootAdapterPresent(store) {
		return domain.ErrValidation
	}
	commit, err := store.GetGitCommitTree(ctx, job.RepoID, job.GitOrigin, job.Commit)
	if err != nil {
		return err
	}
	cache := map[string]domain.GitTreeNode{}
	for _, delta := range deltas {
		if !slices.Equal(commit.Parents, delta.Parents) {
			return domain.ErrIntegrity
		}
		for _, change := range delta.Changes {
			entry, err := cachedGitEntry(ctx, store, job.RepoID, job.GitOrigin, commit.Tree, change.Path, cache)
			if err != nil {
				return err
			}
			if entry != change.After {
				return domain.ErrIntegrity
			}
		}
	}
	return nil
}
