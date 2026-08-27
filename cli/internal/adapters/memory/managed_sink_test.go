package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const (
	testManagedMemoryBegin = "<!-- cxt:begin managed memory (do not edit inside markers) -->"
	testManagedMemoryEnd   = "<!-- cxt:end managed memory -->"
)

func TestMemorySinkPreservesUserContentAndRefreshesManagedBlock(t *testing.T) {
	for _, tt := range []struct {
		name     string
		filename string
		inject   func(context.Context, domain.MemoryDigest, string) (string, error)
	}{
		{name: "claude", filename: "CLAUDE.md", inject: NewClaudeMemorySink().Inject},
		{name: "codex", filename: "AGENTS.md", inject: NewCodexMemorySink().Inject},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			path := filepath.Join(repo, tt.filename)
			before := "# User instructions\nkeep-before\n\n"
			after := "\nkeep-after\n"
			oldManaged := renderMemoryMarkdown(domain.MemoryDigest{Summary: "old managed memory"})
			original := before + oldManaged + after
			if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := tt.inject(context.Background(), domain.MemoryDigest{Summary: "new managed memory"}, repo); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(got)
			if !strings.HasPrefix(text, before) || !strings.HasSuffix(text, after) {
				t.Fatalf("user content changed:\n%s", text)
			}
			if strings.Contains(text, "old managed memory") || !strings.Contains(text, "new managed memory") {
				t.Fatalf("managed block was not refreshed:\n%s", text)
			}
			if strings.Count(text, testManagedMemoryBegin) != 1 || strings.Count(text, testManagedMemoryEnd) != 1 {
				t.Fatalf("managed marker count changed:\n%s", text)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("mode=%v, want 0600", info.Mode().Perm())
			}
		})
	}
}

func TestMemorySinkAppendsOneManagedBlock(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "AGENTS.md")
	user := "# Team instructions\nNever rewrite this text."
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, summary := range []string{"first projection", "second projection"} {
		if _, err := NewCodexMemorySink().Inject(context.Background(), domain.MemoryDigest{Summary: summary}, repo); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.HasPrefix(text, user) || strings.Count(text, testManagedMemoryBegin) != 1 || strings.Count(text, testManagedMemoryEnd) != 1 {
		t.Fatalf("managed append overwrote or duplicated user content:\n%s", text)
	}
	if strings.Contains(text, "first projection") || !strings.Contains(text, "second projection") {
		t.Fatalf("managed append did not refresh in place:\n%s", text)
	}
}

func TestRenderMemoryMarkdownOmitsLegacyNativeProvenanceFacts(t *testing.T) {
	got := renderMemoryMarkdown(domain.MemoryDigest{KeyFacts: []string{
		"native memory: claude:MEMORY.md",
		"absorbed from claude:MEMORY.md",
		"ingested from codex:memories_1.sqlite",
		"Overlay graft parents remain immutable.",
	}})
	for _, forbidden := range []string{"native memory:", "absorbed from", "ingested from"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("provider memory contains legacy provenance %q:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "Overlay graft parents remain immutable.") {
		t.Fatalf("provider memory lost project fact:\n%s", got)
	}
}

func TestMemorySinkRejectsMalformedMarkersWithoutWrite(t *testing.T) {
	for _, body := range []string{
		"user\n" + testManagedMemoryBegin + "\nunterminated\n",
		testManagedMemoryBegin + "\na\n" + testManagedMemoryEnd + "\n" + testManagedMemoryBegin + "\nb\n" + testManagedMemoryEnd + "\n",
	} {
		repo := t.TempDir()
		path := filepath.Join(repo, "CLAUDE.md")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := NewClaudeMemorySink().Inject(context.Background(), domain.MemoryDigest{Summary: "replacement"}, repo); err == nil {
			t.Fatal("malformed managed markers were accepted")
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatalf("malformed file changed: %q err=%v", got, err)
		}
	}
}

func TestRenderedManagedMemoryIsBoundedAndKeepsNewestState(t *testing.T) {
	digest := domain.MemoryDigest{
		SnapshotID: domain.HashContent([]byte("managed-memory-snapshot")),
		Summary:    "OLDEST-SUMMARY\n" + strings.Repeat("é", 80<<10) + "\nNEWEST-SUMMARY",
		KeyFacts:   []string{strings.Repeat("old-fact ", 12<<10), "NEWEST-FACT"},
		OpenTasks:  []string{strings.Repeat("old-task ", 12<<10), "NEWEST-TASK"},
	}
	got := renderMemoryMarkdown(digest)
	if len(got) > 64<<10 || !utf8.ValidString(got) {
		t.Fatalf("managed memory bytes=%d valid=%v", len(got), utf8.ValidString(got))
	}
	for _, want := range []string{"NEWEST-SUMMARY", "NEWEST-FACT", "NEWEST-TASK", string(digest.SnapshotID)} {
		if !strings.Contains(got, want) {
			t.Fatalf("bounded managed memory lost %q", want)
		}
	}
	if strings.Count(got, testManagedMemoryBegin) != 1 || strings.Count(got, testManagedMemoryEnd) != 1 {
		t.Fatalf("invalid managed block:\n%s", got)
	}
}
