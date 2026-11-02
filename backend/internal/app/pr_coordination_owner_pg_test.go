//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func prCoordinationOwnerPG(test *testing.T, service *Service) *PRPromotionCommands {
	test.Helper()
	metadata, blobs, repositories, engine := service.meta, service.blobs, service.repositories, service.engine
	supported := service.DocumentIdentitiesSupported()
	public, err := service.PRPromotionCommands()
	if err != nil {
		test.Fatal(err)
	}
	commands, ok := public.(*PRPromotionCommands)
	if !ok || commands == nil {
		test.Fatalf("public factory returned %T, not the actual coordination owner", public)
	}
	scope, scoped := commands.scope.(*prCoordinationScope)
	publisher, submits := commands.submission.(*prPublicationCommands)
	executor, executes := commands.executor.(*PRPromotionWorker)
	if !scoped || scope == nil || scope.work == nil || !submits || publisher == nil || !executes || executor == nil || executor.refs == nil || executor.history == nil {
		test.Fatal("public owner omitted the actual scope, publisher or executor")
	}
	if publisher.work != scope.work || executor.work != scope.work || scope.work.meta != metadata || scope.work.repositories != repositories || executor.meta != metadata || executor.blobs != blobs || executor.engine != engine || executor.repositories != repositories || commands.profile != PRCoordinationComplete || commands.configurationError != nil || !reflect.DeepEqual(commands.supported, supported) || !reflect.DeepEqual(scope.work.supported, supported) {
		test.Fatal("coordination reconstructed or split original repository work and dependencies")
	}
	refScope, refScoped := executor.refs.scope.(*repositoryRefScope)
	historyScope, historyScoped := executor.history.scope.(*historyCommandScope)
	if !refScoped || refScope == nil || !historyScoped || historyScope == nil || refScope.work != scope.work || historyScope.work != scope.work {
		test.Fatal("actual executor split its nested reference or history work")
	}
	service.meta, service.blobs, service.repositories, service.engine, service.auth = nil, nil, nil, nil, nil
	return commands
}

func TestPGPRCoordinationAcceptanceSurvivesBulkStarvationAndCancellation(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, false)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	commands := prCoordinationOwnerPG(test, fixture.service)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	revision, err := fixture.storage.RepositoryRevision(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	for index, canceled := range []bool{true, false} {
		request := fixture.job.PR
		request.Number += index + 1
		caller, abort := context.WithCancel(ctx)
		defer abort()
		finished := make(chan error, 1)
		var output inbound.UpdateRefOutput
		go func() {
			var deliveryError error
			output, deliveryError = commands.DeliverPRPromotion(caller, fixture.repo, request)
			finished <- deliveryError
		}()
		waitResourceQueue(test, processResources, ResourceBulk, 1)
		identifier := domain.PRPromotionID(fixture.repo, request.Number)
		var state string
		if err := fixture.observer.QueryRow(ctx, `SELECT state FROM pr_promotion_jobs WHERE repo_id=$1 AND id=$2`, fixture.repo, identifier).Scan(&state); err != nil || state != "waiting" {
			test.Fatal("Bulk wait preceded externally visible durable acceptance", state, err)
		}
		accepted, err := fixture.storage.GetPRJob(ctx, fixture.repo, identifier)
		if err != nil || accepted.PR != request || accepted.BaseBranchID != fixture.job.BaseBranchID || accepted.Attempts != 0 || !accepted.LeaseUntil.IsZero() {
			test.Fatal("starved delivery claimed or lost accepted identity", accepted, err)
		}
		want := domain.ErrResourceOverloaded
		if canceled {
			abort()
			want = context.Canceled
		}
		if err := authorityScopeDone(test, ctx, finished); !errors.Is(err, want) || !reflect.DeepEqual(output, inbound.UpdateRefOutput{}) {
			test.Fatal("failed Complete delivery exposed apparent success", output, err)
		}
		if current, err := fixture.storage.GetPRJob(ctx, fixture.repo, identifier); err != nil || !reflect.DeepEqual(current, accepted) {
			test.Fatal("failed caller changed committed acceptance", current, err)
		}
	}
	if current, err := fixture.storage.RepositoryRevision(ctx, fixture.repo); err != nil || current != revision {
		test.Fatal("acceptance advanced graph revision", current, revision, err)
	}
	release()
	assertResourcesReleased(test, processResources)
}

