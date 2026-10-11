package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func genericPreparationFixture(t *testing.T, fixture *prMemoryFixture) (domain.GitDestinationCandidate, *codeEvidence) {
	t.Helper()
	evidence, err := fixture.worker.core.newCodeEvidence(systemTestContext(), fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	root, before, merge, latest := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
	tree := strings.Repeat("f", 40)
	evidence.trees = &introductionTreeReader{commits: map[string]domain.GitCommitTree{
		root:                      {Commit: root, Tree: tree, Parents: []string{}},
		before:                    {Commit: before, Tree: tree, Parents: []string{root}},
		fixture.ordinary.GitAfter: {Commit: fixture.ordinary.GitAfter, Tree: tree, Parents: []string{root}},
		merge:                     {Commit: merge, Tree: tree, Parents: []string{before, fixture.ordinary.GitAfter}},
		latest:                    {Commit: latest, Tree: tree, Parents: []string{merge}},
	}}
	witness := (domain.GitDestinationCandidate{RepoID: fixture.repo.ID, GitOrigin: fixture.repo.GitRemoteURL, InstallationID: 789,
		ProviderRepositoryID: 91, ObservationID: strings.Repeat("b", 64), DestinationBranchID: fixture.main.BranchID,
		ReadReceiptID: strings.Repeat("c", 64), ReadVersion: 1, DestinationBranch: fixture.main.Name, DestinationHead: latest}).WithID()
	return witness, evidence
}

func TestGitContributionCandidatePreservesLateSourceAndMemory(t *testing.T) {
	for _, memoryMode := range []string{"pinned", "empty", "unknown"} {
		t.Run(memoryMode, func(t *testing.T) {
			fixture := newPRMemoryFixture(t)
			ctx := systemTestContext()
			hash := domain.ContentHash("")
			if memoryMode == "pinned" {
				hash = fixture.digest(t, "", "original source memory")
			}
			publication := fixture.publish(t, hash, memoryMode != "unknown")
			latestMemory := fixture.digest(t, hash, "unpublished replacement must not be selected")
			if err := fixture.storage.CompareAndSwapSnapshotMemory(ctx, fixture.repo.ID, fixture.ordinary.Target, "", latestMemory); err != nil {
				t.Fatal(err)
			}
			history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			publications, err := domain.NewGitPublicationIndex(ctx, fixture.repo.ID, history)
			if err != nil {
				t.Fatal(err)
			}
			before, err := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			witness, evidence := genericPreparationFixture(t, fixture)
			prepared, err := fixture.worker.core.inspectGitContributionCandidate(ctx, witness, publication.ID, publication.GitAfter, publications, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.possibleIntroduction != strings.Repeat("d", 40) || prepared.publication != publication || prepared.witness != witness || prepared.sourceHead != publication.GitAfter || !reflect.DeepEqual(prepared.sourceObservations, []string{fixture.ordinary.ID}) || !reflect.DeepEqual(prepared.carryObservations, prepared.sourceObservations) {
				t.Fatalf("late source lost its original or destination identity: %+v", prepared)
			}
			if memoryMode == "unknown" {
				if prepared.memorySource != "" || prepared.publication.MemoryPinned {
					t.Fatalf("unknown memory was finalized: %+v", prepared)
				}
			} else if prepared.publication.MemoryHash != hash || !prepared.publication.MemoryPinned {
				t.Fatalf("original pin replaced by mutable pointer: %+v", prepared)
			}
			retry, err := fixture.worker.core.inspectGitContributionCandidate(ctx, witness, publication.ID, publication.GitAfter, publications, evidence)
			if err != nil || !reflect.DeepEqual(retry, prepared) {
				t.Fatalf("retry changed the immutable plan: %+v %v", retry, err)
			}
			after, err := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("preparation rewrote conversation ancestry or memory pointers", err)
			}
			ref, err := fixture.storage.GetRef(ctx, fixture.repo.ID, domain.RefBranch, fixture.main.Name)
			if err != nil || ref != fixture.main {
				t.Fatal("late preparation moved the newer main tip", ref, err)
			}
			rows, err := fixture.storage.ListGitContributions(ctx, fixture.repo.ID)
			if err != nil || len(rows) != 0 {
				t.Fatal("preparation published before the application transaction", rows, err)
			}
		})
	}
}

func TestGitContributionCandidateWithholdsUnprovenInputs(t *testing.T) {
	for _, scenario := range []string{"missing-publication", "unobserved-publication", "foreign-origin", "foreign-repository", "incomplete-git", "unrelated-git", "corrupt-memory"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPRMemoryFixture(t)
			ctx := systemTestContext()
			publication := fixture.publish(t, "", true)
			history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			witness, evidence := genericPreparationFixture(t, fixture)
			want := domain.ErrNotFound
			switch scenario {
			case "missing-publication":
				publication.ID = strings.Repeat("f", 32)
			case "unobserved-publication":
				history = []domain.HistoryEvent{publication}
			case "foreign-origin":
				witness.GitOrigin += "-other"
				want = domain.ErrIntegrity
			case "foreign-repository":
				witness.RepoID = hh("different repo")
				want = domain.ErrIntegrity
			case "incomplete-git":
				delete(evidence.trees.(*introductionTreeReader).commits, strings.Repeat("2", 40))
				want = errGitContributionEvidencePending
			case "unrelated-git":
				witness.DestinationHead = strings.Repeat("2", 40)
			case "corrupt-memory":
				for index := range history {
					if history[index].ID == publication.ID {
						history[index].MemoryHash = hh("missing memory")
					}
				}
			}
			witness = witness.WithID()
			publications, err := domain.NewGitPublicationIndex(ctx, fixture.repo.ID, history)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := fixture.worker.core.inspectGitContributionCandidate(ctx, witness, publication.ID, publication.GitAfter, publications, evidence)
			if !errors.Is(err, want) || prepared.publication.ID != "" || prepared.witness.ID != "" || prepared.memorySource != "" {
				t.Fatalf("unproven input produced a partial successful plan: %+v err=%v want=%v", prepared, err, want)
			}
		})
	}
}

