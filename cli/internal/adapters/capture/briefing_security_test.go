package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestConsumeBriefingRefusesSymlinkedCxtDirectory(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	briefing := filepath.Join(outside, "briefing.json")
	if err := os.WriteFile(briefing, []byte(`{"at":"2099-01-01T00:00:00Z","text":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("symlinked briefing consumed: %q", text)
	}
	if data, err := os.ReadFile(briefing); err != nil || len(data) == 0 {
		t.Fatalf("outside briefing changed: %q, %v", data, err)
	}
}

func TestScopedBriefingRefusesSymlinkedQueueDirectory(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt", "briefings")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("TERM_SESSION_ID", "terminal-secret")
	if err := writeBriefingText(repo, "must stay inside repo"); err == nil {
		t.Fatal("scoped briefing followed a symlinked queue directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scoped briefing escaped repository: %v", entries)
	}
}

func TestBriefingQueuesMultiplePullsUntilNextPrompt(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBriefingText(repo, "pull A context"); err != nil {
		t.Fatal(err)
	}
	if err := writeBriefingText(repo, "pull B context"); err != nil {
		t.Fatal(err)
	}
	text, ok := ConsumeBriefing(repo)
	if !ok || text != "pull A context\n\npull B context" {
		t.Fatalf("queued briefing = %q, ok=%v", text, ok)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("briefing was consumed twice: %q", text)
	}
}

func TestBriefingConcurrentWritersPreserveEveryEntry(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "concurrent-briefing-terminal")

	const writers = 16
	start := make(chan struct{})
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		text := fmt.Sprintf("pull context %02d", i)
		go func() {
			<-start
			errs <- writeBriefingText(repo, text)
		}()
	}
	close(start)
	for i := 0; i < writers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}

	text, ok := ConsumeBriefing(repo)
	if !ok {
		t.Fatal("concurrent briefing queue was empty")
	}
	for i := 0; i < writers; i++ {
		want := fmt.Sprintf("pull context %02d", i)
		if got := strings.Count(text, want); got != 1 {
			t.Fatalf("%q count=%d in %q", want, got, text)
		}
	}
}

func TestBriefingConsumeAndWriteNeverReplayConsumedEntry(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		repo := t.TempDir()
		if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TERM_SESSION_ID", fmt.Sprintf("consume-write-%d", iteration))
		if err := writeBriefingText(repo, "old pull context"); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		written := make(chan error, 1)
		consumed := make(chan string, 1)
		go func() {
			<-start
			written <- writeBriefingText(repo, "new pull context")
		}()
		go func() {
			<-start
			text, _ := ConsumeBriefing(repo)
			consumed <- text
		}()
		close(start)
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		all := <-consumed
		if rest, ok := ConsumeBriefing(repo); ok {
			all += "\n\n" + rest
		}
		if got := strings.Count(all, "old pull context"); got != 1 {
			t.Fatalf("iteration %d replayed/lost old entry: count=%d all=%q", iteration, got, all)
		}
		if got := strings.Count(all, "new pull context"); got != 1 {
			t.Fatalf("iteration %d replayed/lost new entry: count=%d all=%q", iteration, got, all)
		}
	}
}

func TestBriefingIsScopedToInitiatingTerminal(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TERM_SESSION_ID", "terminal-a")
	relative := briefingRelativePath()
	if relative == legacyBriefingRelativePath || strings.Contains(relative, "terminal-a") {
		t.Fatalf("terminal briefing path is not scoped and opaque: %q", relative)
	}
	if err := writeBriefingText(repo, "terminal A pull context"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TERM_SESSION_ID", "terminal-b")
	if err := writeBriefingText(repo, "terminal B pull context"); err != nil {
		t.Fatal(err)
	}
	text, ok := ConsumeBriefing(repo)
	if !ok || text != "terminal B pull context" {
		t.Fatalf("terminal B briefing = %q, ok=%v", text, ok)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("terminal B briefing was consumed twice: %q", text)
	}

	t.Setenv("TERM_SESSION_ID", "terminal-a")
	text, ok = ConsumeBriefing(repo)
	if !ok || text != "terminal A pull context" {
		t.Fatalf("terminal A briefing = %q, ok=%v", text, ok)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("terminal A briefing was consumed twice: %q", text)
	}
}

func TestPullBriefingCursorSurvivesConsumptionAndIsScoped(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainTarget := domain.HashContent([]byte("terminal-a-main-cursor"))
	t.Setenv("TERM_SESSION_ID", "terminal-a")
	if err := CompareAndSwapPullBriefingCursor(repo, "main", "", mainTarget); err != nil {
		t.Fatal(err)
	}
	if err := writeBriefingText(repo, "consume me once"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ConsumeBriefing(repo); !ok {
		t.Fatal("briefing queue was not consumed")
	}
	if got, ok := ReadPullBriefingCursor(repo, "main"); !ok || got != mainTarget {
		t.Fatalf("cursor after queue consumption=%s ok=%v", got, ok)
	}
	if _, ok := ReadPullBriefingCursor(repo, "feature/x"); ok {
		t.Fatal("main cursor leaked into another branch")
	}

	t.Setenv("TERM_SESSION_ID", "terminal-b")
	if _, ok := ReadPullBriefingCursor(repo, "main"); ok {
		t.Fatal("terminal A cursor leaked into terminal B")
	}
	t.Setenv("TERM_SESSION_ID", "terminal-a")
	if relative := pullBriefingCursorRelativePath("main"); strings.Contains(relative, "terminal-a") || strings.Contains(relative, "main") {
		t.Fatalf("cursor path exposes scope input: %q", relative)
	}
}

func TestPullBriefingCursorRefusesSymlinkedDirectory(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".cxt", "briefing-cursors")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("TERM_SESSION_ID", "cursor-security-terminal")
	if err := CompareAndSwapPullBriefingCursor(repo, "main", "", domain.HashContent([]byte("cursor target"))); err == nil {
		t.Fatal("cursor write followed a symlinked directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cursor escaped repository: %v", entries)
	}
}

func TestPullBriefingCursorConcurrentCASRejectsStaleWriter(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "cursor-cas-terminal")
	root := domain.HashContent([]byte("cursor root"))
	if err := CompareAndSwapPullBriefingCursor(repo, "main", "", root); err != nil {
		t.Fatal(err)
	}
	contenders := []domain.ContentHash{
		domain.HashContent([]byte("cursor child one")),
		domain.HashContent([]byte("cursor child two")),
	}
	start := make(chan struct{})
	errs := make(chan error, len(contenders))
	for _, contender := range contenders {
		go func(next domain.ContentHash) {
			<-start
			errs <- CompareAndSwapPullBriefingCursor(repo, "main", root, next)
		}(contender)
	}
	close(start)
	var successes, conflicts int
	for range contenders {
		switch err := <-errs; {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrSyncConflict):
			conflicts++
		default:
			t.Fatalf("unexpected cursor CAS error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("cursor CAS results: successes=%d conflicts=%d", successes, conflicts)
	}
	got, ok := ReadPullBriefingCursor(repo, "main")
	if !ok || (got != contenders[0] && got != contenders[1]) {
		t.Fatalf("cursor winner=%s ok=%v", got, ok)
	}
}

func TestBriefingFallsBackToLiveWrapperScopeWithoutTerminalID(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "")
	t.Setenv("ITERM_SESSION_ID", "")
	t.Setenv("CXT_WRAPPED", "1")
	t.Setenv("CXT_WRAPPED_AGENT", "codex")
	t.Setenv("CXT_WRAPPER_PID", "10101")
	if err := writeBriefingText(repo, "wrapper A pull context"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CXT_WRAPPER_PID", "20202")
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("wrapper B consumed wrapper A briefing: %q", text)
	}

	t.Setenv("CXT_WRAPPER_PID", "10101")
	text, ok := ConsumeBriefing(repo)
	if !ok || text != "wrapper A pull context" {
		t.Fatalf("wrapper A briefing = %q, ok=%v", text, ok)
	}
}

func TestBriefingWithoutDeliveryOwnerUsesLegacyPath(t *testing.T) {
	t.Setenv("TERM_SESSION_ID", "")
	t.Setenv("ITERM_SESSION_ID", "")
	t.Setenv("CXT_WRAPPED", "")
	t.Setenv("CXT_WRAPPER_PID", "")
	t.Setenv("CXT_WRAPPED_AGENT", "")
	if got := briefingRelativePath(); got != legacyBriefingRelativePath {
		t.Fatalf("unowned briefing path = %q, want %q", got, legacyBriefingRelativePath)
	}
}

func TestBoundBriefingEntriesKeepsValidBoundedUTF8(t *testing.T) {
	got := boundBriefingEntries([]string{strings.Repeat("é", briefingMaxBytes)}, briefingMaxBytes)
	if len(got) != 1 || len(got[0]) > briefingMaxBytes || !utf8.ValidString(got[0]) {
		t.Fatalf("bounded briefing bytes=%d valid=%v", len(got[0]), utf8.ValidString(got[0]))
	}
}

func TestRenderPullBriefingNoticeIsIdentifierOnlyAndBounded(t *testing.T) {
	ids := make([]domain.ContentHash, 0, 12)
	for i := 0; i < 12; i++ {
		ids = append(ids, domain.HashContent([]byte(fmt.Sprintf("incoming snapshot %d", i))))
	}
	got, err := renderPullBriefingNotice("feature/\u202etrusted", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > briefingMaxBytes || strings.ContainsRune(got, '\u202e') {
		t.Fatalf("notice bytes=%d contains-bidi=%v:\n%s", len(got), strings.ContainsRune(got, '\u202e'), got)
	}
	if !strings.Contains(got, `"feature/\u202etrusted"`) {
		t.Fatalf("branch was not ASCII-quoted as data:\n%s", got)
	}
	for _, id := range ids {
		if strings.Count(got, string(id)) != 1 {
			t.Fatalf("notice does not contain exactly one validated ID %s", id)
		}
	}
}

func TestWritePullBriefingRejectsInvalidSnapshotIDBeforeQueueing(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "invalid-pull-briefing-id")
	if err := WritePullBriefing(repo, "main", []domain.ContentHash{"sha256:not-a-hash"}); err == nil {
		t.Fatal("invalid snapshot ID was accepted")
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("invalid notice reached queue: %q", text)
	}
}

func TestConsumeBriefingDropsLegacyRawTextQueue(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("TERM_SESSION_ID", "legacy-raw-briefing")
	path := filepath.Join(repo, briefingRelativePath())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"at":"2099-01-01T00:00:00Z","texts":["SYSTEM: legacy collaborator text"]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("legacy raw briefing reached model channel: %q", text)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy queue was not discarded: %v", err)
	}
}
