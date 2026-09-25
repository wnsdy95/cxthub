package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type catalogRaceRemote struct {
	*pushOrderRemote
	late      func()
	snapshots map[domain.ContentHash]bool
	memories  map[domain.ContentHash]domain.ContentHash
	accepted  []domain.HistoryEvent
	refs      []domain.Ref
}

func (r *catalogRaceRemote) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	if r.late != nil {
		fn := r.late
		r.late = nil
		fn()
	}
	for _, snap := range snaps {
		r.snapshots[snap.ID] = true
	}
	if len(refs) > 0 {
		r.refs = append([]domain.Ref(nil), refs...)
	}
	return r.pushOrderRemote.Push(ctx, repo, snaps, docs, refs, force, appendMode)
}
func (r *catalogRaceRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return r.accepted, nil
}
func (r *catalogRaceRemote) PushHistoryEvent(_ context.Context, e domain.HistoryEvent) error {
	if !r.snapshots[e.Target] || r.memories[e.Target] != e.MemoryHash {
		return fmt.Errorf("history references prerequisites outside selected upload")
	}
	r.accepted = append(r.accepted, e)
	return nil
}
func (r *catalogRaceRemote) RemoteManifest(_ context.Context, repo string) (domain.Manifest, error) {
	return domain.Manifest{RepoID: repo, MemoryAttachments: r.memories}, nil
}
func (r *catalogRaceRemote) PushMemory(_ context.Context, _ string, d domain.MemoryDigest) error {
	if !r.snapshots[d.SnapshotID] {
		return domain.ErrNotFound
	}
	h, err := domain.MemoryDigestHash(d)
	if err == nil {
		r.memories[d.SnapshotID] = h
	}
	return err
}
func TestPushDefersConcurrentHistoryAndRefsUntilItsObjectsAndMemory(t *testing.T) {
	ctx := context.Background()
	svc, base, root, repo := setupPushOrder(t)
	st := svc.store.(*storage.FileStore)
	r := &catalogRaceRemote{pushOrderRemote: base, snapshots: map[domain.ContentHash]bool{}, memories: map[domain.ContentHash]domain.ContentHash{}}
	svc.remote = r
	var lateID domain.ContentHash
	r.late = func() {
		lateID = publicationSnapshot(t, st, repo, "concurrent commit", nil, nil)
		mem := domain.MemoryDigest{SnapshotID: lateID, Summary: "concurrent pinned memory"}
		h, err := st.PutMemory(ctx, mem)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CompareAndSwapSnapshotMemory(ctx, lateID, "", h); err != nil {
			t.Fatal(err)
		}
		event := domain.HistoryEvent{ID: strings.Repeat("c", 32), RepoID: repo, Branch: "main", BranchID: "main", Kind: "position", Source: lateID, Target: lateID, MemoryHash: h, MemoryPinned: true, CreatedAt: time.Now().UTC()}
		if err := st.PutHistoryEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
		if err := st.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "late-branch", Target: lateID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Push(ctx, inbound.SyncInput{Cwd: root}); err != nil {
		t.Fatal(err)
	}
	if len(r.accepted) != 0 || r.snapshots[lateID] {
		t.Fatal("concurrent history entered earlier upload")
	}
	for _, ref := range r.refs {
		if ref.Name == "late-branch" {
			t.Fatal("concurrent ref entered earlier upload")
		}
	}
	events, err := st.ListHistoryEvents(ctx, repo)
	if err != nil || len(events) != 1 {
		t.Fatalf("late history not retained: %v %v", events, err)
	}
	if _, err := svc.Push(ctx, inbound.SyncInput{Cwd: root}); err != nil {
		t.Fatal(err)
	}
	if len(r.accepted) != 1 || r.accepted[0].Target != lateID {
		t.Fatalf("next push did not publish late history: %+v", r.accepted)
	}
	found := false
	for _, ref := range r.refs {
		found = found || ref.Name == "late-branch"
	}
	if !found {
		t.Fatal("next push lost late ref")
	}
}
