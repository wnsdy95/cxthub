package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func joinPreviewFixture(t *testing.T) (*Service, *store.FSStore, inbound.JoinPreviewInput, map[string]domain.ContentHash) {
	t.Helper()
	ctx := systemTestContext()
	st := store.NewFSStore(t.TempDir())
	repositoryRecord := domain.Repository{ID: domain.NewID("ws_"), OwnerID: domain.NewID("user_"), Slug: "join", Name: "Join"}
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte("preview-repo"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: repositoryRecord.ID}); err != nil {
		t.Fatal(err)
	}
	n := map[string]domain.ContentHash{}
	for _, name := range []string{"P", "H", "X", "T"} {
		n[name] = domain.HashContent([]byte(name))
	}
	for _, name := range []string{"P", "H", "X", "T"} {
		var parents []domain.ContentHash
		if name != "P" {
			parents = []domain.ContentHash{n["P"]}
		}
		if name == "T" {
			parents = []domain.ContentHash{n["X"]}
		}
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: n[name], RepoID: repo, DocHash: n[name], Branch: "main", Parents: parents}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: n["H"]}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddGraftParents(ctx, repo, n["H"], []domain.ContentHash{n["T"]}); err != nil {
		t.Fatal(err)
	}
	return NewService(st, st, nil, nil, st), st, inbound.JoinPreviewInput{ActorID: repositoryRecord.OwnerID, RepoID: repo, Snapshot: n["X"]}, n
}

func confirmPreview(in inbound.JoinPreviewInput, p inbound.JoinPreviewOutput, all bool) inbound.ConfirmJoinInput {
	rev := p.OnlyRevision
	if all {
		rev = p.AllRevision
	}
	return inbound.ConfirmJoinInput{ActorID: in.ActorID, JoinInput: inbound.JoinInput{RepoID: in.RepoID, TargetBranch: p.Branch, BranchID: p.BranchID, Snapshot: in.Snapshot, ExpectedHead: p.ExpectedHead, PlanRevision: rev, IncludeDescendants: all}}
}

func TestJoinPreviewConfirmation(t *testing.T) {
	ctx := systemTestContext()
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "whole"}[all], func(t *testing.T) {
			svc, st, in, n := joinPreviewFixture(t)
			p, err := svc.PreviewJoin(ctx, in)
			if err != nil || p.Reason != "" || p.Descendants != 1 || p.Tip != n["T"] || p.ExpectedHead != n["H"] || len(p.DropTargets) != 2 {
				t.Fatalf("preview %+v %v", p, err)
			}
			out, err := svc.ConfirmJoin(ctx, confirmPreview(in, p, all))
			if err != nil {
				t.Fatal(err)
			}
			want := n["X"]
			if all {
				want = n["T"]
			}
			if out.Head != want {
				t.Fatalf("head %s", out.Head)
			}
			x, _ := st.GetSnapshot(ctx, in.RepoID, n["X"])
			if len(x.Parents) != 1 || x.Parents[0] != n["P"] || len(x.GraftParents) != 1 || x.GraftParents[0] != n["H"] {
				t.Fatalf("lost history: %+v", x)
			}
		})
	}
}

func TestJoinPreviewRejectsChangedApproval(t *testing.T) {
	ctx := systemTestContext()
	for _, change := range []string{"new child", "foreign ref", "head", "missing approval", "changed choice", "revoked access"} {
		t.Run(change, func(t *testing.T) {
			svc, st, in, n := joinPreviewFixture(t)
			p, err := svc.PreviewJoin(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			cmd := confirmPreview(in, p, true)
			switch change {
			case "new child":
				z := domain.HashContent([]byte("Z"))
				if err := st.PutSnapshot(ctx, domain.Snapshot{ID: z, RepoID: in.RepoID, DocHash: z, Branch: "main", Parents: []domain.ContentHash{n["T"]}}); err != nil {
					t.Fatal(err)
				}
				if err := st.CompareAndSwapRef(ctx, in.RepoID, domain.Ref{Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "new", Target: z}, ""); err != nil {
					t.Fatal(err)
				}
			case "foreign ref":
				if err := st.CompareAndSwapRef(ctx, in.RepoID, domain.Ref{Kind: domain.RefBranch, Name: "other", Target: n["T"]}, ""); err != nil {
					t.Fatal(err)
				}
			case "head":
				if err := st.CompareAndSwapRef(ctx, in.RepoID, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: n["T"]}, n["H"]); err != nil {
					t.Fatal(err)
				}
			case "changed choice":
				cmd.IncludeDescendants = false
			case "missing approval":
				cmd.PlanRevision = ""
			case "revoked access":
				repo, _ := st.GetRepo(ctx, in.RepoID)
				repositoryRecord, _ := st.GetRepository(ctx, repo.RepositoryID)
				repositoryRecord.Archived = true
				if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
					t.Fatal(err)
				}
			}
			_, err = svc.ConfirmJoin(ctx, cmd)
			want := domain.ErrJoinPreviewChanged
			if change == "revoked access" {
				want = domain.ErrForbidden
			}
			if !errors.Is(err, want) {
				t.Fatalf("error %v want %v", err, want)
			}
			x, _ := st.GetSnapshot(ctx, in.RepoID, n["X"])
			if x.GraftSeq != 0 {
				t.Fatal("rejected confirmation changed graph")
			}
		})
	}
}

func TestJoinPreviewStoreRejectsRaceAfterPlanning(t *testing.T) {
	ctx := systemTestContext()
	svc, st, in, n := joinPreviewFixture(t)
	g, err := svc.loadJoinGraph(ctx, in.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := domain.PlanJoin(g, domain.JoinRequest{RepoID: in.RepoID, Branch: "main", Source: in.Snapshot, IncludeDescendants: true})
	if err != nil {
		t.Fatal(err)
	}
	m, err := p.Mutation("")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CompareAndSwapRef(ctx, in.RepoID, domain.Ref{Kind: domain.RefBranch, Name: "other", Target: n["P"]}, ""); err != nil {
		t.Fatal(err)
	}
	// This ref does not touch the segment or source graft CAS. Approval still
	// describes an older published graph and must be checked inside ApplyJoin.
	if err := st.ApplyJoin(ctx, m); !errors.Is(err, domain.ErrJoinPreviewChanged) {
		t.Fatalf("stale scope accepted: %v", err)
	}
}
