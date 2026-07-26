package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestUpdateRefRequiresExistingTargetOnDirectPaths(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("ref-target-repo")
	missing := hh("missing-ref-target")

	for _, tc := range []struct {
		name  string
		ref   domain.Ref
		force bool
	}{
		{"force branch", domain.Ref{Kind: domain.RefBranch, Name: "main", Target: missing}, true},
		{"new tag", domain.Ref{Kind: domain.RefTag, Name: "v1.0.0", Target: missing}, false},
		{"detached head", domain.Ref{Kind: domain.RefHead, Name: domain.HeadRefName, Target: missing}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: tc.ref, Force: tc.force})
			if !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	_, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{
		RepoID: repo,
		Ref:    domain.Ref{Kind: domain.RefHead, Name: domain.HeadRefName, Symbolic: "refs/heads/missing"},
	})
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("missing symbolic branch error = %v", err)
	}

	target := hh("existing-ref-target")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{
		RepoID: repo, Ref: domain.Ref{Kind: domain.RefTag, Name: "v1.0.0", Target: target},
	}); err != nil {
		t.Fatalf("existing tag target rejected: %v", err)
	}
}

func TestUpdateRefCannotForgeJoinSessionScope(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("session-ref-scope-repo")
	left := hh("session-ref-left")
	right := hh("session-ref-right")
	for _, id := range []domain.ContentHash{left, right} {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	name := domain.SessionRefPrefix("main") + "left"
	input := func(target domain.ContentHash, force bool) inbound.UpdateRefInput {
		return inbound.UpdateRefInput{
			RepoID: repo, Ref: domain.Ref{Kind: domain.RefSession, Name: name, Target: target}, Force: force,
		}
	}
	if _, err := svc.UpdateRef(ctx, input(left, false)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("generic API created internal session ref: %v", err)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
		Kind: domain.RefSession, Name: name, RepoID: repo, Target: left,
	}, ""); err != nil {
		t.Fatal(err)
	}
	out, err := svc.UpdateRef(ctx, input(left, false))
	if err != nil || out.Result != inbound.RefUpToDate {
		t.Fatalf("idempotent session ref echo rejected: out=%+v err=%v", out, err)
	}
	if _, err := svc.UpdateRef(ctx, input(right, true)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("generic API force-moved internal session ref: %v", err)
	}
}
