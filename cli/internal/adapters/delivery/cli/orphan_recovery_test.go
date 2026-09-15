package cli

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"strings"
	"testing"
)

func TestConfirmedOrphanRecoveryAndReplay(t *testing.T) {
	cwd, c, st, repo, source := historyFixture(t)
	ctx := context.Background()
	line := strings.Repeat("0", 40) + " ref:refs/heads/orphan-x HEAD"
	if err := runBirthVote(t, cwd, c, "prepared", line); err != nil {
		t.Fatal(err)
	}
	j, _ := branchjournal.Open(ctx, cwd)
	ops, _ := j.List()
	id := ops[0].Event.ID
	// Missing callback does not authorize replay by itself.
	if err := replayBranchOperations(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	if err := confirmOrphanRecovery(ctx, c, cwd, id); err == nil {
		t.Fatal("wrong HEAD confirmed")
	}
	runLifecycleGit(t, cwd, "switch", "--orphan", "orphan-x")
	if err := confirmOrphanRecovery(ctx, c, cwd, id); err != nil {
		t.Fatal(err)
	}
	if err := replayBranchOperations(ctx, c, cwd); err != nil {
		t.Fatal(err)
	}
	if err := confirmOrphanRecovery(ctx, c, cwd, id); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListHistoryEvents(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].RecoveryEvidence != "user-confirmed-unborn-head" || events[0].Source != "" || events[0].MemorySource != source {
		t.Fatalf("recovery=%+v", events)
	}
	ops, _ = j.List()
	if ops[0].Phase != "applied" {
		t.Fatalf("phase=%s", ops[0].Phase)
	}
}

func TestOrphanRecoveryRequiresExplicitFlag(t *testing.T) {
	for _, args := range [][]string{{"cxt", "branch", "recover", "id"}, {"cxt", "branch", "replay", "--confirm-orphan"}, {"cxt", "branch", "recover", "id", "--confirm-orphan", "--provider", "claude"}} {
		if _, err := PreflightArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
