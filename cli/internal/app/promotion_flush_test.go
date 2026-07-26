package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type promoteRecordingRemote struct {
	outbound.RemoteSync
	calls    map[string]string
	grafts   map[string][]domain.ContentHash
	failID   domain.ContentHash
	seqs     map[string]uint64
	graftErr map[string]error
	snaps    map[string]domain.Snapshot
	getErr   error
	onGraft  func()
}

func (r *promoteRecordingRemote) GetSnapshotRemote(_ context.Context, _ string, id domain.ContentHash) (domain.Snapshot, error) {
	if r.getErr != nil {
		return domain.Snapshot{}, r.getErr
	}
	if snap, ok := r.snaps[string(id)]; ok {
		return snap, nil
	}
	return domain.Snapshot{}, errors.New("snapshot unavailable")
}

func (r *promoteRecordingRemote) PromoteSnapshotMessage(_ context.Context, _ string, id domain.ContentHash, msg string) error {
	if id == r.failID {
		return errors.New("server unavailable")
	}
	r.calls[string(id)] = msg
	return nil
}

func (r *promoteRecordingRemote) GraftSnapshotParents(_ context.Context, _ string, id domain.ContentHash, parents []domain.ContentHash, expectedSeq uint64) error {
	if r.onGraft != nil {
		r.onGraft()
	}
	if err := r.graftErr[string(id)]; err != nil {
		return err
	}
	if id == r.failID {
		return errors.New("server unavailable")
	}
	r.grafts[string(id)] = append(r.grafts[string(id)], parents...)
	if r.seqs != nil {
		r.seqs[string(id)] = expectedSeq
	}
	return nil
}

type fakeStatusError struct{ status int }

func (e fakeStatusError) Error() string   { return "remote status" }
func (e fakeStatusError) StatusCode() int { return e.status }

// TestFlushPromotionsFromRepoRoot fixes promotion queue flush contract:
// Reads from <repoRoot>/.cxt/promotions.json, removes successful items (failed items are kept for next push retry), and removes the queue file if all are successful.
// Regression background (real review P1): Pushing from a subdirectory caused the queue to be silently permanently unflushed — the caller must always pass the worktree root.
func TestFlushPromotionsFromRepoRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	okID := domain.HashContent([]byte("promoted-ok"))
	failID := domain.HashContent([]byte("promoted-fail"))
	queue := map[string]string{
		string(okID):   "feat: promoted [git abc1234]",
		string(failID): "feat: retried later",
	}
	qb, _ := json.Marshal(queue)
	qPath := filepath.Join(root, ".cxt", "promotions.json")
	if err := os.WriteFile(qPath, qb, 0o644); err != nil {
		t.Fatal(err)
	}

	remote := &promoteRecordingRemote{calls: map[string]string{}, grafts: map[string][]domain.ContentHash{}, failID: failID}
	svc := NewSyncRepoService(nil, remote, nil)
	repoID := string(domain.HashContent([]byte("repo")))

	// Passing a subdirectory path causes the queue to be not found — should be a no-op, and the root queue should remain unchanged (to reproduce the regression scenario).
	sub := filepath.Join(root, "packages", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.flushPromotions(ctx, sub, repoID)
	if len(remote.calls) != 0 {
		t.Fatalf("Flush occurred with subdirectory path: %v", remote.calls)
	}

	// Root-based flush: 1 success propagated, 1 failure remains.
	svc.flushPromotions(ctx, root, repoID)
	if got := remote.calls[string(okID)]; got != "feat: promoted [git abc1234]" {
		t.Fatalf("Successful item not propagated: %v", remote.calls)
	}
	left := map[string]string{}
	lb, err := os.ReadFile(qPath)
	if err != nil {
		t.Fatalf("Failed item removed from queue: %v", err)
	}
	if json.Unmarshal(lb, &left) != nil || len(left) != 1 || left[string(failID)] == "" {
		t.Fatalf("Queue residual state abnormal: %v", left)
	}

	// Failed item removed if subsequent success, queue file itself is removed.
	remote.failID = ""
	svc.flushPromotions(ctx, root, repoID)
	if _, err := os.Stat(qPath); !os.IsNotExist(err) {
		t.Fatalf("Queue file remains after full success: %v", err)
	}
}

// TestFlushGrafts enforces the graft queue flush contract (similar to the promotion queue pattern):
// root-based path, remove successful items, remove file after full success.
func TestFlushGrafts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := domain.HashContent([]byte("graft-head"))
	sib := domain.HashContent([]byte("graft-sibling"))
	repoID := string(domain.HashContent([]byte("repo")))
	qb, _ := json.Marshal(map[string][]string{string(head): {string(sib)}})
	qPath := filepath.Join(root, ".cxt", "grafts.json")
	if err := os.WriteFile(qPath, qb, 0o644); err != nil {
		t.Fatal(err)
	}
	local := storage.NewFileStore(root)
	legacy := domain.Snapshot{
		ID: head, RepoID: repoID, Branch: "main", DocHash: head,
		Grafted: true, GraftParents: []domain.ContentHash{sib}, GraftSeq: 0,
	}
	if err := local.PutSnapshot(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	authoritative := legacy
	authoritative.GraftSeq = 1
	remote := &promoteRecordingRemote{
		calls: map[string]string{}, grafts: map[string][]domain.ContentHash{},
		snaps: map[string]domain.Snapshot{string(head): authoritative},
	}
	svc := NewSyncRepoService(local, remote, nil)
	if err := svc.flushGrafts(ctx, root, repoID); err != nil {
		t.Fatal(err)
	}
	if got := remote.grafts[string(head)]; len(got) != 1 || got[0] != sib {
		t.Fatalf("Graft not propagated: %v", remote.grafts)
	}
	if _, err := os.Stat(qPath); !os.IsNotExist(err) {
		t.Fatalf("Queue file remains after success: %v", err)
	}
	got, err := local.GetSnapshot(ctx, head)
	if err != nil || got.GraftSeq != 1 || len(got.GraftParents) != 1 || got.GraftParents[0] != sib {
		t.Fatalf("Local seq not adjusted after legacy success: %+v err=%v", got, err)
	}
}

