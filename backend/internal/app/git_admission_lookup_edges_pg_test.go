//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func gitAdmissionLookupEdgeCarry(publication domain.HistoryEvent) domain.HistoryEvent {
	return domain.HistoryEvent{ID: strings.Repeat("7", 32), RepoID: publication.RepoID, Kind: "position",
		BranchID: publication.BranchID, Branch: publication.Branch, LocalBranch: publication.LocalBranch, WorktreeID: publication.WorktreeID,
		Source: publication.Target, Target: publication.Target, GitAfter: strings.Repeat("e", 40), CreatedAt: time.Unix(200, 0).UTC()}
}

func addGitAdmissionLookupEdgeWitnesses(t *testing.T, storage *gitContributionApplyPGStore, publication domain.HistoryEvent) {
	t.Helper()
	if err := storage.WithinRepository(systemTestContext(), domain.ContentHash(publication.RepoID), func(write context.Context) error {
		for index := range domain.MaxGitAdmissionWitnesses + 1 {
			event := domain.HistoryEvent{ID: fmt.Sprintf("%032x", index+1), RepoID: publication.RepoID, Kind: "position",
				BranchID: publication.BranchID, Branch: publication.Branch, LocalBranch: publication.LocalBranch, WorktreeID: publication.WorktreeID,
				Source: publication.Target, Target: publication.Target, GitAfter: publication.GitAfter, CreatedAt: time.Unix(250, 0).UTC()}
			if err := storage.ApplyHistoryEvent(write, event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGGitAdmissionLookupCarriedHeadReconcilesEndToEnd(t *testing.T) {
	scans, fixture, storage, reader, publications := admissionBatchReconcileFixture(t, 1)
	ctx := systemTestContext()
	publication := publications[0]
	carry := gitAdmissionLookupEdgeCarry(publication)
	if err := fixture.worker.core.RecordHistory(ctx, carry); err != nil {
		t.Fatal(err)
	}
	storage.trees[carry.GitAfter] = domain.GitCommitTree{Commit: carry.GitAfter, Tree: strings.Repeat("f", 40), Parents: []string{publication.GitAfter}}
	reader.headPages[1][0].Commit = carry.GitAfter
	guard := &admissionLookupGuardPG{gitContributionApplyPGStore: &gitContributionApplyPGStore{admissionBatchPG: storage}}
	fixture.worker.core.meta = guard
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
	if err != nil || len(admissions) != 1 {
		t.Fatal("hydrated carry did not survive admission writer validation", admissions, err)
	}
	admission := admissions[0]
	if admission.Validate() != nil || admission.PublicationID != publication.ID || admission.Capture != publication.Target || admission.OriginalHead != publication.GitAfter || admission.SourceHead != carry.GitAfter || admission.SourceHead == admission.OriginalHead {
		t.Fatal("carried admission changed original provenance", admission)
	}
	if !slices.Equal(admission.OrdinaryObservationIDs, fixture.frozen.input.OrdinaryObservationIDs) || !slices.Equal(admission.CarryObservationIDs, []string{carry.ID}) {
		t.Fatal("carried admission did not retain distinct immutable witnesses", admission)
	}
	if guard.witnessRows != 2 || guard.ownerReads != 1 {
		t.Fatal("carried admission bypassed bounded hydration", guard.witnessRows, guard.ownerReads)
	}
	receipts, err := storage.ListGitHeadReceipts(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 10)
	if err != nil || len(receipts) != 1 || !admission.MatchesBaseline(receipts[0]) || admission.AfterReadVersion != receipts[0].Version {
		t.Fatal("carried admission lost its exact baseline receipt and watermark", receipts, err)
	}
	heads, err := storage.ListGitContextHeads(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, admission.Destination.DestinationBranchID, 0, 10)
	if err != nil || len(heads) != 1 || heads[0].Destination.ReadReceiptID != receipts[0].ID || heads[0].ContextTarget != fixture.frozen.destination.Target {
		t.Fatal("hydrated writer lost the pre-provider destination binding", heads, err)
	}
	match, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || match.Version != 1 || match.ThroughPublicationID != "" || match.AfterPublicationID != "" || match.AfterDestination != "" {
		t.Fatal("carried admission did not finish its finite matching sweep", match, err)
	}
	stored, err := storage.GetGitHistoryEvent(ctx, fixture.repo.ID, publication.ID)
	if err != nil || !reflect.DeepEqual(stored, publication) {
		t.Fatal("hydration rewrote the accepted publication", stored, err)
	}
	for _, expected := range []domain.Ref{fixture.frozen.source, fixture.frozen.destination} {
		actual, err := storage.GetRef(ctx, fixture.repo.ID, expected.Kind, expected.Name)
		if err != nil || actual != expected {
			t.Fatal("admission moved a context ref", actual, err)
		}
	}
	contributions, err := storage.ListGitContributions(ctx, fixture.repo.ID)
	if err != nil || len(contributions) != 0 {
		t.Fatal("baseline admission claimed an integration", contributions, err)
	}
}

func TestPGGitAdmissionLookupLateCarrySuppressesHeadsAndAdmissions(t *testing.T) {
	scans, fixture, storage, reader, publications := admissionBatchReconcileFixture(t, 1)
	ctx := systemTestContext()
	carry := gitAdmissionLookupEdgeCarry(publications[0])
	storage.trees[carry.GitAfter] = domain.GitCommitTree{Commit: carry.GitAfter, Tree: strings.Repeat("f", 40), Parents: []string{publications[0].GitAfter}}
	reader.headPages[1][0].Commit = carry.GitAfter
	guard := &admissionLookupGuardPG{gitContributionApplyPGStore: &gitContributionApplyPGStore{admissionBatchPG: storage}}
	fixture.worker.core.meta = guard
	inserted := false
	reader.duringRead = func(read context.Context) {
		reader.duringRead = nil
		before, err := storage.RepositoryRevision(read, fixture.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		generation, err := storage.ReadRepositoryMetadataGeneration(read, fixture.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.ApplyHistoryEvent(read, carry); err != nil {
			t.Fatal(err)
		}
		after, err := storage.RepositoryRevision(read, fixture.repo.ID)
		if err != nil || after != before {
			t.Fatal("late carry must not change Graph/Evidence revisions", before, after, err)
		}
		changed, err := storage.ReadRepositoryMetadataGeneration(read, fixture.repo.ID)
		if err != nil || changed.Epoch != generation.Epoch || changed.Sequence <= generation.Sequence {
			t.Fatal("late carry did not advance the metadata-only fence", generation, changed, err)
		}
		inserted = true
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("provider read did not exercise the late carry mutation")
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
	if err != nil || len(admissions) != 0 {
		t.Fatal("post-provider carry retroactively qualified an admission", admissions, err)
	}
	for _, identity := range []string{fixture.frozen.source.BranchID, fixture.frozen.destination.BranchID} {
		heads, err := storage.ListGitContextHeads(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, identity, 0, 10)
		if err != nil || len(heads) != 0 {
			t.Fatal("stale generation persisted a bound context head", identity, heads, err)
		}
	}
	receipts, err := storage.ListGitHeadReceipts(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 0, 10)
	if err != nil || len(receipts) != 1 {
		t.Fatal("stale semantic preparation lost the authorized receipt", receipts, err)
	}
	observed, found := receipts[0].SourceHead("refs/heads/feature")
	if !found || observed.Commit != carry.GitAfter {
		t.Fatal("durable receipt lost the actual source read", observed)
	}
	claim, err := storage.GetGitHeadScan(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL)
	if err != nil || claim.State != "completed" || claim.Version != receipts[0].Version {
		t.Fatal("stale hydration prevented normal head-scan completion", claim, err)
	}
	match, err := storage.GetGitAdmissionMatch(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, fixture.authorization.provider, 1)
	if err != nil || match.Version != 1 || match.ThroughPublicationID != "" || match.AfterPublicationID != "" || match.AfterDestination != "" {
		t.Fatal("stale hydration did not durably advance the finite cursor", match, err)
	}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err = storage.ListGitContributionAdmissions(ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, "", 10)
	if err != nil || len(admissions) != 1 || admissions[0].Destination.ReadVersion <= receipts[0].Version || admissions[0].OriginalHead != publications[0].GitAfter || admissions[0].SourceHead != carry.GitAfter {
		t.Fatal("fresh provider read could not admit previously deferred carry", admissions, err)
	}
}

func TestPGGitAdmissionLookupForeignOwnerOutsideWitnessWindow(t *testing.T) {
	fixture := newGitContributionApplyPGFixture(t, "empty")
	fixture.observeIntegration(t, "main", true)
	ctx := systemTestContext()
	addGitAdmissionLookupEdgeWitnesses(t, fixture.storage, fixture.publication)
	foreign := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: string(fixture.basis.repo.ID), Kind: "position",
		Branch: "foreign-source", BranchID: "foreign-incarnation", WorktreeID: strings.Repeat("9", 32),
		Source: fixture.basis.frozen.destination.Target, Target: fixture.basis.frozen.destination.Target, GitAfter: fixture.admission.SourceHead, CreatedAt: time.Unix(300, 0).UTC()}
	if foreign.Target == fixture.publication.Target || foreign.WorktreeID == fixture.publication.WorktreeID {
		t.Fatal("foreign owner fixture must differ in capture and worktree")
	}
	if err := fixture.storage.ApplyHistoryEvent(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	query := domain.GitPublicationWitnessQuery{Publication: fixture.publication, GitHead: fixture.admission.SourceHead}
	witnesses, err := fixture.storage.ListGitPublicationWitnesses(ctx, fixture.basis.repo.ID, query)
	if err != nil || len(witnesses) != domain.MaxGitAdmissionWitnesses || query.Matches(foreign) {
		t.Fatal("fixture did not separate bounded witnesses from foreign ownership", len(witnesses), err)
	}
	for _, witness := range witnesses {
		if witness.ID >= foreign.ID || witness.BranchID != fixture.publication.BranchID {
			t.Fatal("foreign owner was not outside the witness window", witness)
		}
	}
	guard := &admissionLookupGuardPG{gitContributionApplyPGStore: fixture.storage}
	fixture.scans.core.meta = guard
	before := fixture.state(t, ctx)
	if _, err := fixture.scans.core.freezeGitAdmission(ctx, fixture.basis.repo.ID, fixture.publication.ID, fixture.admission.SourceHead,
		fixture.admission.Destination.DestinationBranch, fixture.admission.Destination.DestinationBranchID); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("bounded publication witnesses concealed foreign ownership during admission", err)
	}
	if _, err := fixture.scans.prepareGitContribution(ctx, fixture.admission, fixture.head); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("historical witness lookup concealed foreign ownership before application", err)
	}
	if guard.ownerReads != 2 || !reflect.DeepEqual(before, fixture.state(t, ctx)) {
		t.Fatal("foreign-owner checks were skipped or rejection changed semantic state", guard.ownerReads)
	}
}

func TestPGGitAdmissionLookupSupplementFrozenIDsSurviveWitnessWindow(t *testing.T) {
	fixture, storage := newSupplementPGFixture(t)
	publication := publishGitSupplement(t, fixture)
	ctx := systemTestContext()
	addGitAdmissionLookupEdgeWitnesses(t, fixture.storage, fixture.publication)
	for _, evidence := range []struct {
		head string
		ids  []string
	}{{fixture.admission.OriginalHead, fixture.admission.OrdinaryObservationIDs}, {fixture.admission.SourceHead, fixture.admission.CarryObservationIDs}} {
		witnesses, err := storage.ListGitPublicationWitnesses(ctx, fixture.basis.repo.ID, domain.GitPublicationWitnessQuery{Publication: fixture.publication, GitHead: evidence.head})
		if err != nil || len(witnesses) != domain.MaxGitAdmissionWitnesses || len(evidence.ids) == 0 {
			t.Fatal("supplement fixture did not exhaust the fresh witness window", len(witnesses), err)
		}
		for _, witness := range witnesses {
			if slices.Contains(evidence.ids, witness.ID) {
				t.Fatal("frozen witness is still inside the first 128 IDs", witness.ID)
			}
		}
	}
	guard := &admissionLookupGuardPG{gitContributionApplyPGStore: fixture.storage}
	fixture.scans.core.meta = guard
	prepared := prepareGitSupplementClaim(t, fixture, storage, publication)
	if prepared.snapshot == nil || !prepared.snapshot.targeted || prepared.snapshot.generation == nil {
		t.Fatal("supplement preparation bypassed targeted committed evidence")
	}
	if !reflect.DeepEqual(prepared.admission, fixture.admission) || prepared.proof != fixture.proof || prepared.publication != publication {
		t.Fatal("supplement preparation substituted historical provenance")
	}
	for _, ids := range [][]string{fixture.admission.OrdinaryObservationIDs, fixture.admission.CarryObservationIDs} {
		for _, id := range ids {
			event, found := prepared.snapshot.history[id]
			if !found || event.ID != id || event.Target != fixture.admission.Capture {
				t.Fatal("supplement omitted an exact frozen witness", id)
			}
		}
	}
	wantRows := domain.MaxGitAdmissionWitnesses + len(fixture.admission.OrdinaryObservationIDs) + len(fixture.admission.CarryObservationIDs)
	if guard.witnessRows != wantRows {
		t.Fatal("supplement did not fetch frozen IDs separately from its new publication witnesses", guard.witnessRows, wantRows)
	}
	authorization, err := authorizeGitAdmission(ctx, fixture.basis.worker, fixture.basis.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := fixture.state(t, ctx)
	if err := fixture.scans.applyGitContributionSupplement(ctx, prepared, authorization); err != nil {
		t.Fatal("frozen witnesses displaced by lower IDs blocked the supplement writer", err)
	}
	fixture.scans.core.meta = storage
	assertGitSupplementEffect(t, fixture, before, publication)
	after := fixture.state(t, ctx)
	if err := fixture.scans.processGitContributionSupplements(ctx, fixture.basis.repo.ID, 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, fixture.state(t, ctx)) {
		t.Fatal("completed supplement replay duplicated its pin or evidence revision")
	}
	stored, err := storage.GetGitContributionAdmission(ctx, fixture.basis.repo.ID, fixture.admission.ID)
	if err != nil || !reflect.DeepEqual(stored, fixture.admission) {
		t.Fatal("supplement replaced frozen admission witnesses", stored, err)
	}
}
