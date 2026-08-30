package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type appSwitchSave struct{}

func (appSwitchSave) Save(context.Context, inbound.SaveInput) (inbound.SaveOutput, error) {
	return inbound.SaveOutput{SnapshotID: domain.HashContent([]byte("app checkpoint")), Branch: "main"}, nil
}

type appSwitchMemorize struct{}

func (appSwitchMemorize) Memorize(context.Context, inbound.MemorizeInput) (inbound.MemorizeOutput, error) {
	return inbound.MemorizeOutput{MemoryHash: domain.HashContent([]byte("app checkpoint memory")), Attached: true}, nil
}

type appSwitchList struct {
	target domain.ContentHash
}

func (s appSwitchList) List(context.Context, inbound.ListInput) (inbound.ListOutput, error) {
	return inbound.ListOutput{
		Snapshots: []domain.Snapshot{{ID: s.target, DocHash: s.target, Branch: "feature/app"}},
		Refs:      []domain.Ref{{Kind: domain.RefBranch, Name: "feature/app", Target: s.target}},
	}, nil
}

type appSwitchCheckout struct {
	seen *inbound.CheckoutInput
}

func (s appSwitchCheckout) Checkout(_ context.Context, in inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
	*s.seen = in
	return inbound.CheckoutOutput{Branch: "feature/app", Head: domain.HashContent([]byte("app branch target"))}, nil
}

type appSwitchHandoff struct{}

func (appSwitchHandoff) RenderBranchHandoff(context.Context, inbound.BranchHandoffInput) (string, error) {
	return "BOUNDED APP HANDOFF", nil
}

func appSwitchGit(t *testing.T, cwd string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestUnmanagedAppBranchSwitchPreservesProviderSession(t *testing.T) {
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CXT_WRAPPED", "")
	t.Setenv("CXT_WRAPPED_AGENT", "")
	t.Setenv("CXT_WRAPPER_PID", "")

	appSwitchGit(t, repo, "init", "-b", "main")
	appSwitchGit(t, repo, "config", "user.name", "cxt test")
	appSwitchGit(t, repo, "config", "user.email", "cxt@example.test")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	appSwitchGit(t, repo, "config", "core.hooksPath", hooks)
	appSwitchGit(t, repo, "config", "gc.auto", "0")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	appSwitchGit(t, repo, "add", "tracked.txt")
	appSwitchGit(t, repo, "commit", "-m", "initial")
	appSwitchGit(t, repo, "branch", "feature/app")
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}

	const sessionID = "12345678-1234-4abc-8def-1234567890ab"
	sessionDir := filepath.Join(home, ".codex", "sessions", "2026", "08", "31")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "rollout-2026-08-31T00-00-00-"+sessionID+".jsonl")
	raw := `{"type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"` + repo + `","model":"gpt-test"}}` + "\n"
	if err := os.WriteFile(sessionPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	appSwitchGit(t, repo, "switch", "feature/app")

	target := domain.HashContent([]byte("app branch target"))
	var checkoutInput inbound.CheckoutInput
	container := &Container{
		Save:     appSwitchSave{},
		Memorize: appSwitchMemorize{},
		List:     appSwitchList{target: target},
		Checkout: appSwitchCheckout{seen: &checkoutInput},
		Handoff:  appSwitchHandoff{},
	}
	if err := contextSwitch(context.Background(), container, repo); err != nil {
		t.Fatal(err)
	}
	if !checkoutInput.SkipMaterialize {
		t.Fatal("unmanaged app checkout attempted provider materialization")
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("live app session was moved or removed: %v", err)
	}
	if _, err := os.Lstat(sessionPath + ".superseded"); !os.IsNotExist(err) {
		t.Fatalf("unmanaged app session was superseded: %v", err)
	}
	if got, ok := capture.ConsumeSessionHandoff(repo, sessionID); !ok || got != "BOUNDED APP HANDOFF" {
		t.Fatalf("app handoff = %q, %v", got, ok)
	}
}
