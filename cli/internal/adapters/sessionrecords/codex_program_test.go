package sessionrecords

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// TestWhatTheCodexProgramWritesIsWhatCxtReads runs a real Codex program and
// compares what it writes and publishes with what cxt reads. It is for the
// person who maintains the Codex reader, on a new Codex release:
//
//	CXT_CODEX_BIN=/path/to/codex go test ./internal/adapters/sessionrecords/ -run CodexProgram -v
//
// Codex is given a home directory of its own under the test's temporary
// directory. It needs no login: no model is asked anything. Without
// CXT_CODEX_BIN the test does not run, because no test should start another
// program that nobody named.
func TestWhatTheCodexProgramWritesIsWhatCxtReads(t *testing.T) {
	program := os.Getenv("CXT_CODEX_BIN")
	if program == "" {
		t.Skip("CXT_CODEX_BIN names no Codex program")
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 program not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	home, work := t.TempDir(), t.TempDir()
	if resolved, err := filepath.EvalSymlinks(work); err == nil {
		work = resolved
	}
	environment := append(os.Environ(), "CODEX_HOME="+home)

	// Codex creates its databases and one thread that starts in work.
	server := exec.CommandContext(ctx, program, "app-server")
	server.Env = environment
	stdin, err := server.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := server.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReaderSize(stdout, 1<<20)
	next := 0
	call := func(method string, params any) json.RawMessage {
		t.Helper()
		next++
		request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": next, "method": method, "params": params})
		if _, err := stdin.Write(append(request, '\n')); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		for {
			line, err := lines.ReadBytes('\n')
			if err != nil {
				t.Fatalf("%s: Codex gave no answer: %v", method, err)
			}
			var answer struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &answer) != nil || answer.ID == nil || *answer.ID != next {
				continue // a notification, or a request of Codex's own
			}
			if len(answer.Error) > 0 {
				t.Fatalf("%s: %s", method, answer.Error)
			}
			return answer.Result
		}
	}
	call("initialize", map[string]any{"clientInfo": map[string]any{"name": "cxt-test", "title": "cxt test", "version": "0"}})
	if _, err := stdin.Write([]byte(`{"jsonrpc":"2.0","method":"initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(call("thread/start", map[string]any{"cwd": work}), &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("thread/start: %v", err)
	}
	call("thread/inject_items", map[string]any{"threadId": started.Thread.ID, "items": []any{
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "synthetic"}}},
	}})
	// Codex writes its thread list a moment after it answers.
	scope := domain.SessionRecordScope{Folders: []string{work}}
	identity := func(value string) string { return value }
	var batch domain.SessionRecordBatch
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		batch, err = NewCodexRecords(home, identity).ReadSessionRecords(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Sessions) > 0 || time.Now().After(deadline) {
			break
		}
	}
	_ = stdin.Close()
	_ = server.Wait()

	// 1. The databases are laid out the way cxt reads them.
	if differences := CodexLayoutDifferences(ctx, home, nil); len(differences) != 0 {
		t.Errorf("this Codex keeps its threads another way than cxt reads them: %v", differences)
	}
	// 2. The thread Codex started is read as a session of the folder.
	if len(batch.Unread) != 0 || len(batch.Sessions) != 1 || batch.Sessions[0].ID != started.Thread.ID || batch.Sessions[0].StartFolder != work {
		t.Errorf("the thread %s was not read as a session of %s: %+v", started.Thread.ID, work, batch)
	}
	// 3. Every kind of item this Codex publishes is one cxt reads by its
	// fields. A kind that is not is still uploaded, carried whole; listing it
	// here is how a maintainer learns that a rule could be written for it.
	schema := filepath.Join(t.TempDir(), "schema")
	generate := exec.CommandContext(ctx, program, "app-server", "generate-json-schema", "--out", schema)
	generate.Env = environment
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate-json-schema: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(schema, "v2", "ThreadItemsListResponse.json"))
	if err != nil {
		t.Fatalf("Codex no longer publishes the answer of thread/items/list: %v", err)
	}
	var published struct {
		Definitions struct {
			ThreadItem struct {
				OneOf []struct {
					Properties struct {
						Type struct {
							Enum []string `json:"enum"`
						} `json:"type"`
					} `json:"properties"`
				} `json:"oneOf"`
			} `json:"ThreadItem"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &published); err != nil || len(published.Definitions.ThreadItem.OneOf) == 0 {
		t.Fatalf("the published shape of a thread item was not understood: %v", err)
	}
	var unknown []string
	for _, kind := range published.Definitions.ThreadItem.OneOf {
		for _, name := range kind.Properties.Type.Enum {
			if !codec.CodexItemTypeKnown(name) {
				unknown = append(unknown, name)
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("this Codex publishes kinds of item cxt has no rule for, which it carries whole: %v", unknown)
	}
	version, _ := exec.CommandContext(ctx, program, "--version").Output()
	t.Logf("checked against %s", strings.TrimSpace(string(version)))
}
