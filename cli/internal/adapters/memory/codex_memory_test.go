package memory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCodexReadNative verifies the reading of memories_1.sqlite and rollout files from a fake HOME directory, matching cwd to thread_id, reading from SQLite, and assembling NativeMemory.
func TestCodexReadNative(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)

	// rollout file: one thread from this cwd and one thread from another cwd.
	dateDir := filepath.Join(home, ".codex", "sessions", "2026", "07", "05")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "0197aaaa-bbbb-4ccc-8ddd-eeeeffff0001"
	other := "0197aaaa-bbbb-4ccc-8ddd-eeeeffff0002"
	write := func(id, cwd string) {
		line := `{"timestamp":"2026-07-05T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + cwd + `"}}` + "\n"
		p := filepath.Join(dateDir, "rollout-2026-07-05T00-00-00-"+id+".jsonl")
		if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(mine, repo)
	write(other, "/elsewhere")

	// Create stage1_outputs in the real schema and insert memory for two threads.
	db := filepath.Join(home, ".codex", "memories_1.sqlite")
	schema := `CREATE TABLE stage1_outputs (
		thread_id TEXT PRIMARY KEY,
		source_updated_at INTEGER NOT NULL,
		raw_memory TEXT NOT NULL,
		rollout_summary TEXT NOT NULL,
		rollout_slug TEXT,
		generated_at INTEGER NOT NULL,
		usage_count INTEGER,
		last_usage INTEGER,
		selected_for_phase2 INTEGER NOT NULL DEFAULT 0,
		selected_for_phase2_source_updated_at INTEGER);
	INSERT INTO stage1_outputs (thread_id, source_updated_at, raw_memory, rollout_summary, generated_at)
	VALUES ('` + mine + `', 1, 'user prefers tabs', 'summary A', 1),
	       ('` + other + `', 2, 'unrelated memory', 'summary B', 2);`
	if out, err := exec.Command("sqlite3", db, schema).CombinedOutput(); err != nil {
		t.Fatalf("fixture db: %v %s", err, out)
	}

	native, found, err := NewCodexMemorySource().ReadNative(context.Background(), repo, mine)
	if err != nil {
		t.Fatalf("ReadNative: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if !strings.Contains(native.Text, "user prefers tabs") {
		t.Errorf("expected this cwd's memory, got %q", native.Text)
	}
	if strings.Contains(native.Text, "unrelated memory") {
		t.Errorf("other cwd's memory must not leak: %q", native.Text)
	}
	if native.Source != "codex:memories_1.sqlite" || native.Provider != "codex" {
		t.Errorf("source/provider mismatch: %+v", native)
	}

	// An empty raw_memory is a fallback for rollout_summary.
	if out, err := exec.Command("sqlite3", db, "UPDATE stage1_outputs SET raw_memory='' WHERE thread_id='"+mine+"'").CombinedOutput(); err != nil {
		t.Fatalf("update: %v %s", err, out)
	}
	native, found, _ = NewCodexMemorySource().ReadNative(context.Background(), repo, mine)
	if !found || !strings.Contains(native.Text, "summary A") {
		t.Errorf("expected rollout_summary fallback, got found=%v %q", found, native.Text)
	}
}

// TestCodexReadNativeAbsent ensures a silent fallback with found=false when the DB is absent.
func TestCodexReadNativeAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, found, err := NewCodexMemorySource().ReadNative(context.Background(), t.TempDir(), "")
	if err != nil || found {
		t.Fatalf("expected silent fallback, got found=%v err=%v", found, err)
	}
}

func TestCodexReadNativeKeepsNewestRowFullAndSupportsExactSession(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not available")
	}
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	dateDir := filepath.Join(home, ".codex", "sessions", "2026", "08", "27")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldID := "0198aaaa-bbbb-4ccc-8ddd-eeeeffff0001"
	newID := "0198aaaa-bbbb-4ccc-8ddd-eeeeffff0002"
	for _, id := range []string{oldID, newID} {
		line := `{"timestamp":"2026-08-27T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + repo + `"}}` + "\n"
		path := filepath.Join(dateDir, "rollout-2026-08-27T00-00-00-"+id+".jsonl")
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(home, ".codex", "memories_1.sqlite")
	schema := `CREATE TABLE stage1_outputs (
		thread_id TEXT PRIMARY KEY,
		source_updated_at INTEGER NOT NULL,
		raw_memory TEXT NOT NULL,
		rollout_summary TEXT NOT NULL,
		generated_at INTEGER NOT NULL,
		selected_for_phase2 INTEGER NOT NULL DEFAULT 0);`
	if out, err := exec.Command("sqlite3", db, schema).CombinedOutput(); err != nil {
		t.Fatalf("fixture db: %v %s", err, out)
	}
	oldText := "OLDEST-ROW " + strings.Repeat("o", 40<<10)
	newText := strings.Repeat("é", 30<<10) + " NEWEST-ROW"
	for _, row := range []struct {
		id   string
		at   int
		text string
	}{{oldID, 1, oldText}, {newID, 2, newText}} {
		insert := fmt.Sprintf(`INSERT INTO stage1_outputs (thread_id,source_updated_at,raw_memory,rollout_summary,generated_at) VALUES ('%s',%d,'%s','',%d);`, row.id, row.at, row.text, row.at)
		cmd := exec.Command("sqlite3", db, insert)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("insert memory row: %v %s", err, out)
		}
	}

	native, found, err := NewCodexMemorySource().ReadNative(context.Background(), repo, "")
	if err != nil || !found {
		t.Fatalf("ReadNative found=%v err=%v", found, err)
	}
	if !strings.Contains(native.Text, "NEWEST-ROW") || strings.Contains(native.Text, "OLDEST-ROW") {
		t.Fatalf("native selection kept stale rows: prefix=%q", native.Text[:min(len(native.Text), 80)])
	}
	if len(native.Text) != len(newText) || !utf8.ValidString(native.Text) {
		t.Fatalf("native bytes=%d want=%d valid=%v", len(native.Text), len(newText), utf8.ValidString(native.Text))
	}
	exact, found, err := NewCodexMemorySource().ReadNative(context.Background(), repo, oldID)
	if err != nil || !found || !strings.Contains(exact.Text, "OLDEST-ROW") || strings.Contains(exact.Text, "NEWEST-ROW") {
		t.Fatalf("exact-session native selection=%q found=%v err=%v", exact.Text[:min(len(exact.Text), 80)], found, err)
	}
	if _, found, err := NewCodexMemorySource().ReadNative(context.Background(), repo, "0198aaaa-bbbb-4ccc-8ddd-eeeeffff9999"); err != nil || found {
		t.Fatalf("unknown exact session found=%v err=%v", found, err)
	}
}
