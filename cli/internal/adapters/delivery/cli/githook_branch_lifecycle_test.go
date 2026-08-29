package cli

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestParseBranchRefTransactionSupportsDeletionFormsAndObjectFormats(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha256 := strings.Repeat("b", 64)
	zero40 := strings.Repeat("0", 40)
	zero64 := strings.Repeat("0", 64)

	for _, tc := range []struct {
		line        string
		wantName    string
		wantCreated string
		wantDeleted bool
	}{
		{line: zero40 + " " + sha1 + " refs/heads/feature/new", wantName: "feature/new", wantCreated: sha1},
		{line: zero40 + " " + zero40 + " refs/heads/feature/deleted", wantName: "feature/deleted", wantDeleted: true},
		{line: sha1 + " " + zero40 + " refs/heads/feature/explicit-old", wantName: "feature/explicit-old", wantDeleted: true},
		{line: zero64 + " " + zero64 + " refs/heads/feature/sha256", wantName: "feature/sha256", wantDeleted: true},
		{line: zero64 + " " + sha256 + " refs/heads/feature/sha256-new", wantName: "feature/sha256-new", wantCreated: sha256},
	} {
		got := parseBranchRefTransaction(tc.line)
		if got.name != tc.wantName || got.createdOID != tc.wantCreated || got.deleted != tc.wantDeleted {
			t.Fatalf("parse %q = %+v", tc.line, got)
		}
	}

	if got := parseBranchRefTransaction(sha1 + " " + sha1 + " refs/tags/v1"); got.name != "" {
		t.Fatalf("non-branch ref classified: %+v", got)
	}
	for value, want := range map[string]bool{
		sha1: true, sha256: true, zero40: false, "": false, strings.Repeat("g", 40): false,
	} {
		if got := validNonZeroGitOID(value); got != want {
			t.Fatalf("validNonZeroGitOID(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestPreparedBranchTransactionCarriesOldOIDExactlyOnce(t *testing.T) {
	repo := t.TempDir()
	runLifecycleGit(t, repo, "init", "-q")
	runLifecycleGit(t, repo, "config", "user.name", "cxt test")
	runLifecycleGit(t, repo, "config", "user.email", "cxt@example.test")
	runLifecycleGit(t, repo, "commit", "--allow-empty", "-qm", "base")
	runLifecycleGit(t, repo, "branch", "feature/rename")
	oldOID := strings.TrimSpace(runLifecycleGit(t, repo, "rev-parse", "refs/heads/feature/rename"))
	zero := strings.Repeat("0", 40)
	// Git 2.50's files backend reports zero→zero for branch deletion. The
	// prepared hook must read the still-live ref instead of trusting stdin.
	if err := recordPreparedBranchRefTransaction(repo, []string{
		zero + " " + zero + " refs/heads/feature/rename",
	}); err != nil {
		t.Fatal(err)
	}
	got := consumePreparedBranchRefTransaction(repo, "feature/rename")
	if got != oldOID {
		t.Fatalf("prepared old OID = %q, want %q", got, oldOID)
	}
	if again := consumePreparedBranchRefTransaction(repo, "feature/rename"); again != "" {
		t.Fatalf("prepared ledger was not consumed: %q", again)
	}
	if _, err := branchRefTransactionLedgerPath("../escape"); !errors.Is(err, domain.ErrInvalidRef) {
		t.Fatalf("unsafe transaction ID error = %v", err)
	}
	if err := recordPreparedBranchRefTransaction(repo, []string{
		oldOID + " " + strings.Repeat("b", 40) + " refs/heads/main",
	}); err != nil {
		t.Fatal(err)
	}
	if ordinary := consumePreparedBranchRefTransaction(repo, "main"); ordinary != "" {
		t.Fatalf("ordinary branch movement created a deletion ledger: %q", ordinary)
	}
	if err := recordPreparedBranchRefTransaction(repo, []string{
		oldOID + " " + zero + " refs/heads/feature/rename",
	}); err != nil {
		t.Fatal(err)
	}
	clearPreparedBranchRefTransactions(repo, []string{oldOID + " " + zero + " refs/heads/feature/rename"})
	if aborted := consumePreparedBranchRefTransaction(repo, "feature/rename"); aborted != "" {
		t.Fatalf("aborted deletion ledger remains: %q", aborted)
	}
}

func TestRenamedBranchForDeletionRequiresMatchingRenameReflogAndOID(t *testing.T) {
	repo := t.TempDir()
	runLifecycleGit(t, repo, "init", "-q")
	runLifecycleGit(t, repo, "config", "user.name", "cxt test")
	runLifecycleGit(t, repo, "config", "user.email", "cxt@example.test")
	runLifecycleGit(t, repo, "commit", "--allow-empty", "-qm", "base")
	oid := strings.TrimSpace(runLifecycleGit(t, repo, "rev-parse", "HEAD"))
	runLifecycleGit(t, repo, "branch", "feature/old")
	runLifecycleGit(t, repo, "branch", "-m", "feature/old", "feature/new")

	if got := renamedBranchForDeletion(repo, "feature/old", oid); got != "feature/new" {
		t.Fatalf("rename destination = %q, want feature/new", got)
	}
	if got := renamedBranchForDeletion(repo, "feature/old", strings.Repeat("f", len(oid))); got != "" {
		t.Fatalf("mismatched object classified as rename: %q", got)
	}
	runLifecycleGit(t, repo, "branch", "feature/deleted")
	runLifecycleGit(t, repo, "branch", "-D", "feature/deleted")
	if got := renamedBranchForDeletion(repo, "feature/deleted", oid); got != "" {
		t.Fatalf("plain deletion classified as rename: %q", got)
	}
}

func runLifecycleGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