func TestGitContributionDestinationBindingRejectsScopeDrift(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	observation := (domain.GitRefObservation{RepoID: fixture.repo.ID, GitOrigin: fixture.repo.GitRemoteURL, Source: "reconciliation",
		Ref: "refs/heads/main", After: strings.Repeat("e", 40), Provider: &domain.GitProviderObservation{InstallationID: 789, RepositoryID: 91}}).WithID()
	receipt := (domain.GitHeadReceipt{RepoID: fixture.repo.ID, GitOrigin: fixture.repo.GitRemoteURL, Version: 1, Page: 1, Provider: *observation.Provider,
		Heads: []domain.GitHeadReceiptHead{{Ref: observation.Ref, Commit: observation.After, ObservationID: observation.ID}}}).WithID()
	witness, err := fixture.worker.core.inspectGitDestinationCandidate(ctx, receipt, observation, fixture.main, history)
	if err != nil || !witness.MatchesObservation(observation) || !witness.MatchesReceipt(receipt) || witness.DestinationBranchID != fixture.main.BranchID {
		t.Fatalf("exact destination binding failed: %+v %v", witness, err)
	}
	for _, scenario := range []string{"origin", "old-target", "new-identity", "unknown-identity", "wrong-ref", "legacy", "delayed-push"} {
		t.Run(scenario, func(t *testing.T) {
			next, selected := observation, fixture.main
			switch scenario {
			case "origin":
				next.GitOrigin += "-other"
			case "old-target":
				selected.Target = fixture.ordinary.Source
			case "new-identity":
				selected.BranchID = "reused-branch-name"
			case "unknown-identity":
				selected.BranchID = ""
			case "wrong-ref":
				next.Ref = "refs/heads/feature"
			case "legacy":
				next.Provider = nil
			case "delayed-push":
				next.Source, next.Delivery = "github-push", "old-delivery"
			}
			next = next.WithID()
			bound, err := fixture.worker.core.inspectGitDestinationCandidate(ctx, receipt, next, selected, history)
			if err == nil || bound.ID != "" {
				t.Fatalf("changed destination bound as authoritative: %+v %v", bound, err)
			}
		})
	}
	missing := receipt
	missing.Heads = []domain.GitHeadReceiptHead{{Ref: "refs/heads/other", Commit: observation.After}}
	other := observation
	other.Ref = "refs/heads/other"
	missing.Heads[0].ObservationID = other.WithID().ID
	missing = missing.WithID()
	if bound, err := fixture.worker.core.inspectGitDestinationCandidate(ctx, missing, observation, fixture.main, history); err == nil || bound.ID != "" {
		t.Fatalf("an observation absent from the read receipt was bound: %+v %v", bound, err)
	}
	later := receipt
	later.Version++
	later = later.WithID()
	again, err := fixture.worker.core.inspectGitDestinationCandidate(ctx, later, observation, fixture.main, history)
	if err != nil || again.ID == witness.ID || again.ObservationID != witness.ObservationID || again.ReadReceiptID == witness.ReadReceiptID {
		t.Fatalf("same head at different read occurrences collapsed: %+v %+v %v", witness, again, err)
	}
}

