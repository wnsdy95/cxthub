package sessionrecords

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// Every record in these tests is synthetic.

var testScope = domain.SessionRecordScope{Folders: []string{"/work/app", "/work/app-feature"}, Origin: "example.test/team/app"}

type claudeLine map[string]any

func claudeUser(uuid, session, prompt string, second int, cwd, branch string, content any) claudeLine {
	line := claudeLine{"type": "user", "uuid": uuid, "sessionId": session, "timestamp": claudeTime(second), "cwd": cwd, "gitBranch": branch, "message": map[string]any{"role": "user", "content": content}}
	if prompt != "" {
		line["promptId"] = prompt
	}
	return line
}

func claudeAssistant(uuid, session string, second int, cwd, branch string, content any) claudeLine {
	return claudeLine{"type": "assistant", "uuid": uuid, "sessionId": session, "timestamp": claudeTime(second), "cwd": cwd, "gitBranch": branch, "message": map[string]any{"role": "assistant", "content": content}}
}

func claudeTime(second int) string {
	return fmt.Sprintf("2031-01-02T03:%02d:%02d.500Z", second/60, second%60)
}

func claudeMs(second int) int64 {
	at, err := time.Parse(time.RFC3339Nano, claudeTime(second))
	if err != nil {
		panic(err)
	}
	return at.UnixMilli()
}

func bashCall(id string) []any {
	return []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "git commit"}}}
}

func toolResult(id string, content any) []any {
	return []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}}
}

