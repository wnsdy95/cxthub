package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGitContextHeadsBindOnlyPreReadIncarnations(t *testing.T) {
	fixture, batch, receipt, _ := admissionBatchPlanFixture(t)
	before := batch.snapshot.revision
	ref := domain.Ref{RepoID: fixture.repo.ID, Kind: domain.RefBranch, Name: "late", BranchID: domain.LegacyContextBranchID(string(fixture.repo.ID), "late"), Target: fixture.main.Target}
	if err := fixture.storage.CompareAndSwapRef(systemTestContext(), fixture.repo.ID, ref, ""); err != nil {
		t.Fatal(err)
	}
	receipt.Heads = append(receipt.Heads, domain.GitHeadReceiptHead{Ref: "refs/heads/late", Commit: strings.Repeat("a", 40)})
	receipt = rebuildAdmissionReceipt(receipt)
	heads, err := prepareGitContextHeads(batch.snapshot, &receipt)
	if err != nil || len(heads) != 2 {
		t.Fatal("provider response added an unfrozen context incarnation", heads, err)
	}
	for _, head := range heads {
		if head.Destination.DestinationBranch == "late" || !head.Destination.MatchesReceipt(receipt) || head.GraphRevision != before.Graph || head.EvidenceRevision != before.Evidence || head.Validate() != nil {
			t.Fatal("unbound head accepted", head)
		}
		expected := fixture.main.Target
		if head.Destination.DestinationBranch == fixture.ordinary.Branch {
			expected = fixture.ordinary.Target
		}
		if head.ContextTarget != expected {
			t.Fatal("lost pre-read context target", head)
		}
	}
	refreshAdmissionBatchView(t, fixture, batch)
	fresh, err := prepareGitContextHeads(batch.snapshot, &receipt)
	if err != nil || len(fresh) != 3 {
		t.Fatal("later independent read cannot see new incarnation", fresh, err)
	}
}

func TestGitContextHeadsRejectWrongScopeAndUnqualifiedReads(t *testing.T) {
	_, batch, receipt, _ := admissionBatchPlanFixture(t)
	for _, scenario := range []string{"origin", "repository", "provider", "tampered-id"} {
		t.Run(scenario, func(t *testing.T) {
			changed := receipt
			switch scenario {
			case "origin":
				changed.GitOrigin += "-other"
			case "repository":
				changed.RepoID = hh("other context head")
			case "provider":
				changed.Provider = domain.GitProviderObservation{}
			case "tampered-id":
				changed.Version++
			}
			if scenario != "tampered-id" {
				changed = rebuildAdmissionReceipt(changed)
			}
			if heads, err := prepareGitContextHeads(batch.snapshot, &changed); err == nil || len(heads) != 0 {
				t.Fatal("invalid binding produced heads", heads, err)
			}
		})
	}
	for _, input := range []*gitAdmissionSnapshot{nil, batch.snapshot} {
		heads, err := prepareGitContextHeads(input, nil)
		if err != nil || !reflect.DeepEqual(heads, []domain.GitContextHead(nil)) {
			t.Fatal("unqualified page invented context evidence", heads, err)
		}
	}
}
