//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Real native process, isolated HOME, canned local provider, no inference.
// Copy one installed bundled entry only into this fixture's private catalog.
func staticWindowNativeFixture(t *testing.T) (Options, string) {
	t.Helper()
	opts, requests, authorization := offlineNativeFixture(t)
	t.Cleanup(func() {
		if requests.Load() != 0 || authorization.Load() {
			t.Error("native window probe made a generation request or used credentials")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, opts.Executable, "debug", "models", "--bundled")
	cmd.Dir = opts.Cwd
	cmd.Env = opts.Env
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("isolated bundled fixture metadata unavailable", err)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		t.Fatal("invalid bundled fixture response")
	}
	var models []map[string]json.RawMessage
	if json.Unmarshal(root["models"], &models) != nil || len(models) == 0 {
		t.Fatal("missing bundled fixture models")
	}
	model := models[0]
	for key, value := range map[string]string{"slug": `"gpt-5.4"`, "context_window": `272000`, "max_context_window": `1000000`, "effective_context_window_percent": `95`, "auto_compact_token_limit": `null`} {
		model[key] = json.RawMessage(value)
	}
	delete(model, "used_fallback_model_metadata")
	raw, err = json.Marshal(map[string]any{"models": []any{model}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(opts.Cwd), "private-catalog.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Built-in openai cannot be replaced by model_providers.openai. Native
	// 0.157.1 supplies its endpoint through the top-level openai_base_url instead.
	// Keep the fixture's empty HOME/file credential store and loopback endpoint;
	// reading a static catalog and starting an empty thread need no credentials.
	var args []string
	for i := 0; i < len(opts.ConfigArgs); i += 2 {
		setting := opts.ConfigArgs[i+1]
		key, value, _ := strings.Cut(setting, "=")
		switch {
		case key == "model_provider":
			setting = `model_provider="openai"`
		case key == "model_providers.cxt-offline-fixture.base_url":
			setting = "openai_base_url=" + value
		case strings.HasPrefix(key, "model_providers.cxt-offline-fixture."):
			continue
		}
		args = append(args, opts.ConfigArgs[i], setting)
	}
	opts.ConfigArgs = args
	quoted, _ := json.Marshal(path)
	opts.ConfigArgs = append(opts.ConfigArgs, "-c", "model_catalog_json="+string(quoted), "-c", "model_context_window=1000000")
	return opts, path
}
func TestNativeCodexStaticWindowBinding(t *testing.T) {
	opts, path := staticWindowNativeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, thread, binding, err := StartWindowBound(ctx, opts, ThreadOptions{Model: "gpt-5.4", ModelProvider: "openai", Sandbox: "read-only", ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w := binding.ModelWindow()
	if w.PackingUsableWindow != 950000 || w.NativeUsableWindow != 950000 || binding.RuntimeScope() == "" {
		t.Fatal("native packing semantics mismatch", w)
	}
	if err = binding.Validate(ctx, thread); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(original, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err = binding.Validate(ctx, thread); err == nil {
		t.Fatal("changed catalog accepted despite cached native manager")
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err = binding.Validate(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = binding.Validate(ctx, thread); err == nil {
		t.Fatal("closed runtime binding accepted")
	}
	t.Logf("host=%s packing_window=%d native_usable_window=%d model_calls=0 acceptance=unverified", s.HostIdentity(), w.PackingUsableWindow, w.NativeUsableWindow)
}

func TestWindowCatalogFIFOIsRejectedWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog-fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := readWindowCatalog(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as catalog")
		}
	case <-time.After(time.Second):
		t.Fatal("catalog open ignored bounded lifecycle")
	}
}
