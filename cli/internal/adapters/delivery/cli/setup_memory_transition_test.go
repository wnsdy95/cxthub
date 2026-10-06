package cli

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Reproduce T's Save -> first Memorize transition with the actual author store,
// then run setup's real fetch, proof selection, journal and attachment store.
// Only remote transport is fake; all data and Git worktrees belong to this test.
func TestSetupTrackingAfterFirstRecordedMemory(t *testing.T) {
	f := setupTrackingFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	author := storage.NewWorktreeFileStore(root, filepath.Join(root, ".git"), "team-task", f.oid)
	for _, doc := range f.remote.docs {
		if _, err := author.PutDoc(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	for _, snap := range f.remote.snaps {
		snap.MemoryHash = ""
		if err := author.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	birth := f.remote.history[0]
	birth.MemoryHash, birth.MemorySource = "", ""
	if err := author.PutHistoryEvent(ctx, birth); err != nil {
		t.Fatal(err)
	}
	p := domain.WorkingPosition{RepoID: f.repo, Branch: birth.Branch, BranchID: birth.BranchID,
		Snapshot: f.a, SharedTarget: f.a, GitCommit: f.oid, MemoryPinned: true}
	if err := author.PutWorkingPosition(ctx, p); err != nil {
		t.Fatal(err)
	}
	before, err := author.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := author.PutMemory(ctx, f.remote.memories[f.m1]); err != nil {
		t.Fatal(err)
	}
	if err := author.CompareAndSwapSnapshotMemory(ctx, f.a, "", f.m1); err != nil {
		t.Fatal(err)
	}
	if err := author.RecordWorkingMemory(ctx, f.a, f.m1); err != nil {
		t.Fatal(err)
	}
	f.remote.history, err = author.ListHistoryEvents(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the independently newer mutable M2 in the transport snapshot. Setup
	// must choose the actually recorded M1 and retain the old empty event.
	if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatalf("first capture then first memory cannot be attached: %v", err)
	}
	got, err := f.store.GetWorkingPosition(ctx)
	if err != nil || got.Snapshot != f.a || got.BranchID != birth.BranchID || got.MemoryHash != f.m1 || !got.MemoryPinned {
		t.Fatalf("setup did not select the recorded successor: %+v %v", got, err)
	}
	history, err := f.store.ListHistoryEvents(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, e := range history {
		if e.ID == before.Selection.ID {
			retained = reflect.DeepEqual(e, *before.Selection)
		}
	}
	if !retained {
		t.Fatal("old empty selection was rewritten or dropped")
	}
	if err := runSetup(ctx, f.c, f.cwd, []string{"--no-login"}); err != nil {
		t.Fatal(err)
	}
	again, err := f.store.GetWorkingPosition(ctx)
	if err != nil || !reflect.DeepEqual(got, again) || f.remote.pulls != 1 {
		t.Fatalf("repeated setup changed selection or refetched: pulls=%d err=%v", f.remote.pulls, err)
	}
}
