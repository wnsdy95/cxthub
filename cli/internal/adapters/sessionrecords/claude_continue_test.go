package sessionrecords

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func claudeRaw(t *testing.T, line claudeLine) string {
	t.Helper()
	raw, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

// claudeGrowing is a session file at three moments of its life. Each moment
// holds the one before it and more, as a tool leaves it: a command that
// started before the cut and ends after it, a line that was half written at
// the cut, a record written a second time, a record without a turn of its
// own, a line that is no record, and a record without a time.
func claudeGrowing(t *testing.T) []string {
	t.Helper()
	half := claudeRaw(t, claudeAssistant("a2", "grow", 3, "/work/app", "main", bashCall("call-2")))
	first := claudeRaw(t, claudeUser("u1", "grow", "p1", 0, "/work/app", "main", "begin")) +
		claudeRaw(t, claudeAssistant("a1", "grow", 1, "/work/app", "main", bashCall("call-1"))) +
		"<not a session record>\n" +
		half[:len(half)/2]
	second := first + half[len(half)/2:] +
		claudeRaw(t, claudeUser("u2", "grow", "", 5, "/work/app", "main", toolResult("call-1", "[main abc1234] first"))) +
		claudeRaw(t, claudeUser("u1", "grow", "p1", 0, "/work/app", "main", "begin")) +
		claudeRaw(t, claudeAssistant("a3", "grow", 6, "/work/app", "HEAD", "no turn of its own")) +
		strings.Replace(claudeRaw(t, claudeUser("u3", "grow", "p2", 7, "/work/app", "main", "no time")), claudeTime(7), "yesterday", 1)
	third := second +
		claudeRaw(t, claudeUser("u4", "grow", "p2", 8, "/work/app-feature", "topic", toolResult("call-2", "[topic def5678] second"))) +
		"\n" +
		claudeRaw(t, claudeAssistant("a4", "grow", 9, "/work/app-feature", "topic", bashCall("call-3")))
	return []string{first, second, third}
}

// A read that goes on where the last one ended gives exactly what a read of
// the whole file gives: the records, the calls, what could not be read, and
// where it ended.
func TestClaudeFileReadOnFromWhereItEndedIsTheWholeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grow.jsonl")
	moments := claudeGrowing(t)
	earlier := claudeFile{path: path, calls: map[string]string{}}
	for index, content := range moments {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		whole := readClaudeFile(path, nil, true)
		on, went := continueClaudeFile(earlier, nil, true)
		if !went || !reflect.DeepEqual(on, whole) {
			t.Fatalf("moment %d: went on=%v\n  on: %+v\nwhole: %+v", index+1, went, on, whole)
		}
		earlier = on
	}
	// What the whole read gives, so that the comparison above is not one of
	// two empty answers.
	last := earlier
	ids := make([]string, 0, len(last.items))
	for _, item := range last.items {
		ids = append(ids, item.ID)
	}
	if !reflect.DeepEqual(ids, []string{"u1", "a1", "a2", "u2", "a3", "u4", "a4"}) || last.session != "grow" {
		t.Fatalf("records %v of session %q", ids, last.session)
	}
	if last.badLines != 1 || last.firstBadLine != 3 || last.badEntries != 1 || last.end.lines != 11 || last.end.offset != int64(len(moments[2])) {
		t.Fatalf("what could not be read and where it ended: %+v", last)
	}
	// The command that started before the first cut ends after it, with the
	// time and the folder it started in; the turn is carried over the cut.
	if u2 := last.items[3]; len(u2.Commands) != 1 || u2.Commands[0].StartedMs != claudeMs(1) || u2.Commands[0].Folder != "/work/app" || u2.Turn != "p1" {
		t.Fatalf("the command across the cut: %+v", u2)
	}
	if a3 := last.items[4]; a3.Turn != "p1" || !a3.Detached {
		t.Fatalf("the record without a turn of its own: %+v", a3)
	}
	if u4 := last.items[5]; len(u4.Commands) != 1 || u4.Commands[0].StartedMs != claudeMs(3) || u4.Turn != "p2" {
		t.Fatalf("the command of the line that was half written: %+v", u4)
	}
	if _, running := last.end.pending["call-3"]; !running || len(last.end.pending) != 1 || last.end.turn != "p2" {
		t.Fatalf("what was running at the end: %+v", last.end)
	}
	if !reflect.DeepEqual(last.calls, map[string]string{"call-1": "a1", "call-2": "a2", "call-3": "a4"}) {
		t.Fatalf("calls: %v", last.calls)
	}

	// The records read before are kept, not read again: one that is marked
	// in the earlier read is still marked after going on.
	if err := os.WriteFile(path, []byte(moments[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	marked := readClaudeFile(path, nil, true)
	marked.items[0].Folder = "/kept from the earlier read"
	if err := os.WriteFile(path, []byte(moments[2]), 0o600); err != nil {
		t.Fatal(err)
	}
	if on, went := continueClaudeFile(marked, nil, true); !went || on.items[0].Folder != "/kept from the earlier read" || len(on.items) != 7 {
		t.Fatalf("going on read the first part again: went=%v %+v", went, on.items)
	}

	// The part read before is not what it was: nothing is read, and the
	// caller is told to read the file from its beginning.
	changed := strings.Replace(moments[2], `"begin"`, `"BEGIN"`, 1)
	if len(changed) != len(moments[2]) || changed == moments[2] {
		t.Fatal("the changed file is not the same length with other bytes")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readClaudeFileAt(t, path, moments[0])
	if on, went := continueClaudeFile(before, nil, true); went || !reflect.DeepEqual(on, before) {
		t.Fatalf("a file whose first part changed was read on: went=%v", went)
	}
	// A file that is shorter than what was read before is not read on either.
	if err := os.WriteFile(path, []byte(moments[0][:20]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, went := continueClaudeFile(before, nil, true); went {
		t.Fatal("a file shorter than what was read before was read on")
	}
}

// readClaudeFileAt reads the file as it was at an earlier moment and leaves
// it as it is now.
func readClaudeFileAt(t *testing.T, path, moment string) claudeFile {
	t.Helper()
	now, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(moment), 0o600); err != nil {
		t.Fatal(err)
	}
	read := readClaudeFile(path, nil, true)
	if err := os.WriteFile(path, now, 0o600); err != nil {
		t.Fatal(err)
	}
	return read
}

// Through the cache: a session file that grew gives what a reader without a
// cache gives, at every moment, and its first part is not read again.
func TestClaudeRecordsWithCacheGoOnWhenAFileOnlyGrew(t *testing.T) {
	projects, cache := t.TempDir(), filepath.Join(t.TempDir(), "cache")
	path := filepath.Join(projects, "-work-app", "grow.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	read := func(reader *ClaudeRecords) domain.SessionRecordBatch {
		t.Helper()
		batch, err := reader.ReadSessionRecords(context.Background(), testScope)
		if err != nil {
			t.Fatal(err)
		}
		return batch
	}
	cached := NewClaudeRecords(projects).WithCache(cache)
	moments := claudeGrowing(t)
	for index, content := range moments {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if with, without := read(cached), read(NewClaudeRecords(projects)); !reflect.DeepEqual(with, without) || len(sessionByID(t, with, "grow").Items) == 0 {
			t.Fatalf("moment %d with a cache:\n got %+v\nwant %+v", index+1, with, without)
		}
	}
	entries, err := os.ReadDir(cache)
	if err != nil || len(entries) != 1 {
		t.Fatalf("cache entries: %v %v", entries, err)
	}
	entry := filepath.Join(cache, entries[0].Name())
	var kept claudeFileCache
	if !loadCache(entry, &kept) || kept.Offset != int64(len(moments[2])) || kept.Lines != 11 || kept.Turn != "p2" || len(kept.Pending) != 1 || len(kept.PrefixHash) != 32 {
		t.Fatalf("what the cache keeps of where the read ended: %+v", kept)
	}

	// The first part is not read again: a record marked in the cache entry
	// of the first moment is still marked after the file grew.
	if err := os.WriteFile(path, []byte(moments[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	read(cached)
	if !loadCache(entry, &kept) || len(kept.Items) != 2 {
		t.Fatalf("the cache entry of the first moment: %+v", kept)
	}
	kept.Items[0].Branch = "kept-from-the-cache"
	storeCache(entry, kept)
	if err := os.WriteFile(path, []byte(moments[1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if session := sessionByID(t, read(cached), "grow"); len(session.Items) != 5 || session.Items[0].Branch != "kept-from-the-cache" {
		t.Fatalf("after the file grew: %+v", session.Items)
	}
	// A file whose first read held no tool call at all, and whose next one
	// does: the cache keeps no list of calls for it, and going on needs one.
	quiet := filepath.Join(projects, "-work-app", "quiet.jsonl")
	if err := os.WriteFile(quiet, []byte(claudeRaw(t, claudeUser("q1", "quiet", "p1", 20, "/work/app", "main", "only words"))), 0o600); err != nil {
		t.Fatal(err)
	}
	read(cached)
	withCall := claudeRaw(t, claudeUser("q1", "quiet", "p1", 20, "/work/app", "main", "only words")) +
		claudeRaw(t, claudeAssistant("q2", "quiet", 21, "/work/app", "main", []any{map[string]any{"type": "tool_use", "id": "spawn-9", "name": "Agent", "input": map[string]any{}}}))
	if err := os.WriteFile(quiet, []byte(withCall), 0o600); err != nil {
		t.Fatal(err)
	}
	if with, without := sessionByID(t, read(cached), "quiet"), sessionByID(t, read(NewClaudeRecords(projects)), "quiet"); !reflect.DeepEqual(with, without) || len(with.Items) != 2 {
		t.Fatalf("a file that gained its first tool call:\n got %+v\nwant %+v", with, without)
	}
	if err := os.Remove(quiet); err != nil {
		t.Fatal(err)
	}
	// A cache entry of another version of the reader is not read on from:
	// what it holds may have been made by other rules.
	old := kept
	if !loadCache(entry, &old) {
		t.Fatal("no cache entry to make old")
	}
	old.Version = recordCacheVersion - 1
	old.Items[0].Branch = "kept-from-an-old-cache"
	storeCache(entry, old)
	if err := os.WriteFile(path, []byte(moments[2]), 0o600); err != nil {
		t.Fatal(err)
	}
	if session := sessionByID(t, read(cached), "grow"); len(session.Items) != 7 || session.Items[0].Branch != "main" {
		t.Fatalf("after an entry of another version: %+v", session.Items)
	}
	// A file whose first part is another one is read from its beginning, as
	// is one that became shorter: the mark from the cache is gone.
	changed := strings.Replace(moments[2], `"begin"`, `"BEGIN"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	if with, without := read(cached), read(NewClaudeRecords(projects)); !reflect.DeepEqual(with, without) || sessionByID(t, with, "grow").Items[0].Branch != "main" {
		t.Fatalf("a file whose first part changed:\n got %+v\nwant %+v", with, without)
	}
	kept = claudeFileCache{}
	if !loadCache(entry, &kept) {
		t.Fatal("no cache entry after the file changed")
	}
	kept.Items[0].Branch = "kept-from-the-cache"
	storeCache(entry, kept)
	if err := os.WriteFile(path, []byte(moments[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if with, without := read(cached), read(NewClaudeRecords(projects)); !reflect.DeepEqual(with, without) || sessionByID(t, with, "grow").Items[0].Branch != "main" {
		t.Fatalf("a file that became shorter:\n got %+v\nwant %+v", with, without)
	}
}
