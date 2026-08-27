package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type remoteStateCursorRemote struct {
	outbound.RemoteSync
	snapshot domain.Snapshot
	seen     []map[domain.ContentHash]domain.ContentHash
}

func (r *remoteStateCursorRemote) Pull(_ context.Context, _ string, states map[domain.ContentHash]domain.ContentHash, _ []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	copyStates := make(map[domain.ContentHash]domain.ContentHash, len(states))
	for id, state := range states {
		copyStates[id] = state
	}
	r.seen = append(r.seen, copyStates)
	remoteState, err := domain.SnapshotStateHash(r.snapshot)
	if err != nil {
		return nil, nil, nil, err
	}
	if states[r.snapshot.ID] == remoteState {
		return nil, nil, nil, nil
	}
	return []domain.Snapshot{r.snapshot}, nil, nil, nil
}

type remoteStateCursorFixture struct {
	ctx    context.Context
	repoID string
	store  *storage.FileStore
	remote *remoteStateCursorRemote
	svc    *SyncRepoService
	id     domain.ContentHash
}

func newRemoteStateCursorFixture(t *testing.T) remoteStateCursorFixture {
	t.Helper()
	ctx := context.Background()
	repoID := string(domain.HashContent([]byte("remote-state-cursor-repo")))
	doc := pullDoc(t, "local-ahead cursor snapshot")
	store := storage.NewFileStore(t.TempDir())
	if _, err := store.PutDoc(ctx, doc); err != nil {
		t.Fatal(err)
	}
	remoteSnapshot := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repoID, Branch: "main", Message: "recovery snapshot"}
	localSnapshot := remoteSnapshot
	localSnapshot.Grafted = true
	localSnapshot.GraftParents = []domain.ContentHash{domain.HashContent([]byte("local recovery parent"))}
	localSnapshot.GraftSeq = 1
	if err := store.PutSnapshot(ctx, localSnapshot); err != nil {
		t.Fatal(err)
	}
	remote := &remoteStateCursorRemote{snapshot: remoteSnapshot}
	return remoteStateCursorFixture{
		ctx: ctx, repoID: repoID, store: store, remote: remote,
		svc: NewSyncRepoService(store, remote, nil), id: doc.Hash,
	}
}

func (f remoteStateCursorFixture) pull(t *testing.T) inbound.SyncOutput {
	t.Helper()
	out, err := f.svc.Pull(f.ctx, inbound.SyncInput{RepoID: f.repoID, FetchOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPullRemoteStateCursorSkipsRepeatedLocalAheadMetadata(t *testing.T) {
	f := newRemoteStateCursorFixture(t)
	if got := f.pull(t).Pulled; got != 1 {
		t.Fatalf("first pull=%d snapshots, want 1", got)
	}
	if got := f.pull(t).Pulled; got != 0 {
		t.Fatalf("second pull=%d snapshots, want 0", got)
	}
	local, err := f.store.GetSnapshot(f.ctx, f.id)
	if err != nil || local.GraftSeq != 1 || len(local.GraftParents) != 1 {
		t.Fatalf("local-ahead graft was not preserved: %+v err=%v", local, err)
	}
	entries, err := f.store.LoadRemoteSnapshotStateCursor(f.ctx, f.repoID)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cursor=%+v err=%v", entries, err)
	}
}

func TestPullRemoteStateCursorRefetchesRemoteAndLocalMutations(t *testing.T) {
	t.Run("remote mutation", func(t *testing.T) {
		f := newRemoteStateCursorFixture(t)
		f.pull(t)
		f.pull(t)

		parentDoc := pullDoc(t, "new server graft parent")
		if _, err := f.store.PutDoc(f.ctx, parentDoc); err != nil {
			t.Fatal(err)
		}
		if err := f.store.PutSnapshot(f.ctx, domain.Snapshot{ID: parentDoc.Hash, DocHash: parentDoc.Hash, RepoID: f.repoID, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
		f.remote.snapshot.Grafted = true
		f.remote.snapshot.GraftParents = []domain.ContentHash{parentDoc.Hash}
		f.remote.snapshot.GraftSeq = 2
		if got := f.pull(t).Pulled; got != 1 {
			t.Fatalf("changed remote pull=%d snapshots, want 1", got)
		}
		local, err := f.store.GetSnapshot(f.ctx, f.id)
		if err != nil || local.GraftSeq != 2 || len(local.GraftParents) != 1 || local.GraftParents[0] != parentDoc.Hash {
			t.Fatalf("remote mutation not adopted: %+v err=%v", local, err)
		}
		if got := f.pull(t).Pulled; got != 0 {
			t.Fatalf("settled remote pull=%d snapshots, want 0", got)
		}
	})

	t.Run("local mutation", func(t *testing.T) {
		f := newRemoteStateCursorFixture(t)
		f.pull(t)
		f.pull(t)

		local, err := f.store.GetSnapshot(f.ctx, f.id)
		if err != nil {
			t.Fatal(err)
		}
		local.GraftSeq = 3
		local.GraftParents = []domain.ContentHash{domain.HashContent([]byte("new local recovery parent"))}
		if err := f.store.ReconcileGraftState(f.ctx, local); err != nil {
			t.Fatal(err)
		}
		wantLocalState, err := domain.SnapshotStateHash(local)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.pull(t).Pulled; got != 1 {
			t.Fatalf("changed local pull=%d snapshots, want 1", got)
		}
		if advertised := f.remote.seen[len(f.remote.seen)-1][f.id]; advertised != wantLocalState {
			t.Fatalf("local mutation advertised cached remote state %s, want local %s", advertised, wantLocalState)
		}
		if got := f.pull(t).Pulled; got != 0 {
			t.Fatalf("recached local-ahead pull=%d snapshots, want 0", got)
		}
	})
}

type failingRemoteStateCursorStore struct{ *storage.FileStore }

func (s *failingRemoteStateCursorStore) LoadRemoteSnapshotStateCursor(context.Context, string) (map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry, error) {
	return nil, errors.New("cursor unavailable")
}

func (s *failingRemoteStateCursorStore) SaveRemoteSnapshotStateCursor(context.Context, string, map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry) error {
	return errors.New("cursor unavailable")
}

func TestPullRemoteStateCursorFailureIsFailOpen(t *testing.T) {
	f := newRemoteStateCursorFixture(t)
	store := &failingRemoteStateCursorStore{FileStore: f.store}
	svc := NewSyncRepoService(store, f.remote, nil)
	if _, err := svc.Pull(f.ctx, inbound.SyncInput{RepoID: f.repoID, FetchOnly: true}); err != nil {
		t.Fatal(err)
	}
}

func TestPullRemoteStateCursorDoesNotAdvanceBeforeBatchValidation(t *testing.T) {
	f := newRemoteStateCursorFixture(t)
	f.remote.snapshot.RepoID = string(domain.HashContent([]byte("wrong cursor repo")))
	if _, err := f.svc.Pull(f.ctx, inbound.SyncInput{RepoID: f.repoID, FetchOnly: true}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("invalid pull error=%v", err)
	}
	entries, err := f.store.LoadRemoteSnapshotStateCursor(f.ctx, f.repoID)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cursor advanced before validation: %+v err=%v", entries, err)
	}
}
