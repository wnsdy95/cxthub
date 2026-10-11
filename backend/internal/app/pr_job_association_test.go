package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPRJobAssociationPublicListingIsReadOnly(t *testing.T) {
	worker, storage, remote, repo, ordinary, _ := contributionFixture(t)
	ctx := systemTestContext()
	jobs, err := worker.core.ListPRPromotions(ctx, repo.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	status := jobs[0].Association
	if status == nil || status.ProviderHistory != "unknown" || status.Participation != "not_recorded" || status.Contribution != "not_recorded" || *status.ContributionCount != 0 {
		t.Fatalf("absence is not active scanning: %+v", status)
	}
	publishPRSource(t, worker.core, ordinary)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := storage.GetPRJob(ctx, repo.ID, jobs[0].ID)
	if err != nil || stored.Association != nil {
		t.Fatal("worker persisted a query projection", stored, err)
	}
	proofs, err := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if err != nil || len(proofs) == 0 {
		t.Fatal(proofs, err)
	}
	duplicateWitness := proofs[0]
	duplicateWitness.ObservationID = strings.Repeat("f", 64)
	if err := storage.PutPRParticipation(ctx, duplicateWitness.WithID()); err != nil {
		t.Fatal(err)
	}
	beforeRef, _ := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	beforeHistory, _ := worker.core.ListHistory(ctx, repo.ID)
	jobs, err = worker.core.ListPRPromotions(ctx, repo.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	status = jobs[0].Association
	if status.ProviderHistory != "recorded_complete" || *status.ProviderCheckpointCount != 1 || *status.ParticipationCount != len(proofs)+1 || *status.PublicationCount != 1 || *status.ContributionCount != 1 || status.CurrentApplicability != "not_evaluated" {
		t.Fatalf("incorrect recorded counts: %+v", status)
	}
	afterJob, _ := storage.GetPRJob(ctx, repo.ID, jobs[0].ID)
	afterRef, _ := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	afterHistory, _ := worker.core.ListHistory(ctx, repo.ID)
	if !reflect.DeepEqual(stored, afterJob) || beforeRef != afterRef || !reflect.DeepEqual(beforeHistory, afterHistory) {
		t.Fatal("listing mutated durable job, ref or publication history")
	}
	conflict := ordinary
	conflict.ID, conflict.BranchID, conflict.Kind = strings.Repeat("9", 32), "late-ambiguous", "position"
	if err := worker.core.RecordHistory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	jobs, err = worker.core.ListPRPromotions(ctx, repo.ID)
	if err != nil || !reflect.DeepEqual(status, jobs[0].Association) {
		t.Fatal("historical counts must not pretend to assess current ambiguity", jobs, err)
	}
}

type prAssociationBatchStub struct {
	outbound.MetadataStore
	outbound.PRJobStore
	jobs     []domain.PRPromotionJob
	evidence outbound.PRJobAssociationEvidence
	err      error
	calls    int
	numbers  []int
}

func (stub *prAssociationBatchStub) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, domain.ErrNotFound
}

func (stub *prAssociationBatchStub) ListPRJobs(context.Context, domain.ContentHash) ([]domain.PRPromotionJob, error) {
	return stub.jobs, nil
}

func (stub *prAssociationBatchStub) ReadPRJobAssociationEvidence(_ context.Context, _ domain.ContentHash, numbers []int) (outbound.PRJobAssociationEvidence, error) {
	stub.calls++
	stub.numbers = append([]int(nil), numbers...)
	return stub.evidence, stub.err
}

func TestPRJobAssociationBatchBoundsUnknownAndUnavailable(t *testing.T) {
	repo := hh("association-batch")
	stub := &prAssociationBatchStub{}
	for index := range 100 {
		stub.jobs = append(stub.jobs, domain.PRPromotionJob{RepoID: repo, PR: domain.PullRequestMerge{Number: index + 1}, State: "completed"})
	}
	service := NewService(stub, nil, nil, nil, nil)
	jobs, err := service.ListPRPromotions(systemTestContext(), repo)
	if err != nil || len(jobs) != 100 || stub.calls != 1 || len(stub.numbers) != 100 {
		t.Fatal("listing did not use one batch", len(jobs), stub.calls, err)
	}
	for index, job := range jobs {
		status := job.Association
		if status.ProviderHistory != "unknown" || status.Participation != "unknown" || status.Contribution != "unknown" || status.ContributionCount != nil || status.ParticipationCount != nil || status.ProviderCheckpointCount != nil || stub.jobs[index].Association != nil {
			t.Fatal("incomplete batch became a total or mutated input", status)
		}
	}
	stub.err = errors.New("provider-secret-token/raw-private-payload")
	jobs, err = service.ListPRPromotions(systemTestContext(), repo)
	if err != nil || jobs[0].Association.Contribution != "unavailable" {
		t.Fatal(jobs, err)
	}
	raw, _ := json.Marshal(jobs)
	if strings.Contains(string(raw), "provider-secret") || strings.Contains(string(raw), "raw-private") {
		t.Fatal("raw upstream error leaked")
	}
	withoutCapability := struct {
		outbound.MetadataStore
		outbound.PRJobStore
	}{stub, stub}
	service = NewService(withoutCapability, nil, nil, nil, nil)
	jobs, err = service.ListPRPromotions(systemTestContext(), repo)
	if err != nil || jobs[0].Association.ProviderHistory != "unavailable" || jobs[0].Association.ContributionCount != nil {
		t.Fatal("absent adapter fabricated state", jobs, err)
	}
	canceled, cancel := context.WithCancel(systemTestContext())
	cancel()
	if _, err := service.ListPRPromotions(canceled, repo); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation swallowed", err)
	}
}

