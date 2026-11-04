package contextprojection

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func historyOwnerHash(value string) domain.ContentHash {
	return domain.HashContent([]byte(value))
}

func historyOwnerCode(number int) string {
	return fmt.Sprintf("%040x", number)
}

func historyOwnerCompletion(receipt domain.HistoryEvent) domain.HistoryEvent {
	completion := receipt
	completion.ID = domain.PRCompletionID(receipt.ID)
	completion.PRCompleted = true
	return completion
}

type historyOwnerCancellationContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (ctx *historyOwnerCancellationContext) Err() error {
	ctx.checks++
	if ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func historyOwnerCancelAt(test *testing.T, cancelAt int) *historyOwnerCancellationContext {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	test.Cleanup(cancel)
	return &historyOwnerCancellationContext{Context: ctx, cancel: cancel, cancelAt: cancelAt}
}

func TestHistoryIndexOwnerPreservesOrderedLookups(test *testing.T) {
	repo := historyOwnerHash("ordered-repo")
	first, second := historyOwnerHash("first"), historyOwnerHash("second")
	metadata := &domain.PullRequestMerge{HeadSHA: historyOwnerCode(8), MergeSHA: historyOwnerCode(2)}
	history := []domain.HistoryEvent{
		{ID: "duplicate", BranchID: "main-id", Kind: "position", Target: first, GitAfter: historyOwnerCode(1)},
		{ID: "duplicate", BranchID: "main-id", Kind: "position", Target: first, GitAfter: historyOwnerCode(2)},
		{ID: "other", BranchID: "recreated-id", Kind: "position", Target: first},
		{ID: "birth", BranchID: "main-id", Kind: "birth", Source: first, Target: second},
		{ID: "publish", BranchID: "main-id", Kind: "publish", Source: first, Target: first, GitAfter: "not-an-oid"},
		{ID: "different-source", BranchID: "main-id", Kind: "publish", Source: second, Target: first, GitAfter: historyOwnerCode(3)},
		{ID: "empty-code", BranchID: "main-id", Kind: "publish", Source: first, Target: first},
		{ID: "receipt", BranchID: "main-id", Kind: "pr-merge", Target: first, PR: metadata},
		{ID: "completed", BranchID: "main-id", Kind: "pr-merge", Target: first, PR: metadata, PRCompleted: true},
		{ID: "legacy", BranchID: "main-id", Kind: "pr-merge", Target: first},
		{ID: "empty-birth", BranchID: "main-id", Kind: "birth", Target: second},
		{ID: "other-publish", BranchID: "recreated-id", Kind: "publish", Source: first, Target: first, GitAfter: historyOwnerCode(4)},
	}
	for position := range history {
		history[position].RepoID = string(repo)
	}
	index, err := NewHistoryIndex(context.Background(), repo, history)
	if err != nil {
		test.Fatal(err)
	}
	if index.repo != repo || index.EventCount() != len(history) || !reflect.DeepEqual(index.Rows(), history) {
		test.Fatalf("history changed: %+v", index)
	}
	if &index.Rows()[0] != &history[0] || cap(index.Rows()) != cap(history) {
		test.Fatal("rows accessor copied the request history")
	}
	if index.byID["duplicate"] != 0 || len(index.byID) != len(history)-1 {
		test.Fatalf("duplicate ID did not retain its first position: %+v", index.byID)
	}
	if !reflect.DeepEqual(index.byBranch["main-id"], []int{0, 1, 3, 4, 5, 6, 7, 8, 9, 10}) {
		test.Fatalf("branch order changed: %+v", index.byBranch)
	}
	positions := []int{0, 1, 4, 5, 6, 7, 8, 9}
	if !reflect.DeepEqual(index.byTarget[branchTargetKey{"main-id", first}], positions) {
		test.Fatalf("target order changed: %+v", index.byTarget)
	}
	var expected []domain.HistoryEvent
	for _, position := range positions {
		expected = append(expected, history[position])
	}
	rows := index.TargetEvents("main-id", first)
	if !reflect.DeepEqual(rows, expected) {
		test.Fatalf("target events = %+v, want %+v", rows, expected)
	}
	rows[0].Kind = "changed"
	if index.Rows()[0].Kind != "position" {
		test.Fatal("target lookup changed indexed rows")
	}
	if !reflect.DeepEqual(index.ReceiptPositions("main-id"), []int{7}) || len(index.ReceiptPositions("missing")) != 0 {
		test.Fatalf("receipt filtering changed: %+v", index.receipts)
	}
	if &index.ReceiptPositions("main-id")[0] != &index.receipts["main-id"][0] {
		test.Fatal("receipt accessor copied indexed positions")
	}
	if !reflect.DeepEqual(index.births["main-id"], []domain.ContentHash{first}) {
		test.Fatalf("birth filtering changed: %+v", index.births)
	}
	if !reflect.DeepEqual(index.publications[first], []string{"main-id", "recreated-id"}) {
		test.Fatalf("publication filtering changed: %+v", index.publications)
	}
	if len(index.TargetEvents("missing", first)) != 0 || len(index.TargetEvents("main-id", historyOwnerHash("missing"))) != 0 {
		test.Fatal("lookup crossed branch or target boundaries")
	}
}

func TestHistoryIndexOwnerCompletionUsesFirstEvent(test *testing.T) {
	repo := historyOwnerHash("completion-repo")
	receipt := domain.HistoryEvent{ID: "receipt", RepoID: string(repo), Kind: "pr-merge", BranchID: "main-id", SourceBranchID: "feature-id", Source: historyOwnerHash("source"), Target: historyOwnerHash("source"), PR: &domain.PullRequestMerge{Number: 1, HeadSHA: historyOwnerCode(8), MergeSHA: historyOwnerCode(2)}}
	completion := historyOwnerCompletion(receipt)
	completion.Target = historyOwnerHash("retained-destination")
	withBefore := completion
	withBefore.SharedTarget = historyOwnerHash("before")
	wrongSource := completion
	wrongSource.Source = historyOwnerHash("different-source")
	wrongDestination := completion
	wrongDestination.BranchID = "recreated-main-id"
	wrongIdentity := completion
	wrongIdentity.SourceBranchID = "recreated-feature-id"
	wrongPR := completion
	changedPR := *completion.PR
	changedPR.Number++
	wrongPR.PR = &changedPR
	nilPR := completion
	nilPR.PR = nil
	notCompleted := completion
	notCompleted.PRCompleted = false
	wrongKind := completion
	wrongKind.Kind = "position"
	for _, scenario := range []struct {
		name     string
		rows     []domain.HistoryEvent
		complete bool
		wantErr  error
	}{
		{"missing", nil, false, nil},
		{"retained target", []domain.HistoryEvent{completion}, true, nil},
		{"identical duplicate", []domain.HistoryEvent{completion, completion}, true, nil},
		{"valid before conflict", []domain.HistoryEvent{completion, wrongSource}, true, nil},
		{"conflict before valid", []domain.HistoryEvent{wrongSource, completion}, false, domain.ErrRefConflict},
		{"empty before first", []domain.HistoryEvent{completion, withBefore}, true, nil},
		{"recorded before first", []domain.HistoryEvent{withBefore, completion}, true, nil},
		{"wrong destination", []domain.HistoryEvent{wrongDestination, completion}, false, domain.ErrRefConflict},
		{"wrong source identity", []domain.HistoryEvent{wrongIdentity, completion}, false, domain.ErrRefConflict},
		{"wrong PR", []domain.HistoryEvent{wrongPR, completion}, false, domain.ErrRefConflict},
		{"nil PR", []domain.HistoryEvent{nilPR, completion}, false, domain.ErrRefConflict},
		{"incomplete", []domain.HistoryEvent{notCompleted, completion}, false, domain.ErrRefConflict},
		{"wrong kind", []domain.HistoryEvent{wrongKind, completion}, false, domain.ErrRefConflict},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			history := append([]domain.HistoryEvent{receipt}, scenario.rows...)
			index, err := NewHistoryIndex(context.Background(), repo, history)
			if err != nil {
				test.Fatal(err)
			}
			selected, complete, err := index.Completion(receipt)
			if complete != scenario.complete || !errors.Is(err, scenario.wantErr) {
				test.Fatalf("completion = %+v/%t/%v, want %t/%v", selected, complete, err, scenario.complete, scenario.wantErr)
			}
			expected := domain.HistoryEvent{}
			if len(scenario.rows) != 0 {
				expected = scenario.rows[0]
			}
			if !reflect.DeepEqual(selected, expected) {
				test.Fatalf("selected %+v, want first event %+v", selected, expected)
			}
		})
	}
}

