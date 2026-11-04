package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGraphQualificationUsesCapturedReferenceWhileSourceAdvances(test *testing.T) {
	for _, mismatch := range []bool{false, true} {
		test.Run(map[bool]string{false: "match", true: "mismatch"}[mismatch], func(test *testing.T) {
			service, storage, _ := newGraphQualificationTest(test)
			storage.lease.ReferenceExpected = storage.qualificationDigest
			if mismatch {
				storage.lease.ReferenceExpected = hh("independent reference differs")
			}
			storage.source.Sequence++
			storage.graphProjectionReadStore.result.Source, storage.graphProjectionReadStore.result.Ready = storage.source, false
			storage.budgetErr = domain.ErrRepositoryViewSourceLimit
			ctx, resources := admissionTestContext(time.Second)
			if err := service.ProcessGraphQualifications(ctx, 1); err != nil || storage.finishes != 1 || storage.result.Matched() == mismatch || storage.failures != 0 {
				test.Fatal("reference-backed qualification did not finish", err, storage.result)
			}
			if storage.result.Expected != storage.lease.ReferenceExpected || storage.budgetChecks != 0 || storage.snapshotLists != 0 || storage.repoReads != 1 {
				test.Fatal("comparison used current inputs instead of captured reference", storage.result, storage.budgetChecks, storage.snapshotLists, storage.repoReads)
			}
			assertResourcesReleased(test, resources)
		})
	}
}

func TestGraphQualificationReferenceDoesNotBypassCandidateOrSourceIdentity(test *testing.T) {
	for _, failure := range []string{"future", "reset", "foreign", "incomplete", "replacement", "invalid-reference", "disabled", "needs-attention", "different-read"} {
		test.Run(failure, func(test *testing.T) {
			service, storage, _ := newGraphQualificationTest(test)
			storage.lease.ReferenceExpected = storage.qualificationDigest
			switch failure {
			case "future":
				storage.source.Sequence--
			case "reset":
				storage.source.Epoch = "33333333-3333-4333-8333-333333333333"
			case "foreign":
				storage.source.RepoID = hh("other repository")
			case "incomplete":
				storage.graphProjectionReadStore.result.Complete = false
				storage.graphProjectionReadStore.result.Ready = false
			case "replacement":
				storage.graphProjectionReadStore.result.Dataset = "33333333-3333-4333-8333-333333333333"
			case "invalid-reference":
				storage.lease.ReferenceExpected = "not-a-hash"
			case "disabled":
				storage.graphProjectionReadStore.result.Enabled = false
				storage.graphProjectionReadStore.result.Ready = false
			case "needs-attention":
				storage.graphProjectionReadStore.result.NeedsAttention = true
				storage.graphProjectionReadStore.result.Ready = false
			case "different-read":
				storage.graphProjectionReadStore.result.Source.Sequence++
				storage.graphProjectionReadStore.result.Ready = false
			}
			if err := service.ProcessGraphQualifications(systemTestContext(), 1); err == nil || storage.finishes != 0 || storage.snapshotLists != 0 || storage.budgetChecks != 0 {
				test.Fatal("retained reference bypassed identity or completion", err)
			}
		})
	}
}

func TestGraphQualificationReferenceStillValidatesCandidatePayload(test *testing.T) {
	service, storage, _ := newGraphQualificationTest(test)
	storage.lease.ReferenceExpected = storage.qualificationDigest
	storage.source.Sequence++
	storage.graphProjectionReadStore.result.Source = storage.source
	storage.graphProjectionReadStore.result.Ready = false
	storage.graphProjectionReadStore.result.Parts = storage.graphProjectionReadStore.result.Parts[:1]
	ctx, resources := admissionTestContext(time.Second)
	if err := service.ProcessGraphQualifications(ctx, 1); !errors.Is(err, domain.ErrIntegrity) || storage.finishes != 0 || len(storage.repairs) != 1 || storage.failures != 1 {
		test.Fatal("reference bypassed payload verification or bounded repair", err)
	}
	assertResourcesReleased(test, resources)
}

type graphProjectionReferenceTestStore struct {
	*graphQualificationTestStore
	page       domain.RepositoryViewSourcePage
	pageReads  int
	pageInRead bool
	request    domain.RepositoryViewSourceRequest
}

func (storage *graphProjectionReferenceTestStore) RepositoryGraphSourceChanges(ctx context.Context, _ domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	storage.pageReads++
	storage.pageInRead = storage.InReadOnlyTransaction(ctx)
	storage.request = request
	return storage.page, nil
}

