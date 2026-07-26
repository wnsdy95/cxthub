package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func h(c byte) domain.ContentHash {
	sum := sha256.Sum256([]byte{c})
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

// TestUpdateRefAppend fixes the diverged push path for --append(overlay graft):
// Reject (default) → append to head with GraftParents overlay + ff → existing head becomes ancestor of new target.
// Core invariant: Parents (original) are never changed — local/server maintain same Parents for the same snapshot ID to prevent replica disagreement (permanent divergence removal).
// Diverge with common ancestor and missing merge are rejected/handled by append.
func TestUpdateRefAppend(t *testing.T) {
	ctx := context.Background()
	fs := store.NewFSStore(t.TempDir())
	engine := gitengine.NewEngine(fs)
	svc := NewService(fs, fs, nil, engine, nil)

	repoID := h('0')
	put := func(id domain.ContentHash, parents ...domain.ContentHash) {
		if err := fs.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repoID, Branch: "main", DocHash: id, Parents: parents}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	main := func(target domain.ContentHash, force, append_ bool) (inbound.UpdateRefOutput, error) {
		return svc.UpdateRef(ctx, inbound.UpdateRefInput{
			RepoID: repoID,
			Ref:    domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: target},
			Force:  force, Append: append_,
		})
	}

	// Existing chain A←B, main→B.
	a, b := h('a'), h('b')
	put(a)
	put(b, a)
	if out, err := main(b, false, false); err != nil || out.Result != inbound.RefFastForward {
		t.Fatalf("initial ff: %v %v", out.Result, err)
	}

	// Irrelevant chain C←D: basic push is rejected.
	c, d := h('c'), h('d')
	put(c)
	put(d, c)
	if _, err := main(d, false, false); !errors.Is(err, domain.ErrNonFastForward) {
		t.Fatalf("diverged default not rejected: %v", err)
	}

	// --append: overlay B (head) onto root C with ff — existing history (A←B) is preserved as ancestor of D.
	// C.Parents must remain empty (original unchanged), graft only with GraftParents.
	out, err := main(d, false, true)
	if err != nil || out.Result != inbound.RefAppended {
		t.Fatalf("append failed: %v %v", out.Result, err)
	}
	if snap, _ := fs.GetSnapshot(ctx, repoID, c); len(snap.Parents) != 0 || len(snap.GraftParents) != 1 || snap.GraftParents[0] != b || !snap.Grafted {
		t.Fatalf("overlay graft failed: C.parents=%v graft_parents=%v grafted=%v want parents=[] graft=[%s] true", snap.Parents, snap.GraftParents, snap.Grafted, b)
	}
	if ok, _ := engine.IsAncestor(ctx, repoID, a, d); !ok {
		t.Fatal("append after A is not ancestor of D (history lost)")
	}
	if ref, _ := fs.GetRef(ctx, repoID, domain.RefBranch, "main"); ref.Target != d {
		t.Fatalf("ref not moved: %s", ref.Target)
	}

	// Diverge with common ancestor (E branches from A): overlay rebase-graft — E's original parent A is
	// unchanged (invariant) and head(D) is grafted onto it. A remains as
	// (1) E's direct parent and
	// (2) ancestor via D (preservation of reachability + replica agreement).
	e := h('e')
	put(e, a)
	if out, err := main(e, false, true); err != nil || out.Result != inbound.RefAppended {
		t.Fatalf("common ancestor diverge overlay rebase-graft failed: %v %v", out.Result, err)
	}
	if snap, _ := fs.GetSnapshot(ctx, repoID, e); len(snap.Parents) != 1 || snap.Parents[0] != a || len(snap.GraftParents) != 1 || snap.GraftParents[0] != d || !snap.Grafted {
		t.Fatalf("overlay rebase-graft failed: E.parents=%v graft_parents=%v grafted=%v want parents=[%s] graft=[%s] true", snap.Parents, snap.GraftParents, snap.Grafted, a, d)
	}
	if ok, _ := engine.IsAncestor(ctx, repoID, a, e); !ok {
		t.Fatal("overlay rebase-graft after A is not ancestor of E (reachability lost)")
	}
	if ref, _ := fs.GetRef(ctx, repoID, domain.RefBranch, "main"); ref.Target != e {
		t.Fatalf("rebase-graft after ref not moved: %s", ref.Target)
	}

	// Reject append if missing ancestor snapshot (F's parent G does not exist).
	f := h('f')
	put(f, h('9'))
	if _, err := main(f, false, true); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Missing ancestor passed: %v", err)
	}
}

// TestOverlayGraftNoOrphanNoDivergence ensures that an overlay graft does not create orphans (fsck 0 unreachable — removing the cause of the break in the web graph) and that the subsequent normal ancestor continues fast-forwarding from it without leaving any permanent divergence.
func TestOverlayGraftNoOrphanNoDivergence(t *testing.T) {
	ctx := context.Background()
	fs := store.NewFSStore(t.TempDir())
	engine := gitengine.NewEngine(fs)
	svc := NewService(fs, fs, nil, engine, nil)
	repoID := h('0')
	put := func(id domain.ContentHash, parents ...domain.ContentHash) {
		if err := fs.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repoID, Branch: "main", DocHash: id, Parents: parents}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	main := func(target domain.ContentHash, append_ bool) (inbound.UpdateRefOutput, error) {
		return svc.UpdateRef(ctx, inbound.UpdateRefInput{
			RepoID: repoID, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: target}, Append: append_,
		})
	}

	// Two branches diverged from shared ancestor X: server head is X←P(main→p), local is X←Q←R.
	x, p, q, r := h('x'), h('p'), h('q'), h('r')
	put(x)
	put(p, x)
	if _, err := main(p, false); err != nil {
		t.Fatalf("Head setting: %v", err)
	}
	put(q, x)
	put(r, q)
	if out, err := main(r, true); err != nil || out.Result != inbound.RefAppended {
		t.Fatalf("overlay graft: %v %v", out.Result, err)
	}

	// Q's original parent X is immutable (replica agreement), head P is added only via the overlay.
	if snap, _ := fs.GetSnapshot(ctx, repoID, q); len(snap.Parents) != 1 || snap.Parents[0] != x {
		t.Fatalf("Q.Parents contaminated (replica agreement broken): %v want [%s]", snap.Parents, x)
	}

	// fsck: 0 orphans — X, P, Q, R all reachable from main(→R) (P is Q's overlay parent via Q).
	rep, err := svc.Fsck(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unreachable) != 0 {
		t.Fatalf("Orphan after graft: %v (overlay reachability failure)", rep.Unreachable)
	}
	if rep.Total != 4 || rep.Reachable != 4 {
		t.Fatalf("total/reachable=%d/%d want 4/4", rep.Total, rep.Reachable)
	}

	// No permanent divergence: S, a normal capture, fast-forwards on R.
	s := h('s')
	put(s, r)
	if out, err := main(s, false); err != nil || out.Result != inbound.RefFastForward {
		t.Fatalf("Graft failed to continue commit ff (divergence remains): %v %v", out.Result, err)
	}
}