func TestInputsOwnerLookupsAndRequestIsolation(test *testing.T) {
	repo := historyOwnerHash("inputs-repo")
	snapshot := domain.Snapshot{ID: historyOwnerHash("snapshot"), RepoID: repo, Parents: []domain.ContentHash{historyOwnerHash("parent")}}
	receipt := domain.HistoryEvent{ID: "receipt", RepoID: string(repo), Kind: "pr-merge", PR: &domain.PullRequestMerge{Number: 1}}
	completion := historyOwnerCompletion(receipt)
	before, err := NewInputs(context.Background(), repo, nil, []domain.HistoryEvent{receipt})
	if err != nil {
		test.Fatal(err)
	}
	after, err := NewInputs(context.Background(), repo, []domain.Snapshot{snapshot}, []domain.HistoryEvent{receipt, completion})
	if err != nil {
		test.Fatal(err)
	}
	if after.History() != after.history || after.History().EventCount() != 2 || after.SnapshotCount() != 1 || !reflect.DeepEqual(after.Snapshot(snapshot.ID), snapshot) {
		test.Fatalf("input accessors changed data: %+v", after)
	}
	selectedSnapshot := after.Snapshot(snapshot.ID)
	if &selectedSnapshot.Parents[0] != &snapshot.Parents[0] {
		test.Fatal("snapshot accessor copied stored ancestry")
	}
	if !reflect.DeepEqual(after.Snapshot(historyOwnerHash("missing")), domain.Snapshot{}) {
		test.Fatal("missing snapshot was fabricated")
	}
	if before.SnapshotCount() != 0 || before.History().EventCount() != 1 {
		test.Fatal("later inputs changed earlier inputs")
	}
	if _, complete, err := before.History().Completion(receipt); complete || err != nil {
		test.Fatalf("future completion leaked: %t/%v", complete, err)
	}
	if _, complete, err := after.History().Completion(receipt); !complete || err != nil {
		test.Fatalf("current completion missing: %t/%v", complete, err)
	}
}

