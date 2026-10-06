package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestFirstCommitFreezesConnectedIdentityAfterPendingCapture(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cwd := t.TempDir()
	runLifecycleGit(t, cwd, "init", "-q", "-b", "main")
	runLifecycleGit(t, cwd, "config", "core.hooksPath", "/dev/null")
	runLifecycleGit(t, cwd, "config", "user.name", "test")
	runLifecycleGit(t, cwd, "config", "user.email", "test@example.test")
	runLifecycleGit(t, cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "initial")
	ctx := context.Background()
	provisional, err := gitctx.NewGitContextAdapter().CurrentRepo(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	code := gitOut(cwd, "rev-parse", "HEAD")
	st := storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", code)
	if err := st.PutWorkingPosition(ctx, domain.WorkingPosition{RepoID: provisional.ID, Branch: "main", GitCommit: code}); err != nil {
		t.Fatal(err)
	}
	if err := configFixtureSave(cwd, remotecfg.Remotes{"origin": "https://example.invalid/team/context"}); err != nil {
		t.Fatal(err)
	}
	git := remotecfg.Wrap(cwd, gitctx.NewGitContextAdapter())
	connected, err := git.CurrentRepo(ctx, cwd)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: "pending-session"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: pending, DocHash: pending, RepoID: connected.ID, Branch: "main", SessionID: "pending-session", Provider: domain.ProviderClaude}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, domain.Pending{RepoID: connected.ID, SessionID: "pending-session", Provider: domain.ProviderClaude, Branch: "main", Target: pending}); err != nil {
		t.Fatal(err)
	}
	// A post-commit admission sees a newer Git commit than the empty init
	// cursor. Normalization must not claim that old cursor already observed it.
	runLifecycleGit(t, cwd, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "first work")
	currentCode := gitOut(cwd, "rev-parse", "HEAD")
	st = storage.NewWorktreeFileStore(cwd, gitOut(cwd, "rev-parse", "--absolute-git-dir"), "main", currentCode)
	c := &Container{History: app.NewContextHistoryService(st, st), ResolveRepo: git.CurrentRepo}
	pass, err := beginCommitCapture(ctx, c, cwd, []string{domain.ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	if pass.Proof.RepoID != connected.ID || pass.Proof.BranchID != domain.LegacyContextBranchID(connected.ID, "main") || pass.Initial != "" || pass.Proof.GitAfter != currentCode {
		t.Fatalf("first capture froze a provisional identity or invented ancestry: %+v", pass)
	}
	p, err := st.GetWorkingPosition(ctx)
	if err != nil || p.RepoID != connected.ID || p.GitCommit != code || p.Snapshot != "" {
		t.Fatalf("position: %+v %v", p, err)
	}
	if _, err := st.GetSnapshot(ctx, pending); err != nil {
		t.Fatal("pending data lost", err)
	}
	entries, err := os.ReadDir(filepath.Join(cwd, ".cxt", "history"))
	if !os.IsNotExist(err) && (err != nil || len(entries) != 0) {
		t.Fatalf("bootstrap fabricated history: %v %v", entries, err)
	}
}
