package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func inbound_JoinInput(repo domain.ContentHash, branch string, snap domain.ContentHash, all bool) inbound.JoinInput {
	return inbound.JoinInput{RepoID: repo, TargetBranch: branch, Snapshot: snap, IncludeDescendants: all}
}

// TestJoinSnapshot fixes the contract for drag-and-drop join.
//
// Initial graph (sibling fork — real case 578f):
//
//	main:  H ← P, H -graft→ X ← P          session fork: T ← X
//
// Full join: main head advances to T, H is preserved as X's graft parent.
// Partial join: main head advances to X, T remains as scoped session ref and branches from X.
func TestJoinSnapshot(t *testing.T) {
	ctx := context.Background()

	mkSession := func(st *store.FSStore, repo domain.ContentHash, seed, session string, parents ...domain.ContentHash) domain.ContentHash {
		id := domain.HashContent([]byte(seed))
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Provider: "claude", SessionID: session, Parents: parents}
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mk := func(st *store.FSStore, repo domain.ContentHash, seed string, parents ...domain.ContentHash) domain.ContentHash {
		return mkSession(st, repo, seed, "session-a", parents...)
	}
	setup := func(t *testing.T) (*Service, *store.FSStore, domain.ContentHash, map[string]domain.ContentHash) {
		st := store.NewFSStore(t.TempDir())
		svc := NewService(st, st, nil, gitengine.NewEngine(st), st)
		repo := domain.HashContent([]byte("join-repo"))
		p := mk(st, repo, "P")
		h := mk(st, repo, "H", p)
		x := mk(st, repo, "X", p)
		tip := mk(st, repo, "T", x)
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: h}, ""); err != nil {
			t.Fatal(err)
		}
		// Like the real terrain, X..T is not in main's natural history but is shared via auto-graft to the same tip T. A join is not an API that promotes dangling objects to refs, so a test terrain where only X is grafted is incorrect.
		if err := st.AddGraftParents(ctx, repo, h, []domain.ContentHash{tip}); err != nil {
			t.Fatal(err)
		}
		return svc, st, repo, map[string]domain.ContentHash{"P": p, "H": h, "X": x, "T": tip}
	}

	t.Run("Full join: head=T, H is preserved as graft", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true))
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if out.Head != n["T"] || out.ForkBranch != "" {
			t.Fatalf("out = %+v", out)
		}
		ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if ref.Target != n["T"] {
			t.Fatalf("main head = %s, want T", ref.Target)
		}
		x, _ := st.GetSnapshot(ctx, repo, n["X"])
		if len(x.GraftParents) != 1 || x.GraftParents[0] != n["H"] {
			t.Fatalf("H not preserved: graft=%v", x.GraftParents)
		}
	})

	t.Run("Partial join: head=X, remaining branch forks/* from X", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], false))
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		if out.Head != n["X"] || !strings.HasPrefix(out.ForkBranch, domain.SessionRefPrefix("main")) {
			t.Fatalf("out = %+v", out)
		}
		ref, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if ref.Target != n["X"] {
			t.Fatalf("main head = %s, want X", ref.Target)
		}
		fork, err := st.GetRef(ctx, repo, domain.RefSession, out.ForkBranch)
		if err != nil || fork.Target != n["T"] {
			t.Fatalf("Remaining branch ref exceeds: %v %v", fork, err)
		}
		// Branch point verification: T's parent chain passes through X (natural parent — not a graft).
		tip, _ := st.GetSnapshot(ctx, repo, n["T"])
		if len(tip.Parents) != 1 || tip.Parents[0] != n["X"] {
			t.Fatalf("T does not branch from X: %v", tip.Parents)
		}
	})

	t.Run("T retained by a partial join can rejoin the same Git branch", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		partial, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], false))
		if err != nil {
			t.Fatalf("partial join: %v", err)
		}
		if _, err := st.GetRef(ctx, repo, domain.RefSession, partial.ForkBranch); err != nil {
			t.Fatalf("No remaining session ref: %v", err)
		}
		// session ref is not on another git branch. Treating it as a branch would block this second join with a cross-branch conflict, preventing the original UX from being completed.
		joined, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["T"], false))
		if err != nil {
			t.Fatalf("T rejoin: %v", err)
		}
		if joined.Head != n["T"] {
			t.Fatalf("head=%s want T=%s", joined.Head, n["T"])
		}
		listed, err := svc.List(ctx, inbound.ListSnapshotsInput{RepoID: repo})
		if err != nil {
			t.Fatal(err)
		}
		for _, snap := range listed {
			if snap.ID != n["T"] {
				continue
			}
			for _, branch := range snap.Branches {
				if branch == partial.ForkBranch {
					t.Fatalf("session ref leaked as git branch membership: %v", snap.Branches)
				}
			}
		}
	})

	t.Run("Already included in history → ErrConflict", func(t *testing.T) {
		svc, _, repo, n := setup(t)
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["P"], false)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("Repositioning same branch (578f form): Incoming graft supersede with head=X", func(t *testing.T) {
		// Real-world scenario: main is already auto-grafted to the same X on the other side. main: H2 ← H(graft→X) ← P other side: X ← P
		svc, st, repo, n := setup(t)
		if err := st.AddGraftParents(ctx, repo, n["H"], []domain.ContentHash{n["X"]}); err != nil {
			t.Fatal(err) // Re-create auto-graft: H reaches X as a reachable ancestor
		}
		h2 := mk(st, repo, "H2", n["H"])
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: h2}, n["H"]); err != nil {
			t.Fatal(err)
		}
		// X is only connected to main via graft — rebase (full join: X..T) must succeed.
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true))
		if err != nil {
			t.Fatalf("rebase join failed: %v", err)
		}
		if out.Head != n["T"] {
			t.Fatalf("head = %s, want T", out.Head)
		}
		// Incoming edge supersede check: X is removed from H's graft.
		hSnap, _ := st.GetSnapshot(ctx, repo, n["H"])
		for _, g := range hSnap.GraftParents {
			if g == n["X"] {
				t.Fatalf("supersede failed — cycle residual: %v", hSnap.GraftParents)
			}
		}
		// X preserves previous head (h2) via graft — no loss in reach set.
		xSnap, _ := st.GetSnapshot(ctx, repo, n["X"])
		found := false
		for _, g := range xSnap.GraftParents {
			if g == h2 {
				found = true
			}
		}
		if !found {
			t.Fatalf("previous head not preserved: %v", xSnap.GraftParents)
		}
	})

	t.Run("cross git branch join rejection", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		other := domain.HashContent([]byte("other-branch-snap"))
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: other, DocHash: other, RepoID: repo,
			Branch: "feature", Provider: "claude"}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "main", other, false)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("join error from different git branch session: err=%v, want conflict", err)
		}
		_ = n
	})

	t.Run("accept Git branch membership projected from the reflog", func(t *testing.T) {
		st := store.NewFSStore(t.TempDir())
		svc := NewService(st, st, nil, gitengine.NewEngine(st), st)
		repo := domain.HashContent([]byte("join-membership-repo"))
		p := mkSession(st, repo, "membership-P", "session-a")
		h := mkSession(st, repo, "membership-H", "session-a", p)
		x := domain.HashContent([]byte("membership-X"))
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: x, DocHash: x, RepoID: repo,
			Branch: "feature", Provider: "claude", SessionID: "session-b", Parents: []domain.ContentHash{p}}); err != nil {
			t.Fatal(err)
		}
		// Content dedup made feature the first-writer Branch, but this snapshot was also a historical
		// target of the main ref, so it belongs to main too. Checking only scalar Branch would reject this valid join.
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: x}, ""); err != nil {
			t.Fatal(err)
		}
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: h}, x); err != nil {
			t.Fatal(err)
		}
		if err := st.AddGraftParents(ctx, repo, h, []domain.ContentHash{x}); err != nil {
			t.Fatal(err)
		}
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", x, false))
		if err != nil {
			t.Fatalf("reflog-membership join failed: %v", err)
		}
		if out.Head != x {
			t.Fatalf("head=%s want %s", out.Head, x)
		}
	})

	t.Run("reject without mutation when another Git branch reaches the moved snapshot", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		u := domain.HashContent([]byte("feature-U"))
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: u, DocHash: u, RepoID: repo,
			Branch: "feature", Provider: "claude", Grafted: true, GraftParents: []domain.ContentHash{n["X"]}}); err != nil {
			t.Fatal(err)
		}
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "feature", RepoID: repo, Target: u}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("cross-branch reachable snapshot join err=%v", err)
		}
		got, err := st.GetSnapshot(ctx, repo, u)
		if err != nil || len(got.GraftParents) != 1 || got.GraftParents[0] != n["X"] {
			t.Fatalf("graft on another branch changed: %+v %v", got.GraftParents, err)
		}
	})

	t.Run("reject when a session ref scoped to another Git branch reaches the moved snapshot", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
			Kind: domain.RefSession, Name: domain.SessionRefPrefix("feature") + "x", RepoID: repo, Target: n["X"],
		}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("foreign scoped session join err=%v", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if main.Target != n["H"] {
			t.Fatalf("foreign scoped session rejection moved main: %s", main.Target)
		}
	})

	t.Run("reject without mutation when another Git branch shares the supersede source", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		if err := st.AddGraftParents(ctx, repo, n["H"], []domain.ContentHash{n["X"]}); err != nil {
			t.Fatal(err)
		}
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "feature", RepoID: repo, Target: n["H"]}, ""); err != nil {
			t.Fatal(err)
		}
		before, _ := st.GetSnapshot(ctx, repo, n["H"])
		h2 := mk(st, repo, "shared-H2", n["H"])
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: h2}, n["H"]); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("shared source supersede err=%v", err)
		}
		h, _ := st.GetSnapshot(ctx, repo, n["H"])
		if !reflect.DeepEqual(h.GraftParents, before.GraftParents) || h.GraftSeq != before.GraftSeq {
			t.Fatalf("rejected join changed graft: before=%v/%d after=%v/%d", before.GraftParents, before.GraftSeq, h.GraftParents, h.GraftSeq)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if main.Target != h2 {
			t.Fatalf("Rejected join moves main: %s", main.Target)
		}
	})

	t.Run("SessionID differs but first-parent natural descendant moves to same session", func(t *testing.T) {
		st := store.NewFSStore(t.TempDir())
		svc := NewService(st, st, nil, gitengine.NewEngine(st), st)
		repo := domain.HashContent([]byte("join-cross-session-repo"))
		p := mkSession(st, repo, "cross-P", "session-a")
		h := mkSession(st, repo, "cross-H", "session-a", p)
		x := mkSession(st, repo, "cross-X", "session-a", p)
		tip := mkSession(st, repo, "cross-T", "session-b", x)
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: h}, ""); err != nil {
			t.Fatal(err)
		}
		if err := st.AddGraftParents(ctx, repo, h, []domain.ContentHash{tip}); err != nil {
			t.Fatal(err)
		}
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", x, true))
		if err != nil {
			t.Fatalf("SessionID boundary blocks join: %v", err)
		}
		if out.Head != tip {
			t.Fatalf("head=%s want natural tip T=%s", out.Head, tip)
		}
	})

	t.Run("Dangling snapshot on X cannot be promoted to branch ref via join", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		if err := st.SetGraftParents(ctx, repo, n["H"], nil); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("unattached join err=%v", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if main.Target != n["H"] {
			t.Fatalf("rejected unattached join moved main: %s", main.Target)
		}
	})

	t.Run("an unpushed natural descendant above attached X cannot be promoted by a full join", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		u := mk(st, repo, "unpushed-U", n["T"])
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true))
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("unpushed descendant join out=%+v err=%v", out, err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		if main.Target != n["H"] {
			t.Fatalf("rejected descendant promotion moved main: %s", main.Target)
		}
		if _, err := st.GetRef(ctx, repo, domain.RefSession, domain.SessionRefPrefix("main")+strings.TrimPrefix(string(u), "sha256:")[:10]); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("rejected descendant promotion created session ref: %v", err)
		}
	})

	t.Run("Pending hook leaf commit segments are not mixed with join, direct join also rejected", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		pendingID := domain.HashContent([]byte("active-hook-after-T"))
		if err := st.PutSnapshot(ctx, domain.Snapshot{
			ID: pendingID, DocHash: pendingID, RepoID: repo, Branch: "main",
			Provider: "claude", Message: domain.HookMessagePrefix + " checkpoint",
			Parents: []domain.ContentHash{n["T"]},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutPending(ctx, repo, domain.Pending{
			RepoID: repo, SessionID: "active-session", Branch: "main", Target: pendingID,
		}); err != nil {
			t.Fatal(err)
		}
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true))
		if err != nil {
			t.Fatalf("Hidden pending leaf blocks normal commit join: %v", err)
		}
		if out.Head != n["T"] {
			t.Fatalf("Pending moves to head: %s", out.Head)
		}
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "main", pendingID, false)); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("pending hook join error=%v", err)
		}
	})

	t.Run("stale pending of shared snapshot does not block join", func(t *testing.T) {
		svc, st, repo, n := setup(t)
		// auto-grafting X reaches main. The remaining pending pointer should not affect
		// the UI as an uncommitted change, and should not influence join permissions/segmentation.
		if err := st.AddGraftParents(ctx, repo, n["H"], []domain.ContentHash{n["X"]}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutPending(ctx, repo, domain.Pending{
			RepoID: repo, SessionID: "stale-shared-session", Branch: "main", Target: n["X"],
		}); err != nil {
			t.Fatal(err)
		}
		out, err := svc.Join(ctx, inbound_JoinInput(repo, "main", n["X"], true))
		if err != nil {
			t.Fatalf("stale pending blocks join: %v", err)
		}
		if out.Head != n["T"] {
			t.Fatalf("head=%s want T=%s", out.Head, n["T"])
		}
	})

	t.Run("HEAD is not a merge target → reject", func(t *testing.T) {
		svc, _, repo, n := setup(t)
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "HEAD", n["X"], false)); err == nil {
			t.Fatal("HEAD branch merge is allowed")
		}
	})

	t.Run("non-existent branch (no head) → ErrNotFound", func(t *testing.T) {
		svc, st, repo, _ := setup(t)
		ghost := domain.HashContent([]byte("ghost-snap"))
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: ghost, DocHash: ghost, RepoID: repo,
			Branch: "ghost", Provider: "claude"}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Join(ctx, inbound_JoinInput(repo, "ghost", ghost, false)); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("atomic application pre-validation failure does not leave fork ref or graft", func(t *testing.T) {
		_, st, repo, n := setup(t)
		err := st.ApplyJoin(ctx, outbound.JoinMutation{
			RepoID: repo, Branch: "main", Source: n["X"], Segment: []domain.ContentHash{n["X"], n["T"]}, ExpectedHead: n["H"], NewHead: n["X"],
			ForkName: domain.SessionRefPrefix("main") + "atomic", ForkTip: n["T"],
			Grafts: []outbound.GraftPatch{{SnapshotID: n["X"], ExpectedSeq: 99, Parents: []domain.ContentHash{n["H"]}}},
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err=%v", err)
		}
		if _, err := st.GetRef(ctx, repo, domain.RefSession, domain.SessionRefPrefix("main")+"atomic"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("Failed join left fork ref: %v", err)
		}
		x, _ := st.GetSnapshot(ctx, repo, n["X"])
		if len(x.GraftParents) != 0 || x.GraftSeq != 0 {
			t.Fatalf("Failed join left graft: %+v", x)
		}
	})

	t.Run("ApplyJoin atomic boundary revalidates concurrent cross-branch reachability", func(t *testing.T) {
		_, st, repo, n := setup(t)
		if err := st.CompareAndSwapRef(ctx, repo,
			domain.Ref{Kind: domain.RefBranch, Name: "feature", RepoID: repo, Target: n["X"]}, ""); err != nil {
			t.Fatal(err)
		}
		err := st.ApplyJoin(ctx, outbound.JoinMutation{
			RepoID: repo, Branch: "main", Source: n["X"], Segment: []domain.ContentHash{n["X"]}, ExpectedHead: n["H"], NewHead: n["X"],
			Grafts: []outbound.GraftPatch{{SnapshotID: n["X"], ExpectedSeq: 0, Parents: []domain.ContentHash{n["H"]}}},
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("cross-branch race was not rejected: %v", err)
		}
		main, _ := st.GetRef(ctx, repo, domain.RefBranch, "main")
		x, _ := st.GetSnapshot(ctx, repo, n["X"])
		if main.Target != n["H"] || x.GraftSeq != 0 || len(x.GraftParents) != 0 {
			t.Fatalf("rejected atomic join changed state: main=%s x=%+v", main.Target, x)
		}
	})
}
