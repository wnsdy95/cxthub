package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestUpdateRefAppliesBranchLifecycleAndRejectsStaleBranch(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("branch-lifecycle-service-repo")
	target := hh("branch-lifecycle-service-target")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo, DocHash: target, Branch: "feature/archive"}); err != nil {
		t.Fatal(err)
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: "feature/archive", Target: target}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: branch}); err != nil {
		t.Fatalf("create branch: %v", err)
	}
	if err := svc.PutPending(ctx, repo, "archived-session", domain.Pending{Target: target, Branch: branch.Name}); err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repo, branch.Name, target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: archive})
	if err != nil || out.Result != inbound.RefFastForward {
		t.Fatalf("archive update = %+v, %v", out, err)
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, branch.Name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("branch projection survived archive: %v", err)
	}
	if pendings, err := svc.ListPendings(ctx, repo); err != nil || len(pendings) != 0 {
		t.Fatalf("archive-shared pending remains: %+v, %v", pendings, err)
	}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: branch}); !errors.Is(err, domain.ErrBranchArchived) {
		t.Fatalf("stale branch PUT resurrected archive: %v", err)
	}

	active, err := domain.NewBranchLifecycleRef(repo, branch.Name, target, 2, domain.BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: active}); err != nil {
		t.Fatalf("active event: %v", err)
	}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: branch}); err != nil {
		t.Fatalf("branch restore: %v", err)
	}
}

func TestUpdateRefRejectsLifecycleForceAndAppend(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("branch-lifecycle-policy-repo")
	target := hh("branch-lifecycle-policy-target")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewBranchLifecycleRef(repo, "feature/policy", target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []inbound.UpdateRefInput{
		{RepoID: repo, Ref: event, Force: true},
		{RepoID: repo, Ref: event, Append: true},
	} {
		if _, err := svc.UpdateRef(ctx, in); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("mutable lifecycle event accepted: %+v err=%v", in, err)
		}
	}
}

func TestUpdateRefRejectsArchiveOfProtectedDefaultBranch(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("protected-default-lifecycle-repo")
	target := hh("protected-default-lifecycle-target")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", ProtectDefault: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo, DocHash: target, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	branch := domain.Ref{Kind: domain.RefBranch, Name: "main", Target: target}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: branch}); err != nil {
		t.Fatal(err)
	}
	archive, err := domain.NewBranchLifecycleRef(repo, branch.Name, target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: archive}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("protected default archive error = %v", err)
	}
	if got, err := st.GetRef(ctx, repo, domain.RefBranch, branch.Name); err != nil || got.Target != target {
		t.Fatalf("protected branch changed: %+v, %v", got, err)
	}
}

func TestEmptyRefBatchSelfHealsInterruptedPendingReconciliation(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("deferred-lifecycle-reconciliation-repo")
	target := hh("deferred-lifecycle-reconciliation-target")
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, RepoID: repo, DocHash: target}); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutPending(ctx, repo, "deferred-session", domain.Pending{Target: target, Branch: "feature/deferred"}); err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewBranchLifecycleRef(repo, "feature/deferred", target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a process interruption after the durable ref write but before
	// UpdateRef's reconciliation wrapper.
	if _, err := svc.updateRef(ctx, inbound.UpdateRefInput{RepoID: repo, Ref: event}); err != nil {
		t.Fatal(err)
	}
	if pendings, err := svc.ListPendings(ctx, repo); err != nil || len(pendings) != 1 {
		t.Fatalf("interrupted update unexpectedly reconciled: %+v, %v", pendings, err)
	}
	out, err := svc.UpdateRefs(ctx, inbound.UpdateRefsInput{RepoID: repo})
	if err != nil || out.Applied != 0 {
		t.Fatalf("empty recovery batch = %+v, %v", out, err)
	}
	if pendings, err := svc.ListPendings(ctx, repo); err != nil || len(pendings) != 0 {
		t.Fatalf("final replay did not reconcile: %+v, %v", pendings, err)
	}
}

func TestBranchLifecycleEventIsASharedTimelineRoot(t *testing.T) {
	repo := hh("branch-lifecycle-shared-root-repo")
	target := hh("branch-lifecycle-shared-root-target")
	event, err := domain.NewBranchLifecycleRef(repo, "feature/archived", target, 1, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ref  domain.Ref
		want bool
	}{
		{name: "branch", ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: target}, want: true},
		{name: "session", ref: domain.Ref{Kind: domain.RefSession, Name: "main/session", Target: target}, want: true},
		{name: "lifecycle", ref: event, want: true},
		{name: "ordinary tag", ref: domain.Ref{Kind: domain.RefTag, Name: "v1", Target: target}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharedTimelineRef(tc.ref); got != tc.want {
				t.Fatalf("sharedTimelineRef(%+v) = %v, want %v", tc.ref, got, tc.want)
			}
		})
	}
}
