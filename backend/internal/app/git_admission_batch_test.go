package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func admissionBatchPlanFixture(t *testing.T) (*prMemoryFixture, *frozenGitAdmissionBatch, domain.GitHeadReceipt, *codeEvidence) {
	t.Helper()
	fixture, input, receipt, evidence := admissionPreparationFixture(t)
	snapshot, err := fixture.worker.core.readGitAdmissionSnapshot(systemTestContext(), fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := snapshot.index.Publication(input.input.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	previous := domain.GitAdmissionMatch{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin, Provider: receipt.Provider,
		Page: receipt.Page, Version: 1, ThroughPublicationID: publication.ID}
	next := previous
	next.Version++
	batch := &frozenGitAdmissionBatch{
		claim: domain.GitHeadScan{RepoID: receipt.RepoID, GitOrigin: receipt.GitOrigin, Page: receipt.Page,
			Version: receipt.Version, State: "running", LeaseUntil: time.Unix(500, 0).UTC()},
		authorization: gitAdmissionAuthorization{repo: receipt.RepoID, provider: receipt.Provider, fence: func(context.Context) error { return nil }},
		snapshot:      snapshot, previous: previous, next: next, publications: []domain.HistoryEvent{publication},
	}
	if receipt.Validate() != nil || previous.Validate() != nil || next.Validate() != nil {
		t.Fatal("invalid planner fixture")
	}
	return fixture, batch, receipt, evidence
}

func refreshAdmissionBatchView(t *testing.T, fixture *prMemoryFixture, batch *frozenGitAdmissionBatch) {
	t.Helper()
	snapshot, err := fixture.worker.core.readGitAdmissionSnapshot(systemTestContext(), fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	batch.snapshot = snapshot
}

func admissionBatchDestinations(t *testing.T, fixture *prMemoryFixture, batch *frozenGitAdmissionBatch, receipt domain.GitHeadReceipt, count int) domain.GitHeadReceipt {
	t.Helper()
	heads := []domain.GitHeadReceiptHead{}
	for _, head := range receipt.Heads {
		if head.Ref == "refs/heads/"+fixture.ordinary.Branch {
			heads = append(heads, head)
		}
	}
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("destination/%02d", index)
		ref := domain.Ref{RepoID: fixture.repo.ID, Kind: domain.RefBranch, Name: name,
			BranchID: domain.LegacyContextBranchID(string(fixture.repo.ID), name), Target: fixture.main.Target}
		if err := fixture.storage.CompareAndSwapRef(systemTestContext(), fixture.repo.ID, ref, ""); err != nil {
			t.Fatal(err)
		}
		heads = append(heads, domain.GitHeadReceiptHead{Ref: "refs/heads/" + name, Commit: fixture.ordinary.GitAfter})
	}
	refreshAdmissionBatchView(t, fixture, batch)
	receipt.Heads = heads
	receipt = rebuildAdmissionReceipt(receipt)
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func resumeAdmissionBatchPlan(batch *preparedGitAdmissionBatch) *frozenGitAdmissionBatch {
	next := *batch.frozen
	next.previous, next.next = batch.next, batch.next
	next.next.Version++
	return &next
}

func freshAdmissionBatchEvidence(evidence *codeEvidence) *codeEvidence {
	return &codeEvidence{repo: evidence.repo, origin: evidence.origin, trees: evidence.trees,
		commits: map[string]evidenceValue[domain.GitCommitTree]{}}
}

func TestGitAdmissionBatchPlanSelectsCarriedHeadFromFrozenView(t *testing.T) {
	fixture, frozen, receipt, evidence := admissionBatchPlanFixture(t)
	publication := frozen.publications[0]
	before := frozen.next
	batch, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, evidence)
	if err != nil || batch == nil || len(batch.admissions) != 1 {
		t.Fatalf("carried head was not planned: %+v %v", batch, err)
	}
	item := batch.admissions[0]
	admission := item.admission
	if admission.PublicationID != publication.ID || admission.Capture != publication.Target || admission.OriginalHead != publication.GitAfter ||
		admission.SourceHead != strings.Repeat("b", 40) || admission.Destination.DestinationHead != publication.GitAfter ||
		!reflect.DeepEqual(admission.OrdinaryObservationIDs, []string{fixture.ordinary.ID}) ||
		!reflect.DeepEqual(admission.CarryObservationIDs, []string{strings.Repeat("7", 32)}) ||
		admission.SourceAnchorID != fixture.ordinary.ID || admission.Destination.DestinationBranchID != fixture.main.BranchID ||
		admission.GraphRevision != frozen.snapshot.revision.Graph || admission.EvidenceRevision != frozen.snapshot.revision.Evidence ||
		!admission.MatchesBaseline(receipt) || !sameGitAdmissionInput(item.frozen.input, admission) {
		t.Fatalf("response selection changed frozen provenance: %+v", admission)
	}
	if frozen.next != before || !reflect.DeepEqual(frozen.publications[0], publication) {
		t.Fatal("planning mutated frozen inputs")
	}
	admission.CarryObservationIDs[0] = strings.Repeat("f", 32)
	carried, err := frozen.snapshot.index.CarryObservationIDs(publication.ID, strings.Repeat("b", 40))
	if err != nil || !reflect.DeepEqual(carried, []string{strings.Repeat("7", 32)}) {
		t.Fatal("planned evidence aliases the frozen index", carried, err)
	}
}

func TestGitAdmissionBatchPlanBoundsEquivalentWitnessesWithoutStarvation(t *testing.T) {
	fixture, frozen, receipt, evidence := admissionBatchPlanFixture(t)
	history, err := fixture.worker.core.ListHistory(systemTestContext(), fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for number := 0; number < domain.MaxGitAdmissionWitnesses; number++ {
		ordinary := fixture.ordinary
		ordinary.ID, ordinary.Kind, ordinary.Source, ordinary.Creation = fmt.Sprintf("10%030x", number), "position", ordinary.Target, nil
		history = append(history, ordinary)
		ordinary.ID, ordinary.GitAfter = fmt.Sprintf("20%030x", number), strings.Repeat("b", 40)
		history = append(history, ordinary)
	}
	second := frozen.publications[0]
	second.ID = strings.Repeat("f", 32)
	history = append(history, second)
	frozen.publications = append(frozen.publications, second)
	frozen.next.ThroughPublicationID = second.ID
	frozen.snapshot.index, err = domain.NewGitPublicationIndex(systemTestContext(), fixture.repo.ID, history)
	if err != nil {
		t.Fatal(err)
	}
	original := frozen.snapshot.index.ObservationIDs(frozen.publications[0].ID)
	carry, err := frozen.snapshot.index.CarryObservationIDs(frozen.publications[0].ID, strings.Repeat("b", 40))
	if err != nil || len(original) != domain.MaxGitAdmissionWitnesses+1 || len(carry) != domain.MaxGitAdmissionWitnesses+1 {
		t.Fatal("invalid witness limit fixture", len(original), len(carry), err)
	}
	batch, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, evidence)
	if err != nil || batch == nil || len(batch.admissions) != 2 {
		t.Fatal("repeated witnesses blocked eligible successors", batch, err)
	}
	for _, item := range batch.admissions {
		if item.admission.Validate() != nil || !reflect.DeepEqual(item.admission.OrdinaryObservationIDs, original[:domain.MaxGitAdmissionWitnesses]) || !reflect.DeepEqual(item.admission.CarryObservationIDs, carry[:domain.MaxGitAdmissionWitnesses]) {
			t.Fatal("bounded selection fabricated or reordered witnesses", item.admission)
		}
	}
	if len(frozen.snapshot.index.ObservationIDs(second.ID)) != domain.MaxGitAdmissionWitnesses+1 || batch.next.ThroughPublicationID != "" {
		t.Fatal("bounded admission removed original witnesses or stalled sweep")
	}
}

func TestGitAdmissionBatchPlanDoesNotUseLateOrdinaryEvidence(t *testing.T) {
	for _, scenario := range []string{"new carried head", "additional carry witness"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, frozen, receipt, evidence := admissionBatchPlanFixture(t)
			late := fixture.ordinary
			late.ID, late.Kind, late.Source, late.Creation = strings.Repeat("6", 32), "position", late.Target, nil
			late.GitBefore, late.GitAfter = fixture.ordinary.GitAfter, strings.Repeat("b", 40)
			if scenario == "new carried head" {
				late.GitAfter = strings.Repeat("c", 40)
				evidence.trees.(*introductionTreeReader).commits[late.GitAfter] = domain.GitCommitTree{
					Commit: late.GitAfter, Tree: strings.Repeat("f", 40), Parents: []string{strings.Repeat("b", 40)},
				}
			}
			if err := fixture.worker.core.RecordHistory(systemTestContext(), late); err != nil {
				t.Fatal(err)
			}
			for index := range receipt.Heads {
				if receipt.Heads[index].Ref == "refs/heads/"+fixture.ordinary.Branch {
					receipt.Heads[index].Commit = late.GitAfter
				}
			}
			receipt = rebuildAdmissionReceipt(receipt)
			batch, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, evidence)
			if err != nil || batch == nil {
				t.Fatal(batch, err)
			}
			if scenario == "new carried head" {
				if len(batch.admissions) != 0 {
					t.Fatal("post-read ordinary evidence established a new source association")
				}
			} else if len(batch.admissions) != 1 || !reflect.DeepEqual(batch.admissions[0].admission.CarryObservationIDs, []string{strings.Repeat("7", 32)}) {
				t.Fatal("post-read witness enlarged frozen evidence", batch.admissions)
			}
			current := *frozen
			refreshAdmissionBatchView(t, fixture, &current)
			fresh, err := planGitAdmissionBatch(systemTestContext(), &current, receipt, freshAdmissionBatchEvidence(evidence))
			if err != nil || fresh == nil || len(fresh.admissions) != 1 {
				t.Fatalf("next frozen view did not see accepted evidence: %+v %v", fresh, err)
			}
			if fresh.admissions[0].admission.CarryObservationIDs[0] != late.ID {
				t.Fatal("fresh view omitted the new exact ordinary witness")
			}
		})
	}
}

