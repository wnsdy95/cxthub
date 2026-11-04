package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphQualificationTestStore struct {
	*graphRepairTransactions
	lease                      domain.GraphQualificationLease
	source                     domain.RepositoryViewSourceCheckpoint
	result                     domain.GraphQualificationResult
	configured                 []domain.ContentHash
	claimedRepos               []domain.ContentHash
	failureReasons             []string
	claims, finishes, failures int
	claimErr                   error
	finishErr                  error
	budgetErr                  error
	budgetChecks               int
	duringBudget               func()
	repoReads                  int
	check                      func(context.Context)
}

func (storage *graphQualificationTestStore) InReadOnlyTransaction(ctx context.Context) bool {
	return storage.InTransaction(ctx)
}

func (storage *graphQualificationTestStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	storage.repoReads++
	return storage.graphProjectionReadStore.GetRepo(ctx, repo)
}

func (storage *graphQualificationTestStore) CheckGraphQualificationInputs(ctx context.Context, _ domain.ContentHash) error {
	storage.budgetChecks++
	if storage.duringBudget != nil {
		storage.duringBudget()
	}
	if !storage.InTransaction(ctx) {
		return domain.ErrConflict
	}
	return storage.budgetErr
}

func (storage *graphQualificationTestStore) ConfigureGraphQualification(_ context.Context, repo domain.ContentHash, enabled bool) error {
	if !enabled {
		return domain.ErrValidation
	}
	storage.configured = append(storage.configured, repo)
	return nil
}

func (storage *graphQualificationTestStore) ClaimGraphQualification(ctx context.Context, repo domain.ContentHash, _ time.Duration) (domain.GraphQualificationLease, error) {
	storage.claims++
	storage.claimedRepos = append(storage.claimedRepos, repo)
	if storage.check != nil {
		storage.check(ctx)
	}
	return storage.lease, storage.claimErr
}

func (storage *graphQualificationTestStore) FinishGraphQualification(ctx context.Context, _ domain.GraphQualificationLease, result domain.GraphQualificationResult) error {
	storage.finishes++
	if storage.check != nil {
		storage.check(ctx)
	}
	storage.result = result
	return storage.finishErr
}

func (storage *graphQualificationTestStore) FailGraphQualification(ctx context.Context, _ domain.GraphQualificationLease, reason string) error {
	storage.failures++
	storage.failureReasons = append(storage.failureReasons, reason)
	if storage.check != nil {
		storage.check(ctx)
	}
	return nil
}

func (storage *graphQualificationTestStore) ReadRepositoryGraphSourceGeneration(context.Context, domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	return storage.source, nil
}

func newGraphQualificationTest(test *testing.T) (*Service, *graphQualificationTestStore, domain.ContentHash) {
	test.Helper()
	service, original, repo, _ := newGraphProjectionRead(test)
	storage := &graphQualificationTestStore{graphRepairTransactions: &graphRepairTransactions{graphProjectionReadStore: original},
		source: original.result.Applied, lease: domain.GraphQualificationLease{GraphQualificationKey: original.result.QualificationKey(), Fence: 1, LeaseUntil: time.Now().Add(time.Minute)}}
	service.meta = storage
	if err := service.ConfigureGraphQualifications(systemTestContext(), []domain.ContentHash{repo, repo}); err != nil || len(storage.configured) != 1 {
		test.Fatal("qualification configuration failed", err)
	}
	storage.check = func(ctx context.Context) {
		if storage.InTransaction(ctx) || storage.active {
			test.Fatal("qualification write retained read snapshot")
		}
		if permit, ok := ctx.Value(resourcePermitKey{}).(*resourcePermit); !ok || !permit.live.Load() {
			test.Fatal("qualification bypassed resources")
		}
	}
	return service, storage, repo
}

func TestGraphQualificationRequiresExactCompletedReceipt(test *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(test)
	for _, mismatch := range []bool{false, true} {
		storage.qualificationMismatch = mismatch
		storage.qualificationErr = domain.ErrNotFound
		want := domain.ErrGraphProjectionNotReady
		state := "catching_up"
		if mismatch {
			storage.qualificationErr = nil
			want, state = domain.ErrGraphProjectionFailed, "needs_attention"
		}
		if _, err := service.GetRepositoryView(systemTestContext(), repo); !errors.Is(err, want) {
			test.Fatal("unqualified graph served", err)
		}
		notice, err := service.RepositoryChangeState(systemTestContext(), repo)
		if err != nil || notice.GraphProjection == nil || notice.GraphProjection.Complete || notice.GraphProjection.Current || notice.GraphProjection.Generation != "" || notice.GraphProjection.State != state {
			test.Fatal("unqualified ready notice escaped", notice, err)
		}
		if len(storage.repairs) != 0 || storage.snapshotLists != 0 {
			test.Fatal("mismatch silently rebuilt or fell back")
		}
	}
}

