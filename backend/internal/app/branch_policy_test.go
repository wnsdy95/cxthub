package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchPolicyContributionStore struct {
	*store.FSStore
	reads int
}

func (storage *branchPolicyContributionStore) ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error) {
	storage.reads++
	return nil, nil
}

func TestBranchPolicyRejectsInvalidSelectionBeforeContributionIO(test *testing.T) {
	service, filesystem := newFsckSvc(test)
	storage := &branchPolicyContributionStore{FSStore: filesystem}
	service.meta = storage
	repo := domain.HashContent([]byte(test.Name()))
	snapshot := domain.Snapshot{ID: domain.HashContent([]byte("capture")), RepoID: repo}
	if _, err := filesystem.PutRepo(context.Background(), domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	inputs, err := newBranchQueryInputs(context.Background(), repo, []domain.Snapshot{snapshot}, nil)
	if err != nil {
		test.Fatal(err)
	}
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: snapshot.ID}
	for _, name := range []string{"invalid-code", "missing-target", "foreign-index", "canceled"} {
		test.Run(name, func(test *testing.T) {
			selected, ctx, code := ref, context.Background(), ""
			want := domain.ErrValidation
			switch name {
			case "invalid-code":
				code = "invalid"
			case "missing-target":
				selected.Target, want = domain.HashContent([]byte("unknown")), domain.ErrNotFound
			case "foreign-index":
				selected.RepoID, want = domain.HashContent([]byte("foreign")), domain.ErrIntegrity
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			_, err := service.branchContextFromInputs(ctx, selected, code, inputs, nil)
			if !errors.Is(err, want) || storage.reads != 0 {
				test.Fatal("selection validation moved after contribution I/O", err, storage.reads)
			}
		})
	}
	actual, err := service.branchContextFromInputs(context.Background(), ref, "", inputs, nil)
	if err != nil || len(actual.SnapshotIDs) != 1 || actual.SnapshotIDs[0] != snapshot.ID || storage.reads != 1 {
		test.Fatal("valid selection bypassed current contribution reads or changed inclusion", actual, err, storage.reads)
	}
}

func TestBranchPolicyContributionViewMapsValuesWithoutCopyingTheLedger(test *testing.T) {
	stamp := time.Unix(100, 0).UTC()
	records := []contextContributionProjection{{
		record: contributionMemoryRecord{ID: "record", DestinationBranchID: "main", Capture: domain.HashContent([]byte("capture")), IntegrationSHA: "integration"},
		valid:  true, sourceBranch: "topic", destinationBranch: "main", prNumber: 7, mergedAt: stamp,
	}}
	facts := branchContributionFacts(records)
	actual := facts.At(0)
	if facts.Len() != 1 || actual.ID != records[0].record.ID || actual.Capture != records[0].record.Capture || actual.DestinationBranchID != "main" || actual.IntegrationSHA != "integration" || !actual.Valid || actual.SourceBranch != "topic" || actual.DestinationBranch != "main" || actual.PRNumber != 7 || actual.MergedAt != stamp {
		test.Fatal("branch policy lost verified contribution metadata", actual)
	}
	records[0].valid = false
	if facts.At(0).Valid {
		test.Fatal("branch policy created a stale second ledger")
	}
	if branchPolicyEvidence(nil) != nil {
		test.Fatal("nil evidence became a nonnil policy interface")
	}
}
