package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = out
	runErr := run(append([]string{"cxt"}, args...))
	os.Stdout = previous
	if runErr != nil {
		t.Fatalf("cxt %v: %v", args, runErr)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The whole command, from the entry point: a real repository, and synthetic
// session files in a home directory made for the test.
func TestSegmentsCommandReadsRecordsAndWritesNothing(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, repo, hooks := filepath.Join(base, "home"), filepath.Join(base, "app"), filepath.Join(base, "hooks")
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
	sha := strings.TrimSpace(string(head))

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
