package sessionrecords

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// The cache is only a shortcut: with it a reader gives exactly what it gives
// without it, and reads again whatever changed.
func TestClaudeRecordsWithCacheGiveTheSameAndReadOnlyWhatChanged(t *testing.T) {
	projects, cache := t.TempDir(), filepath.Join(t.TempDir(), "cache")
	dir := filepath.Join(projects, "-work-app")
	parent := filepath.Join(dir, "parent.jsonl")
	writeClaudeFile(t, parent,
		claudeUser("u1", "parent", "p1", 0, "/work/app", "main", "delegate"),
		claudeAssistant("a1", "parent", 1, "/work/app", "main", []any{map[string]any{"type": "tool_use", "id": "spawn-1", "name": "Agent", "input": map[string]any{}}}),
		claudeAssistant("a2", "parent", 2, "/work/app", "main", bashCall("call-1")),
		claudeUser("u2", "parent", "p1", 5, "/work/app", "main", toolResult("call-1", "[main abc1234] first")),
		"<not a session record>\n",
	)
	sub := filepath.Join(dir, "parent", "subagents", "agent-helper.jsonl")
	writeClaudeFile(t, sub, claudeUser("s1", "parent", "", 3, "/work/app", "main", "sub task"))
	writeClaudeFile(t, filepath.Join(dir, "parent", "subagents", "agent-helper.meta.json"), `{"toolUseId":"spawn-1"}`)
	other := filepath.Join(dir, "other.jsonl")
	writeClaudeFile(t, other, claudeUser("o1", "other", "p1", 9, "/work/app", "main", "another session"))

	read := func(reader *ClaudeRecords) domain.SessionRecordBatch {
		t.Helper()
		batch, err := reader.ReadSessionRecords(context.Background(), testScope)
		if err != nil {
			t.Fatal(err)
		}
		return batch
	}
	plain := read(NewClaudeRecords(projects))
	cached := NewClaudeRecords(projects).WithCache(cache)
	first := read(cached)
	if !reflect.DeepEqual(first, plain) {
		t.Fatalf("with a cache:\n got %+v\nwant %+v", first, plain)
	}
	// The spawning call is found through the cache as without it.
	if helper := sessionByID(t, first, "helper"); helper.SpawnItem != "a1" || !helper.Sub {
		t.Fatalf("helper = %+v", helper)
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 3 {
		t.Fatalf("cache entries: %v %v", entries, err)
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("cache entry %s: %v %v", entry.Name(), info, err)
		}
	}

	// Unchanged files are answered from the cache: make them unreadable and
	// the answer is still the same.
	stamps := map[string]time.Time{}
	for _, path := range []string{parent, sub, other} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stamps[path] = info.ModTime()
	}
	garble := func(path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		// Same size and time, other bytes: only a reader that goes back to the
		// file would notice.
		if err := os.WriteFile(path, make([]byte, info.Size()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamps[path], stamps[path]); err != nil {
			t.Fatal(err)
		}
	}
	garble(parent)
	garble(other)
	if again := read(cached); !reflect.DeepEqual(again, plain) {
		t.Fatalf("unchanged files were read again:\n got %+v\nwant %+v", again, plain)
	}

	// A file that grew is read again, the others are not.
	writeClaudeFile(t, other,
		claudeUser("o1", "other", "p1", 9, "/work/app", "main", "another session"),
		claudeUser("o2", "other", "p2", 12, "/work/app", "main", "it went on"),
	)
	grown := read(cached)
	if session := sessionByID(t, grown, "other"); len(session.Items) != 2 || session.Items[1].ID != "o2" {
		t.Fatalf("the grown session: %+v", session)
	}
	if session := sessionByID(t, grown, "parent"); len(session.Items) != 4 || len(session.Items[3].Commands) != 1 {
		t.Fatalf("the unchanged session: %+v", session)
	}
	// What could not be read is reported from the cache too.
	if len(grown.Unread) != len(plain.Unread) || len(plain.Unread) == 0 {
		t.Fatalf("unread: %+v, without a cache %+v", grown.Unread, plain.Unread)
	}

	// A cache entry that cannot be read means reading the file again.
	writeClaudeFile(t, other, claudeUser("o1", "other", "p1", 9, "/work/app", "main", "another session"))
	for _, entry := range entries {
		if err := os.WriteFile(filepath.Join(cache, entry.Name()), []byte("not a cache entry"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if session := sessionByID(t, read(cached), "other"); len(session.Items) != 1 {
		t.Fatalf("after a broken cache: %+v", session)
	}
	// Without a cache nothing is written anywhere.
	if _, err := os.Stat(filepath.Join(projects, "cache")); !os.IsNotExist(err) {
		t.Fatalf("a reader without a cache wrote one: %v", err)
	}
}

func TestCodexRecordsWithCacheReadAgainOnlyWhenADatabaseChanged(t *testing.T) {
	home, cache := codexHome(t, "state_5.sqlite", "thread_history_1.sqlite"), filepath.Join(t.TempDir(), "cache")
	fixture := &codexFixture{
		threads: []map[string]any{codexThreadRow("thread-1", "/work/app", nil, "paginated")},
		items:   []map[string]any{codexCommand("thread-1", "t1", "i1", 2000, nil, nil, "/work/app", nil, nil)},
	}
	reader := NewCodexRecords(home, func(raw string) string { return raw }).WithCache(cache)
	reader.rows = fixture.rows
	read := func() domain.SessionRecordBatch {
		t.Helper()
		batch, err := reader.ReadSessionRecords(context.Background(), testScope)
		if err != nil {
			t.Fatal(err)
		}
		return batch
	}
	first := read()
	queries := len(fixture.queries)
	if len(first.Sessions) != 1 || queries == 0 {
		t.Fatalf("first read: %+v queries=%d", first, queries)
	}
	// No database file changed: no query at all, and the same answer.
	if again := read(); !reflect.DeepEqual(again, first) || len(fixture.queries) != queries {
		t.Fatalf("unchanged databases were read again: queries=%d", len(fixture.queries)-queries)
	}
	// Another repository is another answer.
	otherScope := domain.SessionRecordScope{Folders: []string{"/work/other"}}
	if batch, err := reader.ReadSessionRecords(context.Background(), otherScope); err != nil || len(batch.Sessions) != 0 || len(fixture.queries) == queries {
		t.Fatalf("another scope: %+v %v", batch, err)
	}
	queries = len(fixture.queries)
	// The history database changed: everything is read again.
	history := filepath.Join(home, "thread_history_1.sqlite")
	if err := os.WriteFile(history, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.items = append(fixture.items, codexCommand("thread-1", "t1", "i2", 3000, nil, nil, "/work/app", nil, nil))
	if changed := read(); len(changed.Sessions[0].Items) != 2 || len(fixture.queries) == queries {
		t.Fatalf("after a database changed: %+v", changed.Sessions)
	}
	queries = len(fixture.queries)
	// A write-ahead log appearing beside a database is a change too.
	if err := os.WriteFile(history+"-wal", []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	if read(); len(fixture.queries) == queries {
		t.Fatal("a new write-ahead log did not make the reader read again")
	}
	// A read that could not read something is not remembered.
	queries = len(fixture.queries)
	fixture.fail = map[string]error{"FROM thread_spawn_edges": os.ErrPermission}
	if err := os.WriteFile(history, []byte("changed again"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := read()
	fixture.fail = nil
	if len(failed.Unread) == 0 {
		t.Fatalf("the failed read reported nothing: %+v", failed)
	}
	if recovered := read(); len(recovered.Unread) != 0 || len(fixture.queries) <= queries+1 {
		t.Fatalf("the read after a failed one came from the cache: %+v", recovered.Unread)
	}
}
