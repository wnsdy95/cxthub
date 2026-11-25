package sessionrecords

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// codexContentHome writes a thread history database in the layout Codex
// 0.171 keeps, with synthetic items, through the sqlite3 program.
func codexContentHome(t *testing.T, script string) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 program not available")
	}
	home := t.TempDir()
	history := filepath.Join(home, "thread_history_1.sqlite")
	schema := `PRAGMA journal_mode=WAL;
CREATE TABLE thread_items (thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL, created_at_ms INTEGER NOT NULL,
 item_json TEXT NOT NULL, item_type TEXT NOT NULL, updated_at_ordinal INTEGER NOT NULL DEFAULT 0, started_at_ms INTEGER, completed_at_ms INTEGER);
`
	if out, err := exec.Command("sqlite3", history, schema+script).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v\n%s", err, out)
	}
	return home
}

func codexContentInsert(thread, turn, id string, ordinal int, item map[string]any) string {
	item["id"] = id
	raw, _ := json.Marshal(item)
	return "INSERT INTO thread_items VALUES ('" + thread + "','" + turn + "','" + id + "'," + strconv.Itoa(ordinal) + "," + strconv.Itoa(1000+ordinal) + ",'" +
		strings.ReplaceAll(string(raw), "'", "''") + "','" + item["type"].(string) + "',0,NULL,NULL);\n"
}

func TestCodexContentReadsTheNamedRecords(t *testing.T) {
	home := codexContentHome(t,
		codexContentInsert("lead", "t1", "u", 1, map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "Add the parser, it's needed."}}})+
			codexContentInsert("lead", "t1", "c", 2, map[string]any{"type": "commandExecution", "command": "git commit -m parser", "cwd": "/work/app", "aggregatedOutput": "[main abc1234] parser", "exitCode": 0, "status": "completed"})+
			codexContentInsert("lead", "t2", "a", 3, map[string]any{"type": "agentMessage", "text": "Committed the parser."})+
			codexContentInsert("lead", "t2", "n", 4, map[string]any{"type": "holoDeck", "scene": "bridge"})+
			codexContentInsert("other", "t1", "u", 1, map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "Another thread."}}}))
	before, _ := os.ReadDir(home)
	read, err := NewCodexContent(home).ReadSessionParts(context.Background(), t.TempDir(), domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"},
		[]string{"t1/u", "t1/c", "t2/a", "t2/n"}, domain.SessionPartResume{})
	if err != nil {
		t.Fatal(err)
	}
	// A thread this short is one open part; the command is a call and its result.
	if len(read.Missing) != 0 || len(read.Parts) != 1 || read.Parts[0].Sealed || !reflect.DeepEqual(read.Parts[0].Counts, []int{1, 2, 1, 1}) {
		t.Fatalf("parts: %+v missing %v", read.Parts, read.Missing)
	}
	doc := domain.SessionPartsConversation(read.Parts)
	if doc.Envelope.SourceProvider != domain.ProviderCodex || doc.Envelope.SessionOriginID != "lead" || len(doc.Events) != 5 {
		t.Fatalf("envelope: %+v events %d", doc.Envelope, len(doc.Events))
	}
	if _, err := domain.CanonicalBytes(read.Parts[0].Document()); err != nil {
		t.Fatalf("the part is not a canonical document: %v", err)
	}
	var kinds []string
	for index, event := range doc.Events {
		if event.Seq != index {
			t.Fatalf("event %d is numbered %d", index, event.Seq)
		}
		kinds = append(kinds, event.Kind+":"+event.ID)
	}
	if want := []string{"message:u", "tool_call:c", "tool_result:c", "message:a", "tool_call:n"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("events: %v", kinds)
	}
	if doc.Events[0].Blocks[0].Text != "Add the parser, it's needed." || doc.Events[3].Blocks[0].Text != "Committed the parser." || doc.Events[4].ToolName != "unknown:holoDeck" {
		t.Fatalf("texts: %+v", doc.Events)
	}
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), "/work/app") || strings.Contains(string(raw), "Another thread.") {
		t.Fatalf("the document holds a folder of this computer or another thread's text: %s", raw)
	}
	// Reading leaves the tool's files as they were.
	if after, _ := os.ReadDir(home); len(after) != len(before) {
		t.Fatalf("files changed: %v -> %v", before, after)
	}
}

