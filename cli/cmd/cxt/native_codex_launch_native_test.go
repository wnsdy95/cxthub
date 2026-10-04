//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the composition mapping with the installed binary, never the user's
// config/account or a real model. Unlike a mock, native config precedence and
// permission echoes are resolved by the actual app-server.
func TestNativeCodexBoundLaunch(t *testing.T) {
	executable := os.Getenv("CXT_TEST_NATIVE_CODEX")
	if executable == "" {
		t.Skip("set CXT_TEST_NATIVE_CODEX to an installed absolute Codex binary")
	}
	if !filepath.IsAbs(executable) {
		t.Fatal("absolute native executable required")
	}
	var calls atomic.Int64
	var authorized atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authorized.Store(true)
		}
		if r.Method == http.MethodPost {
			calls.Add(1)
			http.Error(w, "fixture forbids model calls", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer provider.Close()
	for _, tc := range []struct {
		name  string
		args  []string
		model string
	}{
		{"native config default", []string{"-c", `model="cxt-first"`, "--config=model=\"cxt-last\"", "-s", "read-only", "-a", "untrusted"}, "cxt-last"},
		{"explicit model and bypass", []string{"-m", "cxt-explicit", "--yolo", "-c", `model="cxt-config"`, "-s", "read-only", "--strict-config", "--search", "--disable=shell_tool", "--enable=shell_tool"}, "cxt-explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"home", "state", "work"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			var args []string
			for _, kv := range []struct {
				key   string
				value any
			}{
				{"model_provider", "cxt-bound-fixture"}, {"model", "cxt-default"},
				{"model_providers.cxt-bound-fixture.name", "Synthetic launch fixture"},
				{"model_providers.cxt-bound-fixture.base_url", provider.URL + "/v1"},
				{"model_providers.cxt-bound-fixture.wire_api", "responses"},
				{"model_providers.cxt-bound-fixture.requires_openai_auth", false},
				{"analytics.enabled", false}, {"cli_auth_credentials_store", "file"},
			} {
				b, _ := json.Marshal(kv.value)
				args = append(args, "-c", kv.key+"="+string(b))
			}
			args = append(args, tc.args...)
			args = append(args, "--", "PRIVATE_SYNTHETIC_TASK_NOT_SUBMITTED")
			req := nativeLaunchRequest(args...)
			req.Executable, req.Cwd = executable, filepath.Join(root, "work")
			launch, err := bindNativeCodexLaunch(req)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			env := []string{"HOME=" + filepath.Join(root, "home"), "CODEX_HOME=" + filepath.Join(root, "state"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8", "TERM=dumb"}
			session, thread, err := launch.start(ctx, env)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			cwd, _ := filepath.EvalSymlinks(req.Cwd)
			if thread.Model != tc.model || thread.ModelProvider != "cxt-bound-fixture" || thread.Cwd != cwd || !strings.HasPrefix(thread.SettingsHash, "sha256:") {
				t.Fatal("native runtime does not match launch")
			}
			if !launch.prompt.Present() || launch.prompt.Text() != "PRIVATE_SYNTHETIC_TASK_NOT_SUBMITTED" {
				t.Fatal("lost initial task")
			}
			if err = session.Close(); err != nil {
				t.Fatal(err)
			}
			t.Logf("model=%s native-settings-bound=true initial-task-submitted=false capacity=unverified", thread.Model)
		})
	}
	if calls.Load() != 0 || authorized.Load() {
		t.Fatal("unexpected fixture model or authorization request")
	}
}
