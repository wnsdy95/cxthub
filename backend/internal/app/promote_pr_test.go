package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// TestPromoteMergedPR is a GitHub PR merged webhook → base branch promotion fix:
// git origin normalization matching · head context to base reachable (overlay graft) ·
// head branchless repo is no-op · idempotent on re-call.
func TestPromoteMergedPR(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("r")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "git@github.com:acme/proj.git"}); err != nil {
		t.Fatal(err)
	}
	// main: M ← shared head / feature: M ← F1 ← F2(tip)
	put := func(id domain.ContentHash, parents ...domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id, Parents: parents}); err != nil {
			t.Fatal(err)
		}
	}
	m, f1, f2 := hh("m"), hh("1"), hh("2")
	put(m)
	put(f1, m)
	put(f2, f1)
	for name, target := range map[string]domain.ContentHash{"main": m, "feature/x": f2} {
		if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: name, RepoID: repo, Target: target}, ""); err != nil {
			t.Fatal(err)
		}
	}

	// URL must match clone_url(https) — normalized to stored ssh origin.
	n, err := svc.PromoteMergedPR(ctx, "https://github.com/acme/proj.git", "main", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("promoted=%d, want 1", n)
	}
	ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != f2 {
		t.Fatalf("main did not move to head tip: %v %v", ref.Target, err)
	}
	// Idempotent: re-call is no-op (behind/up-to-date).
	if n2, err2 := svc.PromoteMergedPR(ctx, "https://github.com/acme/proj.git", "main", "feature/x"); err2 != nil || n2 != 0 {
		t.Fatalf("idempotent promotion failed: n=%d err=%v", n2, err2)
	}
	// Irrelevant repo URL is no-op.
	if n3, _ := svc.PromoteMergedPR(ctx, "https://github.com/other/repo", "main", "feature/x"); n3 != 0 {
		t.Fatalf("irrelevant URL promotion: %d", n3)
	}
	// The same name later represents different work. A delayed webhook must
	// not attach it just because its current ref still uses the old PR name.
	birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "first", Branch: "feature/x", Kind: "birth", Source: f2, Target: f2, CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(ctx, birth); err != nil {
		t.Fatal(err)
	}
	rename := birth
	rename.ID, rename.Kind, rename.Branch, rename.PreviousBranch, rename.BindingParent = strings.Repeat("2", 32), "rename", "feature/renamed", birth.Branch, birth.ID
	if err := st.ApplyHistoryEvent(ctx, rename); err != nil {
		t.Fatal(err)
	}
	reuse := birth
	reuse.ID, reuse.BranchID, reuse.BindingParent = strings.Repeat("3", 32), "second", rename.ID
	if err := st.ApplyHistoryEvent(ctx, reuse); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.PromoteMergedPR(ctx, "https://github.com/acme/proj.git", "main", "feature/x"); n != 0 || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("ambiguous PR promoted: %d %v", n, err)
	}
}