func TestPGPRCoordinationSubmitRetryRollbackAndCurrentAuthority(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, false)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	claimed, err := fixture.storage.ClaimPRJob(ctx, fixture.repo, fixture.job.ID, time.Now().UTC(), prJobLease)
	if err != nil {
		test.Fatal(err)
	}
	claimed.State, claimed.Reason, claimed.LeaseUntil = "attention", "integrity_check_failed", time.Time{}
	if err := fixture.storage.FinishPRJob(ctx, claimed); err != nil {
		test.Fatal(err)
	}
	before, err := fixture.storage.GetPRJob(ctx, fixture.repo, claimed.ID)
	if err != nil {
		test.Fatal(err)
	}
	probe := &documentPublicationProbePG{PostgresStore: fixture.storage, calls: make(map[string]int), failAt: "audit", failure: domain.ErrIntegrity}
	fixture.service.meta, fixture.service.blobs, fixture.service.repositories = probe, probe, probe
	commands := prCoordinationOwnerPG(test, fixture.service)
	owner := inbound.WithRepositoryActor(ctx, fixture.repository.OwnerID)
	image, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	request := fixture.job.PR
	request.Number++
	if output, err := commands.SubmitPRPromotion(owner, fixture.repo, request); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(output, domain.PRPromotionJob{}) {
		test.Fatal("failed acceptance audit exposed uncommitted success", output, err)
	}
	if _, err := fixture.storage.GetPRJob(ctx, fixture.repo, domain.PRPromotionID(fixture.repo, request.Number)); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("failed acceptance persisted a job", err)
	}
	if err := commands.RetryPRPromotion(owner, fixture.repo, claimed.ID); !errors.Is(err, domain.ErrIntegrity) || probe.calls["audit"] != 2 {
		test.Fatal("retry missed its original audit rollback boundary", probe.calls, err)
	}
	if current, err := fixture.storage.GetPRJob(ctx, fixture.repo, claimed.ID); err != nil || !reflect.DeepEqual(current, before) {
		test.Fatal("failed retry changed diagnostics or version", current, err)
	}
	if current, err := fixture.publication(ctx, fixture.storage); err != nil || !reflect.DeepEqual(current, image) {
		test.Fatal("failed submit or retry escaped its transaction", current, err)
	}
	probe.failAt = ""
	if err := commands.RetryPRPromotion(owner, fixture.repo, claimed.ID); err != nil {
		test.Fatal(err)
	}
	retried, err := fixture.storage.GetPRJob(ctx, fixture.repo, claimed.ID)
	if err != nil || retried.State != "waiting" || retried.Version != before.Version+1 || retried.Attempts != 0 || retried.Reason != "" || !retried.LeaseUntil.IsZero() {
		test.Fatal("actual retry did not reset durable delivery state", retried, err)
	}
	if accepted, err := commands.SubmitPRPromotion(owner, fixture.repo, request); err != nil || accepted.PR != request || accepted.BaseBranchID != fixture.job.BaseBranchID || accepted.State != "waiting" {
		test.Fatal("detached publisher could not durably accept after audit recovery", accepted, err)
	}
	committed, err := fixture.publication(ctx, fixture.storage)
	if err != nil || repositoryRefAuditCountPG(committed.audit, "context.promotion.submitted") != repositoryRefAuditCountPG(image.audit, "context.promotion.submitted")+1 || repositoryRefAuditCountPG(committed.audit, "context.updated") != repositoryRefAuditCountPG(image.audit, "context.updated")+1 {
		test.Fatal("submit or retry lost its original single audit", committed.audit, err)
	}
	if revision, err := fixture.storage.RepositoryRevision(ctx, fixture.repo); err != nil || revision != image.revision {
		test.Fatal("submit or retry advanced graph revision", revision, err)
	}
	if _, err := fixture.observer.Exec(ctx, `UPDATE repositories SET archived=true WHERE id=$1`, fixture.repository.ID); err != nil {
		test.Fatal(err)
	}
	if err := commands.RetryPRPromotion(owner, fixture.repo, claimed.ID); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("detached retry retained pre-archive authority", err)
	}
	if output, err := commands.SubmitPRPromotion(owner, fixture.repo, request); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(output, domain.PRPromotionJob{}) {
		test.Fatal("detached submit retained pre-archive authority", output, err)
	}
	for _, promote := range []func(context.Context, domain.ContentHash, domain.PullRequestMerge) (inbound.UpdateRefOutput, error){commands.DeliverPRPromotion, commands.PromoteRepositoryPR} {
		if output, err := promote(owner, fixture.repo, fixture.job.PR); !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(output, inbound.UpdateRefOutput{}) {
			test.Fatal("detached promotion retained pre-archive authority", output, err)
		}
	}
	if current, err := fixture.storage.GetPRJob(ctx, fixture.repo, claimed.ID); err != nil || !reflect.DeepEqual(current, retried) {
		test.Fatal("denied retry changed delivery state", current, err)
	}
}

