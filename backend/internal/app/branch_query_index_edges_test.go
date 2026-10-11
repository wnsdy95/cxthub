package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchIndexCancellationContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   atomic.Int32
	cancelAt int32
}

func (ctx *branchIndexCancellationContext) Err() error {
	if ctx.checks.Add(1) == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func branchIndexCancelOnCheck(t *testing.T, parent context.Context, cancelAt int32) *branchIndexCancellationContext {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	t.Cleanup(cancel)
	return &branchIndexCancellationContext{Context: ctx, cancel: cancel, cancelAt: cancelAt}
}

func TestBranchQueryIndexEdgesCancellationDuringBuild(t *testing.T) {
	repo := hh("branch-index-cancel-repo")
	history := []domain.HistoryEvent{
		{ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), BranchID: "main-id", Kind: "position", Target: hh("first"), GitAfter: effectiveOID(1)},
		{ID: fmt.Sprintf("%032x", 2), RepoID: string(repo), BranchID: "main-id", Kind: "position", Target: hh("second"), GitAfter: effectiveOID(2)},
	}
	snapshots := []domain.Snapshot{{RepoID: repo, ID: history[0].Target}, {RepoID: repo, ID: history[1].Target}}
	for _, scenario := range []struct {
		name     string
		cancelAt int32
	}{{"after first history row", 3}, {"after first snapshot", 5}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := branchIndexCancelOnCheck(t, context.Background(), scenario.cancelAt)
			inputs, err := newBranchQueryInputs(ctx, repo, snapshots, history)
			if !errors.Is(err, context.Canceled) || inputs != nil {
				t.Fatalf("canceled build exposed partial inputs: %+v, %v", inputs, err)
			}
			if got := ctx.checks.Load(); got != scenario.cancelAt {
				t.Fatalf("cancellation observed at check %d, want %d", got, scenario.cancelAt)
			}
			fresh, err := newBranchQueryInputs(context.Background(), repo, snapshots, history)
			if err != nil {
				t.Fatal(err)
			}
			if len(fresh.snapshots) != len(snapshots) || len(fresh.history.byID) != len(history) {
				t.Fatalf("canceled build contaminated fresh inputs: %+v", fresh)
			}
		})
	}
}

func TestBranchQueryIndexEdgesCompletionFirstOccurrence(t *testing.T) {
	repo := hh("branch-index-completion-repo")
	receipt := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 10), RepoID: string(repo), Kind: "pr-merge", Branch: "main", BranchID: "main-id", SourceBranchID: "feature-id", Source: hh("source"), Target: hh("source"), PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(2)}}
	completion := receipt
	key := sha256.Sum256([]byte(receipt.ID + ":completed"))
	completion.ID, completion.PRCompleted = fmt.Sprintf("%x", key[:16]), true
	completion.Target, completion.SharedTarget = hh("retained-current-tip"), ""
	withBefore := completion
	withBefore.SharedTarget = hh("another-before")
	wrongSource := completion
	wrongSource.Source = hh("wrong-source")
	wrongDestination := completion
	wrongDestination.BranchID = "reused-main-id"
	wrongSourceBranch := completion
	wrongSourceBranch.SourceBranchID = "reused-feature-id"
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
		name      string
		rows      []domain.HistoryEvent
		complete  bool
		wantError error
	}{
		{"missing completion", nil, false, nil},
		{"retained target and empty before", []domain.HistoryEvent{completion}, true, nil},
		{"identical duplicates", []domain.HistoryEvent{completion, completion}, true, nil},
		{"valid before conflict", []domain.HistoryEvent{completion, wrongSource}, true, nil},
		{"conflict before valid", []domain.HistoryEvent{wrongSource, completion}, false, domain.ErrRefConflict},
		{"empty before wins", []domain.HistoryEvent{completion, withBefore}, true, nil},
		{"recorded before wins", []domain.HistoryEvent{withBefore, completion}, true, nil},
		{"wrong destination", []domain.HistoryEvent{wrongDestination, completion}, false, domain.ErrRefConflict},
		{"wrong source identity", []domain.HistoryEvent{wrongSourceBranch, completion}, false, domain.ErrRefConflict},
		{"wrong PR", []domain.HistoryEvent{wrongPR, completion}, false, domain.ErrRefConflict},
		{"unknown legacy PR", []domain.HistoryEvent{nilPR, completion}, false, domain.ErrRefConflict},
		{"not completed", []domain.HistoryEvent{notCompleted, completion}, false, domain.ErrRefConflict},
		{"wrong kind", []domain.HistoryEvent{wrongKind, completion}, false, domain.ErrRefConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			history := append([]domain.HistoryEvent{receipt}, scenario.rows...)
			index, err := newBranchHistoryIndex(context.Background(), repo, history)
			if err != nil {
				t.Fatal(err)
			}
			selected, complete, err := index.completion(receipt)
			if complete != scenario.complete || !errors.Is(err, scenario.wantError) {
				t.Fatalf("completion = %+v/%t/%v, want complete=%t error=%v", selected, complete, err, scenario.complete, scenario.wantError)
			}
			legacyComplete, legacyErr := hasPRCompletion(history, receipt)
			if complete != legacyComplete || !errors.Is(err, legacyErr) {
				t.Fatalf("index changed first-occurrence semantics: %t/%v versus %t/%v", complete, err, legacyComplete, legacyErr)
			}
			if len(scenario.rows) > 0 && !reflect.DeepEqual(selected, scenario.rows[0]) {
				t.Fatalf("selected later duplicate: %+v, want %+v", selected, scenario.rows[0])
			}
			if len(scenario.rows) == 0 && !reflect.DeepEqual(selected, domain.HistoryEvent{}) {
				t.Fatalf("missing completion fabricated a row: %+v", selected)
			}
		})
	}
}