func TestGraphQualificationComparesPinnedViewsAndPersistsAfterRead(test *testing.T) {
	for _, mismatch := range []bool{false, true} {
		test.Run(map[bool]string{false: "equal", true: "different"}[mismatch], func(test *testing.T) {
			service, storage, repo := newGraphQualificationTest(test)
			if mismatch {
				view, err := domain.DecodeGraphViewProjection(systemTestContext(), repo, storage.graphProjectionReadStore.result.Parts)
				if err != nil {
					test.Fatal(err)
				}
				view.Snapshots[0].Message = "wrong but valid projection"
				storage.graphProjectionReadStore.result.Parts, err = domain.EncodeGraphViewProjection(systemTestContext(), repo, view)
				if err != nil {
					test.Fatal(err)
				}
			}
			ctx, resources := admissionTestContext(time.Second)
			if err := service.ProcessGraphQualifications(ctx, 1); err != nil || storage.finishes != 1 || storage.failures != 0 || storage.result.Matched() == mismatch {
				test.Fatal("wrong comparison", storage.result, storage.finishes, storage.failures, err)
			}
			if service.graphCache.Stats().Entries != 0 || len(storage.repairs) != 0 {
				test.Fatal("shadow comparison mutated serving/source state")
			}
			assertResourcesReleased(test, resources)
		})
	}
}

func TestGraphQualificationRejectsStaleSourceBeforeFullRead(test *testing.T) {
	service, storage, _ := newGraphQualificationTest(test)
	storage.source.Sequence++
	ctx, resources := admissionTestContext(time.Second)
	if err := service.ProcessGraphQualifications(ctx, 1); !errors.Is(err, domain.ErrGraphProjectionNotReady) || storage.finishes != 0 || storage.failures != 1 || storage.snapshotLists != 0 {
		test.Fatal("stale source counted as mismatch", storage.finishes, storage.failures, err)
	}
	assertResourcesReleased(test, resources)
}

func TestGraphQualificationRejectsBorrowedOrReadOnlyWork(test *testing.T) {
	service, storage, _ := newGraphQualificationTest(test)
	for _, ctx := range []context.Context{context.WithValue(systemTestContext(), graphReadTransactionKey{}, true), outbound.WithDocReadOnly(systemTestContext())} {
		if err := service.ProcessGraphQualifications(ctx, 1); !errors.Is(err, domain.ErrConflict) || storage.claims != 0 {
			test.Fatal("borrowed transaction admitted", err)
		}
	}
	storage.claimErr = domain.ErrNotFound
	if err := service.ProcessGraphQualifications(systemTestContext(), 1); err != nil || storage.finishes != 0 || storage.failures != 0 {
		test.Fatal("no eligible generation became a failure", err)
	}
	if err := service.ConfigureGraphQualifications(systemTestContext(), nil); err != nil {
		test.Fatal(err)
	}
	claims := storage.claims
	if err := service.ProcessGraphQualifications(systemTestContext(), 1); err != nil || storage.claims != claims {
		test.Fatal("disabled worker ran", err)
	}
}

func TestGraphQualificationRotatesPastFailingRepositories(test *testing.T) {
	service, storage, repo := newGraphQualificationTest(test)
	peer := hh("second qualification repository")
	service.graphQualificationRepos = []domain.ContentHash{repo, peer}
	storage.claimErr = domain.ErrIntegrity
	for range 4 {
		if err := service.ProcessGraphQualifications(systemTestContext(), 1); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal(err)
		}
	}
	for index, claimed := range storage.claimedRepos {
		if claimed != service.graphQualificationRepos[index%2] {
			test.Fatal("failure starved the next repository", storage.claimedRepos)
		}
	}
}

func TestGraphQualificationPublicationFailureIsNotHidden(test *testing.T) {
	service, storage, _ := newGraphQualificationTest(test)
	storage.finishErr = domain.ErrConflict
	if err := service.ProcessGraphQualifications(systemTestContext(), 1); !errors.Is(err, domain.ErrConflict) || storage.finishes != 1 || storage.failures != 0 {
		test.Fatal("stale finish treated as success", err)
	}
}

