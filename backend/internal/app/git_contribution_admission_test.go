package app

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func admissionPreparationFixture(t *testing.T) (*prMemoryFixture, frozenGitAdmission, domain.GitHeadReceipt, *codeEvidence) {
	t.Helper()
	fixture := newPRMemoryFixture(t)
	if err := fixture.storage.CompareAndSwapRef(systemTestContext(), fixture.repo.ID, domain.Ref{RepoID: fixture.repo.ID, Kind: domain.RefBranch, Name: fixture.ordinary.Branch, BranchID: fixture.ordinary.BranchID, Target: fixture.ordinary.Target}, ""); err != nil {
		t.Fatal(err)
	}
	publication := fixture.publish(t, "", true)
	carried := fixture.ordinary
	carried.ID, carried.Kind, carried.Source = strings.Repeat("7", 32), "position", carried.Target
	carried.GitBefore, carried.GitAfter, carried.Creation = publication.GitAfter, strings.Repeat("b", 40), nil
	if err := fixture.worker.core.RecordHistory(systemTestContext(), carried); err != nil {
		t.Fatal(err)
	}
	frozen, err := fixture.worker.core.freezeGitAdmission(systemTestContext(), fixture.repo.ID, publication.ID, carried.GitAfter, fixture.main.Name, fixture.main.BranchID)
	if err != nil {
		t.Fatal(err)
	}
	_, evidence := genericPreparationFixture(t, fixture)
	evidence.trees.(*introductionTreeReader).commits[carried.GitAfter] = domain.GitCommitTree{Commit: carried.GitAfter, Tree: strings.Repeat("f", 40), Parents: []string{publication.GitAfter}}
	receipt := domain.GitHeadReceipt{RepoID: fixture.repo.ID, GitOrigin: fixture.repo.GitRemoteURL, Version: 3, Page: 1,
		Provider: domain.GitProviderObservation{InstallationID: 789, RepositoryID: 91},
		Heads:    []domain.GitHeadReceiptHead{{Ref: "refs/heads/" + fixture.main.Name, Commit: publication.GitAfter}, {Ref: "refs/heads/" + fixture.ordinary.Branch, Commit: carried.GitAfter}}}
	receipt = rebuildAdmissionReceipt(receipt)
	return fixture, frozen, receipt, evidence
}

func rebuildAdmissionReceipt(receipt domain.GitHeadReceipt) domain.GitHeadReceipt {
	receipt.Heads = append([]domain.GitHeadReceiptHead{}, receipt.Heads...)
	for index := range receipt.Heads {
		head := &receipt.Heads[index]
		head.ObservationID = (domain.GitRefObservation{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin, Source: "reconciliation", Ref: head.Ref, After: head.Commit, Provider: &receipt.Provider}).WithID().ID
	}
	sort.Slice(receipt.Heads, func(left, right int) bool { return receipt.Heads[left].Ref < receipt.Heads[right].Ref })
	return receipt.WithID()
}

