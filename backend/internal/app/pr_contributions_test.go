package app

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func contributionFixture(t *testing.T) (*GitHubConnections, *store.FSStore, *githubHistoryRemote, domain.Repo, domain.HistoryEvent, domain.Ref) {
	t.Helper()
	worker, storage, remote, repo, ordinary := githubHistoryFixture(t)
	ctx := systemTestContext()
	current, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	latest := prSnapshot(t, storage, repo.ID, "LATEST MAIN WORK", current.Target)
	ref := current
	ref.Target, ref.BranchID = latest, domain.LegacyContextBranchID(string(repo.ID), "main")
	if err := storage.CompareAndSwapRef(ctx, repo.ID, ref, current.Target); err != nil {
		t.Fatal(err)
	}
	position := domain.HistoryEvent{ID: strings.Repeat("8", 32), RepoID: string(repo.ID), BranchID: ref.BranchID, Branch: "main", Kind: "position", Source: latest, Target: latest, GitAfter: strings.Repeat("e", 40), CreatedAt: time.Unix(400, 0).UTC()}
	if err := worker.core.RecordHistory(ctx, position); err != nil {
		t.Fatal(err)
	}
	reader := &scanReader{deltas: map[string]domain.GitCommitDelta{}}
	prior := ""
	for _, character := range []string{"1", "d", "e"} {
		code := strings.Repeat(character, 40)
		parents := []string{}
		if prior != "" {
			parents = append(parents, prior)
		}
		reader.deltas[code] = domain.GitCommitDelta{Commit: code, Parent: prior, Parents: parents, Complete: true, Changes: []domain.GitPathChange{}}
		prior = code
	}
	scans, err := NewGitScans(worker.core, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scans.ObservePush(ctx, repo.GitRemoteURL, "refs/heads/main", "", position.GitAfter, false, "main-latest"); err != nil {
		t.Fatal(err)
	}
	if err := scans.Process(ctx, 100); err != nil {
		t.Fatal(err)
	}
	return worker, storage, remote, repo, ordinary, ref
}

func TestInvalidatedPRContributionStillFencesCompletedReplay(t *testing.T) {
	worker, storage, remote, repo, ordinary, main := contributionFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	final := ordinary
	final.ID, final.Kind, final.GitAfter = strings.Repeat("4", 32), "position", strings.Repeat("c", 40)
	final.Source, final.Target, final.CreatedAt = ordinary.Target, prSnapshot(t, storage, repo.ID, "FINAL HEAD CAPTURE", ordinary.Target), time.Unix(260, 0).UTC()
	if err := worker.core.RecordHistory(ctx, final); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, worker.core, final)
	conflict := ordinary
	conflict.ID, conflict.BranchID, conflict.Kind = strings.Repeat("7", 32), "ambiguous-original", "position"
	if err := worker.core.RecordHistory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	rewound := main
	rewound.Target = ordinary.Source
	if err := storage.CompareAndSwapRef(ctx, repo.ID, rewound, main.Target); err != nil {
		t.Fatal(err)
	}
	position := domain.HistoryEvent{ID: strings.Repeat("9", 32), RepoID: string(repo.ID), BranchID: main.BranchID, Branch: "main", Kind: "position", Source: rewound.Target, Target: rewound.Target, GitAfter: strings.Repeat("1", 40), CreatedAt: time.Unix(600, 0).UTC()}
	if err := worker.core.RecordHistory(ctx, position); err != nil {
		t.Fatal(err)
	}
	job, err := storage.GetPRJob(ctx, repo.ID, domain.PRPromotionID(repo.ID, 7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.core.DeliverPRPromotion(ctx, repo.ID, job.PR); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("invalid completion did not fence fallback: %v", err)
	}
	ref, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if err != nil || ref != rewound {
		t.Fatalf("replay undid rewind: %+v %v", ref, err)
	}
}

func TestPRContributionAlternateHeadSurvivesIndependentAmbiguity(t *testing.T) {
	worker, storage, remote, repo, ordinary, _ := contributionFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	alternative := ordinary
	alternative.ID, alternative.Kind, alternative.GitAfter = strings.Repeat("3", 32), "position", strings.Repeat("b", 40)
	alternative.Source, alternative.CreatedAt = alternative.Target, time.Unix(240, 0).UTC()
	if err := worker.core.RecordHistory(ctx, alternative); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, worker.core, alternative)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(records) != 2 {
		t.Fatalf("expected independent head evidence: %+v %v", records, err)
	}
	conflict := ordinary
	conflict.ID, conflict.BranchID, conflict.Kind = strings.Repeat("7", 32), "ambiguous-original", "position"
	if err := worker.core.RecordHistory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	view, err := worker.core.GetRepositoryView(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	merges := view.Graph.BranchContexts["main"].Merges
	if len(merges) != 1 || merges[0].State != "included" || merges[0].Source != ordinary.Target {
		t.Fatalf("valid alternative lost: %+v", merges)
	}
}

func TestPRContributionsLateRebasePreservesMainAndSharedQueries(t *testing.T) {
	worker, storage, remote, repo, ordinary, main := contributionFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	historyBefore, _ := worker.core.ListHistory(ctx, repo.ID)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(records) != 1 || records[0].OriginalHead != ordinary.GitAfter || records[0].Capture != ordinary.Target {
		t.Fatalf("contributions: %+v %v", records, err)
	}
	after, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if err != nil || after != main {
		t.Fatalf("main moved: %+v %+v %v", main, after, err)
	}
	historyAfter, _ := worker.core.ListHistory(ctx, repo.ID)
	if !reflect.DeepEqual(historyBefore, historyAfter) {
		t.Fatal("contribution manufactured history")
	}
	jobs, err := worker.core.ListPRPromotions(ctx, repo.ID)
	if err != nil || jobs[0].State != "completed" {
		t.Fatalf("job not completed: %+v %v", jobs, err)
	}
	full, err := worker.core.GetRepositoryView(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	inclusion := full.Graph.BranchContexts["main"]
	if inclusion.SnapshotID != main.Target || inclusion.CodeCommit != strings.Repeat("e", 40) || len(inclusion.Merges) != 1 || inclusion.Merges[0].State != "included" || !slices.Contains(inclusion.SnapshotIDs, ordinary.Target) {
		t.Fatalf("missing integration: %+v", inclusion)
	}
	if len(full.Graph.Integrations) == 0 || len(full.Graph.Operations.Merges) != 1 {
		t.Fatalf("graph omitted semantic contribution: %+v", full.Graph)
	}
	live, err := worker.core.GetPendingView(ctx, repo.ID)
	if err != nil || !reflect.DeepEqual(full.Graph, live.Graph) {
		t.Fatalf("live parity: %v", err)
	}
	query, err := worker.core.QueryContext(ctx, repo.ID, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
	if err != nil || !reflect.DeepEqual(query.Inclusion, &inclusion) {
		t.Fatalf("context parity: %+v %v", query.Inclusion, err)
	}
	past, err := worker.core.QueryContext(ctx, repo.ID, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main", CodeCommit: strings.Repeat("1", 40)})
	if err != nil || slices.Contains(past.Inclusion.SnapshotIDs, ordinary.Target) || past.Inclusion.Merges[0].State != "not_selected" {
		t.Fatalf("historical code included future PR: %+v %v", past.Inclusion, err)
	}
	if _, err := worker.core.DeliverPRPromotion(ctx, repo.ID, jobs[0].PR); err != nil {
		t.Fatal(err)
	}
	ref, _ := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if ref != main {
		t.Fatal("completed-job replay moved main")
	}
}

func TestPRContributionsSupplementCompletedPRAndRevalidateAmbiguity(t *testing.T) {
	worker, storage, remote, repo, ordinary, main := contributionFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	late := ordinary
	late.ID, late.Kind = strings.Repeat("3", 32), "position"
	late.Source, late.Target = ordinary.Target, prSnapshot(t, storage, repo.ID, "LATE GENUINE CONTEXT", ordinary.Target)
	late.GitAfter, late.CreatedAt = strings.Repeat("b", 40), time.Unix(240, 0).UTC()
	if err := worker.core.RecordHistory(ctx, late); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, worker.core, late)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(records) != 2 {
		t.Fatalf("supplement duplicated/lost: %+v %v", records, err)
	}
	proofs, _ := storage.ListPRParticipations(ctx, repo.ID, repo.GitRemoteURL, 7)
	if len(proofs) != 3 {
		t.Fatalf("test requires duplicate witnesses: %+v", proofs)
	}
	view, err := worker.core.GetRepositoryView(ctx, repo.ID)
	if err != nil || !slices.Contains(view.Graph.BranchContexts["main"].SnapshotIDs, late.Target) {
		t.Fatalf("supplement absent: %v", err)
	}
	if len(view.Graph.Operations.Merges) != 1 {
		t.Fatalf("one PR rendered as multiple merges: %+v", view.Graph.Operations.Merges)
	}
	parents := view.Graph.Integrations[0].Parents[view.Graph.Operations.Merges[0].ID]
	if !slices.Contains(parents, string(ordinary.Target)) || !slices.Contains(parents, string(late.Target)) {
		t.Fatalf("grouping dropped a contribution: %v", parents)
	}
	conflict := late
	conflict.ID, conflict.BranchID, conflict.Kind = strings.Repeat("7", 32), "reused-context-identity", "position"
	if err := worker.core.RecordHistory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	view, err = worker.core.GetRepositoryView(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, merge := range view.Graph.BranchContexts["main"].Merges {
		if merge.Source == late.Target && (merge.State != "review" || merge.Reason != "participation_evidence_conflict") {
			t.Fatalf("late ambiguity ignored: %+v", merge)
		}
	}
	if slices.Contains(view.Graph.BranchContexts["main"].SnapshotIDs, late.Target) {
		t.Fatal("ambiguous supplement remains included")
	}
	ref, _ := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if ref != main {
		t.Fatal("supplement or ambiguity moved main")
	}
}
