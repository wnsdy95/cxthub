package app

import (
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// TestPendingTailOf fixes the commit tail selection rule for "inject" seeds:
// It adopts the latest pending reachable from the head and ignores pending from other branches.
func TestPendingTailOf(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := NewLoadSessionService(st, nil, nil, nil, nil, nil)
	repoID := string(domain.HashContent([]byte("repo")))

	h, p, q := gh('h'), gh('p'), gh('q')
	put := func(id domain.ContentHash, parents ...domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repoID, Branch: "main", Parents: parents}); err != nil {
			t.Fatal(err)
		}
	}
	put(h)
	put(p, h) // Capture hook for connecting head
	put(q)    // Ignore irrelevant branches (e.g., residual states)

	// No pending → maintain head.
	if got := svc.pendingTailOf(ctx, repoID, "main", h, ""); got != h {
		t.Fatalf("No pending: got %s want %s", got, h)
	}
	// Ignore pending from irrelevant branches.
	if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: "sq", Branch: "main", Target: q, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got := svc.pendingTailOf(ctx, repoID, "main", h, ""); got != h {
		t.Fatalf("Irrelevant branch pending adopted: got %s", got)
	}
	// Ignore pending from other branches even if connected to head (to prevent cross-branch leakage).
	if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: "sf", Branch: "feature", Target: p, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got := svc.pendingTailOf(ctx, repoID, "main", h, ""); got != h {
		t.Fatalf("Another branch pending adopted: got %s", got)
	}
	// Joining head of the same branch → tail adopted.
	if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: "sp", Branch: "main", Target: p, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got := svc.pendingTailOf(ctx, repoID, "main", h, ""); got != p {
		t.Fatalf("Tail not adopted: got %s want %s", got, p)
	}

	// PR append moves main to a sibling lineage B while the exact live session
	// remains P→H. Exact session identity preserves P; an anonymous load must not
	// loosen reachability and accidentally revive it.
	b := gh('b')
	put(b, h)
	if got := svc.pendingTailOf(ctx, repoID, "main", b, ""); got != b {
		t.Fatalf("anonymous diverged pending adopted: got %s want %s", got, b)
	}
	if got := svc.pendingTailOf(ctx, repoID, "main", b, "sp"); got != p {
		t.Fatalf("live session tail lost after ref append: got %s want %s", got, p)
	}
}
