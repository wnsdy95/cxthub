package sessionrecords

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// codexFixture stands in for the two thread databases with synthetic rows.
type codexFixture struct {
	threads []map[string]any
	edges   []map[string]any
	items   []map[string]any
	fail    map[string]error
	queries []string
	// layout replaces the columns a table says it has. A table that is not
	// named has the columns cxt reads.
	layout map[string][]string
}

var codexInList = regexp.MustCompile(`thread_id IN \(([^)]*)\)`)

func (f *codexFixture) rows(_ context.Context, database, query string, row func(json.RawMessage) error) error {
	f.queries = append(f.queries, query)
	for fragment, err := range f.fail {
		if strings.Contains(query, fragment) {
			return err
		}
	}
	var rows []map[string]any
	switch {
	case strings.Contains(query, "pragma_table_info"):
		for _, table := range codexLayout {
			if !strings.Contains(query, "'"+table.Table+"'") {
				continue
			}
			columns, replaced := f.layout[table.Table]
			if !replaced {
				columns = table.Columns
			}
			for _, column := range columns {
				rows = append(rows, map[string]any{"name": column})
			}
		}
	case strings.Contains(query, "FROM threads"):
		rows = f.threads
	case strings.Contains(query, "FROM thread_spawn_edges"):
		rows = f.edges
	case strings.Contains(query, "SELECT DISTINCT"):
		seen := map[string]bool{}
		for _, item := range f.items {
			if item["item_type"] != "commandExecution" {
				continue
			}
			key := fmt.Sprint(item["thread_id"], item["cwd"])
			if !seen[key] {
				seen[key] = true
				rows = append(rows, map[string]any{"thread_id": item["thread_id"], "cwd": item["cwd"]})
			}
		}
	case strings.Contains(query, "FROM thread_items"):
		named := map[string]bool{}
		for _, id := range strings.Split(codexInList.FindStringSubmatch(query)[1], ",") {
			named[strings.Trim(id, "'")] = true
		}
		for _, item := range f.items {
			if named[item["thread_id"].(string)] {
				rows = append(rows, item)
			}
		}
	default:
		return fmt.Errorf("unexpected query against %s: %s", database, query)
	}
	for _, values := range rows {
		raw, err := json.Marshal(values)
		if err != nil {
			return err
		}
		if err := row(raw); err != nil {
			return err
		}
	}
	return nil
}

func codexHome(t *testing.T, files ...string) string {
	t.Helper()
	home := t.TempDir()
	for _, name := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func testNormalize(raw string) string {
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "git@"), ".git")
	return strings.ToLower(strings.Replace(raw, ":", "/", 1))
}

