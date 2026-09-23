package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type deleteFailStore struct {
	*store.FSStore
	deleteSnapshotCalls int
	deleteDocCalls      int
}

func (s *deleteFailStore) DeleteSnapshot(context.Context, domain.ContentHash, domain.ContentHash) error {
	s.deleteSnapshotCalls++
	return errors.New("injected snapshot delete failure")
}

func (s *deleteFailStore) DeleteDoc(ctx context.Context, repoID, hash domain.ContentHash) error {
	s.deleteDocCalls++
	return s.FSStore.DeleteDoc(ctx, repoID, hash)
}

func newFsckSvc(t *testing.T) (*Service, *store.FSStore) {
	t.Helper()
	st := store.NewFSStore(t.TempDir())
	return NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st), st
}

func hh(c string) domain.ContentHash {
	sum := sha256.Sum256([]byte(c))
	return domain.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

// TestFsckReachability: Reachability audit — classification of reachable/unreachable/root/dangling-parent nodes.
func TestFsckReachability(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("r")
	mk := func(id string, parents ...domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: hh(id), RepoID: repo, Parents: parents, DocHash: hh(id)}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a")          // parentless root
	mk("b", hh("a")) // b→a
	mk("c", hh("b")) // c→b (main tip)
	mk("d", hh("b")) // d→b (unreachable sibling)
	mk("e", hh("z")) // e→nonexistent z (dangling parent + unreachable)
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: hh("c")}, ""); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Fsck(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 5 || rep.Reachable != 3 { // c,b,a reachable
		t.Fatalf("total/reachable=%d/%d, want 5/3", rep.Total, rep.Reachable)
	}
	if len(rep.Roots) != 1 || rep.Roots[0] != hh("a") {
		t.Fatalf("roots=%v, want [a]", rep.Roots)
	}
	unreach := map[domain.ContentHash]bool{}
	for _, u := range rep.Unreachable {
		unreach[u] = true
	}
	if len(rep.Unreachable) != 2 || !unreach[hh("d")] || !unreach[hh("e")] {
		t.Fatalf("unreachable=%v, want {d,e}", rep.Unreachable)
	}
	if len(rep.DanglingParents) != 1 || rep.DanglingParents[0].Snapshot != hh("e") || rep.DanglingParents[0].Missing != hh("z") {
		t.Fatalf("dangling=%v, want [{e,z}]", rep.DanglingParents)
	}
}

func TestFsckTreatsPendingAsReachabilityRoot(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("pending-root-repo")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: hh("base"), RepoID: repo, DocHash: hh("base")}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: hh("pending"), RepoID: repo, DocHash: hh("pending"), Parents: []domain.ContentHash{hh("base")}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "session", Branch: "main", Target: hh("pending")}); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Fsck(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reachable != 2 || len(rep.Unreachable) != 0 {
		t.Fatalf("pending reachability = %d, unreachable=%v", rep.Reachable, rep.Unreachable)
	}
}

// TestFsckDanglingGraftParent: Audit dangling nodes like reachability —
// edges pointing to nonexistent snapshots in overlay grafts should be reported as corruption (overlay grafts can create exactly this corruption class that the auditor might have missed).
func TestFsckDanglingGraftParent(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("q")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: hh("a"), RepoID: repo, DocHash: hh("a")}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: hh("b"), RepoID: repo, DocHash: hh("b"),
		Parents: []domain.ContentHash{hh("a")}, GraftParents: []domain.ContentHash{hh("z")}, Grafted: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: hh("b")}, ""); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Fsck(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DanglingParents) != 1 || rep.DanglingParents[0].Snapshot != hh("b") || rep.DanglingParents[0].Missing != hh("z") {
		t.Fatalf("graft dangling not detected: %v, want [{b,z}]", rep.DanglingParents)
	}
	if len(rep.Unreachable) != 0 { // a is reachable as a natural parent of b
		t.Fatalf("unreachable=%v, want []", rep.Unreachable)
	}
}

// TestGCHookLeafReachabilityGuard: GC does not delete hook leaves under ref ancestors (invariant R).
// An unreachable leaf is collected only after a verified prefix extension.
func TestGCHookLeafReachabilityGuard(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("g")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	leaf := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderClaude, "first"))
	next := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderClaude, "first", "continued"))
	// The old capture is an ancestor of main, not directly its target.
	must(st.PutSnapshot(ctx, domain.Snapshot{ID: hh("c"), RepoID: repo, Parents: []domain.ContentHash{leaf.ID}, Message: "commit [git abc]", DocHash: hh("c")}))
	must(st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: hh("c")}, ""))

	svc.gcHookLeaf(ctx, repo, leaf.ID, next.ID)
	assertPendingGCCapture(t, st, leaf)

	orphanDoc := pendingGCCIR(domain.ProviderClaude, "first")
	orphanDoc.Envelope.Cwd = "/work/another-worktree"
	orphan := putPendingGCCapture(t, st, repo, orphanDoc)
	svc.gcHookLeaf(ctx, repo, orphan.ID, next.ID)
	if _, err := st.GetSnapshot(ctx, repo, orphan.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Unreachable superseded hook leaf not deleted: %v", err)
	}
	if _, err := st.GetDoc(ctx, repo, orphan.DocHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Superseded doc not deleted: %v", err)
	}
}

func TestGCHookLeafKeepsDocWhenSnapshotDeleteFails(t *testing.T) {
	ctx := systemTestContext()
	base := store.NewFSStore(t.TempDir())
	st := &deleteFailStore{FSStore: base}
	svc := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(base), base)
	repo := hh("gc-delete-race")
	old := putPendingGCCapture(t, base, repo, pendingGCCIR(domain.ProviderClaude, "first"))
	next := putPendingGCCapture(t, base, repo, pendingGCCIR(domain.ProviderClaude, "first", "continued"))

	svc.gcHookLeaf(ctx, repo, old.ID, next.ID)
	if st.deleteSnapshotCalls != 1 || st.deleteDocCalls != 0 {
		t.Fatalf("snapshot/doc delete calls=%d/%d, want 1/0", st.deleteSnapshotCalls, st.deleteDocCalls)
	}
	assertPendingGCCapture(t, base, old)
}
