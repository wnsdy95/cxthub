//go:build darwin || linux

package nativecodex

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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Real native TUI/app-server, credential-free loopback canned response: protocol
// ordering and exact bytes only, never actual provider acceptance or capacity.
func TestNativeCodexGenerationOffline(t *testing.T) {
	opts, forbidden, auth := offlineNativeFixture(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 PTY helper required")
	}
	const question = "CXT_INITIAL_QUESTION"
	history := "CXT_ARCHIVE_START\n" + strings.Repeat("synthetic archived data only\n", 32000) + "CXT_ARCHIVE_END"
	var posts, prepared, validated, recorded atomic.Int32
	var exact, unauthorized atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			unauthorized.Store(true)
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		posts.Add(1)
		data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "invalid", 400)
			return
		}
		var body struct {
			Input []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(data, &body)
		archive, task := false, false
		for _, item := range body.Input {
			for _, c := range item.Content {
				archive = archive || c.Text == history
				task = task || c.Text == question
			}
		}
		exact.Store(archive && task && prepared.Load() == 1 && validated.Load() == 2)
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"response.created","response":{"id":"response-1"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"answer-1","type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"answer-1","output_index":0,"content_index":0,"delta":"READY"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"answer-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"READY","annotations":[]}]}}`,
			`{"type":"response.completed","response":{"id":"response-1","status":"completed","usage":{"input_tokens":200000,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":200001}}}`,
		}
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer provider.Close()
	url, _ := json.Marshal(provider.URL + "/v1")
	opts.ConfigArgs = append(opts.ConfigArgs, "-c", "model_providers.cxt-offline-fixture.base_url="+string(url), "-c", `web_search="disabled"`)
	opts.Cwd, err = filepath.EvalSymlinks(opts.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := json.Marshal(opts.Cwd)
	if err = os.WriteFile(filepath.Join(filepath.Dir(opts.Cwd), "state", "config.toml"), []byte("[projects."+string(key)+"]\ntrust_level=\"trusted\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
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
	h, err := s.OpenGenerationHandoff(ctx, func(_ context.Context, got Thread, prompt string) (PreparedGeneration, error) {
		prepared.Add(1)
		if got != thread || prompt != question {
			return PreparedGeneration{}, ErrState
		}
		return PreparedGeneration{History: []HistoryMessage{{Role: "user", Text: history}}, Validate: func(context.Context) error { validated.Add(1); return nil }, Observe: func(_ context.Context, o GenerationObservation) error { recorded.Add(1); return nil }}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	argv := []string{opts.Executable, "--remote", h.URL(), "--no-alt-screen", "-C", opts.Cwd, "-m", thread.Model}
	argv = append(argv, opts.ConfigArgs...)
	argv = append(argv, "resume", thread.ID, "--", question)
	encoded, _ := json.Marshal(argv)
	cmd := exec.Command(python, "-c", nativePTYFixture)
	cmd.Dir = opts.Cwd
	cmd.Env = append(append([]string(nil), opts.Env...), "TERM=xterm-256color")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = stdin.Write(append(encoded, '\n'))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = stdin.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("PTY cleanup timed out")
		}
	}()
	if _, err = h.Wait(ctx); err != nil {
		t.Fatal("TUI resume", err)
	}
	o, err := h.WaitGeneration(ctx)
	if err != nil {
		h.mu.Lock()
		cause := h.err
		h.mu.Unlock()
		t.Fatalf("generation: %v (%v); prepare=%d validation=%d posts=%d", err, cause, prepared.Load(), validated.Load(), posts.Load())
	}
	if !exact.Load() || unauthorized.Load() || auth.Load() || forbidden.Load() != 0 || posts.Load() != 1 || recorded.Load() != 1 || o.Outcome != "completed" {
		t.Fatalf("canned transport: exact=%v auth=%v posts=%d observations=%d outcome=%s", exact.Load(), unauthorized.Load(), posts.Load(), recorded.Load(), o.Outcome)
	}
	t.Logf("same_thread=true exact_history_and_question=true bytes=%d local_fixture_requests=%d real_model_calls=0 provider_acceptance=unverified usage_known=%v", len(history), posts.Load(), o.UsageKnown)
}
