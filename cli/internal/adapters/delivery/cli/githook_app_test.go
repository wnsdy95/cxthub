package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/boundary"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type appSwitchSave struct{ err error }

func (s appSwitchSave) Save(_ context.Context, in inbound.SaveInput) (inbound.SaveOutput, error) {
	if s.err != nil {
		return inbound.SaveOutput{}, s.err
	}
	info, err := os.Stat(in.SessionPath)
	if err != nil {
		return inbound.SaveOutput{}, err
	}
	return inbound.SaveOutput{
		SnapshotID: domain.HashContent([]byte("app checkpoint")), Branch: "main", CapturedBytes: info.Size(),
	}, nil
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
	seen     *inbound.CheckoutInput
	prepared string
	err      error
}

func (s appSwitchCheckout) Checkout(_ context.Context, in inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
	*s.seen = in
	if s.err != nil {
		return inbound.CheckoutOutput{}, s.err
	}
	if s.prepared != "" {
		return inbound.CheckoutOutput{Branch: "feature/app", Head: domain.HashContent([]byte("app branch target")), WrittenPath: s.prepared, ResumeCmd: "codex resume " + providerfs.SessionIDFromPath(s.prepared), Fidelity: domain.FidelityMemory}, nil
	}
	return inbound.CheckoutOutput{Branch: "feature/app", Head: domain.HashContent([]byte("app branch target"))}, nil
}

type appSwitchHandoff struct{ err error }

func (s appSwitchHandoff) RenderBranchHandoff(context.Context, inbound.BranchHandoffInput) (string, error) {
	return "BOUNDED APP HANDOFF", s.err
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
	for _, mode := range []string{"app", "app-handoff-failure", "app-checkout-failure", "app-save-failure", "prepare-first-wrapper"} {
		t.Run(mode, func(t *testing.T) {
			repo := t.TempDir()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CXT_WRAPPED", "")
			t.Setenv("CXT_WRAPPED_AGENT", "")
			t.Setenv("CXT_WRAPPER_PID", "")
			t.Setenv("CXT_WRAPPER_TRANSITION_PROTOCOL", "")
			if mode == "prepare-first-wrapper" {
				t.Setenv("CXT_WRAPPED", "1")
				t.Setenv("CXT_WRAPPED_AGENT", "codex")
				t.Setenv("CXT_WRAPPER_PID", strconv.Itoa(os.Getppid()))
				t.Setenv("CXT_WRAPPER_TRANSITION_PROTOCOL", "prepare-first-v1")
			}

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
			if err := os.WriteFile(filepath.Join(repo, ".cxt", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
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
			prepared := ""
			if mode == "prepare-first-wrapper" {
				prepared = filepath.Join(sessionDir, "rollout-2026-08-31T00-00-00-aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl")
			}
			container := &Container{
				Save:     appSwitchSave{},
				Memorize: appSwitchMemorize{},
				List:     appSwitchList{target: target},
				Checkout: appSwitchCheckout{seen: &checkoutInput, prepared: prepared},
				Handoff:  appSwitchHandoff{},
			}
			failure := errors.New("synthetic transition failure")
			switch mode {
			case "app-handoff-failure":
				container.Handoff = appSwitchHandoff{err: failure}
			case "app-checkout-failure":
				container.Checkout = appSwitchCheckout{seen: &checkoutInput, err: failure}
			case "app-save-failure":
				container.Save = appSwitchSave{err: failure}
			}
			if err := contextSwitch(context.Background(), container, repo); err != nil {
				t.Fatal(err)
			}
			if checkoutInput.SkipMaterialize != (mode != "prepare-first-wrapper") {
				t.Fatal("checkout preparation did not match session ownership")
			}
			if _, err := os.Stat(sessionPath); err != nil {
				t.Fatalf("live app session was moved or removed: %v", err)
			}
			if _, err := os.Lstat(sessionPath + ".superseded"); !os.IsNotExist(err) {
				t.Fatalf("unmanaged app session was superseded: %v", err)
			}
			if mode == "prepare-first-wrapper" {
				b, ok := boundary.Load(repo)
				if !ok || b.Branch != "feature/app" || b.SeedPath != prepared || len(b.Superseded) != 0 {
					t.Fatalf("missing deferred boundary: %+v %v", b, ok)
				}
				if providerfs.CaptureExcluded(repo, sessionPath, int64(len(raw))) {
					t.Fatal("live session excluded before actual restart preparation")
				}
				return
			}
			got, ok := capture.ConsumeSessionHandoff(repo, sessionID)
			if mode == "app-handoff-failure" || mode == "app-checkout-failure" {
				if ok {
					t.Fatalf("failed handoff was queued: %q", got)
				}
			} else if !ok || got != "BOUNDED APP HANDOFF" {
				t.Fatalf("app handoff = %q, %v", got, ok)
			}
			if mode == "app-save-failure" {
				if providerfs.CaptureExcluded(repo, sessionPath, int64(len(raw))) {
					t.Fatal("failed checkpoint excluded uncaptured conversation")
				}
				return
			}
			if !providerfs.CaptureExcluded(repo, sessionPath, int64(len(raw))) {
				t.Fatal("unchanged app session was not held at the branch-switch baseline")
			}
			if err := os.WriteFile(sessionPath, []byte(raw+"{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if providerfs.CaptureExcluded(repo, sessionPath, int64(len(raw)+3)) {
				t.Fatal("grown app session remained excluded after the branch switch")
			}
		})
	}
}