func TestGitContributionCandidateDoesNotJoinBranchAtExistingMainCommit(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	publication := fixture.publish(t, fixture.digest(t, "", "unmerged branch memory"), true)
	history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	publications, err := domain.NewGitPublicationIndex(ctx, fixture.repo.ID, history)
	if err != nil {
		t.Fatal(err)
	}
	witness, evidence := genericPreparationFixture(t, fixture)
	evidence.trees.(*introductionTreeReader).commits[witness.DestinationHead] = domain.GitCommitTree{
		Commit: witness.DestinationHead, Tree: strings.Repeat("f", 40), Parents: []string{publication.GitAfter},
	}
	before, err := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := fixture.worker.core.inspectGitContributionCandidate(ctx, witness, publication.ID, publication.GitAfter, publications, evidence)
	if err != nil || candidate.possibleIntroduction != publication.GitAfter {
		t.Fatalf("expected code-inclusion candidate, not a context join: %+v %v", candidate, err)
	}
	contributions, err := fixture.storage.ListGitContributions(ctx, fixture.repo.ID)
	if err != nil || len(contributions) != 0 {
		t.Fatal("a branch created at an existing main commit was marked joined", contributions, err)
	}
	memories, err := fixture.storage.ListGitContributionMemories(ctx, fixture.repo.ID)
	if err != nil || len(memories) != 0 {
		t.Fatal("unmerged branch memory was applied", memories, err)
	}
	after, err := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("code inclusion changed context ancestry or memory pointers", err)
	}
	afterHistory, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
	if err != nil || !reflect.DeepEqual(history, afterHistory) {
		t.Fatal("code inclusion fabricated a merge history event", err)
	}
	ref, err := fixture.storage.GetRef(ctx, fixture.repo.ID, domain.RefBranch, fixture.main.Name)
	if err != nil || ref != fixture.main {
		t.Fatal("unmerged branch moved the destination", ref, err)
	}
}

func TestGitContributionCandidateSeparatesOriginalCaptureAndCarriedTip(t *testing.T) {
	for _, scenario := range []string{"same-capture-at-later-code", "missing-carry", "unrelated-code"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newPRMemoryFixture(t)
			ctx := systemTestContext()
			originalMemory := fixture.digest(t, "", "original fork-point memory")
			publication := fixture.publish(t, originalMemory, true)
			history, err := fixture.worker.core.ListHistory(ctx, fixture.repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			sourceHead := strings.Repeat("b", 40)
			carried := fixture.ordinary
			carried.ID, carried.Kind = strings.Repeat("7", 32), "position"
			carried.Source, carried.GitBefore, carried.GitAfter = carried.Target, publication.GitAfter, sourceHead
			carried.Creation = nil
			if scenario != "missing-carry" {
				history = append(history, carried)
			}
			publications, err := domain.NewGitPublicationIndex(ctx, fixture.repo.ID, history)
			if err != nil {
				t.Fatal(err)
			}
			witness, evidence := genericPreparationFixture(t, fixture)
			commits := evidence.trees.(*introductionTreeReader).commits
			commits[sourceHead] = domain.GitCommitTree{Commit: sourceHead, Tree: strings.Repeat("f", 40), Parents: []string{publication.GitAfter}}
			merge := strings.Repeat("d", 40)
			commits[merge] = domain.GitCommitTree{Commit: merge, Tree: strings.Repeat("f", 40), Parents: []string{publication.GitAfter, sourceHead}}
			if scenario == "unrelated-code" {
				commits[sourceHead] = domain.GitCommitTree{Commit: sourceHead, Tree: strings.Repeat("f", 40), Parents: []string{}}
			}
			candidate, err := fixture.worker.core.inspectGitContributionCandidate(ctx, witness, publication.ID, sourceHead, publications, evidence)
			if scenario == "same-capture-at-later-code" {
				if err != nil || candidate.publication != publication || candidate.sourceHead != sourceHead || candidate.publication.GitAfter == sourceHead || candidate.possibleIntroduction != merge || !reflect.DeepEqual(candidate.sourceObservations, []string{fixture.ordinary.ID}) || !reflect.DeepEqual(candidate.carryObservations, []string{carried.ID}) {
					t.Fatalf("capture provenance and carrying tip were conflated: %+v %v", candidate, err)
				}
				introduced, err := locateGitContributionInRange(ctx, evidence, sourceHead, publication.GitAfter, witness.DestinationHead)
				if err != nil || introduced != merge {
					t.Fatalf("new source tip was not introduced after the existing fork point: %s %v", introduced, err)
				}
				if _, err := locateGitContributionInRange(ctx, evidence, publication.GitAfter, publication.GitAfter, witness.DestinationHead); !errors.Is(err, errGitContributionAlreadyPresent) {
					t.Fatalf("original capture head was incorrectly new to main: %v", err)
				}
			} else if !errors.Is(err, domain.ErrNotFound) || candidate.publication.ID != "" || candidate.sourceHead != "" || len(candidate.carryObservations) != 0 {
				t.Fatalf("unproven carry produced a successful candidate: %+v %v", candidate, err)
			}
			stored, err := publications.Publication(publication.ID)
			if err != nil || stored != publication || stored.MemoryHash != originalMemory {
				t.Fatal("candidate inspection rewrote original publication or memory", stored, err)
			}
			contributions, err := fixture.storage.ListGitContributions(ctx, fixture.repo.ID)
			if err != nil || len(contributions) != 0 {
				t.Fatal("carry candidate was applied without causal admission", contributions, err)
			}
		})
	}
}