func readCodex(t *testing.T, home string, fixture *codexFixture) domain.SessionRecordBatch {
	t.Helper()
	reader := &CodexRecords{home: home, rows: fixture.rows, normalize: testNormalize}
	batch, err := reader.ReadSessionRecords(context.Background(), testScope)
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func codexThreadRow(id, cwd string, origin any, mode string) map[string]any {
	return map[string]any{"id": id, "cwd": cwd, "git_origin_url": origin, "created_at_ms": 1000, "history_mode": mode}
}

func codexCommand(thread, turn, item string, created int64, started, completed any, cwd string, duration any, output any) map[string]any {
	return map[string]any{"thread_id": thread, "turn_id": turn, "item_id": item, "item_type": "commandExecution", "created_at_ms": created,
		"started_at_ms": started, "completed_at_ms": completed, "cwd": cwd, "duration_ms": duration, "output": output,
		"tool": nil, "sender": nil, "receivers": nil, "status": nil}
}

func codexOther(thread, turn, item, kind string, created int64) map[string]any {
	return map[string]any{"thread_id": thread, "turn_id": turn, "item_id": item, "item_type": kind, "created_at_ms": created,
		"started_at_ms": nil, "completed_at_ms": nil, "cwd": nil, "duration_ms": nil, "output": nil, "tool": nil, "sender": nil, "receivers": nil, "status": nil}
}

func codexCall(thread, turn, item, tool string, created int64, receivers []string, status string) map[string]any {
	encoded, _ := json.Marshal(receivers)
	row := codexOther(thread, turn, item, "collabAgentToolCall", created)
	row["tool"], row["sender"], row["receivers"], row["status"] = tool, thread, string(encoded), status
	return row
}

func TestCodexRecordsPlacesThreadsInTheRepository(t *testing.T) {
	home := codexHome(t, "state_5.sqlite", "thread_history_1.sqlite")
	fixture := &codexFixture{
		threads: []map[string]any{
			codexThreadRow("by-start-folder", "/work/app/pkg", nil, "paginated"),
			codexThreadRow("by-command-folder", "/home/dev", nil, "paginated"),
			codexThreadRow("by-origin", "/gone/app-old", "git@Example.test:Team/App.git", "paginated"),
			codexThreadRow("other-origin", "/gone/other", "git@example.test:team/other.git", "paginated"),
			codexThreadRow("nowhere", "/home/dev", nil, "paginated"),
		},
		items: []map[string]any{
			codexCommand("by-command-folder", "t1", "i1", 2000, nil, nil, "/work/app-feature", nil, nil),
			codexCommand("nowhere", "t1", "i1", 2000, nil, nil, "/tmp", nil, nil),
		},
	}
	batch := readCodex(t, home, fixture)
	got := map[string]domain.SessionScopeRule{}
	for _, session := range batch.Sessions {
		got[session.ID] = session.Scope
	}
	want := map[string]domain.SessionScopeRule{
		"by-start-folder":   domain.SessionScopeFolder,
		"by-command-folder": domain.SessionScopeFolder,
		"by-origin":         domain.SessionScopeOriginURL,
	}
	if !reflect.DeepEqual(got, want) || len(batch.Unread) != 0 {
		t.Fatalf("scopes = %v, unread = %+v", got, batch.Unread)
	}
	if session := sessionByID(t, batch, "by-origin"); session.StartFolder != "/gone/app-old" || session.StartedMs != 1000 {
		t.Fatalf("session = %+v", session)
	}
}

func TestCodexRecordsReadsItemsCommandsAndRelations(t *testing.T) {
	home := codexHome(t, "state_4.sqlite", "state_5.sqlite", "thread_history_1.sqlite")
	fixture := &codexFixture{
		threads: []map[string]any{
			codexThreadRow("lead", "/work/app", nil, "paginated"),
			codexThreadRow("worker", "/work/app", nil, "paginated"),
			codexThreadRow("orphan", "/work/app", nil, "paginated"),
		},
		edges: []map[string]any{
			{"parent_thread_id": "lead", "child_thread_id": "worker"},
			{"parent_thread_id": "not-read", "child_thread_id": "orphan"},
		},
		items: []map[string]any{
			codexOther("lead", "t1", "i1", "userMessage", 1000),
			codexCall("lead", "t1", "i2", "spawnAgent", 1100, []string{"worker"}, "completed"),
			codexCall("lead", "t1", "i3", "sendInput", 1200, []string{"worker"}, "completed"),
			// Timed by the tool: started and completed.
			codexCommand("worker", "t7", "i1", 2000, int64(2100), int64(2900), "/work/app", 123.0, "[main abc1234] change"),
			// Only a creation time and a duration.
			codexCommand("worker", "t7", "i2", 3000, nil, nil, "/work/app/pkg", 250.5, nil),
			codexOther("worker", "t8", "i1", "agentMessage", 4000),
		},
	}
	batch := readCodex(t, home, fixture)
	if !strings.HasSuffix(fixture.queries[0], "FROM threads ORDER BY id") {
		t.Fatalf("first query = %q", fixture.queries[0])
	}
	worker := sessionByID(t, batch, "worker")
	if !worker.Sub || worker.Parent != "lead" || worker.SpawnItem != "t1/i2" {
		t.Fatalf("worker = %+v", worker)
	}
	wantItems := []domain.RecordedItem{
		{ID: "t7/i1", Turn: "t7", AtMs: 2000, Folder: "/work/app", Commands: []domain.RecordedCommand{{StartedMs: 2100, EndedMs: 2900, Folder: "/work/app", Tokens: []string{"abc1234"}}}},
		{ID: "t7/i2", Turn: "t7", AtMs: 3000, Folder: "/work/app/pkg", Commands: []domain.RecordedCommand{{StartedMs: 3000, EndedMs: 3250, Folder: "/work/app/pkg"}}},
		{ID: "t8/i1", Turn: "t8", AtMs: 4000},
	}
	if !reflect.DeepEqual(worker.Items, wantItems) {
		t.Fatalf("worker items:\n got %+v\nwant %+v", worker.Items, wantItems)
	}
	if lead := sessionByID(t, batch, "lead"); lead.Sub || len(lead.Items) != 3 {
		t.Fatalf("lead = %+v", lead)
	}
	if orphan := sessionByID(t, batch, "orphan"); !orphan.Sub || orphan.Parent != "not-read" || orphan.SpawnItem != "" {
		t.Fatalf("orphan = %+v", orphan)
	}
	wantCalls := []domain.RecordedAgentCall{
		{Provider: domain.ProviderCodex, Tool: "spawnAgent", From: "lead", To: []string{"worker"}, Status: "completed", AtMs: 1100, Item: "t1/i2"},
		{Provider: domain.ProviderCodex, Tool: "sendInput", From: "lead", To: []string{"worker"}, Status: "completed", AtMs: 1200, Item: "t1/i3"},
	}
	if !reflect.DeepEqual(batch.Calls, wantCalls) {
		t.Fatalf("calls = %+v", batch.Calls)
	}
}

func TestCodexRecordsNamesOnlyABoundedNumberOfThreadsPerQuery(t *testing.T) {
	home := codexHome(t, "state_5.sqlite", "thread_history_1.sqlite")
	fixture := &codexFixture{}
	for index := 0; index < 2*codexItemBatch+1; index++ {
		id := fmt.Sprintf("thread-%04d", index)
		fixture.threads = append(fixture.threads, codexThreadRow(id, "/work/app", nil, "paginated"))
		fixture.items = append(fixture.items, codexOther(id, "t1", "i1", "userMessage", 1000))
	}
	batch := readCodex(t, home, fixture)
	if len(batch.Sessions) != 2*codexItemBatch+1 {
		t.Fatalf("sessions = %d", len(batch.Sessions))
	}
	itemQueries := 0
	for _, query := range fixture.queries {
		if match := codexInList.FindStringSubmatch(query); match != nil {
			itemQueries++
			if named := strings.Count(match[1], ",") + 1; named > codexItemBatch {
				t.Fatalf("a query names %d threads", named)
			}
		}
	}
	if itemQueries != 3 {
		t.Fatalf("item queries = %d", itemQueries)
	}
	for _, session := range batch.Sessions {
		if len(session.Items) != 1 {
			t.Fatalf("session %s items = %d", session.ID, len(session.Items))
		}
	}
}

func TestCodexRecordsReportsWhatItCouldNotRead(t *testing.T) {
	both := []string{"state_5.sqlite", "thread_history_1.sqlite"}
	inRepo := []map[string]any{codexThreadRow("t1", "/work/app", nil, "paginated")}
	for _, tc := range []struct {
		name    string
		files   []string
		fixture *codexFixture
		source  string
		reason  string
	}{
		{"the sqlite3 program is missing", both, &codexFixture{fail: map[string]error{"FROM threads": errSQLiteMissing}},
			"state_5.sqlite", "the sqlite3 program is not installed, so the thread database was not read"},
		{"the thread table changed", both, &codexFixture{fail: map[string]error{"FROM threads": errors.New("sqlite3: no such column: history_mode")}, layout: map[string][]string{"threads": {"id", "cwd", "created_at_ms"}}},
			"state_5.sqlite", "Codex keeps its threads another way than this cxt reads them (state_5.sqlite: table threads has no column git_origin_url, history_mode). A newer cxt is needed; the threads stay on this computer and are uploaded once it reads them"},
		{"the item table is gone", both, &codexFixture{threads: inRepo, fail: map[string]error{"SELECT DISTINCT": errors.New("sqlite3: no such table: thread_items")}, layout: map[string][]string{"thread_items": nil}},
			"thread_history_1.sqlite", "Codex keeps its threads another way than this cxt reads them (thread_history_1.sqlite has no table thread_items). A newer cxt is needed; the threads stay on this computer and are uploaded once it reads them"},
		{"a read fails in the layout cxt knows", both, &codexFixture{fail: map[string]error{"FROM threads": errors.New("sqlite3: database is locked")}},
			"state_5.sqlite", "thread database could not be read: sqlite3: database is locked"},
		{"a thread keeps its history in a rollout file", both, &codexFixture{threads: []map[string]any{codexThreadRow("old-thread", "/work/app", nil, "legacy")}},
			"state_5.sqlite", "thread old-thread keeps its history in an older rollout file, which this command does not read"},
		{"a thread identifier is not safe to query", both, &codexFixture{threads: []map[string]any{codexThreadRow("x'; DROP TABLE threads; --", "/work/app", nil, "paginated")}},
			"state_5.sqlite", "a thread identifier cannot be used in a query, so its items were not read"},
		{"an item cannot be decoded", both, &codexFixture{threads: inRepo, items: []map[string]any{{"thread_id": "t1", "turn_id": "t", "item_id": "i", "item_type": "commandExecution", "created_at_ms": "soon", "cwd": "/work/app"}}},
			"thread_history_1.sqlite", "1 thread item(s) could not be decoded"},
		{"only rollout files exist", []string{"sessions/2031/01/02/rollout-synthetic.jsonl"}, &codexFixture{},
			"sessions", "Codex rollout files are not read by this command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := codexHome(t, tc.files...)
			batch := readCodex(t, home, tc.fixture)
			want := []domain.UnreadSessionRecord{{Provider: domain.ProviderCodex, Source: filepath.Join(home, tc.source), Reason: tc.reason}}
			if !reflect.DeepEqual(batch.Unread, want) {
				t.Fatalf("unread:\n got %+v\nwant %+v", batch.Unread, want)
			}
		})
	}
	// A legacy thread is a session of the repository even though its records
	// were not read; an empty thread in the current format is not unread.
	home := codexHome(t, both...)
	batch := readCodex(t, home, &codexFixture{threads: []map[string]any{codexThreadRow("empty", "/work/app", nil, "paginated")}})
	if len(batch.Sessions) != 1 || len(batch.Unread) != 0 {
		t.Fatalf("empty thread: %+v", batch)
	}
}