func TestFlushGraftsDropsTerminalStaleEvent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := domain.HashContent([]byte("stale-graft-head"))
	sib := domain.HashContent([]byte("stale-graft-sibling"))
	later := domain.HashContent([]byte("stale-graft-later"))
	other := domain.HashContent([]byte("other-graft-head"))
	repoID := string(domain.HashContent([]byte("repo")))
	local := storage.NewFileStore(root)
	if err := local.PutSnapshot(ctx, domain.Snapshot{
		ID: head, RepoID: repoID, Branch: "main", DocHash: head,
		Grafted: true, GraftParents: []domain.ContentHash{sib}, GraftSeq: 10,
	}); err != nil {
		t.Fatal(err)
	}
	state := graftQueueState{Version: graftQueueVersion, Events: []graftQueueEvent{
		{Snapshot: string(head), Parents: []string{string(sib)}, ExpectedSeq: 9},
		// Events from the same snapshot should also be discarded from the first stale state.
		{Snapshot: string(head), Parents: []string{string(later)}, ExpectedSeq: 10},
		// Events from different snapshots are preserved for the next push.
		{Snapshot: string(other), Parents: []string{string(sib)}, ExpectedSeq: 0},
	}}
	if err := writeGraftQueue(root, ".cxt/grafts.json", state); err != nil {
		t.Fatal(err)
	}
	remote := &promoteRecordingRemote{
		calls: map[string]string{}, grafts: map[string][]domain.ContentHash{}, seqs: map[string]uint64{},
		graftErr: map[string]error{string(head): fakeStatusError{status: 409}},
		snaps: map[string]domain.Snapshot{string(head): {
			ID: head, RepoID: repoID, Branch: "main", DocHash: head, GraftSeq: 10,
		}},
	}
	err := NewSyncRepoService(local, remote, nil).flushGrafts(ctx, root, repoID)
	if !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("Push interruption error after stale graft: %v", err)
	}
	left, rerr := readGraftQueue(root, ".cxt/grafts.json")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(left.Events) != 1 || left.Events[0].Snapshot != string(other) {
		t.Fatalf("stale derived event discard/other snapshot preservation failure: %+v", left.Events)
	}
	got, err := local.GetSnapshot(ctx, head)
	if err != nil || got.GraftSeq != 10 || got.Grafted || len(got.GraftParents) != 0 {
		t.Fatalf("409 after server graft, not adjusted to source of truth: %+v err=%v", got, err)
	}
}

func TestFlushGraftsKeepsQueueWhenConflictReconcileFetchFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := domain.HashContent([]byte("fetch-fail-head"))
	parent := domain.HashContent([]byte("fetch-fail-parent"))
	if err := queueGraft(root, head, parent, 3); err != nil {
		t.Fatal(err)
	}
	remote := &promoteRecordingRemote{
		calls: map[string]string{}, grafts: map[string][]domain.ContentHash{},
		graftErr: map[string]error{string(head): fakeStatusError{status: 409}},
		getErr:   errors.New("temporary fetch failure"),
	}
	err := NewSyncRepoService(storage.NewFileStore(root), remote, nil).
		flushGrafts(ctx, root, string(domain.HashContent([]byte("repo"))))
	if err == nil {
		t.Fatal("source of truth retrieval failure treated as success")
	}
	state, rerr := readGraftQueue(root, ".cxt/grafts.json")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(state.Events) != 1 || state.Events[0].Snapshot != string(head) {
		t.Fatalf("source of truth retrieval failure leads to queue loss: %+v", state.Events)
	}
}

func TestFlushGraftsPreservesConcurrentAppend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := domain.HashContent([]byte("graft-a"))
	b := domain.HashContent([]byte("graft-b"))
	p := domain.HashContent([]byte("parent"))
	if err := queueGraft(root, a, p, 0); err != nil {
		t.Fatal(err)
	}
	remote := &promoteRecordingRemote{
		calls: map[string]string{}, grafts: map[string][]domain.ContentHash{}, seqs: map[string]uint64{}, graftErr: map[string]error{},
	}
	remote.onGraft = func() {
		remote.onGraft = nil
		if err := queueGraft(root, b, p, 1); err != nil {
			t.Errorf("concurrent append failed: %v", err)
		}
	}
	if err := NewSyncRepoService(nil, remote, nil).flushGrafts(ctx, root, string(domain.HashContent([]byte("repo")))); err != nil {
		t.Fatal(err)
	}
	state, err := readGraftQueue(root, ".cxt/grafts.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Events) != 1 || state.Events[0].Snapshot != string(b) {
		t.Fatalf("concurrent tail was lost: %+v", state.Events)
	}
}