func TestPGPRCoordinationActualExecutorRollbackRetryAndCompletedRewindReplay(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, true)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	probe := &prPromotionPGTransactionStore{PostgresStore: fixture.storage, failure: "finish"}
	fixture.service.meta, fixture.service.blobs, fixture.service.repositories = probe, probe, probe
	fixture.service.requireCompleteMemory = true
	commands := prCoordinationOwnerPG(test, fixture.service)
	before, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	if output, err := commands.DeliverPRPromotion(ctx, fixture.repo, fixture.job.PR); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(output, inbound.UpdateRefOutput{}) || probe.injected != "finish" {
		test.Fatal("public delivery bypassed actual completion failure", output, probe.injected, err)
	}
	if after, err := fixture.publication(ctx, fixture.storage); err != nil || !reflect.DeepEqual(after, before) {
		test.Fatal("failed executor published refs, history, memory, audit or outbox", after, err)
	}
	if failed, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID); err != nil || failed.State != "attention" || failed.Reason != "integrity_check_failed" || !failed.LeaseUntil.IsZero() {
		test.Fatal("failed executor lost its durable retry state", failed, err)
	}
	probe.failure = ""
	if err := commands.RetryPRPromotion(ctx, fixture.repo, fixture.job.ID); err != nil {
		test.Fatal(err)
	}
	if output, err := commands.DeliverPRPromotion(ctx, fixture.repo, fixture.job.PR); err != nil || output.Ref.Target != fixture.tip {
		test.Fatal("detached owner could not recover through the original executor", output, err)
	}
	completed, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID)
	if err != nil || completed.State != "completed" || completed.Attempts != 1 || completed.BaseBranchID != fixture.job.BaseBranchID {
		test.Fatal("actual recovery lost accepted base identity", completed, err)
	}
	after, err := fixture.publication(ctx, fixture.storage)
	if err != nil || after.revision.Graph != before.revision.Graph+1 || len(after.notifications) != len(before.notifications)+1 {
		test.Fatal("actual recovery split its graph revision or outbox", after, err)
	}
	bindings, completions := prPromotionPGReceipts(after.history, fixture.job.PR)
	if bindings != 1 || completions != 1 {
		test.Fatal("recovery omitted or duplicated immutable receipts", bindings, completions)
	}
	executor := commands.executor.(*PRPromotionWorker)
	if output, err := executor.refs.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: fixture.repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", BranchID: completed.BaseBranchID, Target: fixture.base}, ExpectedTarget: fixture.tip, Force: true}); err != nil || output.Result != inbound.RefForced {
		test.Fatal("actual reference owner could not rewind later main", output, err)
	}
	rewound, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	for range 2 {
		for _, replay := range []func(context.Context, domain.ContentHash, domain.PullRequestMerge) (inbound.UpdateRefOutput, error){commands.DeliverPRPromotion, commands.PromoteRepositoryPR} {
			if output, err := replay(ctx, fixture.repo, completed.PR); err != nil || output.Result != inbound.RefUpToDate || output.Ref.Target != fixture.base || output.Ref.BranchID != completed.BaseBranchID {
				test.Fatal("completed replay undid a later main rewind", output, err)
			}
		}
	}
	replayed, err := fixture.publication(ctx, fixture.storage)
	if err != nil || replayed.revision.Graph < rewound.revision.Graph {
		test.Fatal("completed replay regressed repository state", replayed, err)
	}
	replayed.revision = rewound.revision
	if !reflect.DeepEqual(replayed, rewound) {
		test.Fatal("completed replay changed immutable receipts, ancestry, memory or outbox")
	}
	if current, err := fixture.storage.GetPRJob(ctx, fixture.repo, completed.ID); err != nil || !reflect.DeepEqual(current, completed) {
		test.Fatal("completed replay reclaimed terminal work", current, err)
	}
	assertResourcesReleased(test, processResources)
}

