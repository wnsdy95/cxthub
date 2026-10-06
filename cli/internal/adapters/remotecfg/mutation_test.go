package remotecfg

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func observeConfig(t *testing.T, root string) Observation {
	t.Helper()
	o, e := Observe(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func readConfigFields(t *testing.T, root string) map[string]json.RawMessage {
	t.Helper()
	o := observeConfig(t, root)
	f, e := o.fields()
	if e != nil {
		t.Fatal(e)
	}
	return f
}

func TestConfigCoordinationPreservesUnrelatedAndUnknownFields(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	if err := providerfs.WriteRepoFileAtomic(root, ".cxt/config", []byte(`{"future":{"nested":[1,"two"]},"remotes":{"mirror":"https://example.invalid/team/mirror"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 2)
	// Pause a real mutation after its fresh locked read. The other setter must
	// read after it, and must preserve its field as well as unknown JSON.
	go func() {
		done <- mutate(ctx, root, nil, false, func(f map[string]json.RawMessage, _ Observation) error {
			close(entered)
			<-release
			f["checkout_mode"] = json.RawMessage(`"prepare"`)
			return nil
		})
	}()
	<-entered
	go func() { done <- SetLoadMode(ctx, root, "memory") }()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	f := readConfigFields(t, root)
	if CheckoutMode(root) != "prepare" || LoadMode(root) != "memory" || string(f["future"]) == "" {
		t.Fatalf("lost fields: %s", f)
	}
	var future any
	if err := json.Unmarshal(f["future"], &future); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(future)
	if string(encoded) != `{"nested":[1,"two"]}` {
		t.Fatalf("unknown changed: %s", encoded)
	}
	if remotes, err := Load(root); err != nil || remotes["mirror"] == "" {
		t.Fatalf("lost remote: %v %v", remotes, err)
	}
	before := observeConfig(t, root)
	if err := SetLoadMode(ctx, root, "memory"); err != nil {
		t.Fatal(err)
	}
	if sameObservation(before, observeConfig(t, root)) {
		t.Fatal("same-value successful write reused version")
	}
}

func TestConfigCoordinationExactCASRejectsABAAndUnrelatedEdits(t *testing.T) {
	for _, kind := range []string{"ABA", "setting", "different-name"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			a := "https://example.invalid/team/a"
			b := "https://example.invalid/team/b"
			if err := Replace(ctx, observeConfig(t, root), Remotes{"origin": a}); err != nil {
				t.Fatal(err)
			}
			stale := observeConfig(t, root)
			switch kind {
			case "ABA":
				if err := Replace(ctx, observeConfig(t, root), Remotes{"origin": b}); err != nil {
					t.Fatal(err)
				}
				if err := Replace(ctx, observeConfig(t, root), Remotes{"origin": a}); err != nil {
					t.Fatal(err)
				}
			case "setting":
				if err := SetLoadMode(ctx, root, "memory"); err != nil {
					t.Fatal(err)
				}
			case "different-name":
				if err := Add(ctx, observeConfig(t, root), "mirror", b, nil); err != nil {
					t.Fatal(err)
				}
			}
			current := observeConfig(t, root)
			if err := Remove(ctx, stale, "origin"); !errors.Is(err, ErrChanged) {
				t.Fatalf("stale remove: %v", err)
			}
			if err := Replace(ctx, stale, Remotes{}); !errors.Is(err, ErrChanged) {
				t.Fatalf("stale replace: %v", err)
			}
			if err := Add(ctx, stale, "new", b, nil); !errors.Is(err, ErrChanged) {
				t.Fatalf("stale add: %v", err)
			}
			if !sameObservation(current, observeConfig(t, root)) {
				t.Fatal("failed CAS changed file")
			}
		})
	}
}

func TestConfigCoordinationCaptureGateCancellationAndUpgrade(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	if err := st.WithCaptureTrackingGate(ctx, func(held context.Context) error {
		if err := SetLoadMode(held, root, "memory"); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatalf("nested upgrade: %v", err)
		}
		wait, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		defer cancel()
		if err := SetLoadMode(wait, root, "memory"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("writer crossed SH: %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, ".cxt/config")); !os.IsNotExist(err) {
			t.Fatalf("cancelled writer published: %v", err)
		}
		return st.WithCaptureTrackingGate(held, func(context.Context) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if err := SetLoadMode(ctx, root, "memory"); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	untouched := t.TempDir()
	if err := SetLoadMode(cancelled, untouched, "memory"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(untouched); len(entries) != 0 {
		t.Fatal("pre-cancelled write created files")
	}
}

func TestConfigCoordinationPassiveReadsCreateNothing(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	if _, err := Observe(ctx, root); err != nil {
		t.Fatal(err)
	}
	_, _ = Load(root)
	_, _ = LoadAtRoot(root)
	_, _ = Origin(root)
	_ = CheckoutMode(root)
	_ = LoadMode(root)
	_ = SecretsRedact(root)
	_ = SecretsMinLen(root)
	_ = SecretsScrub(root)
	_ = CaptureDebounce(root)
	_ = BoundaryEnforce(root)
	_ = StagedProviders(root)
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("passive reads created %v", entries)
	}
}

func TestConfigCoordinationLinkedWorktreeUsesSameGate(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	linked := filepath.Join(t.TempDir(), "linked")
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	git("init", "--template=", "-q", "-b", "main")
	git("-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "fixture")
	git("worktree", "add", "-qb", "linked", linked)
	st := storage.NewFileStore(root)
	if err := st.WithCaptureTrackingGate(context.Background(), func(held context.Context) error {
		if err := SetLoadMode(held, linked, "memory"); !errors.Is(err, domain.ErrSyncConflict) {
			t.Fatalf("linked nested upgrade: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if err := SetLoadMode(ctx, linked, "memory"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("linked writer crossed SH: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SetLoadMode(context.Background(), linked, "memory"); err != nil {
		t.Fatal(err)
	}
	if LoadMode(root) != "memory" {
		t.Fatal("linked config did not reach shared root")
	}
	if _, err := os.Stat(filepath.Join(linked, ".cxt")); !os.IsNotExist(err) {
		t.Fatalf("linked store created: %v", err)
	}
}

func TestConfigCoordinationRepairExactExpectation(t *testing.T) {
	for _, kind := range []string{"valid", "malformed", "concurrent-valid", "concurrent-malformed"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			backup := t.TempDir()
			ctx := context.Background()
			raw := []byte(`{"load_mode":"memory","future":{"v":1},"remotes":{"mirror":"https://example.invalid/team/mirror"}}`)
			if kind == "malformed" || kind == "concurrent-malformed" {
				raw = []byte(`{"broken":`)
			}
			if err := providerfs.WriteRepoFileAtomic(root, ".cxt/config", raw, 0600); err != nil {
				t.Fatal(err)
			}
			before := observeConfig(t, root)
			if kind == "concurrent-valid" {
				if err := SetCheckoutMode(ctx, root, "prepare"); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "concurrent-malformed" {
				if err := providerfs.WriteRepoFileAtomic(root, ".cxt/config", []byte(`{"other":`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			current := observeConfig(t, root)
			err := RepairOrigin(ctx, before, "https://example.invalid/team/a", backup)
			if kind == "concurrent-valid" || kind == "concurrent-malformed" {
				if !errors.Is(err, ErrChanged) || !sameObservation(current, observeConfig(t, root)) {
					t.Fatalf("concurrent repair changed config: %v", err)
				}
				if entries, _ := os.ReadDir(backup); len(entries) != 0 {
					t.Fatal("backup made before CAS")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(filepath.Join(backup, "config.before"))
			if err != nil || string(saved) != string(raw) {
				t.Fatalf("backup mismatch: %v", err)
			}
			if origin, _ := Origin(root); origin != "https://example.invalid/team/a" {
				t.Fatal("repair origin missing")
			}
			if kind == "valid" {
				f := readConfigFields(t, root)
				remotes, _ := Load(root)
				if LoadMode(root) != "memory" || len(f["future"]) == 0 || remotes["mirror"] == "" {
					t.Fatal("repair erased preferences")
				}
			}
		})
	}
}

// All exported setting writers participate; lock release after cancellation is
// also exercised above. Values are independent and every acknowledgement lasts.
func TestConfigCoordinationAllSetters(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	var wg sync.WaitGroup
	setters := []func() error{func() error { return SetLoadMode(ctx, root, "memory") }, func() error { return SetCheckoutMode(ctx, root, "prepare") }, func() error { return SetSecretsRedact(ctx, root, "hidden") }, func() error { return SetSecretsMinLen(ctx, root, 8) }, func() error { return SetSecretsScrub(ctx, root, "strict") }, func() error { return SetBoundaryEnforce(ctx, root, "none") }, func() error { return SetCaptureDebounce(ctx, root, 7) }, func() error { return SetStagedProviders(ctx, root, []string{"claude"}) }}
	for _, set := range setters {
		wg.Add(1)
		go func(set func() error) {
			defer wg.Done()
			if err := set(); err != nil {
				t.Error(err)
			}
		}(set)
	}
	wg.Wait()
	if LoadMode(root) != "memory" || CheckoutMode(root) != "prepare" || SecretsRedact(root) != "hidden" || SecretsMinLen(root) != 8 || SecretsScrub(root) != "strict" || BoundaryEnforce(root) != "none" || CaptureDebounce(root) != 7*time.Second || len(StagedProviders(root)) != 1 {
		t.Fatal("setter lost an unrelated setting")
	}
}

func TestConfigCoordinationBrokenGitRootDoesNotForkConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+filepath.Join(root, "missing")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := SetLoadMode(context.Background(), sub, "memory"); err == nil {
		t.Fatal("unresolved Git root accepted")
	}
	for _, dir := range []string{root, sub} {
		if _, err := os.Stat(filepath.Join(dir, ".cxt")); !os.IsNotExist(err) {
			t.Fatalf("split config created: %v", err)
		}
	}
}
