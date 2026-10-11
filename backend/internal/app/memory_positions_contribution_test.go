package app

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestQueryMemoryPositionsPRContribution(t *testing.T) {
	worker, storage, remote, repo, ordinary, main := contributionFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("contributions: %+v %v", records, err)
	}
	record := records[0]
	descendant := prSnapshot(t, storage, repo.ID, "UNPUBLISHED DESCENDANT", ordinary.Target)
	history, err := worker.core.ListHistory(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := worker.core.RepositoryRevision(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	providerCalls := len(remote.calls)
	selected, err := worker.core.QueryMemoryPositions(ctx, repo.ID, record.Capture, record.ID)
	if err != nil || selected.SnapshotID != record.Capture || selected.EventID != record.ID || selected.CodeCommit != record.PR.MergeSHA || selected.Reason != "selected_event" {
		t.Fatalf("ledger merge selection: %+v %v", selected, err)
	}
	want := domain.MemoryCodePosition{EventID: record.ID, CodeCommit: record.PR.MergeSHA, Branch: record.PR.BaseBranch, Kind: "pr-merge", PRNumber: record.PR.Number, CreatedAt: remote.pages[""].MergedAt}
	if !slices.Contains(selected.Options, want) || len(selected.Options) != 2 || selected.Options[0] != want {
		t.Fatalf("ledger option metadata/order: %+v", selected.Options)
	}
	publication := prPublication(ordinary)
	original, err := worker.core.QueryMemoryPositions(ctx, repo.ID, record.Capture, publication.ID)
	if err != nil || original.CodeCommit != ordinary.GitAfter || original.Reason != "selected_event" {
		t.Fatalf("original position lost: %+v %v", original, err)
	}
	for _, option := range domain.ResolveMemoryPositions(record.Capture, "", history).Options {
		if !slices.Contains(original.Options, option) {
			t.Fatalf("history option lost: %+v", option)
		}
	}
	byHash, err := worker.core.QueryMemoryPositions(ctx, repo.ID, record.Capture, "")
	if err != nil || byHash.Reason != "ambiguous" || byHash.CodeCommit != "" || !reflect.DeepEqual(byHash.Options, selected.Options) {
		t.Fatalf("hash selection guessed a position: %+v %v", byHash, err)
	}
	for _, snapshot := range []domain.ContentHash{ordinary.Source, main.Target, descendant} {
		positions, err := worker.core.QueryMemoryPositions(ctx, repo.ID, snapshot, record.ID)
		want := domain.ResolveMemoryPositions(snapshot, record.ID, history)
		if err != nil || !reflect.DeepEqual(positions, want) || positions.Reason != "unavailable" || positions.CodeCommit != "" {
			t.Fatalf("unassociated snapshot inherited merge position: %s %+v %v", snapshot, positions, err)
		}
	}
	afterHistory, err := worker.core.ListHistory(ctx, repo.ID)
	if err != nil || !reflect.DeepEqual(history, afterHistory) {
		t.Fatalf("query changed history: %v", err)
	}
	afterRevision, err := worker.core.RepositoryRevision(ctx, repo.ID)
	if err != nil || afterRevision != revision || len(remote.calls) != providerCalls {
		t.Fatalf("query changed revision or fetched remote evidence: %v", err)
	}
	afterMain, err := storage.GetRef(ctx, repo.ID, domain.RefBranch, "main")
	if err != nil || afterMain != main {
		t.Fatalf("query changed main: %+v %v", afterMain, err)
	}
}

func TestQueryMemoryPositionsInvalidatedPRContribution(t *testing.T) {
	worker, storage, remote, repo, ordinary, _ := contributionFixture(t)
	ctx := systemTestContext()
	publishPRSource(t, worker.core, ordinary)
	for range 2 {
		if err := worker.reconcilePRHistory(ctx, remote, storage, historyWork(t, worker, storage, repo)); err != nil {
			t.Fatal(err)
		}
	}
	records, err := storage.ListPRContributions(ctx, repo.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("contributions: %+v %v", records, err)
	}
	record := records[0]
	conflict := ordinary
	conflict.ID, conflict.BranchID, conflict.Kind = strings.Repeat("7", 32), "ambiguous-original", "position"
	if err := worker.core.RecordHistory(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	history, err := worker.core.ListHistory(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := worker.core.QueryMemoryPositions(ctx, repo.ID, record.Capture, record.ID)
	want := domain.ResolveMemoryPositions(record.Capture, record.ID, history)
	if err != nil || !reflect.DeepEqual(selected, want) || selected.Reason != "unavailable" || selected.CodeCommit != "" || len(selected.Options) == 0 {
		t.Fatalf("invalidated contribution fell back: %+v %v", selected, err)
	}
	original, err := worker.core.QueryMemoryPositions(ctx, repo.ID, record.Capture, prPublication(ordinary).ID)
	if err != nil || original.Reason != "selected_event" || original.CodeCommit != ordinary.GitAfter {
		t.Fatalf("invalidation erased recorded original: %+v %v", original, err)
	}
	byHash, err := worker.core.QueryMemoryPositions(ctx, repo.ID, record.Capture, "")
	if err != nil || !reflect.DeepEqual(byHash, domain.ResolveMemoryPositions(record.Capture, "", history)) {
		t.Fatalf("invalidation changed hash positions: %+v %v", byHash, err)
	}
}