func TestGraphProjectionCapturesReferenceInsideOwnedFirstPageRead(test *testing.T) {
	service, base, repo := newGraphQualificationTest(test)
	storage := &graphProjectionReferenceTestStore{graphQualificationTestStore: base,
		page: graphCapturePage(base.source, "baseline", nil, "")}
	service.meta = storage
	job := domain.GraphProjectionLease{RepoID: repo, Dataset: base.lease.Dataset, Epoch: base.source.Epoch,
		Phase: "capture", Fence: 1, AppliedSequence: -1, InputSequence: -1, LeaseUntil: time.Now().Add(time.Minute)}
	result, err := service.captureGraphProjectionFirstPage(systemTestContext(), storage, job)
	if err != nil || result.reference.Expected != base.qualificationDigest || result.reference.GraphQualificationKey != base.lease.GraphQualificationKey || !storage.pageInRead || storage.active {
		test.Fatal("first page and independent reference were not coherently captured", result, err)
	}
	if storage.budgetChecks != 1 || storage.snapshotLists != 1 || storage.request.Limit != domain.DefaultRepositoryViewSourceLimit || service.graphCache.Stats().Entries != 0 {
		test.Fatal("capture skipped bounded independent read or populated serving cache")
	}
	job.Cursor = "continuation"
	if _, err := service.captureGraphProjectionFirstPage(systemTestContext(), storage, job); !errors.Is(err, domain.ErrValidation) || storage.pageReads != 1 {
		test.Fatal("continuation recomputed reference", err)
	}
}

func TestGraphProjectionReferenceRejectsIncoherentOrOversizedCapture(test *testing.T) {
	for _, failure := range []string{"checkpoint", "page", "budget"} {
		test.Run(failure, func(test *testing.T) {
			service, base, repo := newGraphQualificationTest(test)
			storage := &graphProjectionReferenceTestStore{graphQualificationTestStore: base,
				page: graphCapturePage(base.source, "baseline", nil, "")}
			service.meta = storage
			job := domain.GraphProjectionLease{RepoID: repo, Dataset: base.lease.Dataset, Epoch: base.source.Epoch,
				Phase: "capture", Fence: 1, AppliedSequence: -1, InputSequence: -1, LeaseUntil: time.Now().Add(time.Minute)}
			switch failure {
			case "checkpoint":
				base.source.Sequence++
			case "page":
				storage.page.RepoID = hh("foreign page")
			case "budget":
				base.budgetErr = domain.ErrRepositoryViewSourceLimit
			}
			result, err := service.captureGraphProjectionFirstPage(systemTestContext(), storage, job)
			if err == nil || result.reference.Expected != "" || storage.snapshotLists != 0 || storage.active {
				test.Fatal("failed capture escaped with reference or retained read", result, err)
			}
		})
	}
}

func TestGraphProjectionReferenceExcludesInheritedEvidenceAndCaches(test *testing.T) {
	service, storage, repo := newGraphQualificationTest(test)
	poison := &capturedGraphEvidence{metadata: domain.Repo{ID: repo}, err: domain.ErrIntegrity}
	service.projectionEvidence = poison
	service.branchCache.Put(branchProjectionKey{Repo: repo, Generation: storage.source}, map[string]domain.BranchContext{"poison": {}})
	want := storage.qualificationDigest
	got, err := repositoryRead(systemTestContext(), service, func(bound context.Context) (domain.ContentHash, error) {
		return service.graphQualificationReference(bound, repo)
	})
	if err != nil || got != want || poison.calls != 0 || service.branchCache.Stats().Entries != 1 || storage.snapshotLists != 1 {
		test.Fatal("independent reference reused inherited evidence or cache", got, want, err, poison.calls)
	}
}

func TestGraphProjectionReferenceCancellationReleasesOwnedRead(test *testing.T) {
	service, base, repo := newGraphQualificationTest(test)
	storage := &graphProjectionReferenceTestStore{graphQualificationTestStore: base,
		page: graphCapturePage(base.source, "baseline", nil, "")}
	service.meta = storage
	ctx, cancel := context.WithCancel(systemTestContext())
	defer cancel()
	base.duringBudget = cancel
	job := domain.GraphProjectionLease{RepoID: repo, Dataset: base.lease.Dataset, Epoch: base.source.Epoch,
		Phase: "capture", Fence: 1, AppliedSequence: -1, InputSequence: -1, LeaseUntil: time.Now().Add(time.Minute)}
	result, err := service.captureGraphProjectionFirstPage(ctx, storage, job)
	if !errors.Is(err, context.Canceled) || result.reference.Expected != "" || storage.active || storage.snapshotLists != 0 || storage.budgetChecks != 1 {
		test.Fatal("canceled reference retained read or produced a receipt", result, err)
	}
}
