package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// segmentsTestTree records every path under root with its type, size and
// content, to prove the command left the repository and the home directory as
// they were.
func segmentsTestTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		tree[path] = fmt.Sprintf("%v %d %d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func segmentsTestRun(t *testing.T, args ...string) string {
	t.Helper()
	out, err := segmentsTestRunErr(t, args...)
	if err != nil {
		t.Fatalf("cxt %v: %v", args, err)
	}
	return out
}

// segmentsTestRunErr runs the command from the entry point and returns what
// it printed together with its error.
func segmentsTestRunErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = out
	runErr := run(append([]string{"cxt"}, args...))
	os.Stdout = previous
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), runErr
}

// segmentsTestFixture makes a real repository with one commit, and synthetic
// session files in a home directory made for the test. It leaves the process
// in the repository.
func segmentsTestFixture(t *testing.T) (base, repo, sha string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, hooks := filepath.Join(base, "home"), filepath.Join(base, "hooks")
	repo = filepath.Join(base, "app")
	for _, dir := range []string{home, repo, hooks} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	mainTestGit(t, repo, "init", "-b", "main")
	for _, setting := range [][2]string{{"user.name", "cxt test"}, {"user.email", "cxt@example.test"}, {"core.hooksPath", hooks}, {"gc.auto", "0"}, {"commit.gpgsign", "false"}} {
		mainTestGit(t, repo, "config", setting[0], setting[1])
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("synthetic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainTestGit(t, repo, "add", "README.md")
	commit := exec.Command("git", "-C", repo, "commit", "-m", "first")
	commit.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_DATE=2031-01-02T03:00:05Z", "GIT_COMMITTER_DATE=2031-01-02T03:00:05Z")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha = strings.TrimSpace(string(head))

	record := func(kind, uuid, session, prompt, at string, content any) string {
		line := map[string]any{"type": kind, "uuid": uuid, "sessionId": session, "timestamp": "2031-01-02T03:00:" + at + "Z", "cwd": repo, "gitBranch": "main", "message": map[string]any{"content": content}}
		if prompt != "" {
			line["promptId"] = prompt
		}
		raw, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	shared := func(session string) string {
		return record("user", "m1", session, "p1", "00.000", "commit it") +
			record("assistant", "m2", session, "", "01.000", []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "Bash"}}) +
			record("user", "m3", session, "p1", "06.000", []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "[main (root-commit) " + sha[:7] + "] first"}}) +
			record("assistant", "m4", session, "", "07.000", []any{map[string]any{"type": "text", "text": "committed"}})
	}
	projects := filepath.Join(home, ".claude", "projects", "-synthetic")
	files := map[string]string{
		filepath.Join(projects, "original.jsonl"): shared("original"),
		// A fork copies the earlier records and adds its own.
		filepath.Join(projects, "fork.jsonl"):                                  shared("fork") + record("user", "f1", "fork", "p2", "40.000", "continue elsewhere"),
		filepath.Join(projects, "original", "subagents", "agent-helper.jsonl"): record("user", "h1", "original", "", "02.000", "sub task"),
		filepath.Join(projects, "fork", "subagents", "agent-helper.jsonl"):     record("user", "h1", "original", "", "02.000", "sub task"),
		filepath.Join(projects, "unreadable.jsonl"):                            "<not a session record>\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Chdir(repo)
	return base, repo, sha
}

// The whole command, from the entry point.
func TestSegmentsCommandReadsRecordsAndWritesNothing(t *testing.T) {
	base, repo, sha := segmentsTestFixture(t)
	before := segmentsTestTree(t, base)
	first := segmentsTestRun(t, "segments", "--json")
	second := segmentsTestRun(t, "segments", "--json")
	text := segmentsTestRun(t, "segments")
	if after := segmentsTestTree(t, base); !reflect.DeepEqual(before, after) {
		t.Fatal("cxt segments changed a file in the repository or the home directory")
	}
	if first != second {
		t.Fatalf("two runs over the same records differ:\n%s\n%s", first, second)
	}
	var report domain.SegmentReport
	if err := json.Unmarshal([]byte(first), &report); err != nil {
		t.Fatalf("output is not a report: %v\n%s", err, first)
	}
	summary := report.Summary
	if summary.Sessions.Claude != 2 || summary.Sessions.ClaudeSub != 1 || summary.LinkedCommits != 1 || summary.LinkedByPrintedNumber != 1 ||
		summary.CommitsWithOneSession != 1 || summary.HeadLogCommits != 1 || summary.CopiedRecords != 4 || summary.SessionFileCopies != 1 ||
		summary.Spawns != 1 || summary.UnreadRecords != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if report.Repository.Root != repo || len(report.Commits) != 1 || report.Commits[0].SHA != sha || report.Commits[0].Sessions[0].ID != "original" || report.Commits[0].Local.State != domain.LocalCommitOnBranch {
		t.Fatalf("commits = %+v", report.Commits)
	}
	for _, session := range report.Sessions {
		switch session.ID {
		case "original":
			if len(session.Segments) != 1 || session.Segments[0].First != "m1" || session.Segments[0].Last != "m4" || !session.Segments[0].ClosesTurn || session.Uncommitted != nil {
				t.Fatalf("original = %+v", session)
			}
		case "fork":
			if len(session.Segments) != 0 || session.Records != 1 || session.Uncommitted == nil || session.Uncommitted.Places[0].Branch != "main" {
				t.Fatalf("fork = %+v", session)
			}
		case "helper":
			if session.Parent != "original" || session.FileCopies != 1 {
				t.Fatalf("helper = %+v", session)
			}
		}
	}
	if !strings.Contains(text, "linked by printed-number") || !strings.Contains(text, "Unread session records (1)") {
		t.Fatalf("text output:\n%s", text)
	}
}