func TestGitAdmissionBatchPlanResumesBoundedDestinationFanout(t *testing.T) {
	fixture, frozen, receipt, evidence := admissionBatchPlanFixture(t)
	receipt = admissionBatchDestinations(t, fixture, frozen, receipt, gitAdmissionBatchLimit+2)
	first, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, evidence)
	if err != nil || first == nil || len(first.admissions) != gitAdmissionBatchLimit {
		t.Fatalf("first fanout was not bounded: %+v %v", first, err)
	}
	if first.next.AfterPublicationID != "" || first.next.AfterDestination != "destination/07" || first.next.ThroughPublicationID != frozen.next.ThroughPublicationID || first.next.Validate() != nil {
		t.Fatalf("partial publication cursor skipped unvisited destinations: %+v", first.next)
	}
	second, err := planGitAdmissionBatch(systemTestContext(), resumeAdmissionBatchPlan(first), receipt, freshAdmissionBatchEvidence(evidence))
	if err != nil || second == nil || len(second.admissions) != 2 {
		t.Fatalf("destination cursor did not resume: %+v %v", second, err)
	}
	seen := map[string]bool{}
	for _, batch := range []*preparedGitAdmissionBatch{first, second} {
		for _, item := range batch.admissions {
			name := item.admission.Destination.DestinationBranch
			if seen[name] || item.admission.PublicationID != frozen.publications[0].ID || !item.admission.MatchesBaseline(receipt) {
				t.Fatal("resumption duplicated or changed a tuple", item.admission)
			}
			seen[name] = true
		}
	}
	for index := 0; index < gitAdmissionBatchLimit+2; index++ {
		if !seen[fmt.Sprintf("destination/%02d", index)] {
			t.Fatal("destination starved", index)
		}
	}
	if second.next.ThroughPublicationID != "" || second.next.AfterPublicationID != "" || second.next.AfterDestination != "" || second.next.Validate() != nil {
		t.Fatal("exhausted fanout retained a completed sweep", second.next)
	}
}

