//go:build postgres

package app

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitContributionQueryPGFixture struct {
	*gitContributionApplyPGFixture
	proof domain.GitContributionProof
	main  domain.Ref
	base  domain.ContentHash
}

func newGitContributionQueryPGFixture(t *testing.T) *gitContributionQueryPGFixture {
	t.Helper()
	scans, basis, original, reader, _ := admissionBatchReconcileFixture(t, 1)
	storage := &gitContributionApplyPGStore{admissionBatchPG: original}
	basis.worker.core.meta = storage
	ctx := systemTestContext()
	base := basis.frozen.destination.Target
	capture := collaborationSnapshot(t, storage.PostgresStore, basis.repo.ID, "independent pinned original", base)
	source := basis.frozen.source
	source.Target = capture
	if _, err := basis.worker.core.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: basis.repo.ID, Ref: source, Force: true}); err != nil {
		t.Fatal(err)
	}
	ordinary := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: string(basis.repo.ID), Branch: source.Name, BranchID: source.BranchID,
		Kind: "position", Source: capture, Target: capture, GitAfter: strings.Repeat("3", 40), WorktreeID: strings.Repeat("5", 32), CreatedAt: time.Unix(150, 0).UTC()}
	if err := basis.worker.core.RecordHistory(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	applied := &gitContributionApplyPGFixture{scans: scans, basis: basis, storage: storage, reader: reader, publication: prPublication(ordinary)}
	applied.publication.MemoryHash, applied.publication.MemorySource = applied.memory(t, "original immutable pin"), capture
	if err := basis.worker.core.RecordHistory(ctx, applied.publication); err != nil {
		t.Fatal(err)
	}
	storage.trees[ordinary.GitAfter] = domain.GitCommitTree{Commit: ordinary.GitAfter, Tree: strings.Repeat("f", 40), Parents: []string{strings.Repeat("a", 40)}}
	reader.headPages[1] = []outbound.GitHead{{Ref: "refs/heads/feature", Commit: ordinary.GitAfter}, {Ref: "refs/heads/main", Commit: strings.Repeat("b", 40)}}
	if err := scans.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	admissions, err := storage.ListGitContributionAdmissions(ctx, basis.repo.ID, basis.repo.GitRemoteURL, "", 101)
	if err != nil || len(admissions) != 1 || admissions[0].PublicationID != applied.publication.ID {
		t.Fatalf("independent original publication not admitted: %+v %v", admissions, err)
	}
	applied.admission = admissions[0]
	applied.observeIntegration(t, "main", true)
	prepared, authorization := applied.prepare(t)
	proof, err := applied.scans.applyGitContribution(ctx, prepared, authorization)
	if err != nil {
		t.Fatal(err)
	}
	main, err := applied.storage.GetRef(ctx, applied.basis.repo.ID, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &gitContributionQueryPGFixture{gitContributionApplyPGFixture: applied, proof: proof, main: main, base: main.Target}
	continued := collaborationSnapshot(t, applied.storage.PostgresStore, main.RepoID, "destination capture after integration", main.Target)
	selected := strings.Repeat("e", 40)
	applied.storage.trees[selected] = domain.GitCommitTree{Commit: selected, Tree: strings.Repeat("f", 40), Parents: []string{proof.Contribution.IntegrationSHA}}
	fixture.selectMain(t, continued, selected, strings.Repeat("8", 32))
	mutable := applied.memory(t, "unpublished mutable pointer must stay invisible")
	if err := applied.storage.CompareAndSwapSnapshotMemory(ctx, main.RepoID, applied.admission.Capture, "", mutable); err != nil {
		t.Fatal(err)
	}
	applied.reader.offline, applied.reader.headError = true, true
	applied.reader.duringRead = func(_ context.Context) {
		t.Error("public query fetched provider heads")
	}
	return fixture
}

func (fixture *gitContributionQueryPGFixture) selectMain(t *testing.T, target domain.ContentHash, code, eventID string) {
	t.Helper()
	ctx := systemTestContext()
	ref := fixture.main
	ref.Target = target
	if _, err := fixture.basis.worker.core.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: ref.RepoID, Ref: ref, Force: true}); err != nil {
		t.Fatal(err)
	}
	position := domain.HistoryEvent{ID: eventID, RepoID: string(ref.RepoID), Branch: ref.Name, BranchID: ref.BranchID,
		Kind: "position", Source: target, Target: target, GitAfter: code, CreatedAt: time.Unix(400, 0).UTC()}
	if err := fixture.basis.worker.core.RecordHistory(ctx, position); err != nil {
		t.Fatal(err)
	}
	fixture.main = ref
}

