package app

import (
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestContextQueryUsesFullGenerationAndExplicitPosition(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo, a, b := hh(t.Name()), hh("selected"), hh("retained")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "trunk"}); err != nil {
		t.Fatal(err)
	}
	for _, snap := range []domain.Snapshot{{ID: a, RepoID: repo, DocHash: a}, {ID: b, RepoID: repo, DocHash: b, GraftParents: []domain.ContentHash{a}}} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "trunk", RepoID: repo, Target: b}, ""); err != nil {
		t.Fatal(err)
	}
	full, err := svc.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	in := domain.ContextSelection{Position: string(a), Scope: "previous"}
	want, err := domain.SelectContext(full, in, "trunk")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.QueryContext(ctx, repo, in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || len(got.Snapshots) != 1 || got.Snapshots[0].ID != b {
		t.Fatalf("query differs from view: %+v", got)
	}
	head, err := svc.QueryContext(ctx, repo, domain.ContextSelection{Position: "HEAD", Scope: "current"})
	if err != nil || head.Position != b || len(head.Snapshots) != 2 {
		t.Fatalf("repository default position: %+v %v", head, err)
	}
}
