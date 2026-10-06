package app

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Uses real staging, Save, immutable objects, and FileStore finalization. No
// provider process, server, Git mutation, or invented observation is involved.
func inheritedStagingCommitFixture(t *testing.T) (stagingFixture, domain.StagingCommit, domain.ContentHash) {
	t.Helper()
	ctx := context.Background()
	f := newStagingFixture(t)
	f.stage(t, f.source(t, "owner", "first selected context"))
	first, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	src := f.source(t, "pending", "existing hook snapshot")
	saved, err := f.svc.save.Save(ctx, inbound.SaveInput{Cwd: f.root, Provider: src.Provider, SessionPath: src.Path, Pending: true, Message: domain.HookMessagePrefix + " pending"})
	if err != nil {
		t.Fatal(err)
	}
	ma, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: first.Position.Snapshot, Summary: "inherited memory"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.CommitWorkingMemory(ctx, outbound.WorkingMemoryCommit{RepoID: f.git.repo.ID, Snapshot: first.Position.Snapshot, Memory: ma, ExpectedPosition: &first.Position}); err != nil {
		t.Fatal(err)
	}
	f.stage(t, src)
	op, err := f.svc.Commit(ctx, inbound.StagingCommitInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	if op.Position.Snapshot != saved.SnapshotID || op.Position.MemoryHash != ma || op.Position.MemorySource != first.Position.Snapshot || op.Position.Selection.Kind != "publish" {
		t.Fatalf("actual inherited staging precondition not reached: selected=%+v", op.Position)
	}
	return f, op, ma
}

func TestStagingFinalTargetHasOneAuthoritativeMemory(t *testing.T) {
	ctx := context.Background()
	f, op, ma := inheritedStagingCommitFixture(t)
	history, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	observations := domain.StagingObservations(op)
	var empty, inherited []domain.HistoryEvent
	for _, e := range observations {
		found := false
		for _, old := range history {
			if reflect.DeepEqual(old, e) {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("staging observation was not retained exactly")
		}
		if e.Target != op.Position.Snapshot {
			continue
		}
		switch e.MemoryHash {
		case "":
			empty = append(empty, e)
		case ma:
			inherited = append(inherited, e)
		}
	}
	if len(empty) != 0 || len(inherited) != 1 {
		t.Fatalf("want only exact final selected witness: empty=%d inherited=%d", len(empty), len(inherited))
	}
	p := *op.Position.Selection
	want := p
	want.ID = string(domain.HashContent([]byte("staging-observation/v1/" + p.ID)))[7:39]
	want.Kind = "position"
	if !reflect.DeepEqual(want, inherited[0]) {
		t.Fatal("selected publication witness not exact")
	}
	// Contribution publication is retained; only duplicate authoritative position
	// generation is removed for the final selected target in new operations.
	for _, pub := range append(append([]domain.HistoryEvent{}, op.Publications...), *op.Position.Selection) {
		found := false
		for _, e := range history {
			if reflect.DeepEqual(e, pub) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("publication discarded: %s", pub.ID)
		}
	}
	again, err := f.svc.ResumeCommit(ctx, f.root, op.ID)
	if err != nil || !reflect.DeepEqual(domain.StagingObservations(again), observations) {
		t.Fatalf("resume changed observations: %v", err)
	}
}

func TestStagingFinalTargetMemorizeReplay(t *testing.T) {
	ctx := context.Background()
	f, op, inherited := inheritedStagingCommitFixture(t)
	svc := NewMemorizeService(f.git, nil, nil, nil, selectionChangingDistiller{}, f.store)
	out, err := svc.Memorize(ctx, inbound.MemorizeInput{Cwd: f.root})
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	witness := domain.StagingObservations(op)
	if p.Selection.MemorySelectionParent != witness[len(witness)-1].ID || p.MemoryHash != out.MemoryHash || p.MemorySource != "" {
		t.Fatalf("missing exact staged witness: %+v", p)
	}
	digest, err := f.store.GetMemory(ctx, out.MemoryHash)
	if err != nil || digest.PreviousMemoryHash != "" || digest.SnapshotID != p.Snapshot {
		t.Fatalf("incorrect self-owned root: %v", err)
	}
	receipt := domain.HistoryEvent{ID: strings.Repeat("f", 32), RepoID: f.git.repo.ID, Kind: "pr-merge", PRCompleted: true, Branch: "integrated", BranchID: "base", SourceBranchID: p.BranchID, Source: p.Snapshot, Target: p.Snapshot, CreatedAt: time.Now().UTC(), PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "integrated", HeadBranch: p.Branch, HeadSHA: p.GitCommit, MergeSHA: strings.Repeat("b", 40)}}
	history := NewContextHistoryService(f.store, f.store)
	got, err := history.ResolvePRSourcePosition(ctx, receipt)
	if err != nil || got.MemoryHash != out.MemoryHash || got.MemorySource != "" {
		t.Fatalf("PR selected %s want %s: %v", got.MemoryHash, out.MemoryHash, err)
	}
	events, err := f.store.ListHistoryEvents(ctx, f.git.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := f.store.ListSnapshots(ctx, f.git.repo.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.store.GetRef(ctx, f.git.repo.ID, domain.RefBranch, p.Branch)
	if err != nil {
		t.Fatal(err)
	}
	input := *p.Selection
	input.ID = strings.Repeat("e", 32)
	input.Kind = "attach"
	input.MemorySelectionParent = ""
	attached, err := history.PrepareTrackingAttachment(ctx, input, inbound.RemoteBranchObservation{Ref: ref, History: events, Snapshots: snapshots}, []string{p.GitCommit})
	if err != nil || attached.Event.MemoryHash != out.MemoryHash {
		t.Fatalf("tracking selected %s want %s: %v", attached.Event.MemoryHash, out.MemoryHash, err)
	}
	// Raw inherited memory and its original witness survive the new selection.
	if _, err := f.store.GetMemory(ctx, inherited); err != nil {
		t.Fatal(err)
	}
	for _, e := range witness {
		found := false
		for _, kept := range events {
			if reflect.DeepEqual(e, kept) {
				found = true
			}
		}
		if !found {
			t.Fatal("historical witness lost")
		}
	}
	if _, err := f.svc.ResumeCommit(ctx, f.root, op.ID); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(after, p) {
		t.Fatalf("staging retry rewound memory selection: %v", err)
	}
}

func TestStagingFinalTargetStatusAndDiff(t *testing.T) {
	f, op, _ := inheritedStagingCommitFixture(t)
	ctx := context.Background()
	history := NewHistoryQueryService(f.git, f.git, f.store, nil)
	state := NewWorkingStateService(f.git, f.git, f.store, f.store, history)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := state.Status(ctx, f.root); err != nil {
			t.Fatal("status after v2 commit", err)
		}
		for _, staged := range []bool{false, true} {
			if _, err := state.Diff(ctx, inbound.ContextDiffInput{Cwd: f.root, Staged: staged}); err != nil {
				t.Fatal("diff after v2 commit", err)
			}
		}
		if _, err := f.svc.ResumeCommit(ctx, f.root, op.ID); err != nil {
			t.Fatal(err)
		}
	}
}
