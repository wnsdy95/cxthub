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
	deleteDocCalls int
}

func (s *deleteFailStore) DeleteSnapshot(context.Context, domain.ContentHash, domain.ContentHash) error {
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
	ctx := context.Background()
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

// TestFsckDanglingGraftParent: Audit dangling nodes like reachability —
// edges pointing to nonexistent snapshots in overlay grafts should be reported as corruption (overlay grafts can create exactly this corruption class that the auditor might have missed).
func TestFsckDanglingGraftParent(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
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
// Conversely, hook leaves that cannot be reached by any ref are still deleted.
func TestGCHookLeafReachabilityGuard(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("g")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Hook leaf L above commit C is stacked and main→C. L is an ancestor of main (not directly the target).
	must(st.PutSnapshot(ctx, domain.Snapshot{ID: hh("l"), RepoID: repo, Message: "hook: session", DocHash: hh("l")}))
	must(st.PutSnapshot(ctx, domain.Snapshot{ID: hh("c"), RepoID: repo, Parents: []domain.ContentHash{hh("l")}, Message: "commit [git abc]", DocHash: hh("c")}))
	must(st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: hh("c")}, ""))

	svc.gcHookLeaf(ctx, repo, hh("l"), hh("c")) // Reachable → Must be preserved
	if _, err := st.GetSnapshot(ctx, repo, hh("l")); err != nil {
		t.Fatalf("Reachable hook leaf deleted (invariant R violation): %v", err)
	}

	// Contrast: Any unreachable hook leaf is deleted.
	must(st.PutSnapshot(ctx, domain.Snapshot{ID: hh("m"), RepoID: repo, Message: "hook: orphan", DocHash: hh("m")}))
	svc.gcHookLeaf(ctx, repo, hh("m"), hh("c"))
	if _, err := st.GetSnapshot(ctx, repo, hh("m")); err == nil {
		t.Fatal("Unreachable hook leaf not deleted")
	}
}

func TestGCHookLeafKeepsDocWhenSnapshotDeleteFails(t *testing.T) {
	ctx := context.Background()
	base := store.NewFSStore(t.TempDir())
	st := &deleteFailStore{FSStore: base}
	svc := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(base), base)
	repo := hh("gc-delete-race")
	old := hh("gc-delete-old")
	if err := base.PutSnapshot(ctx, domain.Snapshot{ID: old, RepoID: repo, Message: "hook: pending", DocHash: old}); err != nil {
		t.Fatal(err)
	}

	svc.gcHookLeaf(ctx, repo, old, hh("replacement"))
	if st.deleteDocCalls != 0 {
		t.Fatalf("doc deletion ran after snapshot deletion failed: %d calls", st.deleteDocCalls)
	}
}
