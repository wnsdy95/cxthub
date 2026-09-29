package storage

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"os"
	"testing"
)

func TestRemoteObservationCASAndIdentityIsolation(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("observation cas")))
	first, err := store.ReadRemoteObservation(ctx, repo, "origin")
	if err != nil {
		t.Fatal(err)
	}
	first.Refs = []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: domain.HashContent([]byte("root"))}}
	if err := store.CompareAndSwapRemoteObservation(ctx, "", first); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadRemoteObservation(ctx, repo, "origin")
	if err != nil || got.Revision == "" {
		t.Fatalf("observation=%+v %v", got, err)
	}
	if err := store.CompareAndSwapRemoteObservation(ctx, "", first); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("stale observation overwrote: %v", err)
	}
	other, err := store.ReadRemoteObservation(ctx, repo, "upstream")
	if err != nil || other.Revision != "" || len(other.Refs) != 0 {
		t.Fatalf("remote scope leak=%+v %v", other, err)
	}
	path, err := store.remoteObservationPath(repo, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"repo_id":"wrong"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadRemoteObservation(ctx, repo, "origin"); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("corruption accepted=%v", err)
	}
	invalid := outbound.RemoteObservation{Version: 1, RepoID: repo, Remote: "origin", Snapshots: []domain.Snapshot{{ID: domain.HashContent([]byte("x")), RepoID: "another"}}}
	if err := store.CompareAndSwapRemoteObservation(ctx, "", invalid); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("foreign metadata accepted=%v", err)
	}
}

func TestPulledRefCASPreservesConcurrentWriterAndRetainsOldTarget(t *testing.T) {
	ctx := context.Background()
	store := NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("pull cas")))
	old := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: domain.HashContent([]byte("old"))}
	if err := store.PutRef(ctx, old); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	winner := current
	winner.Target = domain.HashContent([]byte("winner"))
	if err := store.PutRef(ctx, winner); err != nil {
		t.Fatal(err)
	}
	proposed := current
	proposed.Target = domain.HashContent([]byte("remote"))
	if err := store.CompareAndSwapPulledRef(ctx, &current, proposed); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatalf("lost-update CAS=%v", err)
	}
	got, err := store.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || got.Target != winner.Target {
		t.Fatalf("winner lost=%+v %v", got, err)
	}
	if err := store.CompareAndSwapPulledRef(ctx, &got, proposed); err != nil {
		t.Fatal(err)
	}
	refs, err := store.ListRefs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, ref := range refs {
		if ref.Kind == domain.RefTag && ref.Target == winner.Target {
			retained = true
		}
	}
	if !retained {
		t.Fatal("superseded target not retained")
	}
}