func TestPRJobAssociationExactScopeAndCheckpointWithoutProofs(t *testing.T) {
	repo := hh("association-scope")
	job := domain.PRPromotionJob{RepoID: repo, GitOrigin: "github.com/acme/repo", BaseBranchID: "main-id", PR: domain.PullRequestMerge{Number: 7, MergeSHA: strings.Repeat("a", 40)}}
	matching := domain.PRContribution{ID: "matching", RepoID: repo, GitOrigin: "https://github.com/acme/repo", PR: job.PR, DestinationBranchID: job.BaseBranchID}
	otherRepo, otherOrigin, otherMerge, otherHead, otherDestination := matching, matching, matching, matching, matching
	otherRepo.RepoID = hh("other-repo")
	otherOrigin.GitOrigin = "https://github.com/other/repo"
	otherMerge.PR.MergeSHA = strings.Repeat("b", 40)
	otherHead.PR.HeadSHA = strings.Repeat("c", 40)
	otherDestination.DestinationBranchID = "reused-main"
	checkpoint := domain.PRHistoryCheckpoint{RepoID: repo, GitOrigin: matching.GitOrigin, PR: job.PR, Complete: true}
	stub := &prAssociationBatchStub{jobs: []domain.PRPromotionJob{job}, evidence: outbound.PRJobAssociationEvidence{
		Checkpoints: []domain.PRHistoryCheckpoint{checkpoint}, CheckpointsComplete: true,
		ParticipationsComplete: true, ContributionsComplete: true,
		Contributions: []domain.PRContribution{matching, matching, otherRepo, otherOrigin, otherMerge, otherHead, otherDestination},
	}}
	service := NewService(stub, nil, nil, nil, nil)
	jobs, err := service.ListPRPromotions(systemTestContext(), repo)
	if err != nil {
		t.Fatal(err)
	}
	status := jobs[0].Association
	if status.ProviderHistory != "recorded_complete" || *status.ProviderCheckpointCount != 1 || *status.ContributionCount != 1 || *status.ParticipationCount != 0 || status.CurrentApplicability != "not_evaluated" {
		t.Fatal("scope or empty participation was inferred incorrectly", status)
	}
	stub.evidence.Checkpoints[0].Complete = false
	jobs, err = service.ListPRPromotions(systemTestContext(), repo)
	if err != nil || jobs[0].Association.ProviderHistory != "recorded_partial" {
		t.Fatal("partial checkpoint became active or complete", jobs, err)
	}
	stub.evidence.Checkpoints = nil
	jobs, err = service.ListPRPromotions(systemTestContext(), repo)
	if err != nil || jobs[0].Association.ProviderHistory != "unknown" || *jobs[0].Association.ProviderCheckpointCount != 0 {
		t.Fatal("no checkpoint inferred scanner activity", jobs, err)
	}
	stub.jobs[0].BaseBranchID = ""
	jobs, err = service.ListPRPromotions(systemTestContext(), repo)
	if err != nil || jobs[0].Association.Contribution != "unknown" || jobs[0].Association.ContributionCount != nil {
		t.Fatal("missing destination identity became a zero count", jobs, err)
	}
}