func TestGitAdmissionBatchPlanDeferredPublicationDoesNotBlockNext(t *testing.T) {
	for _, scenario := range []string{"incomplete source", "absent carry ancestry", "missing carry witness", "missing source head", "already present"} {
		t.Run(scenario, func(t *testing.T) {
			fixture, frozen, receipt, evidence := admissionBatchPlanFixture(t)
			ordinary := fixture.ordinary
			ordinary.ID, ordinary.Kind, ordinary.Source, ordinary.Creation = fmt.Sprintf("%032x", 20000), "position", ordinary.Target, nil
			ordinary.Branch, ordinary.LocalBranch = "deferred-source", "deferred-source"
			ordinary.BranchID = domain.LegacyContextBranchID(string(fixture.repo.ID), ordinary.Branch)
			ordinary.GitBefore, ordinary.GitAfter = "", strings.Repeat("c", 40)
			if scenario == "already present" {
				ordinary.GitAfter = strings.Repeat("1", 40)
			}
			if scenario == "absent carry ancestry" {
				ordinary.GitAfter = strings.Repeat("9", 40)
			}
			ref := domain.Ref{RepoID: fixture.repo.ID, Kind: domain.RefBranch, Name: ordinary.Branch, BranchID: ordinary.BranchID, Target: ordinary.Target}
			if err := fixture.storage.CompareAndSwapRef(systemTestContext(), fixture.repo.ID, ref, ""); err != nil {
				t.Fatal(err)
			}
			if err := fixture.worker.core.RecordHistory(systemTestContext(), ordinary); err != nil {
				t.Fatal(err)
			}
			publication := prPublication(ordinary)
			publication.ID = fmt.Sprintf("%032x", 1)
			if err := fixture.worker.core.RecordHistory(systemTestContext(), publication); err != nil {
				t.Fatal(err)
			}
			sourceHead := ordinary.GitAfter
			if scenario == "missing carry witness" {
				sourceHead = strings.Repeat("9", 40)
				evidence.trees.(*introductionTreeReader).commits[sourceHead] = domain.GitCommitTree{
					Commit: sourceHead, Tree: strings.Repeat("f", 40), Parents: []string{strings.Repeat("1", 40)},
				}
			}
			if scenario == "absent carry ancestry" {
				carry := ordinary
				carry.ID, carry.GitAfter = fmt.Sprintf("%032x", 20001), strings.Repeat("c", 40)
				if err := fixture.worker.core.RecordHistory(systemTestContext(), carry); err != nil {
					t.Fatal(err)
				}
				sourceHead = carry.GitAfter
				reader := evidence.trees.(*introductionTreeReader)
				reader.commits[ordinary.GitAfter] = domain.GitCommitTree{Commit: ordinary.GitAfter, Tree: strings.Repeat("f", 40), Parents: []string{}}
				reader.commits[sourceHead] = domain.GitCommitTree{Commit: sourceHead, Tree: strings.Repeat("f", 40), Parents: []string{strings.Repeat("1", 40)}}
			}
			if scenario != "missing source head" {
				receipt.Heads = append(receipt.Heads, domain.GitHeadReceiptHead{Ref: "refs/heads/" + ordinary.Branch, Commit: sourceHead})
			}
			receipt = rebuildAdmissionReceipt(receipt)
			later := fixture.publish(t, "", true)
			frozen.previous.ThroughPublicationID, frozen.next.ThroughPublicationID = later.ID, later.ID
			refreshAdmissionBatchView(t, fixture, frozen)
			ready := frozen.publications[0]
			frozen.publications, frozen.more = []domain.HistoryEvent{publication, ready}, true
			batch, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, evidence)
			if err != nil || batch == nil {
				t.Fatalf("deferred publication blocked planning: %+v %v", batch, err)
			}
			found := false
			for _, item := range batch.admissions {
				if item.admission.PublicationID != ready.ID {
					t.Fatal("deferred publication became admitted", item.admission)
				}
				found = found || item.admission.Destination.DestinationBranch == fixture.main.Name
			}
			if !found || batch.next.AfterPublicationID != ready.ID || batch.next.AfterDestination != "" || batch.next.ThroughPublicationID != frozen.next.ThroughPublicationID {
				t.Fatalf("ready successor starved or cursor skipped the next page: %+v", batch)
			}
		})
	}
}