// The upload, from the entry point, against a server made for the test: one
// request per session, nothing but names in them, and nothing written here.
func TestSegmentsPushUploadsEachSessionAndWritesNothing(t *testing.T) {
	base, repo, sha := segmentsTestFixture(t)
	type request struct {
		method, path, auth string
		body               string
	}
	var mu sync.Mutex
	var requests []request
	answer := func(session string) (int, string) {
		return http.StatusOK, `{"revision":1,"body_hash":"sha256:` + strings.Repeat("0", 64) + `","changed":true}`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, request{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(raw)})
		status, body := answer(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	t.Setenv("CXT_REMOTE", server.URL+"/api/v1")
	t.Setenv("CXT_TOKEN", "synthetic-token")

	before := segmentsTestTree(t, base)
	out := segmentsTestRun(t, "segments", "push", "--json")
	if after := segmentsTestTree(t, base); !reflect.DeepEqual(before, after) {
		t.Fatal("cxt segments push changed a file in the repository or the home directory")
	}
	var result domain.SegmentPushResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not a result: %v\n%s", err, out)
	}
	if result.Stored != 3 || result.Unchanged != 0 || result.Refused != 0 || len(result.Sessions) != 3 || len(result.Unread) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(requests) != 3 {
		t.Fatalf("requests = %+v", requests)
	}
	bodies := map[string]domain.AnchoredSessionBody{}
	for _, sent := range requests {
		prefix := "/api/v1/repos/"
		if sent.method != http.MethodPut || sent.auth != "Bearer synthetic-token" || !strings.HasPrefix(sent.path, prefix) || !strings.Contains(sent.path, "/anchored-sessions/claude/") {
			t.Fatalf("request = %+v", sent)
		}
		// A worktree is a name on the server; no folder of this computer is sent.
		if strings.Contains(sent.body, base) || strings.Contains(sent.body, filepath.Dir(repo)) {
			t.Fatalf("a path was uploaded: %s", sent.body)
		}
		var body domain.AnchoredSessionBody
		if err := json.Unmarshal([]byte(sent.body), &body); err != nil {
			t.Fatalf("body is not a report: %v\n%s", err, sent.body)
		}
		bodies[sent.path[strings.LastIndex(sent.path, "/")+1:]] = body
	}
	original, fork, helper := bodies["original"], bodies["fork"], bodies["helper"]
	if original.RuleVersion != domain.AnchoredSessionRuleVersion || original.Records != 4 || len(original.Segments) != 1 || original.Uncommitted != nil {
		t.Fatalf("original = %+v", original)
	}
	if commit := original.Segments[0].Commits[0]; commit.SHA != sha || commit.Rule != domain.SegmentLinkPrintedNumber || commit.Subject != "first" ||
		commit.AuthorEmail != "cxt@example.test" || commit.AuthoredAt != "2031-01-02T03:00:05Z" || commit.CreatedAt != "2031-01-02T03:00:05Z" {
		t.Fatalf("commit = %+v", commit)
	}
	if fork.CopiedRecords != 4 || !reflect.DeepEqual(fork.CopiedFrom, []string{"original"}) || fork.Records != 1 || fork.Uncommitted == nil ||
		!reflect.DeepEqual(fork.Uncommitted.Places, []domain.AnchoredPlace{{Worktree: "app", Branch: "main", Rule: domain.SessionPlaceRecorded}}) {
		t.Fatalf("fork = %+v", fork)
	}
	if !helper.Sub || helper.Parent != "original" || helper.Spawn == nil || helper.FileCopies != 1 {
		t.Fatalf("helper = %+v", helper)
	}
	if host, _ := os.Hostname(); original.Machine != host {
		t.Fatalf("machine = %q, host = %q", original.Machine, host)
	}

	// A report the server refuses is listed and fails the command; the others are still sent.
	mu.Lock()
	requests = nil
	answer = func(session string) (int, string) {
		if session == "fork" {
			return http.StatusConflict, `{"error":{"code":"conflict","message":"conflict"}}`
		}
		return http.StatusOK, `{"revision":1,"body_hash":"sha256:` + strings.Repeat("0", 64) + `","changed":false}`
	}
	mu.Unlock()
	text, err := segmentsTestRunErr(t, "segments", "push")
	if err == nil || !strings.Contains(err.Error(), "1 of 3 session reports were refused") || len(requests) != 3 ||
		!strings.Contains(text, "Session reports: 0 stored, 2 unchanged, 1 refused (3 sessions)") || !strings.Contains(text, "  claude fork: ") {
		t.Fatalf("err=%v requests=%d\n%s", err, len(requests), text)
	}

	// A caller the server does not accept stops the run at the first request.
	mu.Lock()
	requests = nil
	answer = func(string) (int, string) {
		return http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"unauthorized"}}`
	}
	mu.Unlock()
	if _, err := segmentsTestRunErr(t, "segments", "push"); err == nil || len(requests) != 1 || !strings.Contains(err.Error(), "stopped after 0 of 3 session reports") {
		t.Fatalf("err=%v requests=%d", err, len(requests))
	}
	if after := segmentsTestTree(t, base); !reflect.DeepEqual(before, after) {
		t.Fatal("a failed upload changed a file in the repository or the home directory")
	}
}
