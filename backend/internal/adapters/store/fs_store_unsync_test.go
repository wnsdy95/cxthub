package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestDeleteUnsyncRemovesLegacySchemeZombies ensures that unsync pointers (names not reachable via derived paths) are also removed by DeleteUnsync, based on content (user, branch). If not, they become zombie entries in ListUnsyncs, preventing deletion and permanently contaminating web On Hold/continuing dialog judgments after push resolution (bug: 2026-07-06 residual).
func TestDeleteUnsyncRemovesLegacySchemeZombies(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := rlHash('0')
	target := rlHash('a')

	u := domain.Unsync{RepoID: repo, User: "alice", Branch: "main", Target: target, UpdatedAt: time.Now().UTC()}
	// Current scheme file.
	if err := st.PutUnsync(ctx, repo, u); err != nil {
		t.Fatalf("PutUnsync: %v", err)
	}
	// Legacy zombie: same content (user, branch) but different file name from any derived path.
	dir := filepath.Join(st.repoDir(repo), "unsync")
	data, _ := json.Marshal(u)
	if err := os.WriteFile(filepath.Join(dir, "alice__main.json"), data, 0o644); err != nil {
		t.Fatalf("zombie write: %v", err)
	}

	if err := st.DeleteUnsync(ctx, repo, "alice", "main"); err != nil {
		t.Fatalf("DeleteUnsync: %v", err)
	}
	list, err := st.ListUnsyncs(ctx, repo)
	if err != nil {
		t.Fatalf("ListUnsyncs: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("stale entry remains: %v", list)
	}
	// Other users/branches are not affected.
	other := domain.Unsync{RepoID: repo, User: "dan", Branch: "main", Target: target, UpdatedAt: time.Now().UTC()}
	if err := st.PutUnsync(ctx, repo, other); err != nil {
		t.Fatalf("PutUnsync(other): %v", err)
	}
	if err := st.DeleteUnsync(ctx, repo, "alice", "main"); err != nil {
		t.Fatalf("DeleteUnsync(retry): %v", err)
	}
	list, _ = st.ListUnsyncs(ctx, repo)
	if len(list) != 1 || list[0].User != "dan" {
		t.Fatalf("Irrelevant pointer deleted: %v", list)
	}
}

func TestDeleteUnsyncIfPresentTracksLegacyFiles(t *testing.T) {
	ctx := context.Background()
	storage := NewFSStore(t.TempDir())
	repo, target := rlHash('0'), rlHash('a')
	if changed, err := storage.DeleteUnsyncIfPresent(ctx, repo, "alice", "main"); err != nil || changed {
		t.Fatalf("absent deletion: changed=%v err=%v", changed, err)
	}
	pointer := domain.Unsync{RepoID: repo, User: "alice", Branch: "main", Target: target}
	raw, err := json.Marshal(pointer)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(storage.repoDir(repo), "unsync")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old-pointer.json", "duplicate-pointer.json"} {
		if err := os.WriteFile(filepath.Join(directory, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := storage.DeleteUnsyncIfPresent(ctx, repo, "alice", "main"); err != nil || !changed {
		t.Fatalf("legacy deletion: changed=%v err=%v", changed, err)
	}
	if changed, err := storage.DeleteUnsyncIfPresent(ctx, repo, "alice", "main"); err != nil || changed {
		t.Fatalf("replayed deletion: changed=%v err=%v", changed, err)
	}
	pointers, err := storage.ListUnsyncs(ctx, repo)
	if err != nil || len(pointers) != 0 {
		t.Fatalf("legacy pointers remain: %+v %v", pointers, err)
	}
}
