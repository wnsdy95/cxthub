//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Exercise the actual public composition hook with stock native processes, an
// isolated account/configuration, and a TEST-ONLY catalog. The real TUI reaches
// first-question preparation, where invalid cloud state blocks model requests.
// This verifies readiness, source authorization and cleanup, never acceptance.
func TestNativeDeferredPublicCompositionReadiness(t *testing.T) {
	executable := os.Getenv("CXT_TEST_NATIVE_CODEX")
	if executable == "" {
		t.Skip("set CXT_TEST_NATIVE_CODEX to installed native executable")
	}
	if !filepath.IsAbs(executable) {
		t.Fatal("absolute executable required")
	}
	f := newEmptyBootstrapFixture(t, false)
	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(f.root)
	if err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(canonical)
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte("[projects."+string(quoted)+"]\ntrust_level = \"trusted\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int64
	var allowCanned, exactInput atomic.Bool
	var authorization atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" && auth != "Bearer cxt-offline-fixture-not-a-real-key" {
			authorization.Store(true)
		}
		if r.Method == http.MethodPost {
			posts.Add(1)
			if allowCanned.Load() {
				data, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
				var body struct {
					Input []struct{ Content []struct{ Text string } }
				}
				_ = json.Unmarshal(data, &body)
				archive, question := false, false
				for _, item := range body.Input {
					for _, c := range item.Content {
						archive = archive || strings.Contains(c.Text, "SYNTHETIC_MAIN_ARCHIVE") && strings.Contains(c.Text, "SYNTHETIC_PROJECT_MEMORY")
						question = question || c.Text == "PRIVATE_TASK\nnext"
					}
				}
				exactInput.Store(archive && question && nativeDeferredHasReceipt(f.root, "injected_ready"))
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []string{
					`{"type":"response.created","response":{"id":"response-1"}}`,
					`{"type":"response.output_item.added","output_index":0,"item":{"id":"answer-1","type":"message","role":"assistant","content":[]}}`,
					`{"type":"response.output_text.delta","item_id":"answer-1","output_index":0,"content_index":0,"delta":"READY"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"id":"answer-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"READY","annotations":[]}]}}`,
					`{"type":"response.completed","response":{"id":"response-1","status":"completed","usage":{"input_tokens":12000,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":12001}}}`,
				} {
					fmt.Fprintf(w, "data: %s\n\n", event)
					w.(http.Flusher).Flush()
				}
				return
			}
			http.Error(w, "no model calls permitted", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "debug", "models", "--bundled")
	cmd.Dir = f.root
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("native fixture metadata", err)
	}
	var catalog struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if json.Unmarshal(raw, &catalog) != nil || len(catalog.Models) == 0 {
		t.Fatal("missing fixture catalog")
	}
	model := catalog.Models[0]
	for k, v := range map[string]string{"slug": `"gpt-5.4"`, "context_window": "272000", "max_context_window": "1000000", "effective_context_window_percent": "95", "auto_compact_token_limit": "null"} {
		model[k] = json.RawMessage(v)
	}
	delete(model, "used_fallback_model_metadata")
	raw, _ = json.Marshal(map[string]any{"models": []any{model}})
	path := filepath.Join(f.home, "fixture-catalog.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var args []string
	for _, setting := range []struct {
		k string
		v any
	}{
		{"model_provider", "openai"}, {"model", "gpt-5.4"}, {"service_tier", "default"}, {"model_catalog_json", path},
		{"openai_base_url", server.URL + "/v1"}, {"web_search", "disabled"},
		{"analytics.enabled", false}, {"cli_auth_credentials_store", "file"},
	} {
		value, _ := json.Marshal(setting.v)
		args = append(args, "-c", setting.k+"="+string(value))
	}
	args = append(args, "--yolo", "--no-alt-screen", "--", "PRIVATE_TASK\r\nnext")
	req := nativeLaunchRequest(args...)
	req.Cwd, req.Executable = f.root, executable
	req.Intent.Pull, req.Intent.ContextBudget = true, 800000
	prepared, err := f.hooks.PrepareDeferred(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Cleanup()
	if prepared.SessionID == "" || prepared.Validate == nil || len(prepared.Args) < 3 || prepared.Args[len(prepared.Args)-2] != "--" || prepared.Args[len(prepared.Args)-1] != "PRIVATE_TASK\nnext" {
		t.Fatal("lost literal normalized first task")
	}
	if !slices.Equal(prepared.Env, req.Environment(ctx)) {
		t.Fatal("native and TUI environments differ")
	}
	if err := prepared.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	reads := f.reads
	f.mu.Unlock()
	if reads != 0 {
		t.Fatal("runtime readiness pinned main before first question")
	}
	packages, _ := filepath.Glob(filepath.Join(f.root, ".cxt", "input-packages", "*.json"))
	if len(packages) != 0 {
		t.Fatal("runtime readiness claimed package delivery")
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Validate(ctx); err == nil {
		t.Fatal("catalog changed before TUI launch but was accepted")
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Validate(ctx); err == nil {
		t.Fatal("closed runtime accepted")
	}
	if posts.Load() != 0 || authorization.Load() {
		t.Fatal("native preparation contacted model or used credentials")
	}
	for _, arg := range prepared.Args {
		if strings.HasPrefix(arg, "unix://") {
			if _, err := os.Stat(strings.TrimPrefix(arg, "unix://")); !os.IsNotExist(err) {
				t.Fatal("private handoff socket leaked")
			}
		}
	}
	t.Log("public hook runtime ready, actual question withheld, latest-main selection deferred; model_calls=0")

	// Complete the real TUI -> first-question -> application boundary. This
	// deliberately malformed synthetic main must fail before history injection
	// or an inference request. A dummy local key only avoids the TUI login UI;
	// it is not a credential and every inference URL remains loopback.
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 PTY fixture unavailable")
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "cxt-offline-fixture-not-a-real-key")
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"cxt-offline-fixture-not-a-real-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM", "xterm-256color")
	f.setMode("nonempty")
	second, err := f.hooks.PrepareDeferred(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Cleanup()
	stop := startNativeDeferredPTY(t, python, executable, f.root, second)
	defer stop()
	if err := second.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case failure, open := <-second.Failure:
		if !open || failure == nil {
			t.Fatal("invalid source accepted")
		}

	case <-ctx.Done():
		t.Fatal("TUI did not reach preparation", ctx.Err())
	}
	f.mu.Lock()
	reads = f.reads
	f.mu.Unlock()
	if reads == 0 || posts.Load() != 0 || authorization.Load() {
		t.Fatalf("application gate: source_reads=%d model_calls=%d", reads, posts.Load())
	}
	t.Log("actual native TUI question reached latest-main preparation; invalid source blocked; model_calls=0")
	stop()
	_ = second.Cleanup()

	// Valid synthetic cloud sources exercise the entire public composition.
	// The provider is still loopback and its response proves transport only.
	cloud := nativeDeferredCloudFixture(t, f.repo)
	defer cloud.Close()
	cfg := f.cfg
	cfg.RemoteEndpoint = cloud.URL
	selectionGit(t, f.root, "switch", "-qc", "feature/local-position")
	third, err := providerLaunchHooks(cfg).PrepareDeferred(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Cleanup()
	allowCanned.Store(true)
	stopThird := startNativeDeferredPTY(t, python, executable, f.root, third)
	defer stopThird()
	if err := third.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for !nativeDeferredHasReceipt(f.root, "first_turn_observed") {
		select {
		case failure := <-third.Failure:
			t.Fatalf("valid synthetic source failed: %v", failure)
		case <-ctx.Done():
			t.Fatal("canned first turn did not finish", ctx.Err())
		case <-ticker.C:
		}
	}
	select {
	case failure := <-third.Failure:
		t.Fatalf("lifecycle monitor ended after completion: %v", failure)
	default:
	}

	if posts.Load() != 1 || !exactInput.Load() || !nativeDeferredHasReceipt(f.root, "first_turn_observed") {
		t.Fatalf("canned composition: requests=%d exact=%v outcome=%v", posts.Load(), exactInput.Load(), nativeDeferredHasReceipt(f.root, "first_turn_observed"))
	}
	t.Log("public composition: latest main history+memory, actual question, durable receipt before one loopback request, correlated completion; real_model_calls=0")
	stopThird()
	select {
	case failure, open := <-third.Failure:
		if !open || failure == nil {
			t.Fatal("unexpected TUI disconnect was not reported")
		}
	case <-ctx.Done():
		t.Fatal("lifecycle did not observe post-completion disconnect")
	}

}

// Discard terminal output; only answer cursor-position queries. EOF from the
// test terminates/reaps its own child, so failed assertions cannot leak a TUI.
const nativeDeferredPTY = `
import fcntl,json,os,pty,selectors,struct,subprocess,sys,termios,time
argv=json.loads(sys.stdin.readline())
master,slave=pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack("HHHH",32,120,0,0))
child=subprocess.Popen(argv,stdin=slave,stdout=slave,stderr=slave,start_new_session=True)
os.close(slave)
selector=selectors.DefaultSelector()
selector.register(master,selectors.EVENT_READ)
selector.register(sys.stdin,selectors.EVENT_READ)
tail=b""
try:
    deadline=time.monotonic()+35
    stop=False
    while child.poll() is None and time.monotonic()<deadline and not stop:
        for key,_ in selector.select(.1):
            if key.fileobj is sys.stdin:
                if not os.read(sys.stdin.fileno(),4096):stop=True
            else:
                try:part=os.read(master,65536)
                except OSError:stop=True;break
                data=tail+part
                if b"\x1b[6n" in data:os.write(master,b"\x1b[1;1R")
                tail=data[-3:]
finally:
    child.terminate()
    try:child.wait(timeout=2)
    except subprocess.TimeoutExpired:child.kill();child.wait(timeout=2)
    selector.close()
    os.close(master)
`

func startNativeDeferredPTY(t *testing.T, python, executable, cwd string, p delivcli.DeferredProviderLaunch) func() {
	t.Helper()
	argv, _ := json.Marshal(append([]string{executable}, p.Args...))
	cmd := exec.Command(python, "-c", nativeDeferredPTY)
	cmd.Dir, cmd.Env = cwd, p.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = stdin.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("PTY cleanup timeout")
			}
		})
	}
	if _, err = stdin.Write(append(argv, '\n')); err != nil {
		stop()
		t.Fatal(err)
	}
	return stop
}

