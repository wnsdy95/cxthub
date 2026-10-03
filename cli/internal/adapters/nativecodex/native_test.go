//go:build darwin || linux

package nativecodex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in only: use an installed native binary with a unique empty home and a
// loopback synthetic provider which cannot generate a response. This verifies
// real protocol/persistence, never a model, capacity or interactive TUI attach.
func TestNativeCodexOfflineTransport(t *testing.T) {
	opts, requests, authorization := offlineNativeFixture(t)
	root := filepath.Dir(opts.Cwd)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	s, err := Start(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	thread, err := s.StartThread(ctx, ThreadOptions{Model: "cxt-synthetic-model", ModelProvider: "cxt-offline-fixture", Sandbox: "read-only", ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	text := "CXT_SYNTHETIC_START\n" + strings.Repeat("\ubcf4\uad00\ub41c \ud569\uc131 \ub300\ud654 \u00b7 archived data only\n", 32000) + "CXT_SYNTHETIC_END"
	receipt, err := s.InjectHistory(ctx, []HistoryMessage{{Role: "user", Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Acknowledged || receipt.ProviderAcceptance != "unverified" || receipt.ThreadID != thread.ID {
		t.Fatal(receipt)
	}
	// Poll only this fixture's generated rollout; never inspect native user data.
	found := false
	deadline := time.Now().Add(3 * time.Second)
	for !found && time.Now().Before(deadline) {
		err = filepath.WalkDir(filepath.Join(root, "state"), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
			for scanner.Scan() {
				var row struct {
					Type    string `json:"type"`
					Payload struct {
						Role    string `json:"role"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"payload"`
				}
				if json.Unmarshal(scanner.Bytes(), &row) == nil && row.Type == "response_item" && row.Payload.Role == "user" {
					for _, content := range row.Payload.Content {
						if content.Text == text {
							found = true
						}
					}
				}
			}
			return scanner.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("ACK did not yield the exact synthetic persisted item within fixture deadline")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || authorization.Load() {
		t.Fatalf("unexpected provider activity: calls=%d auth=%v", requests.Load(), authorization.Load())
	}
	t.Logf("host=%s bytes=%d items=%d hash=%s ack=true persisted=true model_calls=0 native_acceptance=unverified", s.HostIdentity(), receipt.UTF8Bytes, receipt.Items, receipt.PayloadHash)
	if receipt.UTF8Bytes < 1<<20 {
		t.Fatal(fmt.Sprint("fixture too small: ", receipt.UTF8Bytes))
	}
}

func offlineNativeFixture(t *testing.T) (Options, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	executable := os.Getenv("CXT_TEST_NATIVE_CODEX")
	if executable == "" {
		t.Skip("set CXT_TEST_NATIVE_CODEX to an absolute installed binary for isolated offline protocol validation")
	}
	if !filepath.IsAbs(executable) {
		t.Fatal("native binary must be an absolute path")
	}
	root := t.TempDir()
	for _, dir := range []string{"home", "state", "work"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var requests atomic.Int64
	var authorization atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			authorization.Store(true)
		}
		if r.Method == http.MethodPost {
			requests.Add(1)
			http.Error(w, "model calls are forbidden in this fixture", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(provider.Close)
	settings := []struct {
		key   string
		value any
	}{
		{"model_provider", "cxt-offline-fixture"}, {"model", "cxt-synthetic-model"},
		{"model_providers.cxt-offline-fixture.name", "CXTHub offline fixture"},
		{"model_providers.cxt-offline-fixture.base_url", provider.URL + "/v1"},
		{"model_providers.cxt-offline-fixture.wire_api", "responses"},
		{"model_providers.cxt-offline-fixture.requires_openai_auth", false},
		{"analytics.enabled", false},
		{"cli_auth_credentials_store", "file"},
	}
	var args []string
	for _, setting := range settings {
		b, _ := json.Marshal(setting.value)
		args = append(args, "-c", setting.key+"="+string(b))
	}
	env := []string{"HOME=" + filepath.Join(root, "home"), "CODEX_HOME=" + filepath.Join(root, "state"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8", "TERM=dumb"}
	return Options{Executable: executable, Cwd: filepath.Join(root, "work"), Env: env, ConfigArgs: args}, &requests, &authorization
}