func TestBranchQueryIndexEdgesDuplicateCompletionProjection(t *testing.T) {
	fixture, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	snapshots, err := fixture.st.ListSnapshots(ctx, fixture.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	receipt, recordedBefore := history[0], history[1]
	emptyBefore := recordedBefore
	emptyBefore.SharedTarget = ""
	emptyBefore.Target = ref.Target
	for _, scenario := range []struct {
		name        string
		completions []domain.HistoryEvent
	}{
		{"recorded before first", []domain.HistoryEvent{recordedBefore, emptyBefore}},
		{"empty before first", []domain.HistoryEvent{emptyBefore, recordedBefore}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			rows := append([]domain.HistoryEvent{receipt}, scenario.completions...)
			inputs, err := newBranchQueryInputs(ctx, fixture.repo, snapshots, rows)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := fixture.svc.newCodeEvidence(ctx, fixture.repo)
			if err != nil {
				t.Fatal(err)
			}
			inclusion, err := fixture.svc.branchContextFromInputs(ctx, ref, effectiveOID(2), inputs, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if len(inclusion.Merges) != 1 {
				t.Fatalf("duplicate completion changed merge count: %+v", inclusion)
			}
			merge := inclusion.Merges[0]
			if merge.Before != scenario.completions[0].SharedTarget || merge.Source != receipt.Source || merge.State != "included" || !merge.DeliveryAcknowledged {
				t.Fatalf("first completion lost projection meaning: %+v", merge)
			}
			if got, want := slices.Contains(inclusion.Roots, recordedBefore.SharedTarget), merge.Before != ""; got != want {
				t.Fatalf("later duplicate changed baseline roots: %+v", inclusion.Roots)
			}
			count := 0
			for _, snapshot := range inclusion.SnapshotIDs {
				if snapshot == receipt.Source {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("source appears %d times: %+v", count, inclusion.SnapshotIDs)
			}
		})
	}
}

func TestBranchQueryIndexEdgesSharedInputsKeepCausalSelectionsSeparate(t *testing.T) {
	fixture, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	for position := range history {
		if history[position].PRCompleted {
			history[position].Target = ref.Target
			history[position].SharedTarget = fixture.a
		}
	}
	resetTarget, ambiguousTarget := hh("indexed-reset"), hh("indexed-ambiguous")
	for _, observation := range []struct {
		branch string
		target domain.ContentHash
		code   int
	}{
		{ref.BranchID, fixture.a, 8},
		{ref.BranchID, resetTarget, 1},
		{ref.BranchID, ambiguousTarget, 2},
		{ref.BranchID, ambiguousTarget, 3},
		{"recreated-main-id", ref.Target, 5},
	} {
		history = append(history, domain.HistoryEvent{ID: fmt.Sprintf("%032x", len(history)+1000), RepoID: string(fixture.repo), Branch: ref.Name, BranchID: observation.branch, Kind: "position", Target: observation.target, GitAfter: effectiveOID(observation.code)})
	}
	index, err := newBranchHistoryIndex(ctx, fixture.repo, history)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := fixture.svc.newCodeEvidence(ctx, fixture.repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range []struct {
		name, branch string
		target       domain.ContentHash
		code         string
	}{
		{"current normalizes exact carried head", ref.BranchID, ref.Target, effectiveOID(3)},
		{"past source ignores future completion", ref.BranchID, fixture.a, effectiveOID(8)},
		{"ordinary ambiguity remains unknown", ref.BranchID, ambiguousTarget, ""},
		{"ordinary reset stays selected", ref.BranchID, resetTarget, effectiveOID(1)},
		{"reused name has independent identity", "recreated-main-id", ref.Target, effectiveOID(5)},
		{"current remains unchanged after other selections", ref.BranchID, ref.Target, effectiveOID(3)},
	} {
		t.Run(selection.name, func(t *testing.T) {
			selected := ref
			selected.BranchID, selected.Target = selection.branch, selection.target
			code, err := resolvedBranchCodeFromIndex(ctx, selected, index, evidence)
			if err != nil || code != selection.code {
				t.Fatalf("selected code = %q/%v, want %q", code, err, selection.code)
			}
		})
	}
}

func TestBranchQueryIndexEdgesCancellationDuringTraversal(t *testing.T) {
	repo := hh("branch-index-traversal-repo")
	first, second := hh("traversal-first"), hh("traversal-second")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: first}
	history := []domain.HistoryEvent{
		{ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), BranchID: ref.BranchID, Kind: "pr-merge", Target: first, SharedTarget: second, PRCompleted: true, PR: &domain.PullRequestMerge{MergeSHA: effectiveOID(2)}},
		{ID: fmt.Sprintf("%032x", 2), RepoID: string(repo), BranchID: ref.BranchID, Kind: "position", Target: second, GitAfter: effectiveOID(1)},
		{ID: fmt.Sprintf("%032x", 3), RepoID: string(repo), BranchID: "child-id", Kind: "birth", Source: first, Target: first},
		{ID: fmt.Sprintf("%032x", 4), RepoID: string(repo), BranchID: ref.BranchID, Kind: "publish", Source: first, Target: first, GitAfter: effectiveOID(2)},
	}
	snapshots := []domain.Snapshot{{RepoID: repo, ID: first, Parents: []domain.ContentHash{second}}, {RepoID: repo, ID: second}}
	inputs, err := newBranchQueryInputs(context.Background(), repo, snapshots, history)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("causal target walk", func(t *testing.T) {
		ctx := branchIndexCancelOnCheck(t, context.Background(), 2)
		code, err := resolvedBranchCodeFromIndex(ctx, ref, inputs.history, &codeEvidence{repo: repo})
		if !errors.Is(err, context.Canceled) || code != "" {
			t.Fatalf("canceled resolver returned partial selection: %q/%v", code, err)
		}
		if ctx.checks.Load() != 2 {
			t.Fatalf("resolver did not cancel during its second target: %d", ctx.checks.Load())
		}
	})
	t.Run("inherited natural ancestry walk", func(t *testing.T) {
		ctx := branchIndexCancelOnCheck(t, context.Background(), 2)
		scopes, err := inheritedIntegrationScopes(ctx, "child-id", inputs.snapshots, inputs.history)
		if !errors.Is(err, context.Canceled) || scopes != nil {
			t.Fatalf("canceled inheritance exposed partial scopes: %+v/%v", scopes, err)
		}
		fresh, err := inheritedIntegrationScopes(context.Background(), "child-id", inputs.snapshots, inputs.history)
		if err != nil || !reflect.DeepEqual(fresh, map[string]bool{"child-id": true, ref.BranchID: true}) {
			t.Fatalf("cancellation contaminated reusable index: %+v/%v", fresh, err)
		}
	})
}

func TestBranchQueryIndexEdgesContributionHistoryCompleteMissingComplete(t *testing.T) {
	fixture := newPRMemoryFixture(t)
	ctx := systemTestContext()
	selectedCode := fixture.scanMemoryCode(t)
	const claim = "immutable original memory survives restored query inputs"
	digest := fixture.digest(t, "", claim)
	publication := fixture.publish(t, digest, true)
	fixture.reconcile(t)
	service := fixture.worker.core
	history, err := service.ListHistory(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := fixture.storage.ListSnapshots(ctx, fixture.repo.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	missingPublication := slices.DeleteFunc(slices.Clone(history), func(event domain.HistoryEvent) bool {
		return event.ID == publication.ID
	})
	missingObservation := slices.DeleteFunc(slices.Clone(history), func(event domain.HistoryEvent) bool {
		return event.ID == fixture.ordinary.ID
	})
	if len(missingPublication) != len(history)-1 || len(missingObservation) != len(history)-1 {
		t.Fatal("fixture must contain both immutable publication and ordinary observation")
	}
	evidence, err := service.newCodeEvidence(ctx, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	var initial domain.MemoryProjection
	for _, scenario := range []struct {
		name    string
		history []domain.HistoryEvent
		valid   bool
	}{
		{"complete", history, true},
		{"publication missing", missingPublication, false},
		{"complete new backing slice", slices.Clone(history), true},
		{"ordinary witness missing", missingObservation, false},
		{"complete original backing slice", history, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			inputs, err := newBranchQueryInputs(ctx, fixture.repo.ID, snapshots, scenario.history)
			if err != nil {
				t.Fatal(err)
			}
			inclusion, err := service.branchContextFromInputs(ctx, fixture.main, selectedCode, inputs, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if len(inclusion.Merges) != 1 {
				t.Fatalf("historical contribution disappeared: %+v", inclusion)
			}
			merge := inclusion.Merges[0]
			if merge.DeliveryAcknowledged != scenario.valid || (merge.State == "included") != scenario.valid {
				t.Fatalf("previous history verification leaked: %+v", merge)
			}
			if !scenario.valid && (merge.State != "review" || merge.Reason != "participation_evidence_conflict") {
				t.Fatalf("missing witness was not preserved for review: %+v", merge)
			}
			if slices.Contains(inclusion.SnapshotIDs, fixture.ordinary.Target) != scenario.valid {
				t.Fatalf("previous history inclusion leaked: %+v", inclusion.SnapshotIDs)
			}
			contributions, err := service.contextContributionsForEvidence(ctx, fixture.repo.ID, scenario.history, evidence)
			if err != nil || len(contributions) != 1 || contributions[0].valid != scenario.valid {
				t.Fatalf("contribution cache retained different inputs: %+v/%v", contributions, err)
			}
			pinned, found := contributions[0].publications[publication.ID]
			if found != scenario.valid || (found && (pinned.MemoryHash != digest || !pinned.MemoryPinned)) {
				t.Fatalf("wrong immutable pin for current history: %+v/%t", pinned, found)
			}
			projection, err := service.projectBranchMemoryWithContributions(ctx, fixture.repo.ID, inclusion, snapshots, contributions)
			if err != nil {
				t.Fatal(err)
			}
			items, err := effectiveMemoryItems(projection.Digest, "claims")
			if err != nil {
				t.Fatal(err)
			}
			if scenario.valid {
				if len(items) != 1 || items[0].Text != claim {
					t.Fatalf("restored history lost original memory: %+v", items)
				}
				if scenario.name == "complete" {
					initial = projection
				} else if !reflect.DeepEqual(projection, initial) {
					t.Fatalf("restored immutable history changed projection: %+v versus %+v", projection, initial)
				}
			} else if len(items) != 0 {
				t.Fatalf("missing witness reused accepted memory: %+v", items)
			}
		})
	}
	storedHistory, err := service.ListHistory(ctx, fixture.repo.ID)
	if err != nil || !reflect.DeepEqual(history, storedHistory) {
		t.Fatalf("query input replacement changed accepted history: %v", err)
	}
}