func TestCodexRecordsWithoutCodexOrWhenCancelled(t *testing.T) {
	if batch := readCodex(t, filepath.Join(t.TempDir(), "missing"), &codexFixture{}); len(batch.Sessions) != 0 || len(batch.Unread) != 0 {
		t.Fatalf("no Codex home = %+v", batch)
	}
	home := codexHome(t, "state_5.sqlite", "thread_history_1.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &CodexRecords{home: home, normalize: testNormalize, rows: func(ctx context.Context, _, _ string, _ func(json.RawMessage) error) error {
		return ctx.Err()
	}}
	if _, err := reader.ReadSessionRecords(ctx, testScope); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestDecodeJSONRows(t *testing.T) {
	var got []string
	collect := func(raw json.RawMessage) error {
		got = append(got, string(raw))
		return nil
	}
	if err := decodeJSONRows(strings.NewReader(""), collect); err != nil || got != nil {
		t.Fatalf("no output: %v %v", got, err)
	}
	if err := decodeJSONRows(strings.NewReader("[{\"a\":1},\n{\"a\":2}]\n"), collect); err != nil || !reflect.DeepEqual(got, []string{`{"a":1}`, `{"a":2}`}) {
		t.Fatalf("rows = %v, err = %v", got, err)
	}
	if err := decodeJSONRows(strings.NewReader("[{\"a\":1},{"), collect); err == nil {
		t.Fatal("a cut-off result must be an error")
	}
}