func TestGitAdmissionPreparesCarriedHeadWithoutRelabelingOriginal(t *testing.T) {
	fixture, frozen, receipt, evidence := admissionPreparationFixture(t)
	prepared, err := prepareGitAdmission(systemTestContext(), frozen, receipt, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.OriginalHead != fixture.ordinary.GitAfter || prepared.SourceHead != strings.Repeat("b", 40) || prepared.Destination.DestinationHead != fixture.ordinary.GitAfter ||
		!reflect.DeepEqual(prepared.OrdinaryObservationIDs, []string{fixture.ordinary.ID}) || !reflect.DeepEqual(prepared.CarryObservationIDs, []string{strings.Repeat("7", 32)}) ||
		prepared.SourceAnchorID != fixture.ordinary.ID || prepared.DestinationAnchorID != "" || !prepared.MatchesBaseline(receipt) || !sameGitAdmissionInput(frozen.input, prepared) {
		t.Fatalf("lost frozen provenance: %+v", prepared)
	}
	prepared.CarryObservationIDs[0] = strings.Repeat("f", 32)
	if frozen.input.CarryObservationIDs[0] != strings.Repeat("7", 32) {
		t.Fatal("prepared output mutated the frozen input")
	}
	rows, err := fixture.storage.ListGitContributions(systemTestContext(), fixture.repo.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("preparation emitted a join: %+v %v", rows, err)
	}
}

func TestGitAdmissionPreparationRequiresCompleteAbsentBaseline(t *testing.T) {
	for _, scenario := range []string{"already-present", "missing-source", "missing-destination", "incomplete-baseline", "unrelated-carry", "foreign-origin", "providerless", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			_, frozen, receipt, evidence := admissionPreparationFixture(t)
			ctx := systemTestContext()
			want := domain.ErrNotFound
			switch scenario {
			case "already-present":
				for index := range receipt.Heads {
					if receipt.Heads[index].Ref == "refs/heads/main" {
						receipt.Heads[index].Commit = frozen.input.SourceHead
					}
				}
				want = errGitContributionAlreadyPresent
			case "missing-source":
				receipt.Heads = receipt.Heads[1:]
			case "missing-destination":
				receipt.Heads = receipt.Heads[:1]
			case "incomplete-baseline":
				delete(evidence.trees.(*introductionTreeReader).commits, strings.Repeat("1", 40))
				want = errGitContributionEvidencePending
			case "unrelated-carry":
				proof := evidence.trees.(*introductionTreeReader).commits[frozen.input.SourceHead]
				proof.Parents = []string{}
				evidence.trees.(*introductionTreeReader).commits[proof.Commit] = proof
			case "foreign-origin":
				receipt.GitOrigin += "-other"
				want = domain.ErrIntegrity
			case "providerless":
				receipt.Provider = domain.GitProviderObservation{}
				want = domain.ErrIntegrity
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			}
			receipt = rebuildAdmissionReceipt(receipt)
			got, err := prepareGitAdmission(ctx, frozen, receipt, evidence)
			if !errors.Is(err, want) || !reflect.DeepEqual(got, domain.GitContributionAdmission{}) {
				t.Fatalf("unproven admission: %+v %v; want %v", got, err, want)
			}
		})
	}
}

func TestGitAdmissionFreezeTracksConcurrentContextChanges(t *testing.T) {
	fixture, frozen, receipt, evidence := admissionPreparationFixture(t)
	prepared, err := prepareGitAdmission(systemTestContext(), frozen, receipt, evidence)
	if err != nil {
		t.Fatal(err)
	}
	before := frozen.input
	late := fixture.ordinary
	late.ID, late.Kind, late.Source, late.Creation = strings.Repeat("6", 32), "position", late.Target, nil
	if err := fixture.worker.core.RecordHistory(systemTestContext(), late); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.worker.core.freezeGitAdmission(systemTestContext(), fixture.repo.ID, before.PublicationID, before.SourceHead, fixture.main.Name, fixture.main.BranchID)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(current, frozen) || !reflect.DeepEqual(frozen.input, before) || !sameGitAdmissionInput(frozen.input, prepared) || sameGitAdmissionInput(current.input, prepared) {
		t.Fatal("late ordinary evidence silently enlarged the pre-read inputs")
	}
	if got, err := fixture.worker.core.persistGitAdmission(systemTestContext(), frozen, prepared, gitAdmissionAuthorization{}); !errors.Is(err, domain.ErrValidation) || got.ID != "" {
		t.Fatalf("FS claimed a transactional admission: %+v %v", got, err)
	}
}

func TestGitAdmissionRejectsAmbiguousSourceOwner(t *testing.T) {
	fixture, frozen, _, _ := admissionPreparationFixture(t)
	other := fixture.ordinary
	other.ID, other.Kind, other.BranchID, other.Source, other.Creation = strings.Repeat("6", 32), "position", "other-incarnation", other.Target, nil
	other.GitAfter = frozen.input.SourceHead
	if err := fixture.worker.core.RecordHistory(systemTestContext(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.worker.core.freezeGitAdmission(systemTestContext(), fixture.repo.ID, frozen.input.PublicationID, frozen.input.SourceHead, fixture.main.Name, fixture.main.BranchID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ambiguous head chose a context by arrival: %v", err)
	}
}

func TestGitAdmissionAuthorizationRejectsUnqualifiedSource(t *testing.T) {
	if got, err := authorizeGitAdmission(systemTestContext(), nil, hh("unqualified admission")); !errors.Is(err, domain.ErrForbidden) || got.fence != nil || got.provider != (domain.GitProviderObservation{}) {
		t.Fatalf("legacy source became an admission authority: %+v %v", got, err)
	}
}
