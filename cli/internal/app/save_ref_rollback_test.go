package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// TestSaveNeverMovesRefBackward ensures that a branch ref does not move backward when content-hash dedup matches an existing ancestor snapshot. Without the reachability guard, repeatedly capturing an unchanged session (for example, an old Codex rollout) after other commits could roll the ref back and orphan intervening snapshots. Forward dedup for a replaceable hook leaf must continue to work because that leaf is not a head ancestor.
func TestSaveNeverMovesRefBackward(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := &SaveSessionService{store: st}
	repoID := string(domain.HashContent([]byte("repo")))

	d1, c2, leaf := gh('1'), gh('2'), gh('3')
	put := func(id domain.ContentHash, parents ...domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repoID, Branch: "main", Parents: parents}); err != nil {
			t.Fatal(err)
		}
	}
	// D1 (old codex snapshot) ← C2 (new commit snapshot) = current head.
	put(d1)
	put(c2, d1)
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: c2}); err != nil {
		t.Fatal(err)
	}

	// Backward case: docHash is ancestor of head (D1) → do not move ref.
	if !svc.reachable(ctx, repoID, c2, d1) {
		t.Fatal("reachable regression: D1 must be ancestor of C2")
	}
	// Forward case: head's child (hook leaf) → ref movement allowed.
	put(leaf, c2)
	if svc.reachable(ctx, repoID, c2, leaf) {
		t.Fatal("reachability over-detection: child leaf treated as an ancestor, blocking forward hook-leaf deduplication")
	}
}
