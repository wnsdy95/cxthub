package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// TestQueueGraftFailClosed fixes error propagation in graft queue persistence — Save must stop ref movement on queue record failure (fail-closed invariant) so it cannot be silently swallowed. It checks three paths: success, idempotence, and record failure (.cxt is a file).
func TestQueueGraftFailClosed(t *testing.T) {
	head := domain.HashContent([]byte("head"))
	parent := domain.HashContent([]byte("parent"))

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := queueGraft(root, head, parent, 7); err != nil {
		t.Fatalf("normal record failure: %v", err)
	}
	if err := queueGraft(root, head, parent, 7); err != nil {
		t.Fatalf("idempotent re-record error: %v", err)
	}

	broken := t.TempDir()
	// Occupy .cxt as a file — queue record must fail and error must be returned.
	if err := os.WriteFile(filepath.Join(broken, ".cxt"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := queueGraft(broken, head, parent, 7); err == nil {
		t.Fatal("error swallowed in record failure scenario — fail-closed of Save is invalidated")
	}
}

func TestQueueGraftConcurrentWritersDoNotLoseEvents(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	parent := domain.HashContent([]byte("shared-parent"))
	const writers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			head := domain.HashContent([]byte{byte(i + 1)})
			errCh <- queueGraft(root, head, parent, uint64(i))
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err := readGraftQueue(root, ".cxt/grafts.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Events) != writers {
		t.Fatalf("concurrent queue lost events: got=%d want=%d", len(state.Events), writers)
	}
}

func TestGraftLocalAndQueueAdvancesExpectedSeq(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	svc := &SaveSessionService{store: st}
	head := domain.HashContent([]byte("head"))
	p1 := domain.HashContent([]byte("parent-1"))
	p2 := domain.HashContent([]byte("parent-2"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: head, DocHash: head, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.graftLocalAndQueue(ctx, root, head, p1); err != nil {
		t.Fatal(err)
	}
	if err := svc.graftLocalAndQueue(ctx, root, head, p2); err != nil {
		t.Fatal(err)
	}
	state, err := readGraftQueue(root, ".cxt/grafts.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Events) != 2 || state.Events[0].ExpectedSeq != 0 || state.Events[1].ExpectedSeq != 1 {
		t.Fatalf("ordered expected_seq mismatch: %+v", state.Events)
	}
	snap, err := st.GetSnapshot(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if snap.GraftSeq != 2 || len(snap.GraftParents) != 2 {
		t.Fatalf("local graft register mismatch: %+v", snap)
	}
}

func TestGraftLocalAndQueueDoesNotMutateWhenQueueWriteFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	svc := &SaveSessionService{store: st}
	head := domain.HashContent([]byte("head"))
	parent := domain.HashContent([]byte("parent"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: head, DocHash: head, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	// Occupy queue target path as a directory to fail atomic write. Lock file must be obtainable, and local register must remain unchanged after failure.
	if err := os.MkdirAll(filepath.Join(root, ".cxt", "grafts.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.graftLocalAndQueue(ctx, root, head, parent); err == nil {
		t.Fatal("queue write failure was ignored")
	}
	snap, err := st.GetSnapshot(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if snap.GraftSeq != 0 || len(snap.GraftParents) != 0 {
		t.Fatalf("queue failure mutated local register: %+v", snap)
	}
}

func TestGraftLocalAndQueueNeverWrapsSequence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	svc := &SaveSessionService{store: st}
	head := domain.HashContent([]byte("max-seq-head"))
	parent := domain.HashContent([]byte("max-seq-parent"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: head, DocHash: head, Branch: "main", GraftSeq: domain.MaxGraftSeq,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.graftLocalAndQueue(ctx, root, head, parent); err == nil {
		t.Fatal("max graft sequence wrapped instead of failing closed")
	}
	got, err := st.GetSnapshot(ctx, head)
	if err != nil || got.GraftSeq != domain.MaxGraftSeq || len(got.GraftParents) != 0 {
		t.Fatalf("max sequence changed: %+v err=%v", got, err)
	}
	state, err := readGraftQueue(root, ".cxt/grafts.json")
	if err != nil || len(state.Events) != 0 {
		t.Fatalf("max sequence left an unsendable queue event: %+v err=%v", state.Events, err)
	}
}

func TestGraftLocalAndQueueBlocksNewEventBehindLegacyQueue(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	svc := &SaveSessionService{store: st}
	head := domain.HashContent([]byte("legacy-head"))
	p1 := domain.HashContent([]byte("legacy-parent"))
	p2 := domain.HashContent([]byte("new-parent"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{
		ID: head, DocHash: head, Branch: "main", Grafted: true,
		GraftParents: []domain.ContentHash{p1}, GraftSeq: 0,
	}); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal(map[string][]string{string(head): {string(p1)}})
	if err := os.MkdirAll(filepath.Join(root, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cxt", "grafts.json"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.graftLocalAndQueue(ctx, root, head, p2); err == nil {
		t.Fatal("new expected_seq=0 event added after legacy expected_seq=0")
	}
	state, err := readGraftQueue(root, ".cxt/grafts.json")
	if err != nil || len(state.Events) != 1 || !state.Events[0].Legacy {
		t.Fatalf("legacy queue changed: %+v err=%v", state.Events, err)
	}
	got, err := st.GetSnapshot(ctx, head)
	if err != nil || got.GraftSeq != 0 || len(got.GraftParents) != 1 || got.GraftParents[0] != p1 {
		t.Fatalf("blocked legacy append changed local register: %+v err=%v", got, err)
	}
}