func TestInputsOwnerRejectInvalidSources(test *testing.T) {
	repo, foreign := historyOwnerHash("repo"), historyOwnerHash("foreign")
	snapshot := domain.Snapshot{ID: historyOwnerHash("snapshot"), RepoID: repo}
	for _, scenario := range []struct {
		name      string
		history   []domain.HistoryEvent
		snapshots []domain.Snapshot
	}{
		{"foreign history", []domain.HistoryEvent{{RepoID: string(foreign)}}, nil},
		{"foreign snapshot", nil, []domain.Snapshot{{ID: snapshot.ID, RepoID: foreign}}},
		{"duplicate snapshot", nil, []domain.Snapshot{snapshot, snapshot}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			inputs, err := NewInputs(context.Background(), repo, scenario.snapshots, scenario.history)
			if inputs != nil || !errors.Is(err, domain.ErrIntegrity) {
				test.Fatalf("invalid sources returned %+v/%v", inputs, err)
			}
		})
	}
}

func TestInputsOwnerCancellationCheckpoints(test *testing.T) {
	repo := historyOwnerHash("canceled-repo")
	history := []domain.HistoryEvent{{RepoID: string(repo), ID: "first"}, {RepoID: string(repo), ID: "second"}}
	snapshots := []domain.Snapshot{{ID: historyOwnerHash("first"), RepoID: repo}, {ID: historyOwnerHash("second"), RepoID: repo}}
	for _, scenario := range []struct {
		name     string
		cancelAt int
	}{
		{"before history", 1},
		{"first history row", 2},
		{"second history row", 3},
		{"first snapshot", 4},
		{"second snapshot", 5},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx := historyOwnerCancelAt(test, scenario.cancelAt)
			inputs, err := NewInputs(ctx, repo, snapshots, history)
			if inputs != nil || !errors.Is(err, context.Canceled) || ctx.checks != scenario.cancelAt {
				test.Fatalf("cancellation checkpoint changed: %+v/%v after %d checks", inputs, err, ctx.checks)
			}
			fresh, err := NewInputs(context.Background(), repo, snapshots, history)
			if err != nil || fresh.History().EventCount() != 2 || fresh.SnapshotCount() != 2 {
				test.Fatalf("canceled build contaminated fresh inputs: %+v/%v", fresh, err)
			}
		})
	}
	ctx := historyOwnerCancelAt(test, 1)
	if inputs, err := NewInputs(ctx, repo, nil, nil); inputs != nil || !errors.Is(err, context.Canceled) {
		test.Fatalf("empty build skipped cancellation: %+v/%v", inputs, err)
	}
	ctx = historyOwnerCancelAt(test, 2)
	if index, err := NewHistoryIndex(ctx, repo, []domain.HistoryEvent{{RepoID: "foreign"}}); index != nil || !errors.Is(err, context.Canceled) {
		test.Fatalf("integrity overtook history cancellation: %+v/%v", index, err)
	}
	ctx = historyOwnerCancelAt(test, 2)
	if inputs, err := NewInputs(ctx, repo, []domain.Snapshot{{RepoID: "foreign"}}, nil); inputs != nil || !errors.Is(err, context.Canceled) {
		test.Fatalf("integrity overtook snapshot cancellation: %+v/%v", inputs, err)
	}
}
