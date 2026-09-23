package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
	"testing"
)

type fakeGitEvidence struct {
	t        *testing.T
	origin   string
	deltas   map[string]domain.GitCommitDelta
	ancestor bool
	err      error
}

func (f fakeGitEvidence) ReadCommitDelta(_ context.Context, origin, oid, parent string) (domain.GitCommitDelta, error) {
	if origin != f.origin {
		f.t.Error("untrusted repository origin")
	}
	return f.deltas[oid], f.err
}
func (f fakeGitEvidence) IsGitAncestor(_ context.Context, origin, ancestor, descendant string) (bool, error) {
	if origin != f.origin {
		f.t.Error("wrong origin")
	}
	return f.ancestor, f.err
}
func TestVerifyGitReversalChecksOwnershipAncestryAndImmutableResponse(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	origin := "git@github.com:example/project.git"
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	oid := func(c string) string { return strings.Repeat(c, 40) }
	target, candidate := oid("a"), oid("b")
	change := domain.GitPathChange{Path: "feature", Before: domain.GitEntry{OID: oid("1"), Mode: "100644"}, After: domain.GitEntry{OID: oid("2"), Mode: "100644"}}
	a := domain.GitCommitDelta{Commit: target, Parents: []string{oid("c")}, Parent: oid("c"), Changes: []domain.GitPathChange{change}, Complete: true}
	b := domain.GitCommitDelta{Commit: candidate, Parents: []string{target}, Parent: target, Changes: []domain.GitPathChange{{Path: change.Path, Before: change.After, After: change.Before}}, Complete: true}
	reader := fakeGitEvidence{t: t, origin: origin, deltas: map[string]domain.GitCommitDelta{target: a, candidate: b}, ancestor: true}
	proof, err := svc.VerifyGitReversal(ctx, reader, repo, target, "", candidate, "")
	if err != nil || proof.Coverage != "full" {
		t.Fatalf("verified relation=%+v %v", proof, err)
	}
	reader.ancestor = false
	proof, err = svc.VerifyGitReversal(ctx, reader, repo, target, "", candidate, "")
	if err != nil || proof.Coverage != "unverified" || proof.Reason != "target_not_in_candidate_ancestry" {
		t.Fatalf("foreign ancestry=%+v %v", proof, err)
	}
	reader.ancestor = true
	b.Commit = oid("d")
	reader.deltas[candidate] = b
	if _, err := svc.VerifyGitReversal(ctx, reader, repo, target, "", candidate, ""); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("wrong immutable object accepted", err)
	}
	reader.err = errors.New("provider temporarily unavailable")
	if _, err := svc.VerifyGitReversal(ctx, reader, repo, target, "", candidate, ""); !errors.Is(err, reader.err) {
		t.Fatal("provider failure became verified cancellation", err)
	}
	history, err := svc.ListHistory(ctx, repo)
	if err != nil || len(history) != 0 {
		t.Fatalf("evidence inspection mutated historical completion: %v %v", history, err)
	}
}