func TestPGPRCoordinationListingUsesCurrentEvidenceInBorrowedReadOnlySnapshot(test *testing.T) {
	fixture := newPRPromotionPGFixture(test, true, false)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), fixture.storage), 20*time.Second)
	defer cancel()
	commands := prCoordinationOwnerPG(test, fixture.service)
	now := time.Now().UTC()
	metadata, err := fixture.storage.GetRepo(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	checkpoint := domain.PRHistoryCheckpoint{RepoID: fixture.repo, GitOrigin: metadata.GitRemoteURL, InstallationID: 62, ProviderRepositoryID: 71, PR: fixture.job.PR, Version: 1, PullRequestID: 71, MergedAt: now, NextAttempt: now}
	if err := fixture.storage.SavePRHistoryCheckpoint(ctx, checkpoint, 0); err != nil {
		test.Fatal(err)
	}
	before, err := fixture.publication(ctx, fixture.storage)
	if err != nil {
		test.Fatal(err)
	}
	for index, expected := range []string{"recorded_partial", "recorded_complete", "unavailable"} {
		if index == 1 {
			checkpoint.Version, checkpoint.Complete, checkpoint.HistoryCheckedAt = 2, true, now
			if err := fixture.storage.SavePRHistoryCheckpoint(ctx, checkpoint, 1); err != nil {
				test.Fatal(err)
			}
		}
		if index == 2 {
			if _, err := fixture.observer.Exec(ctx, `UPDATE pr_history_checkpoints SET payload=jsonb_set(payload,'{version}','0'::jsonb) WHERE repo_id=$1`, fixture.repo); err != nil {
				test.Fatal(err)
			}
		}
		err := fixture.storage.WithinReadSnapshot(ctx, func(read context.Context) error {
			jobs, err := commands.ListPRPromotions(read, fixture.repo)
			if err != nil || len(jobs) != 1 || jobs[0].State != "waiting" || jobs[0].Association == nil || !fixture.storage.InReadOnlyTransaction(read) {
				return errors.Join(domain.ErrIntegrity, err)
			}
			association := jobs[0].Association
			if association.ProviderHistory != expected || association.CurrentApplicability != "not_evaluated" {
				return domain.ErrIntegrity
			}
			if expected == "unavailable" {
				if association.ProviderCheckpointCount != nil || association.ParticipationCount != nil || association.PublicationCount != nil || association.ContributionCount != nil || association.Participation != "unavailable" || association.Contribution != "unavailable" {
					return domain.ErrIntegrity
				}
			} else if association.ProviderCheckpointCount == nil || *association.ProviderCheckpointCount != 1 || association.Participation != "not_recorded" || association.Contribution != "not_recorded" {
				return domain.ErrIntegrity
			}
			_, err = fixture.storage.GetRepo(read, fixture.repo)
			return err
		})
		if err != nil {
			test.Fatal("listing cached evidence, fabricated completeness or poisoned borrowed read", expected, err)
		}
	}
	if after, err := fixture.publication(ctx, fixture.storage); err != nil || !reflect.DeepEqual(after, before) {
		test.Fatal("listing wrote publication state", after, err)
	}
	if job, err := fixture.storage.GetPRJob(ctx, fixture.repo, fixture.job.ID); err != nil || !reflect.DeepEqual(job, fixture.job) {
		test.Fatal("listing persisted a projection on the durable job", job, err)
	}
}