func TestCodexContentMasksLikeCapture(t *testing.T) {
	home := codexContentHome(t,
		codexContentInsert("lead", "t1", "u", 1, map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "use synthetic-listed-value-123 at https://user:synthetic-password@example.invalid/path"}}})+
			codexContentInsert("lead", "t1", "c", 2, map[string]any{"type": "commandExecution", "command": "echo synthetic-listed-value-123", "aggregatedOutput": "synthetic-listed-value-123", "exitCode": 0, "status": "completed"}))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".cxtsecrets"), []byte("synthetic-listed-value-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := NewCodexContent(home).ReadSessionParts(context.Background(), dir, domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"}, []string{"t1/u", "t1/c"}, domain.SessionPartResume{})
	if err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionPartsConversation(read.Parts)
	raw, _ := json.Marshal(doc)
	if strings.Contains(string(raw), "synthetic-listed-value-123") || strings.Contains(string(raw), "synthetic-password") {
		t.Fatalf("a secret reached the content: %s", raw)
	}
	if len(doc.Events) != 3 || !strings.Contains(string(raw), "deleted by security policy") || !strings.Contains(string(raw), "redacted:password") {
		t.Fatalf("the masking marks are missing: %s", raw)
	}
}

func TestCodexContentNamesWhatItCannotReadBack(t *testing.T) {
	home := codexContentHome(t, codexContentInsert("lead", "t1", "u", 1, map[string]any{"type": "agentMessage", "text": "kept"}))
	reader := NewCodexContent(home)
	ctx := context.Background()
	// A record that is gone is named, and nothing is returned of the rest.
	read, err := reader.ReadSessionParts(ctx, t.TempDir(), domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"}, []string{"t1/u", "t9/gone"}, domain.SessionPartResume{})
	if err != nil || !reflect.DeepEqual(read.Missing, []string{"t9/gone"}) || len(read.Parts) != 0 {
		t.Fatalf("a missing record: %+v %v", read, err)
	}
	// A thread with no item is all missing.
	if read, err := reader.ReadSessionParts(ctx, t.TempDir(), domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "nobody"}, []string{"t1/u"}, domain.SessionPartResume{}); err != nil || len(read.Missing) != 1 {
		t.Fatalf("an unknown thread: %+v %v", read, err)
	}
	for name, records := range map[string]domain.SessionRecordRanges{
		"another tool's session":        {Provider: domain.ProviderClaude, ID: "lead"},
		"an identifier that is not one": {Provider: domain.ProviderCodex, ID: "lead' OR '1'='1"},
	} {
		if _, err := reader.ReadSessionParts(ctx, t.TempDir(), records, []string{"t1/u"}, domain.SessionPartResume{}); err == nil {
			t.Fatalf("%s was read", name)
		}
	}
	// No database, a database of another layout, and no sqlite3 program are
	// each said in words.
	if _, err := NewCodexContent(t.TempDir()).ReadSessionParts(ctx, t.TempDir(), domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"}, []string{"t1/u"}, domain.SessionPartResume{}); err == nil || !strings.Contains(err.Error(), "no Codex thread history database") {
		t.Fatalf("no database: %v", err)
	}
	changed := t.TempDir()
	if out, err := exec.Command("sqlite3", filepath.Join(changed, "thread_history_2.sqlite"), "CREATE TABLE thread_items (thread_id TEXT, payload TEXT);").CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v\n%s", err, out)
	}
	if _, err := NewCodexContent(changed).ReadSessionParts(ctx, t.TempDir(), domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"}, []string{"t1/u"}, domain.SessionPartResume{}); err == nil || !strings.Contains(err.Error(), "thread_history_2.sqlite: table thread_items has no column") || !strings.Contains(err.Error(), "A newer cxt is needed") {
		t.Fatalf("another layout: %v", err)
	}
	missing := NewCodexContent(home)
	missing.rows = func(context.Context, string, string, func(json.RawMessage) error) error { return errSQLiteMissing }
	if _, err := missing.ReadSessionParts(ctx, t.TempDir(), domain.SessionRecordRanges{Provider: domain.ProviderCodex, ID: "lead"}, []string{"t1/u"}, domain.SessionPartResume{}); err == nil || !strings.Contains(err.Error(), "sqlite3 program is not installed") {
		t.Fatalf("no sqlite3 program: %v", err)
	}
}