func writeClaudeFile(t *testing.T, path string, lines ...any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, line := range lines {
		if raw, ok := line.(string); ok {
			out.WriteString(raw)
			continue
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readClaude(t *testing.T, projects string) domain.SessionRecordBatch {
	t.Helper()
	batch, err := NewClaudeRecords(projects).ReadSessionRecords(context.Background(), testScope)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func sessionByID(t *testing.T, batch domain.SessionRecordBatch, id string) domain.RecordedSession {
	t.Helper()
	for _, session := range batch.Sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("session %q was not read; got %d sessions", id, len(batch.Sessions))
	return domain.RecordedSession{}
}

func TestClaudeRecordsReadsConversationRecordsAndCommands(t *testing.T) {
	projects := t.TempDir()
	writeClaudeFile(t, filepath.Join(projects, "-work-app", "s1.jsonl"),
		claudeLine{"type": "mode", "sessionId": "s1"},
		claudeUser("u1", "s1", "p1", 0, "/work/app", "main", "please commit"),
		claudeAssistant("a1", "s1", 1, "/work/app", "main", bashCall("call-1")),
		// The same call and result repeated inside a non-conversation record
		// must not be counted a second time.
		claudeLine{"type": "api-request-blob", "uuid": "blob", "timestamp": claudeTime(2), "message": map[string]any{"content": append(bashCall("call-1"), toolResult("call-1", "[main abc1234] again")...)}},
		claudeUser("u2", "s1", "p1", 9, "/work/app/pkg", "main", toolResult("call-1", "[main abc1234] change\n 1 file changed")),
		claudeAssistant("a2", "s1", 10, "/work/app/pkg", "main", []any{map[string]any{"type": "text", "text": "done abc1234"}}),
		claudeUser("u3", "s1", "p2", 20, "/work/app", "HEAD", "now detached"),
		claudeAssistant("a3", "s1", 21, "/work/app", "HEAD", bashCall("call-2")),
		claudeUser("u4", "s1", "p2", 25, "/work/app", "HEAD", toolResult("call-2", []any{
			map[string]any{"type": "text", "text": "first line"},
			map[string]any{"type": "text", "text": "def5678 at the start of a line"},
			map[string]any{"type": "image", "source": map[string]any{"data": "0000000"}},
		})),
		// A user record without a turn identifier stays in the current turn.
		claudeUser("u5", "s1", "", 30, "/work/app", "", "injected"),
	)
	batch := readClaude(t, projects)
	if len(batch.Sessions) != 1 || len(batch.Unread) != 0 {
		t.Fatalf("sessions=%d unread=%+v", len(batch.Sessions), batch.Unread)
	}
	session := sessionByID(t, batch, "s1")
	if session.Provider != domain.ProviderClaude || session.Sub || session.Scope != domain.SessionScopeFolder {
		t.Fatalf("session = %+v", session)
	}
	want := []domain.RecordedItem{
		{ID: "u1", Turn: "p1", AtMs: claudeMs(0), Folder: "/work/app", Branch: "main"},
		{ID: "a1", Turn: "p1", AtMs: claudeMs(1), Folder: "/work/app", Branch: "main"},
		{ID: "u2", Turn: "p1", AtMs: claudeMs(9), Folder: "/work/app/pkg", Branch: "main", Commands: []domain.RecordedCommand{
			{StartedMs: claudeMs(1), EndedMs: claudeMs(9), Folder: "/work/app", Tokens: []string{"abc1234"}},
		}},
		{ID: "a2", Turn: "p1", AtMs: claudeMs(10), Folder: "/work/app/pkg", Branch: "main"},
		{ID: "u3", Turn: "p2", AtMs: claudeMs(20), Folder: "/work/app", Detached: true},
		{ID: "a3", Turn: "p2", AtMs: claudeMs(21), Folder: "/work/app", Detached: true},
		{ID: "u4", Turn: "p2", AtMs: claudeMs(25), Folder: "/work/app", Detached: true, Commands: []domain.RecordedCommand{
			{StartedMs: claudeMs(21), EndedMs: claudeMs(25), Folder: "/work/app", Tokens: []string{"def5678"}},
		}},
		{ID: "u5", Turn: "p2", AtMs: claudeMs(30), Folder: "/work/app"},
	}
	if !reflect.DeepEqual(session.Items, want) {
		t.Fatalf("items:\n got %+v\nwant %+v", session.Items, want)
	}
}

func TestClaudeRecordsKeepsOnlySessionsThatWorkedInTheRepository(t *testing.T) {
	projects := t.TempDir()
	// The project folder name does not decide; the records' folders do.
	writeClaudeFile(t, filepath.Join(projects, "-somewhere-else", "visited.jsonl"),
		claudeUser("v1", "visited", "p1", 0, "/somewhere/else", "x", "start outside"),
		claudeUser("v2", "visited", "p1", 5, "/work/app-feature/pkg", "feature", "then inside"),
	)
	writeClaudeFile(t, filepath.Join(projects, "-work-app-other", "outside.jsonl"),
		claudeUser("o1", "outside", "p1", 0, "/work/app-other", "main", "a sibling folder is not inside"),
	)
	batch := readClaude(t, projects)
	if len(batch.Sessions) != 1 || batch.Sessions[0].ID != "visited" || len(batch.Sessions[0].Items) != 2 {
		t.Fatalf("sessions = %+v", batch.Sessions)
	}
}

func TestClaudeRecordsReadsSubSessionsAndTheirSpawningCall(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-work-app")
	writeClaudeFile(t, filepath.Join(dir, "parent.jsonl"),
		claudeUser("u1", "parent", "p1", 0, "/work/app", "main", "delegate"),
		claudeAssistant("a1", "parent", 1, "/work/app", "main", []any{map[string]any{"type": "tool_use", "id": "spawn-1", "name": "Agent", "input": map[string]any{}}}),
		claudeAssistant("a2", "parent", 2, "/work/app", "main", []any{map[string]any{"type": "tool_use", "id": "unrelated", "name": "Read", "input": map[string]any{}}}),
	)
	writeClaudeFile(t, filepath.Join(dir, "parent", "subagents", "agent-withcall.jsonl"),
		claudeUser("s1", "parent", "", 3, "/work/app", "main", "sub task"),
	)
	writeClaudeFile(t, filepath.Join(dir, "parent", "subagents", "agent-withcall.meta.json"), `{"agentType":"general","toolUseId":"spawn-1","spawnDepth":1}`)
	writeClaudeFile(t, filepath.Join(dir, "parent", "subagents", "workflows", "review", "agent-inworkflow.jsonl"),
		claudeUser("w1", "parent", "", 4, "/work/app", "main", "workflow task"),
	)
	// Not a sub-session file: wrong name.
	writeClaudeFile(t, filepath.Join(dir, "parent", "subagents", "notes.jsonl"), claudeUser("n1", "parent", "", 5, "/work/app", "main", "x"))

	batch := readClaude(t, projects)
	if len(batch.Sessions) != 3 {
		t.Fatalf("sessions = %+v", batch.Sessions)
	}
	withCall := sessionByID(t, batch, "withcall")
	if !withCall.Sub || withCall.Parent != "parent" || withCall.SpawnItem != "a1" || withCall.FileCopies != 0 {
		t.Fatalf("sub-session with a recorded call = %+v", withCall)
	}
	inWorkflow := sessionByID(t, batch, "inworkflow")
	if !inWorkflow.Sub || inWorkflow.Parent != "parent" || inWorkflow.SpawnItem != "" {
		t.Fatalf("workflow sub-session = %+v", inWorkflow)
	}
}

func TestClaudeRecordsReadsACopiedSubSessionFileOnce(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-work-app")
	original := []any{
		claudeUser("s1", "original", "", 3, "/work/app", "main", "sub task"),
		claudeAssistant("s2", "original", 4, "/work/app", "main", []any{map[string]any{"type": "text", "text": "ok"}}),
	}
	writeClaudeFile(t, filepath.Join(dir, "original.jsonl"), claudeUser("u1", "original", "p1", 0, "/work/app", "main", "go"))
	// The fork's folder sorts first, and its copy keeps the records' own
	// session identifier. The parent comes from the records, not the folder.
	writeClaudeFile(t, filepath.Join(dir, "a-fork", "subagents", "agent-shared.jsonl"), original...)
	writeClaudeFile(t, filepath.Join(dir, "original", "subagents", "agent-shared.jsonl"), original...)
	writeClaudeFile(t, filepath.Join(dir, "original", "subagents", "agent-shared.meta.json"), `{"toolUseId":"missing-call"}`)
	// Another parent with an agent of the same name is a different session.
	writeClaudeFile(t, filepath.Join(dir, "other", "subagents", "agent-shared.jsonl"), claudeUser("x1", "other", "", 9, "/work/app", "main", "different"))

	batch := readClaude(t, projects)
	var subs []domain.RecordedSession
	for _, session := range batch.Sessions {
		if session.Sub {
			subs = append(subs, session)
		}
	}
	if len(subs) != 2 {
		t.Fatalf("sub-sessions = %+v", subs)
	}
	for _, sub := range subs {
		switch sub.Parent {
		case "original":
			if sub.ID != "shared" || sub.FileCopies != 1 || len(sub.Items) != 2 || sub.SpawnItem != "" {
				t.Fatalf("copied sub-session = %+v", sub)
			}
		case "other":
			if sub.FileCopies != 0 || len(sub.Items) != 1 {
				t.Fatalf("other sub-session = %+v", sub)
			}
		default:
			t.Fatalf("unexpected parent %q", sub.Parent)
		}
	}
}

func TestClaudeRecordsLeavesCopiedConversationForTheRulesToCountOnce(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-work-app")
	shared := func(session string) []any {
		// A fork copies records with their identifiers and rewrites only the
		// session identifier.
		return []any{
			claudeUser("m1", session, "p1", 0, "/work/app", "main", "start"),
			claudeAssistant("m2", session, 1, "/work/app", "main", bashCall("call-1")),
			claudeUser("m3", session, "p1", 5, "/work/app", "main", toolResult("call-1", "[main aaaaaaa] before the fork")),
		}
	}
	writeClaudeFile(t, filepath.Join(dir, "original.jsonl"), append(shared("original"), claudeUser("m4", "original", "p2", 10, "/work/app", "main", "continue"))...)
	writeClaudeFile(t, filepath.Join(dir, "fork.jsonl"), append(shared("fork"), claudeUser("f1", "fork", "p9", 60, "/work/app", "main", "branch off"))...)

	batch := readClaude(t, projects)
	report := domain.BuildSegmentReport(domain.SegmentInputs{
		Git: domain.GitRecords{
			Worktrees: []domain.RecordedWorktree{{Root: "/work/app", Main: true, Current: "main"}},
			Commits:   []domain.RecordedCommit{{SHA: strings.Repeat("a", 40), CreatedSec: claudeMs(3) / 1000}},
		},
		Sessions: batch.Sessions,
	})
	for _, session := range report.Sessions {
		switch session.ID {
		case "original":
			if session.Records != 4 || session.CopiedRecords != 0 || len(session.Segments) != 1 {
				t.Fatalf("original = %+v", session)
			}
		case "fork":
			if session.Records != 1 || session.CopiedRecords != 3 || len(session.Segments) != 0 || !reflect.DeepEqual(session.CopiedFrom, []string{"original"}) {
				t.Fatalf("fork = %+v", session)
			}
		}
	}
	if report.Summary.CopiedRecords != 3 || report.Summary.LinkedCommits != 1 || report.Summary.CommitsWithOneSession != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestClaudeRecordsReportsWhatItCouldNotRead(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-work-app")
	writeClaudeFile(t, filepath.Join(dir, "partly.jsonl"),
		claudeUser("u1", "partly", "p1", 0, "/work/app", "main", "fine"),
		"{this line is not JSON}\n",
		claudeLine{"type": "user", "sessionId": "partly", "timestamp": claudeTime(2), "cwd": "/work/app"},
		claudeLine{"type": "assistant", "uuid": "a9", "sessionId": "partly", "timestamp": "yesterday", "cwd": "/work/app"},
		// A record whose fields changed type cannot be decoded either.
		`{"type":"user","uuid":"u9","timestamp":12345}`+"\n",
		claudeUser("u2", "partly", "p1", 5, "/work/app", "main", "still fine"),
		// A last line without its newline is still being written.
		`{"type":"user","uuid":"u3","timestamp":"`+claudeTime(6)+`","cwd":"/work/app"`,
	)
	// Nothing in this file can be read, so it cannot be placed in any
	// repository. It is reported rather than passed over.
	writeClaudeFile(t, filepath.Join(projects, "-unknown", "newformat.jsonl"), "<session format=\"2\">\n", "</session>\n")
	// Readable records place this session outside the repository.
	writeClaudeFile(t, filepath.Join(projects, "-elsewhere", "elsewhere.jsonl"),
		claudeUser("e1", "elsewhere", "p1", 0, "/elsewhere", "main", "other project"),
		"{broken}\n",
	)
	batch := readClaude(t, projects)
	if got := sessionByID(t, batch, "partly").Items; len(got) != 2 || got[0].ID != "u1" || got[1].ID != "u2" {
		t.Fatalf("readable records = %+v", got)
	}
	want := []domain.UnreadSessionRecord{
		{Provider: domain.ProviderClaude, Source: filepath.Join(projects, "-unknown", "newformat.jsonl"), Reason: "2 line(s) could not be decoded as session records, first at line 1"},
		{Provider: domain.ProviderClaude, Source: filepath.Join(dir, "partly.jsonl"), Reason: "2 line(s) could not be decoded as session records, first at line 2"},
		{Provider: domain.ProviderClaude, Source: filepath.Join(dir, "partly.jsonl"), Reason: "2 conversation record(s) have no readable identifier or time"},
	}
	if !reflect.DeepEqual(batch.Unread, want) {
		t.Fatalf("unread:\n got %+v\nwant %+v", batch.Unread, want)
	}
}

func TestClaudeRecordsCountsARepeatedRecordOnceWithinAFile(t *testing.T) {
	projects := t.TempDir()
	line := claudeUser("u1", "s1", "p1", 0, "/work/app", "main", "once")
	writeClaudeFile(t, filepath.Join(projects, "-work-app", "s1.jsonl"), line, line)
	if items := sessionByID(t, readClaude(t, projects), "s1").Items; len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
}

func TestClaudeRecordsWithoutASessionFolderOrWhenCancelled(t *testing.T) {
	if batch := readClaude(t, filepath.Join(t.TempDir(), "missing")); len(batch.Sessions) != 0 || len(batch.Unread) != 0 {
		t.Fatalf("missing folder = %+v", batch)
	}
	projects := t.TempDir()
	writeClaudeFile(t, filepath.Join(projects, "-work-app", "s1.jsonl"), claudeUser("u1", "s1", "p1", 0, "/work/app", "main", "x"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewClaudeRecords(projects).ReadSessionRecords(ctx, testScope); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}
