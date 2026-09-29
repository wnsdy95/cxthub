package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestCommandContractRejectsInvalidArguments(t *testing.T) {
	cases := [][]string{
		{"pull", "upstream", "feature"}, {"push", "origin"},
		{"load", "main", "extra"}, {"load", "--mode", "typo"},
		{"load", "--mode", "memory", "--mode=full"},
		{"fork", "main"}, {"fork", "--as", "feature"},
		{"repo", "delete", "https://example.test/a/b"}, {"repo", "create"},
		{"push", "--force", "--append"}, {"push", "-f", "--force"},
		{"save", "--provider", "unknown"}, {"add", "claude", "unknown"},
		{"init", "ignored-url"}, {"repack", "extra"}, {"fsck", "extra"},
		{"tag", "one", "two", "three"}, {"config"},
		{"config", "load.mode", "memory", "extra"},
		{"login", "literal-token", "-t", "another-token"},
		{"remote", "add", "origin"}, {"remote", "rm", "a", "b"},
		{"remote", "typo"}, {"settings", "restore", "not-an-index"},
		{"settings", "restore", "-1"}, {"settings", "pull", "extra"},
		{"secrets", "push", "--force"}, {"secrets", "typo"},
		{"hooks", "install", "extra"}, {"stash", "pop", "extra"},
		{"stash", "list", "--provider", "codex"},
		{"hook", "--provider", "codex"}, {"hook", "--event", "stop"},
		{"branch", "list", "--mode", "memory"},
		{"version", "extra"}, {"commit", "-m", "--help"},
		{"fork", "", "--as", "feature"}, {"tag", ""},
		{"load", ""}, {"login", ""}, {"remote", "add", "origin", ""},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			if handled, err := PreflightArgs(append([]string{"cxt"}, args...)); handled || err == nil {
				t.Fatalf("invalid invocation accepted: handled=%v error=%v", handled, err)
			}
			// A nil container proves dispatch rejects the invocation before
			// touching any command service, not merely in the process wrapper.
			if err := Run(nil, append([]string{"cxt"}, args...)); err == nil {
				t.Fatal("invalid invocation reached dispatch")
			}
		})
	}
}

func TestReadOnlyClassificationUsesParsedSubcommand(t *testing.T) {
	for _, args := range [][]string{{"log"}, {"branch", "list"}, {"branch", "--json", "operations"}, {"settings", "list"}, {"tag"}, {"stash", "list"}, {"remote", "-v"}, {"config", "load.mode"}, {"fsck"}, {"reflog"}} {
		if !ReadOnlyInvocation(append([]string{"cxt"}, args...)) {
			t.Errorf("read classified as write: %v", args)
		}
	}
	for _, args := range [][]string{{"branch", "replay"}, {"settings", "pull"}, {"tag", "name"}, {"stash", "pop"}, {"remote", "rm", "origin"}, {"config", "load.mode", "memory"}, {"capture", "retry", "id", "--expect", "hash"}, {"codex"}, {"invalid-command"}} {
		if ReadOnlyInvocation(append([]string{"cxt"}, args...)) {
			t.Errorf("write classified as read: %v", args)
		}
	}
}

func TestTypedArgumentsPreserveLiteralValues(t *testing.T) {
	p, help, err := parseCommand("load", []string{"--mode=memory", "--", "--provider=claude"})
	if err != nil || help || p.first() != "--provider=claude" || p.flags["--provider"] != "" || p.flags["--mode"] != "memory" {
		t.Fatalf("literal boundary: %+v %v %v", p, help, err)
	}
	p, help, err = parseCommand("commit", []string{"-m=--help"})
	if err != nil || help || p.flags["-m"] != "--help" {
		t.Fatalf("literal help: %+v %v %v", p, help, err)
	}
	if _, _, err := parseCommand("secrets", []string{"pull", "--typo=private-passphrase"}); err == nil || strings.Contains(err.Error(), "private-passphrase") {
		t.Fatalf("argument error leaked a value: %v", err)
	}
}

func TestCommandContractAcceptsSupportedArguments(t *testing.T) {
	cases := [][]string{
		{"branch"}, {"branch", "list"}, {"branch", "operations", "--json"},
		{"branch", "restore", "feature", "--mode=memory", "--provider=codex"},
		{"branch", "recover", "operation", "--confirm-orphan"},
		{"secrets", "pull", "--force", "-p=-passphrase"},
		{"secrets", "push", "--rotate", "--remember"},
		{"fork", "main", "--as", "feature"}, {"checkout", "-b", "feature"},
		{"load", "--mode=memory"}, {"add", "claude", "codex"},
		{"remote", "add", "origin", "https://example.test/a/b"},
		{"remote", "rm", "origin"}, {"remote", "-v"},
		{"settings", "restore", "0"}, {"settings", "restore"},
		{"capture", "resolve", "id", "--expect", "hash", "--json"},
		{"repair", "--from-server"}, {"push", "-f", "--wait-history"},
		{"commit", "-m=--help"}, {"tag", "--", "literal"},
		{"hook", "--provider", "codex", "--event", "stop"},
		{"config", "secrets.redact", ""},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			if handled, err := PreflightArgs(append([]string{"cxt"}, args...)); handled || err != nil {
				t.Fatalf("valid invocation rejected: handled=%v error=%v", handled, err)
			}
		})
	}
}

func TestReadCommandsDoNotReplayCommittedBranchOperations(t *testing.T) {
	for _, args := range [][]string{{"log"}, {"list"}, {"config", "load.mode"}, {"remote", "-v"}, {"branch", "list"}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			cwd, c, store, repoID, _ := historyFixture(t)
			t.Chdir(cwd)
			t.Setenv("CXT_KEEP_SESSION", "1")
			ctx := context.Background()
			oid := gitOut(cwd, "rev-parse", "HEAD")
			if err := runBirthVote(t, cwd, c, "prepared", strings.Repeat("0", 40)+" "+oid+" refs/heads/queued"); err != nil {
				t.Fatal(err)
			}
			j, err := branchjournal.Open(ctx, cwd)
			if err != nil {
				t.Fatal(err)
			}
			ops, err := j.List()
			if err != nil || len(ops) != 1 {
				t.Fatalf("operations: %v %v", ops, err)
			}
			runLifecycleGit(t, cwd, "branch", "queued")
			commitBirthJournal(t, cwd, ops[0].Event.ID)
			before, _ := j.List()
			if err := Run(c, append([]string{"cxt"}, args...)); err != nil {
				t.Fatal(err)
			}
			after, err := j.List()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("read command replayed journal: before=%+v after=%+v err=%v", before, after, err)
			}
			if events, err := store.ListHistoryEvents(ctx, repoID); err != nil || len(events) != 0 {
				t.Fatalf("read published history: %+v %v", events, err)
			}
			if _, err := store.GetRef(ctx, repoID, domain.RefBranch, "queued"); err != domain.ErrNotFound {
				t.Fatalf("read published branch: %v", err)
			}
			// Suppressing replay on reads must not disable explicit recovery.
			if err := replayBranchOperations(ctx, c, cwd); err != nil {
				t.Fatal(err)
			}
			if events, err := store.ListHistoryEvents(ctx, repoID); err != nil || len(events) != 1 {
				t.Fatalf("explicit recovery: %+v %v", events, err)
			}
		})
	}
}