func TestGitAdmissionBatchPlanEvidenceLimitAdvancesCurrentPair(t *testing.T) {
	fixture, frozen, receipt, evidence := admissionBatchPlanFixture(t)
	receipt = admissionBatchDestinations(t, fixture, frozen, receipt, gitAdmissionBatchLimit+2)
	for attempt := 0; attempt < 3; attempt++ {
		limited := freshAdmissionBatchEvidence(evidence)
		limited.reads = maxCodeEvidenceReads
		batch, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, limited)
		if err != nil || batch == nil || len(batch.admissions) != 0 || !limited.limited {
			t.Fatalf("evidence limit lost resumable planning: %+v %v", batch, err)
		}
		if batch.next.AfterPublicationID != "" || batch.next.AfterDestination != fmt.Sprintf("destination/%02d", attempt) || batch.next.ThroughPublicationID != frozen.next.ThroughPublicationID || batch.next.Validate() != nil {
			t.Fatalf("budget exhaustion retried the same prefix or skipped unvisited tuples: %+v", batch.next)
		}
		frozen = resumeAdmissionBatchPlan(batch)
	}
	batch, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, freshAdmissionBatchEvidence(evidence))
	if err != nil || batch == nil || len(batch.admissions) != gitAdmissionBatchLimit-1 {
		t.Fatalf("later destinations starved after repeated evidence limits: %+v %v", batch, err)
	}
	for index, item := range batch.admissions {
		if item.admission.Destination.DestinationBranch != fmt.Sprintf("destination/%02d", index+3) {
			t.Fatal("resumption restarted the failed prefix", item.admission)
		}
	}
	if batch.next.ThroughPublicationID != "" || batch.next.AfterPublicationID != "" || batch.next.AfterDestination != "" {
		t.Fatal("finite sweep did not wrap for deferred pairs", batch.next)
	}
}

