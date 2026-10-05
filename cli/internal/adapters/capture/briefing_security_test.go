package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const briefingLockTimeoutMessage = "briefing file lock timeout"

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

func TestBriefingConcurrentWritersPreserveSuccessfulEntries(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "concurrent-briefing-terminal")

	const writers = 16
	start := make(chan struct{})
	errs := make([]error, writers)
	var workers sync.WaitGroup
	for i := 0; i < writers; i++ {
		text := fmt.Sprintf("pull context %02d", i)
		workers.Go(func() {
			<-start
			errs[i] = writeBriefingText(repo, text)
		})
	}
	close(start)
	workers.Wait() // Join every writer even when an earlier write failed.
	successes := 0
	for i, err := range errs {
		if err == nil {
			successes++
		} else if err.Error() != briefingLockTimeoutMessage {
			t.Fatalf("writer %d: unexpected error: %v", i, err)
		}
	}

	// The one-second acquisition bound need not admit every contender on a
	// slow machine. Successful writes must commit once; timed-out writes must
	// not appear. Explicit retry is verified separately under a held lock.
	text, ok := ConsumeBriefing(repo)
	if successes == 0 || !ok || len(strings.Split(text, "\n\n")) != successes {
		t.Fatalf("successes=%d queue=%q ok=%v", successes, text, ok)
	}
	counts := make(map[string]int)
	for _, entry := range strings.Split(text, "\n\n") {
		counts[entry]++
	}
	for i, err := range errs {
		want := fmt.Sprintf("pull context %02d", i)
		count := 0
		if err == nil {
			count = 1
		}
		if got := counts[want]; got != count {
			t.Fatalf("%q count=%d want=%d (write error=%v) in %q", want, got, count, err, text)
		}
	}
}

func TestBriefingWriteTimeoutPreservesQueueForExplicitRetry(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".cxt"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_SESSION_ID", "briefing-timeout-retry")
	if err := writeBriefingText(repo, "committed entry"); err != nil {
		t.Fatal(err)
	}
	relative := briefingRelativePath()
	path := filepath.Join(repo, relative)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	locked, release := make(chan struct{}), make(chan struct{})
	ownerDone := make(chan error, 1)
	var owner sync.WaitGroup
	owner.Go(func() {
		ownerDone <- withBriefingFileLock(repo, relative, func() error {
			close(locked) // The real lock is owned before the contender starts.
			<-release
			return nil
		})
	})
	select {
	case <-locked:
	case err := <-ownerDone:
		owner.Wait()
		t.Fatalf("lock owner did not enter callback: %v", err)
	}
	// Hold ownership until the normal production write returns. No sleep or
	// elapsed-time assertion is needed to force its bounded acquisition failure.
	writeErr := writeBriefingText(repo, "retry entry")
	after, readErr := os.ReadFile(path)
	close(release)
	owner.Wait()
	ownerErr := <-ownerDone
	if ownerErr != nil {
		t.Fatal(ownerErr)
	}
	if writeErr == nil || writeErr.Error() != briefingLockTimeoutMessage {
		t.Fatalf("contended write=%v; want lock timeout", writeErr)
	}
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("failed write changed committed queue: before=%q after=%q error=%v", before, after, readErr)
	}
	if err := writeBriefingText(repo, "retry entry"); err != nil {
		t.Fatalf("explicit retry after owner release: %v", err)
	}
	if text, ok := ConsumeBriefing(repo); !ok || text != "committed entry\n\nretry entry" {
		t.Fatalf("queue after explicit retry=%q ok=%v", text, ok)
	}
	if text, ok := ConsumeBriefing(repo); ok || text != "" {
		t.Fatalf("retried queue consumed twice: %q ok=%v", text, ok)
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
		var workers sync.WaitGroup
		var writeErr error
		var all string
		workers.Go(func() {
			<-start
			writeErr = writeBriefingText(repo, "new pull context")
		})
		workers.Go(func() {
			<-start
			all, _ = ConsumeBriefing(repo)
		})
		close(start)
		workers.Wait()
		wantNew := 1
		if writeErr != nil {
			if writeErr.Error() != briefingLockTimeoutMessage {
				t.Fatalf("unexpected write error: %v", writeErr)
			}
			wantNew = 0
		}
		if rest, ok := ConsumeBriefing(repo); ok {
			all += "\n\n" + rest
		}
		if got := strings.Count(all, "old pull context"); got != 1 {
			t.Fatalf("iteration %d replayed/lost old entry: count=%d all=%q", iteration, got, all)
		}
		if got := strings.Count(all, "new pull context"); got != wantNew {
			t.Fatalf("iteration %d new entry: count=%d want=%d (write error=%v) all=%q", iteration, got, wantNew, writeErr, all)
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
	errs := make([]error, len(contenders))
	var workers sync.WaitGroup
	for i, contender := range contenders {
		workers.Go(func() {
			<-start
			errs[i] = CompareAndSwapPullBriefingCursor(repo, "main", root, contender)
		})
	}
	close(start)
	workers.Wait()
	var successes, conflicts, timeouts int
	var winner domain.ContentHash
	for i, err := range errs {
		switch {
		case err == nil:
			successes++
			winner = contenders[i]
		case errors.Is(err, domain.ErrSyncConflict):
			conflicts++
		case err.Error() == briefingLockTimeoutMessage:
			timeouts++
		default:
			t.Fatalf("unexpected cursor CAS error: %v", err)
		}
	}
	if successes != 1 || conflicts+timeouts != 1 {
		t.Fatalf("cursor CAS results: successes=%d conflicts=%d timeouts=%d", successes, conflicts, timeouts)
	}
	for i, err := range errs {
		if err != nil {
			// Once the winner is joined, even a previously timed-out contender
			// must reject the stale expected cursor on an explicit later attempt.
			if err := CompareAndSwapPullBriefingCursor(repo, "main", root, contenders[i]); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("stale cursor retry: %v", err)
			}
		}
	}
	got, ok := ReadPullBriefingCursor(repo, "main")
	if !ok || got != winner {
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