func TestGraphQualificationLimitsInputsBeforeDirectMaterialization(test *testing.T) {
	service, storage, _ := newGraphQualificationTest(test)
	storage.budgetErr = domain.ErrRepositoryViewSourceLimit
	if err := service.ProcessGraphQualifications(systemTestContext(), 1); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || storage.budgetChecks != 1 || storage.repoReads != 0 || storage.snapshotLists != 0 || storage.finishes != 0 || storage.failures != 1 {
		test.Fatal("input budget was enforced too late", err)
	}
}

func TestGraphQualificationReceiptBindsValidatedPayload(test *testing.T) {
	service, storage, repo, view := newGraphProjectionRead(test)
	view.Snapshots[0].Message = "changed output after qualification"
	parts, err := domain.EncodeGraphViewProjection(systemTestContext(), repo, view)
	if err != nil {
		test.Fatal(err)
	}
	storage.result.Parts = parts
	if _, err := service.GetRepositoryView(systemTestContext(), repo); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("valid but unqualified payload reused receipt", err)
	}
	if len(storage.repairs) != 1 {
		test.Fatal("post-qualification payload drift did not request bounded repair")
	}
}

func TestGraphQualificationRepairsCandidateOnlyAfterOwnedRead(test *testing.T) {
	service, storage, _ := newGraphQualificationTest(test)
	storage.graphProjectionReadStore.result.Parts = storage.graphProjectionReadStore.result.Parts[:1]
	storage.graphRepairTransactions.check = func(bound context.Context) {
		if storage.active || storage.InTransaction(bound) {
			test.Fatal("candidate repair held comparison snapshot")
		}
		if storage.failures != 1 {
			test.Fatal("repair preceded durable failure")
		}
	}
	ctx, resources := admissionTestContext(time.Second)
	if err := service.ProcessGraphQualifications(ctx, 1); !errors.Is(err, domain.ErrIntegrity) || storage.finishes != 0 || len(storage.repairs) != 1 ||
		storage.repairs[0].Serving || storage.servingRepairs != 0 || storage.repairs[0].Dataset != storage.lease.Dataset {
		test.Fatal("candidate corruption lost scoped repair", err, storage.repairs)
	}
	assertResourcesReleased(test, resources)
}

func TestGraphQualificationNeverRepairsUnidentifiedOrServingCorruption(test *testing.T) {
	for _, kind := range []string{"foreign", "newer", "serving", "unknown", "canceled"} {
		test.Run(kind, func(test *testing.T) {
			service, storage, _ := newGraphQualificationTest(test)
			failure := &domain.GraphProjectionCorruption{RepoID: storage.lease.RepoID, Dataset: storage.lease.Dataset,
				Applied: storage.lease.Applied, Rules: storage.lease.Rules, Cause: domain.ErrIntegrity}
			switch kind {
			case "foreign":
				failure.RepoID = hh("other repository")
				failure.Applied.RepoID = failure.RepoID
			case "newer":
				failure.Applied.Sequence++
			case "serving":
				failure.Serving, failure.ServingFence = true, 1
			case "unknown":
				failure.Dataset = ""
			case "canceled":
				failure.Cause = context.Canceled
			}
			storage.readErr = failure
			if err := service.ProcessGraphQualifications(systemTestContext(), 1); err == nil || len(storage.repairs) != 0 {
				test.Fatal("unrelated output scheduled candidate repair", err, storage.repairs)
			}
		})
	}
}

func TestGraphQualificationCandidateSizeCorruptionRemainsRetryable(test *testing.T) {
	service, storage, _ := newGraphQualificationTest(test)
	storage.readErr = &domain.GraphProjectionCorruption{RepoID: storage.lease.RepoID, Dataset: storage.lease.Dataset,
		Applied: storage.lease.Applied, Rules: storage.lease.Rules, Cause: domain.ErrRepositoryViewSourceLimit}
	storage.repairErr = errors.New("temporary repair failure")
	if err := service.ProcessGraphQualifications(systemTestContext(), 1); !errors.Is(err, storage.repairErr) || len(storage.repairs) != 1 ||
		len(storage.failureReasons) != 1 || storage.failureReasons[0] != "read_error" {
		test.Fatal("oversized candidate was suppressed after repair failure", err, storage.failureReasons)
	}
	service, storage, _ = newGraphQualificationTest(test)
	storage.budgetErr = domain.ErrRepositoryViewSourceLimit
	if err := service.ProcessGraphQualifications(systemTestContext(), 1); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) ||
		len(storage.repairs) != 0 || len(storage.failureReasons) != 1 || storage.failureReasons[0] != "resource_limit" {
		test.Fatal("source input limit started candidate repair", err, storage.failureReasons)
	}
}