func TestGitAdmissionBatchPlanDoesNotCombineProviderPages(t *testing.T) {
	fixture, frozen, receipt, evidence := admissionBatchPlanFixture(t)
	for page, branch := range []string{fixture.ordinary.Branch, fixture.main.Name} {
		partial := receipt
		partial.Heads = nil
		partial.Page, partial.Version, partial.More = page+1, receipt.Version+int64(page), page == 0
		for _, head := range receipt.Heads {
			if head.Ref == "refs/heads/"+branch {
				partial.Heads = append(partial.Heads, head)
			}
		}
		partial = rebuildAdmissionReceipt(partial)
		if err := partial.Validate(); err != nil {
			t.Fatal(err)
		}
		scoped := *frozen
		scoped.claim.Page, scoped.claim.Version = partial.Page, partial.Version
		scoped.previous.Page, scoped.next.Page = partial.Page, partial.Page
		batch, err := planGitAdmissionBatch(systemTestContext(), &scoped, partial, freshAdmissionBatchEvidence(evidence))
		if err != nil || batch == nil || len(batch.admissions) != 0 || batch.next.Page != partial.Page {
			t.Fatalf("separate page supplied missing membership: %+v %v", batch, err)
		}
	}
}

func TestGitAdmissionBatchPlanExhaustionClearsFiniteUpper(t *testing.T) {
	for _, empty := range []bool{false, true} {
		_, frozen, receipt, evidence := admissionBatchPlanFixture(t)
		if empty {
			frozen.publications, frozen.snapshot = nil, nil
			frozen.previous.AfterPublicationID = frozen.previous.ThroughPublicationID
			frozen.next.AfterPublicationID = frozen.next.ThroughPublicationID
		}
		before := frozen.next
		batch, err := planGitAdmissionBatch(systemTestContext(), frozen, receipt, evidence)
		if err != nil || batch == nil {
			t.Fatal(batch, err)
		}
		want := before
		want.ThroughPublicationID, want.AfterPublicationID, want.AfterDestination = "", "", ""
		if batch.next != want || batch.next.Validate() != nil || frozen.next != before {
			t.Fatalf("exhaustion changed scope/version or retained the old upper bound: %+v", batch.next)
		}
		if empty && len(batch.admissions) != 0 {
			t.Fatal("empty sweep emitted an admission")
		}
	}
}

func TestGitAdmissionBatchPlanCancellationDoesNotPublishProgress(t *testing.T) {
	for _, scenario := range []string{"before planning", "empty sweep", "during traversal"} {
		t.Run(scenario, func(t *testing.T) {
			_, frozen, receipt, evidence := admissionBatchPlanFixture(t)
			ctx, cancel := context.WithCancel(systemTestContext())
			defer cancel()
			if scenario == "during traversal" {
				evidence.trees.(*introductionTreeReader).onRead = func(string) { cancel() }
			} else {
				cancel()
			}
			if scenario == "empty sweep" {
				frozen.publications, frozen.snapshot = nil, nil
			}
			before := frozen.next
			batch, err := planGitAdmissionBatch(ctx, frozen, receipt, evidence)
			if !errors.Is(err, context.Canceled) || batch != nil || frozen.next != before {
				t.Fatalf("cancellation published progress: %+v %v", batch, err)
			}
		})
	}
}