func (fixture *gitContributionQueryPGFixture) query(t *testing.T) domain.ContextQueryView {
	t.Helper()
	view, err := fixture.basis.worker.core.QueryContext(systemTestContext(), fixture.basis.repo.ID,
		domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
	if err != nil || view.Inclusion == nil {
		t.Fatalf("public branch-context query failed: %+v %v", view, err)
	}
	ids := make([]domain.ContentHash, 0, len(view.Snapshots))
	for _, snapshot := range view.Snapshots {
		ids = append(ids, snapshot.ID)
	}
	if !slices.Equal(ids, view.Inclusion.SnapshotIDs) {
		t.Fatalf("actual captures disagree with ordered inclusion: %v %+v", ids, view.Inclusion)
	}
	graph, err := fixture.basis.worker.core.GetRepositoryView(systemTestContext(), fixture.basis.repo.ID)
	if err != nil || !reflect.DeepEqual(graph.Graph.BranchContexts["main"], *view.Inclusion) {
		t.Fatalf("graph disagrees with public branch context: %+v %v", graph.Graph.BranchContexts, err)
	}
	return view
}

func (fixture *gitContributionQueryPGFixture) assertMerge(t *testing.T, inclusion *domain.BranchContext, state string, acknowledged bool) {
	t.Helper()
	if inclusion == nil || len(inclusion.Merges) != 1 {
		t.Fatalf("expected one semantic merge: %+v", inclusion)
	}
	merge := inclusion.Merges[0]
	if merge.Source != fixture.admission.Capture || merge.DestinationBranchID != fixture.main.BranchID || merge.MergeSHA != fixture.proof.Contribution.IntegrationSHA || merge.State != state || merge.DeliveryAcknowledged != acknowledged {
		t.Fatalf("wrong contribution identity/selection: %+v", merge)
	}
}

func (fixture *gitContributionQueryPGFixture) assertPinnedMemory(t *testing.T, inclusion *domain.BranchContext, included bool) domain.MemoryProjection {
	t.Helper()
	memory, err := fixture.basis.worker.core.QueryBranchMemory(systemTestContext(), fixture.basis.repo.ID, "main", fixture.main.Target, "")
	if err != nil || !reflect.DeepEqual(memory.Inclusion, inclusion) {
		t.Fatalf("memory inclusion disagrees with branch context: %+v %v", memory, err)
	}
	items, err := effectiveMemoryItems(memory.Digest, "claims")
	if err != nil {
		t.Fatal(err)
	}
	if !included {
		if len(items) != 0 || memory.Found {
			t.Fatalf("excluded contribution supplied memory: %+v", memory)
		}
		return memory
	}
	if !memory.Found || len(items) != 1 || items[0].Text != "original immutable pin" || items[0].SourceSnapshot != fixture.admission.Capture || items[0].Code == nil || items[0].Code.Commit != fixture.admission.OriginalHead {
		t.Fatalf("original pin replaced or duplicated: %+v", memory)
	}
	if len(memory.Digest.Fragments) != 1 || memory.Digest.SnapshotID != fixture.main.Target {
		t.Fatalf("wrong memory provenance/root: %+v", memory.Digest)
	}
	return memory
}

func (fixture *gitContributionQueryPGFixture) assertReadOnly(t *testing.T) func() {
	t.Helper()
	ctx := systemTestContext()
	before := fixture.state(t, ctx)
	history, err := fixture.basis.worker.core.ListHistory(ctx, fixture.basis.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := fixture.storage.ListSnapshots(ctx, fixture.basis.repo.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	headReads, codeReads := len(fixture.reader.headReads), fixture.reader.reads
	return func() {
		t.Helper()
		if after := fixture.state(t, ctx); !reflect.DeepEqual(after, before) {
			t.Fatalf("query mutated application state: before=%+v after=%+v", before, after)
		}
		afterHistory, err := fixture.basis.worker.core.ListHistory(ctx, fixture.basis.repo.ID)
		if err != nil || !reflect.DeepEqual(afterHistory, history) {
			t.Fatalf("query changed history: %v", err)
		}
		afterSnapshots, err := fixture.storage.ListSnapshots(ctx, fixture.basis.repo.ID, "")
		if err != nil || !reflect.DeepEqual(afterSnapshots, snapshots) {
			t.Fatalf("query changed natural/graft ancestry or memory pointers: %v", err)
		}
		if len(fixture.reader.headReads) != headReads || fixture.reader.reads != codeReads {
			t.Fatal("query requested new provider evidence")
		}
	}
}

func TestPGGitContributionQueryOrderedCaptureOriginalPinAndPositions(t *testing.T) {
	fixture := newGitContributionQueryPGFixture(t)
	defer fixture.assertReadOnly(t)()
	view := fixture.query(t)
	fixture.assertMerge(t, view.Inclusion, "included", true)
	want := []domain.ContentHash{fixture.main.Target, fixture.admission.Capture, fixture.base}
	if view.Position != fixture.main.Target || view.Inclusion.CodeCommit != strings.Repeat("e", 40) || !slices.Equal(view.Inclusion.SnapshotIDs, want) || view.Inclusion.Merges[0].Order != 1 || view.Inclusion.Merges[0].PRNumber != 0 {
		t.Fatalf("integration did not order the actual capture: %+v", view)
	}
	fixture.assertPinnedMemory(t, view.Inclusion, true)
	ctx := systemTestContext()
	core, repo := fixture.basis.worker.core, fixture.basis.repo.ID
	positions, err := core.QueryMemoryPositions(ctx, repo, fixture.admission.Capture, fixture.proof.Contribution.ID)
	wantPosition := domain.MemoryCodePosition{EventID: fixture.proof.Contribution.ID, CodeCommit: fixture.proof.Contribution.IntegrationSHA, Branch: "main", Kind: "merge"}
	if err != nil || positions.Reason != "selected_event" || positions.CodeCommit != wantPosition.CodeCommit || !slices.Contains(positions.Options, wantPosition) || len(positions.Options) != 3 {
		t.Fatalf("generic merge browsing position absent: %+v %v", positions, err)
	}
	original, err := core.QueryMemoryPositions(ctx, repo, fixture.admission.Capture, fixture.publication.ID)
	if err != nil || original.Reason != "selected_event" || original.CodeCommit != fixture.admission.OriginalHead {
		t.Fatalf("integration relabeled original publication: %+v %v", original, err)
	}
	ambiguous, err := core.QueryMemoryPositions(ctx, repo, fixture.admission.Capture, "")
	if err != nil || ambiguous.Reason != "ambiguous" || ambiguous.CodeCommit != "" {
		t.Fatalf("unselected browsing position guessed merge code: %+v %v", ambiguous, err)
	}
	unrelated, err := core.QueryMemoryPositions(ctx, repo, fixture.main.Target, fixture.proof.Contribution.ID)
	if err != nil || unrelated.Reason != "unavailable" || unrelated.CodeCommit != "" {
		t.Fatalf("merge association leaked to destination capture: %+v %v", unrelated, err)
	}
}

func TestPGGitContributionQueryResetRetainsAcknowledgementNotInclusion(t *testing.T) {
	fixture := newGitContributionQueryPGFixture(t)
	before := fixture.query(t)
	fixture.assertMerge(t, before.Inclusion, "included", true)
	fixture.selectMain(t, fixture.base, strings.Repeat("b", 40), strings.Repeat("9", 32))
	defer fixture.assertReadOnly(t)()
	view := fixture.query(t)
	fixture.assertMerge(t, view.Inclusion, "not_selected", true)
	if view.Position != fixture.base || !slices.Equal(view.Inclusion.SnapshotIDs, []domain.ContentHash{fixture.base}) || view.Inclusion.CodeCommit != strings.Repeat("b", 40) || view.Inclusion.Merges[0].Order != -1 {
		t.Fatalf("historical acknowledgement restored reset context: %+v", view)
	}
	fixture.assertPinnedMemory(t, view.Inclusion, false)
	positions, err := fixture.basis.worker.core.QueryMemoryPositions(systemTestContext(), fixture.basis.repo.ID, fixture.admission.Capture, fixture.proof.Contribution.ID)
	if err != nil || positions.Reason != "selected_event" || positions.CodeCommit != fixture.proof.Contribution.IntegrationSHA {
		t.Fatalf("reset erased historical browsing association: %+v %v", positions, err)
	}
}

func TestPGGitContributionQueryLateOwnerPreservesAcceptedProof(t *testing.T) {
	fixture := newGitContributionQueryPGFixture(t)
	view := fixture.query(t)
	fixture.assertMerge(t, view.Inclusion, "included", true)
	conflict := domain.HistoryEvent{ID: strings.Repeat("7", 32), RepoID: string(fixture.basis.repo.ID), BranchID: "ambiguous-owner", Branch: "other",
		Kind: "position", Source: fixture.admission.Capture, Target: fixture.admission.Capture, GitAfter: fixture.admission.SourceHead, CreatedAt: time.Unix(500, 0).UTC()}
	if err := fixture.basis.worker.core.RecordHistory(systemTestContext(), conflict); err != nil {
		t.Fatal(err)
	}
	assertUnchanged := fixture.assertReadOnly(t)
	after := fixture.query(t)
	fixture.assertMerge(t, after.Inclusion, "included", true)
	if !reflect.DeepEqual(after.Inclusion, view.Inclusion) {
		t.Fatalf("later owner revoked immutable completion: before=%+v after=%+v", view.Inclusion, after.Inclusion)
	}
	fixture.assertPinnedMemory(t, after.Inclusion, true)
	positions, err := fixture.basis.worker.core.QueryMemoryPositions(systemTestContext(), fixture.basis.repo.ID, fixture.admission.Capture, fixture.proof.Contribution.ID)
	if err != nil || positions.Reason != "selected_event" || positions.CodeCommit != fixture.proof.Contribution.IntegrationSHA {
		t.Fatalf("later owner erased historical merge code: %+v %v", positions, err)
	}
	original, err := fixture.basis.worker.core.QueryMemoryPositions(systemTestContext(), fixture.basis.repo.ID, fixture.admission.Capture, fixture.publication.ID)
	if err != nil || original.Reason != "selected_event" || original.CodeCommit != fixture.admission.OriginalHead {
		t.Fatalf("later owner erased accepted original: %+v %v", original, err)
	}
	assertUnchanged()
	fixture.selectMain(t, fixture.base, strings.Repeat("b", 40), strings.Repeat("9", 32))
	defer fixture.assertReadOnly(t)()
	reset := fixture.query(t)
	fixture.assertMerge(t, reset.Inclusion, "not_selected", true)
	if !slices.Equal(reset.Inclusion.SnapshotIDs, []domain.ContentHash{fixture.base}) {
		t.Fatalf("historical proof restored reset context: %+v", reset.Inclusion)
	}
	fixture.assertPinnedMemory(t, reset.Inclusion, false)
}

func TestPGGitContributionQueryMixedPRDeduplicatesMergeAndMemory(t *testing.T) {
	fixture := newGitContributionQueryPGFixture(t)
	ctx := systemTestContext()
	worker, repo := fixture.basis.worker, fixture.basis.repo
	pr := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: fixture.admission.OriginalHead, MergeSHA: fixture.proof.Contribution.IntegrationSHA}
	if _, err := worker.core.SubmitPRPromotion(ctx, repo.ID, pr); err != nil {
		t.Fatal(err)
	}
	remote := &githubHistoryRemote{GitHubApp: worker.remote, pages: map[string]domain.GitHubPRHistoryPage{"": {
		RepositoryID: 91, HeadRepositoryID: 91, PullRequestID: "PR_7", PullRequestDatabaseID: 701, Number: 7,
		BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeSHA, Merged: true, MergedAt: time.Unix(300, 0).UTC(),
		Rewrites: []domain.GitHubHeadRewrite{{ID: "original-head-restored", Before: strings.Repeat("6", 40), After: pr.HeadSHA, CreatedAt: time.Unix(200, 0).UTC()}},
	}}}
	if err := worker.reconcilePRHistory(ctx, remote, fixture.storage, historyWork(t, worker, fixture.storage, repo)); err != nil {
		t.Fatal(err)
	}
	contributions, err := fixture.storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(contributions) != 1 || contributions[0].Capture != fixture.admission.Capture || contributions[0].DestinationBranchID != fixture.main.BranchID || contributions[0].PR.MergeSHA != fixture.proof.Contribution.IntegrationSHA {
		t.Fatalf("genuine PR contribution did not match Git integration: %+v %v", contributions, err)
	}
	pins, err := fixture.storage.ListPRContributionMemories(ctx, repo.ID)
	if err != nil || len(pins) != 1 || pins[0].PublicationID != fixture.publication.ID || pins[0].MemoryHash != fixture.publication.MemoryHash {
		t.Fatalf("PR application did not pin the same accepted original: %+v %v", pins, err)
	}
	defer fixture.assertReadOnly(t)()
	providerReads := len(remote.calls)
	remote.err = domain.ErrForbidden
	view := fixture.query(t)
	fixture.assertMerge(t, view.Inclusion, "included", true)
	if !slices.Equal(view.Inclusion.SnapshotIDs, []domain.ContentHash{fixture.main.Target, fixture.admission.Capture, fixture.base}) {
		t.Fatalf("mixed ledgers duplicated the ordered capture: %+v", view.Inclusion)
	}
	fixture.assertPinnedMemory(t, view.Inclusion, true)
	graph, err := worker.core.GetRepositoryView(ctx, repo.ID)
	if err != nil || len(graph.Graph.Operations.Merges) != 1 || len(graph.Graph.Integrations) != 1 {
		t.Fatalf("mixed ledgers duplicated graph merge: %+v %v", graph.Graph, err)
	}
	for _, event := range []string{fixture.proof.Contribution.ID, contributions[0].ID} {
		positions, err := worker.core.QueryMemoryPositions(ctx, repo.ID, fixture.admission.Capture, event)
		if err != nil || positions.Reason != "selected_event" || positions.CodeCommit != pr.MergeSHA {
			t.Fatalf("deduplication erased independent event identity: %+v %v", positions, err)
		}
	}
	if len(remote.calls) != providerReads {
		t.Fatal("query re-fetched PR participation evidence")
	}
}
