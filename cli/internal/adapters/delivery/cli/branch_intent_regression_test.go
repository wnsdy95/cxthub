package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestFrozenBindingCorruptionFailsClosed(t *testing.T) {
	for _, binding := range []*branchjournal.BindingIntent{
		{Kind: "attach"}, {Kind: "birth", RemoteBranch: "main"}, {Kind: "invented"}, {Kind: "attach", RemoteBranch: "../main"},
	} {
		cwd, c, _, _, _ := historyFixture(t)
		if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+gitOut(cwd, "rev-parse", "HEAD")+" refs/heads/feature"); err != nil {
			t.Fatal(err)
		}
		j, _ := branchjournal.Open(context.Background(), cwd)
		ops, _ := j.List()
		op := ops[0]
		op.Binding = binding
		if err := j.Transaction(context.Background(), func() error { return j.Save(op) }); err == nil {
			t.Fatalf("invalid binding accepted: %+v", binding)
		}
		// Only a synthetic fixture is damaged here; reading it must also fail.
		raw, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(cwd, ".git", "cxt", "operations", op.Event.ID+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = j.List(); err == nil {
			t.Fatalf("damaged binding accepted on read: %+v", binding)
		}
	}
}

func TestFrozenBirthCommandCannotBecomeTrackingAfterPush(t *testing.T) {
	ctx := context.Background()
	cwd, c, _, repo, _ := historyFixture(t)
	oid := gitOut(cwd, "rev-parse", "HEAD")
	runLifecycleGit(t, cwd, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/main", oid)
	if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+oid+" refs/heads/feature"); err != nil {
		t.Fatal(err)
	}
	runLifecycleGit(t, cwd, "branch", "feature", "origin/main")
	journal, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := journal.List()
	if err != nil || len(ops) != 1 {
		t.Fatal(ops, err)
	}
	op := ops[0]
	// Legacy frozen process evidence, recorded before a later push -u.
	op.Binding = nil
	op.Event.Creation = &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature", "origin/main"}, StartRef: "origin/main", StartCommit: oid, OriginBranch: "main", OriginBranchID: domain.LegacyContextBranchID(repo, "main")}
	if err := journal.Save(op); err != nil {
		t.Fatal(err)
	}
	commitBirthJournal(t, cwd, op.Event.ID)
	runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/feature", oid)
	runLifecycleGit(t, cwd, "config", "branch.feature.remote", "origin")
	runLifecycleGit(t, cwd, "config", "branch.feature.merge", "refs/heads/feature")
	if err := replayBranchOperations(ctx, c, cwd); err != nil {
		t.Fatalf("later upstream configuration reinterpreted frozen birth: %v", err)
	}
	ops, err = journal.List()
	if err != nil || ops[0].Phase != "applied" || ops[0].Event.Kind != "birth" || ops[0].Event.BranchID != op.Event.BranchID {
		t.Fatal("birth identity changed", ops, err)
	}
}

func TestCreationBindingFreezesExplicitTrackingOnly(t *testing.T) {
	cwd, _, _, _, _ := historyFixture(t)
	oid := gitOut(cwd, "rev-parse", "HEAD")
	runLifecycleGit(t, cwd, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/team-task", oid)
	runLifecycleGit(t, cwd, "config", "branch.main.remote", "origin")
	runLifecycleGit(t, cwd, "config", "branch.main.merge", "refs/heads/team-task")
	for _, tc := range []struct {
		command                     []string
		start, origin, kind, branch string
	}{
		{[]string{"git", "switch", "-c", "feature", "origin/team-task"}, "origin/team-task", "team-task", "birth", ""},
		{[]string{"git", "checkout", "-b", "feature", "origin/team-task"}, "origin/team-task", "team-task", "birth", ""},
		{[]string{"git", "branch", "feature", "origin/team-task"}, "origin/team-task", "team-task", "birth", ""},
		{[]string{"git", "worktree", "add", "-b", "feature", "<worktree>", "origin/team-task"}, "origin/team-task", "team-task", "birth", ""},
		{[]string{"git", "switch", "--track", "-c", "feature", "origin/team-task"}, "origin/team-task", "team-task", "attach", "team-task"},
		{[]string{"git", "branch", "--track=direct", "feature", "origin/team-task"}, "origin/team-task", "team-task", "attach", "team-task"},
		{[]string{"git", "branch", "--track=inherit", "feature", "main"}, "main", "main", "attach", "team-task"},
		{[]string{"git", "branch", "--track", "--no-track", "feature", "origin/team-task"}, "origin/team-task", "team-task", "birth", ""},
		{[]string{"git", "branch", "--track", "feature", "main"}, "main", "main", "birth", ""},
	} {
		t.Run(strings.Join(tc.command, " "), func(t *testing.T) {
			e := domain.HistoryEvent{Kind: "birth", Branch: "feature", Creation: &domain.GitCreation{Evidence: "process-argv", Command: tc.command, StartRef: tc.start, StartCommit: oid, OriginBranch: tc.origin}}
			got := freezeBranchBinding(cwd, e)
			if got.Kind != tc.kind || got.RemoteBranch != tc.branch {
				t.Fatalf("binding = %+v", got)
			}
		})
	}
}

func TestUnprovenBindingNeverBorrowsCurrentUpstream(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, currentTracking := range []bool{false, true} {
			cwd, c, _, _, _ := historyFixture(t)
			ctx := context.Background()
			oid := gitOut(cwd, "rev-parse", "HEAD")
			if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+oid+" refs/heads/feature"); err != nil {
				t.Fatal(err)
			}
			runLifecycleGit(t, cwd, "branch", "feature")
			j, _ := branchjournal.Open(ctx, cwd)
			ops, _ := j.List()
			op := ops[0]
			op.Event.Creation = &domain.GitCreation{Evidence: "unavailable", StartCommit: oid}
			op.Binding = &branchjournal.BindingIntent{Kind: "unavailable"}
			if legacy {
				op.Binding = nil
			}
			op.Phase = "committed"
			if err := j.Transaction(ctx, func() error { return j.Save(op) }); err != nil {
				t.Fatal(err)
			}
			if currentTracking {
				runLifecycleGit(t, cwd, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
				runLifecycleGit(t, cwd, "update-ref", "refs/remotes/origin/feature", oid)
				runLifecycleGit(t, cwd, "branch", "--set-upstream-to=origin/feature", "feature")
			}
			if err := replayBranchOperations(ctx, c, cwd); err == nil || !strings.Contains(err.Error(), "creation-time branch binding is unavailable") {
				t.Fatalf("unproven binding: %v", err)
			}
			after, _ := j.List()
			if after[0].Resolved || after[0].Phase != "committed" || !reflect.DeepEqual(after[0].Event, op.Event) {
				t.Fatal("unproven history mutated")
			}
		}
	}
}

func TestLegacyTrackingRequiresOriginalEvidence(t *testing.T) {
	for _, tc := range []struct{ mode, start, origin, kind string }{
		{"--track", "origin/task", "task", "attach"},
		{"--track", "origin/task", "origin/task", "birth"},
		{"--track", "HEAD", "task", "birth"},
		{"--track", "@{-1}", "task", "unavailable"},
		{"--track=inherit", "main", "main", "unavailable"},
	} {
		e := domain.HistoryEvent{Kind: "birth", Branch: "feature", Creation: &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "branch", tc.mode, "feature", tc.start}, StartRef: tc.start, OriginBranch: tc.origin}}
		if got := legacyBranchBinding(e); got.Kind != tc.kind {
			t.Fatalf("%+v = %+v", tc, got)
		}
	}
}