// The remaining test reads real database files in the layout the tool uses,
// written with synthetic rows, through the sqlite3 program.
func TestCodexRecordsReadsWriteAheadLoggedDatabasesWithoutChangingThem(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 program not available")
	}
	home := t.TempDir()
	state, history := filepath.Join(home, "state_5.sqlite"), filepath.Join(home, "thread_history_1.sqlite")
	sqlite := func(database, script string) {
		t.Helper()
		if out, err := exec.Command("sqlite3", database, script).CombinedOutput(); err != nil {
			t.Fatalf("sqlite3: %v\n%s", err, out)
		}
	}
	sqlite(state, `PRAGMA journal_mode=WAL;
CREATE TABLE threads (id TEXT PRIMARY KEY, cwd TEXT NOT NULL, git_origin_url TEXT, created_at_ms INTEGER, history_mode TEXT, title TEXT);
CREATE TABLE thread_spawn_edges (parent_thread_id TEXT NOT NULL, child_thread_id TEXT NOT NULL PRIMARY KEY, status TEXT NOT NULL);
INSERT INTO threads VALUES ('lead', '/work/app', NULL, 1000, 'paginated', 'synthetic');
INSERT INTO threads VALUES ('worker', '/elsewhere', 'git@example.test:team/app.git', 1500, 'paginated', 'synthetic');
INSERT INTO threads VALUES ('unrelated', '/elsewhere', NULL, 1600, 'paginated', 'synthetic');
INSERT INTO thread_spawn_edges VALUES ('lead', 'worker', 'closed');`)
	sqlite(history, `PRAGMA journal_mode=WAL;
CREATE TABLE thread_items (thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL,
 created_at_ms INTEGER NOT NULL, item_json TEXT NOT NULL, item_type TEXT NOT NULL DEFAULT '', updated_at_ordinal INTEGER NOT NULL DEFAULT 0,
 started_at_ms INTEGER, completed_at_ms INTEGER, PRIMARY KEY (thread_id, turn_id, item_id));
INSERT INTO thread_items VALUES ('lead','t1','b',2,2000,json_object('type','collabAgentToolCall','tool','spawnAgent','senderThreadId','lead','receiverThreadIds',json_array('worker'),'status','completed'),'collabAgentToolCall',0,NULL,NULL);
INSERT INTO thread_items VALUES ('lead','t1','a',1,1900,json_object('type','userMessage'),'userMessage',0,NULL,NULL);
INSERT INTO thread_items VALUES ('worker','t2','a',1,3000,json_object('type','commandExecution','cwd','/elsewhere','durationMs',400,'aggregatedOutput','[detached abc1234] change' || char(10) || 'def5678 next line'),'commandExecution',0,3100,3500);
INSERT INTO thread_items VALUES ('unrelated','t3','a',1,3000,json_object('type','commandExecution','cwd','/elsewhere','aggregatedOutput','0000000'),'commandExecution',0,NULL,NULL);`)

	// files lists the directory with each file's content, so that a changed
	// database and a lock file left behind both show.
	files := func() map[string]string {
		t.Helper()
		entries, err := os.ReadDir(home)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, entry := range entries {
			raw, err := os.ReadFile(filepath.Join(home, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[entry.Name()] = string(raw)
		}
		return out
	}
	before := files()
	if len(before) != 2 {
		t.Fatalf("closed databases should have no log beside them: %d files", len(before))
	}
	batch, err := NewCodexRecords(home, testNormalize).ReadSessionRecords(context.Background(), testScope)
	if err != nil {
		t.Fatal(err)
	}
	if after := files(); !reflect.DeepEqual(before, after) {
		t.Fatalf("reading changed the Codex directory: %d files before, %d after", len(before), len(after))
	}
	if len(batch.Unread) != 0 || len(batch.Sessions) != 2 {
		t.Fatalf("batch = %+v", batch)
	}
	lead, worker := sessionByID(t, batch, "lead"), sessionByID(t, batch, "worker")
	if got := []string{lead.Items[0].ID, lead.Items[1].ID}; !reflect.DeepEqual(got, []string{"t1/a", "t1/b"}) {
		t.Fatalf("items are not in the tool's order: %v", got)
	}
	wantCommand := domain.RecordedCommand{StartedMs: 3100, EndedMs: 3500, Folder: "/elsewhere", Tokens: []string{"abc1234", "def5678"}}
	if worker.Scope != domain.SessionScopeOriginURL || worker.Parent != "lead" || worker.SpawnItem != "t1/b" || !reflect.DeepEqual(worker.Items[0].Commands, []domain.RecordedCommand{wantCommand}) {
		t.Fatalf("worker = %+v", worker)
	}
	if len(batch.Calls) != 1 || batch.Calls[0].Tool != "spawnAgent" || !reflect.DeepEqual(batch.Calls[0].To, []string{"worker"}) {
		t.Fatalf("calls = %+v", batch.Calls)
	}

	// While the tool is running, its newest items are only in the write-ahead
	// log. They are read, and reading still leaves every file as it was.
	writer := exec.Command("sqlite3", history)
	stdin, err := writer.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "INSERT INTO thread_items VALUES ('lead','t1','c',3,2500,json_object('type','agentMessage'),'agentMessage',0,NULL,NULL);\n"); err != nil {
		t.Fatal(err)
	}
	logged := false
	for attempt := 0; attempt < 200 && !logged; attempt++ {
		if info, err := os.Stat(history + "-wal"); err == nil && info.Size() > 0 {
			logged = true
		} else {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if !logged {
		t.Fatal("the writer did not log its item")
	}
	before = files()
	batch, err = NewCodexRecords(home, testNormalize).ReadSessionRecords(context.Background(), testScope)
	if err != nil {
		t.Fatal(err)
	}
	// SQLite keeps reader marks in the writer's shared-memory file, so that
	// one lock file may differ. The databases, the log and the set of files
	// may not.
	after := files()
	if len(after) != len(before) {
		t.Fatalf("reading a database with a live write-ahead log added or removed files: %d before, %d after", len(before), len(after))
	}
	for name, content := range before {
		if !strings.HasSuffix(name, "-shm") && after[name] != content {
			t.Fatalf("reading changed %s", name)
		}
	}
	if items := sessionByID(t, batch, "lead").Items; len(batch.Unread) != 0 || len(items) != 3 || items[2].ID != "t1/c" {
		t.Fatalf("the logged item was not read: items=%+v unread=%+v", items, batch.Unread)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Wait(); err != nil {
		t.Fatal(err)
	}

	if differences := CodexLayoutDifferences(context.Background(), home, nil); len(differences) != 0 {
		t.Fatalf("the databases are in the layout cxt reads, but: %v", differences)
	}
	// A database in a layout this reader does not know is reported, with what
	// differs.
	sqlite(history, "ALTER TABLE thread_items RENAME COLUMN item_json TO payload;")
	batch, err = NewCodexRecords(home, testNormalize).ReadSessionRecords(context.Background(), testScope)
	if err != nil || len(batch.Unread) != 1 || batch.Unread[0].Source != history || !strings.Contains(batch.Unread[0].Reason, "thread_history_1.sqlite: table thread_items has no column item_json)") {
		t.Fatalf("changed layout: batch=%+v err=%v", batch, err)
	}
}
