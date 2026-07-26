package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func gh(c byte) domain.ContentHash {
	sum := sha256.Sum256([]byte{c})
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

// TestIsAncestorCrossesGraftOverlay ensures that pull's ff determination (isAncestor) does not pass through server overlay graft edges (GraftParents). If this check fails, the server will classify ref movements as conflict-free appends, which will permanently block team members' pulls with --force until the conflict is resolved.
// (Reachability = Parents ∪ GraftParents — symmetric to server engine.parentsOf).
func TestIsAncestorCrossesGraftOverlay(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	svc := NewSyncRepoService(st, nil, nil)

	// Server-side graft form: no shared ancestor X, old head H (X's child), diverged segment Q (parent=X, graft=H) ← R (tip).
	x, hOld, q, r := gh('x'), gh('h'), gh('q'), gh('r')
	put := func(s domain.Snapshot) {
		if err := st.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	put(domain.Snapshot{ID: x, DocHash: x, Branch: "main"})
	put(domain.Snapshot{ID: hOld, DocHash: hOld, Branch: "main", Parents: []domain.ContentHash{x}})
	put(domain.Snapshot{ID: q, DocHash: q, Branch: "main", Parents: []domain.ContentHash{x},
		GraftParents: []domain.ContentHash{hOld}, Grafted: true})
	put(domain.Snapshot{ID: r, DocHash: r, Branch: "main", Parents: []domain.ContentHash{q}})

	// Old head H is only an ancestor of new tip R via the overlay edge — ff determination must be true.
	if !svc.isAncestor(ctx, hOld, r) {
		t.Fatal("isAncestor did not pass through GraftParents — graft followed by pull is misclassified as conflict")
	}
	// Natural ancestry still works.
	if !svc.isAncestor(ctx, x, r) {
		t.Fatal("natural parent chain determination regression")
	}
	// Reverse direction is still false (preventing overestimation of reachability).
	if svc.isAncestor(ctx, r, hOld) {
		t.Fatal("reverse direction is true — overestimation of reachability")
	}
}

// TestPutSnapshotMergesGraftOverlay ensures that pull adds the server's GraftParents overlay to the existing local snapshot (Parents remain unchanged — replica convergence).
func TestPutSnapshotMergesGraftOverlay(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	x, hOld, q := gh('x'), gh('h'), gh('q')

	// Local snapshot already exists (one I created — no overlay).
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: q, DocHash: q, Branch: "main", Parents: []domain.ContentHash{x}}); err != nil {
		t.Fatal(err)
	}
	// Pull received server copy: same ID, GraftParents=[H].
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: q, DocHash: q, Branch: "main", Parents: []domain.ContentHash{x},
		GraftParents: []domain.ContentHash{hOld}, Grafted: true}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSnapshot(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Parents) != 1 || got.Parents[0] != x {
		t.Fatalf("Parents modified (must remain unchanged): %v", got.Parents)
	}
	if len(got.GraftParents) != 1 || got.GraftParents[0] != hOld || !got.Grafted {
		t.Fatalf("Overlay merge failed: graft=%v grafted=%v", got.GraftParents, got.Grafted)
	}
	// Idempotent: applying the same copy again results in no duplicates.
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: q, DocHash: q, GraftParents: []domain.ContentHash{hOld}}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetSnapshot(ctx, q)
	if len(got.GraftParents) != 1 {
		t.Fatalf("Idempotent overlay merge failed: %v", got.GraftParents)
	}
}
