package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func ph(c string) domain.ContentHash {
	sum := sha256.Sum256([]byte(c))
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

// TestFSPutSnapshotPromotesStashLabel ensures server-side stash → commit promotion is fixed (CLI parity):
// "(stash)" label arriving first promotes branch/message, while reverse (commit first) is an immutable no-op. Server-side convergence path for stash objects overlapping commit ancestry (stash-dedup trap).
func TestFSPutSnapshotPromotesStashLabel(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := ph("r")

	id := ph("a")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, Branch: domain.StashBranchLabel, DocHash: id, Message: "WIP on main"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, Branch: "main", DocHash: id, Message: "feat: x"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSnapshot(ctx, repo, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "main" || got.Message != "feat: x" {
		t.Fatalf("Promotion failed: branch=%q message=%q", got.Branch, got.Message)
	}

	id2 := ph("b")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id2, RepoID: repo, Branch: "main", DocHash: id2, Message: "feat: y"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id2, RepoID: repo, Branch: domain.StashBranchLabel, DocHash: id2, Message: "WIP"}); err != nil {
		t.Fatal(err)
	}
	got2, err := st.GetSnapshot(ctx, repo, id2)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Branch != "main" || got2.Message != "feat: y" {
		t.Fatalf("Commit label promoted by stash re-save: branch=%q message=%q", got2.Branch, got2.Message)
	}
}

func TestFSManifestAdvertisesSnapshotState(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo, id := ph("manifest-repo"), ph("manifest-snapshot")
	snap := domain.Snapshot{ID: id, RepoID: repo, Branch: "main", DocHash: id, Message: "commit", GraftSeq: 2}
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	manifest, err := st.GetManifest(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	want, err := domain.SnapshotStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.SnapshotStates) != 1 || manifest.SnapshotStates[id] != want {
		t.Fatalf("snapshot states = %+v, want %s=%s", manifest.SnapshotStates, id, want)
	}
}