func nativeDeferredHasReceipt(root, state string) bool {
	paths, _ := filepath.Glob(filepath.Join(root, ".cxt", "delivery-receipts", "*.json"))
	for _, path := range paths {
		raw, _ := os.ReadFile(path)
		var r struct {
			Receipt delivcli.ProviderLaunchReceipt
		}
		if json.Unmarshal(raw, &r) == nil && r.Receipt.State == state {
			return true
		}
	}
	return false
}

func nativeDeferredCloudFixture(t *testing.T, repo string) *httptest.Server {
	t.Helper()
	events := []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "SYNTHETIC_MAIN_ARCHIVE"}}}}
	eventBytes, _ := json.Marshal(events)
	doc := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "fixture-native-main"}, Events: events}
	canonical, err := domain.CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.HashContent(canonical)
	state, memory := domain.HashContent([]byte("cloud state")), domain.HashContent([]byte("cloud memory"))
	code := strings.Repeat("b", 40)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("unexpected fixture authorization")
			http.Error(w, "denied", 403)
			return
		}
		q := r.URL.Query()
		switch {
		case strings.HasSuffix(r.URL.Path, "/context-query"):
			if q.Get("branch") != "main" {
				t.Error("not latest main")
			}
			_ = json.NewEncoder(w).Encode(domain.ContextQueryView{Version: 1, Branch: "main", Position: id, StateHash: state, Revision: domain.RepositoryRevision{Graph: 1, Evidence: 1},
				Snapshots: []domain.Snapshot{{ID: id, DocHash: id, RepoID: repo, Provider: domain.ProviderCodex, Branch: "main"}},
				Inclusion: &domain.BranchContext{CodeCommit: code, SnapshotID: id, SnapshotIDs: []domain.ContentHash{id}}})
		case strings.HasSuffix(r.URL.Path, "/effective-memory"):
			if q.Get("branch") != "main" || q.Get("snapshot_id") != string(id) || q.Get("code_commit") != code {
				t.Error("wrong memory selection")
			}
			page := domain.EffectiveMemoryPage{Content: "prompt", Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: id, CodeCommit: code}, StateHash: memory, LineageHash: memory, Total: 1,
				Items: []domain.EffectiveMemoryItem{{ID: memory, SourceSnapshot: id, Kind: "decision", Text: "SYNTHETIC_PROJECT_MEMORY", State: "retained", Reason: "project_decision"}}}
			page.Revision.Graph, page.Revision.Evidence = 1, 1
			_ = json.NewEncoder(w).Encode(page)
		case strings.HasSuffix(r.URL.Path, "/turns"):
			_ = json.NewEncoder(w).Encode(domain.AgentHistoryPage{Version: 1, Hash: id, Provider: domain.ProviderCodex, SessionID: "fixture-native-main", Total: 1, Before: 1, NextBefore: -1, Turns: []domain.AgentHistoryTurn{{Start: 0, End: 1, Hash: domain.HashContent(eventBytes), Events: events}}})
		default:
			t.Errorf("unexpected fixture path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}
