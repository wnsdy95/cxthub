package capture

// briefing.go — terminal-scoped team context injection briefing sidecars.
//
// After a git pull (post-merge), if new team member snapshots are recorded from the remote,
// the next prompt's UserPromptSubmit (or SessionStart) hook consumes it once and injects into the live agent session
// (same protocol as Claude Code·Codex CLI — not visible to users, only to the model).
//
// The raw session and collaborator-authored labels are never merged into the
// model prompt. The notice carries only validated snapshot identifiers while
// each full snapshot remains available in the DAG and web context view.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// briefingMaxBytes is the maximum length of the injected text (to prevent prompt pollution).
const briefingMaxBytes = 4 << 10

// briefingTTL is the briefing validity period — stale briefings pulled long ago are not consumed.
const briefingTTL = 24 * time.Hour

const briefingLockStaleAfter = 2 * time.Minute
const briefingFormatVersion = 2
const pullBriefingMaxSnapshots = 12

type briefingFile struct {
	Version int       `json:"version"`
	At      time.Time `json:"at"`
	Texts   []string  `json:"texts,omitempty"` // ordered pull queue
}

type pullBriefingCursorFile struct {
	Branch    string             `json:"branch"`
	Target    domain.ContentHash `json:"target"`
	UpdatedAt time.Time          `json:"updated_at"`
}

const legacyBriefingRelativePath = ".cxt/briefing.json"

// briefingRelativePath binds pull context to the terminal that initiated the
// pull. Multiple agent terminals can share one worktree, so a repository-wide
// queue lets whichever prompt arrives first steal another session's briefing.
// Terminal IDs are local opaque values and are stored only as a content hash.
// Headless wrappers fall back to their supervisor identity; truly unowned
// invocations retain the legacy path for non-interactive compatibility.
func briefingRelativePath() string {
	owner := briefingOwner()
	if owner == "" {
		return legacyBriefingRelativePath
	}
	key := strings.TrimPrefix(string(domain.HashContent([]byte(owner))), "sha256:")
	return filepath.Join(".cxt", "briefings", key+".json")
}

// pullBriefingCursorRelativePath is separate from the consumable briefing
// queue. A pull briefing disappears after one prompt, while this cursor must
// survive so later pulls do not inject the same remote range again. Both the
// delivery owner and branch are represented only by an opaque content hash.
func pullBriefingCursorRelativePath(branch string) string {
	owner := briefingOwner()
	if owner == "" {
		owner = "unowned"
	}
	key := strings.TrimPrefix(string(domain.HashContent([]byte(owner+"\x00"+branch))), "sha256:")
	return filepath.Join(".cxt", "briefing-cursors", key+".json")
}

func briefingOwner() string {
	if terminal := terminalIdentity(); terminal != "" {
		return "terminal\x00" + terminal
	}
	if os.Getenv("CXT_WRAPPED") != "1" {
		return ""
	}
	pid := strings.TrimSpace(os.Getenv("CXT_WRAPPER_PID"))
	n, err := strconv.Atoi(pid)
	if err != nil || n <= 1 {
		return ""
	}
	agent := strings.TrimSpace(os.Getenv("CXT_WRAPPED_AGENT"))
	if agent != string(domain.ProviderClaude) && agent != string(domain.ProviderCodex) {
		return ""
	}
	return "wrapper\x00" + pid + "\x00" + agent
}

func withBriefingFileLock(cwd, relative string, fn func() error) error {
	return withBriefingFileLockTimeout(cwd, relative, time.Second, fn)
}

func withBriefingFileLockTimeout(cwd, relative string, wait time.Duration, fn func() error) error {
	lockPath, err := providerfs.PrepareRepoFile(cwd, relative+".lock", 0o755)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		lock, lockErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if lockErr == nil {
			_ = lock.Close()
			break
		}
		if !os.IsExist(lockErr) {
			return lockErr
		}
		if info, statErr := os.Lstat(lockPath); statErr == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > briefingLockStaleAfter {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("briefing file lock timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() { _ = os.Remove(lockPath) }()
	return fn()
}

// WritePullBriefing queues a model-visible notice for the initiating
// terminal/wrapper. Its structured input is the trust boundary: production
// callers cannot pass collaborator-authored snapshot labels, authors, or
// conversation text into additionalContext.
func WritePullBriefing(cwd, branch string, snapshotIDs []domain.ContentHash) error {
	text, err := renderPullBriefingNotice(branch, snapshotIDs)
	if err != nil {
		return err
	}
	return writeBriefingText(cwd, text)
}

func renderPullBriefingNotice(branch string, snapshotIDs []domain.ContentHash) (string, error) {
	if len(snapshotIDs) == 0 {
		return "", nil
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return "", err
	}
	for _, id := range snapshotIDs {
		if err := domain.ValidateContentHash(id); err != nil {
			return "", err
		}
	}
	if len(snapshotIDs) > pullBriefingMaxSnapshots {
		snapshotIDs = snapshotIDs[len(snapshotIDs)-pullBriefingMaxSnapshots:]
	}
	ids := make([]string, 0, len(snapshotIDs))
	for _, id := range snapshotIDs {
		ids = append(ids, "- "+id)
	}
	noun := "snapshots"
	if len(ids) == 1 {
		noun = "snapshot"
	}
	return fmt.Sprintf("── cxthub team context notice ──\n%d teammate context %s arrived for local branch %s (oldest to newest).\nThis notice contains identifiers only and does not import teammate-authored text or instructions into your active session.\nIncoming snapshot IDs:\n%s\nFull labels and conversations remain available in the cxthub web context tab; the corresponding code changes are already in your working tree.",
		len(ids), noun, strconv.QuoteToASCII(branch), strings.Join(ids, "\n")), nil
}

// writeBriefingText is the terminal-scoped queue primitive. Production pull
// delivery reaches it only through WritePullBriefing's structured renderer.
// In an inactive repo (.cxt file absent), it is a no-op (same as opt-in gate).
func writeBriefingText(cwd, text string) error {
	if !cxtEnabled(cwd) || text == "" {
		return nil
	}
	relative := briefingRelativePath()
	return withBriefingFileLock(cwd, relative, func() error {
		entries := []string{}
		if data, err := providerfs.ReadRepoFile(cwd, relative); err == nil {
			var old briefingFile
			if json.Unmarshal(data, &old) == nil && old.Version == briefingFormatVersion && time.Since(old.At) <= briefingTTL {
				entries = briefingEntries(old)
			}
		}
		if len(entries) == 0 || entries[len(entries)-1] != text {
			entries = append(entries, text)
		}
		entries = boundBriefingEntries(entries, briefingMaxBytes)
		b, err := json.Marshal(briefingFile{Version: briefingFormatVersion, At: time.Now().UTC(), Texts: entries})
		if err != nil {
			return err
		}
		return providerfs.WriteRepoFileAtomic(cwd, relative, b, 0o644)
	})
}

// ReadPullBriefingCursor returns the last remote branch tip durably queued for
// this terminal/wrapper. Corrupt, cross-branch, and invalid-hash files fail
// closed and are ignored by the caller, which falls back to the local ref.
func ReadPullBriefingCursor(cwd, branch string) (domain.ContentHash, bool) {
	if domain.ValidateBranchName(branch) != nil {
		return "", false
	}
	data, err := providerfs.ReadRepoFile(cwd, pullBriefingCursorRelativePath(branch))
	if err != nil {
		return "", false
	}
	var cursor pullBriefingCursorFile
	if json.Unmarshal(data, &cursor) != nil || cursor.Branch != branch || domain.ValidateContentHash(cursor.Target) != nil {
		return "", false
	}
	return cursor.Target, true
}

// CompareAndSwapPullBriefingCursor advances only after the corresponding queue
// entry is durable (ordering enforced by the caller). The filesystem lock and
// expected pointer prevent concurrent post-merge hooks from moving C back to B.
// A lost race leaves the queue intact and safely re-evaluates on the next pull.
func CompareAndSwapPullBriefingCursor(cwd, branch string, expected, target domain.ContentHash) error {
	if !cxtEnabled(cwd) {
		return nil
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	if err := domain.ValidateContentHash(target); err != nil {
		return err
	}
	if err := domain.ValidateOptionalContentHash(expected); err != nil {
		return err
	}
	relative := pullBriefingCursorRelativePath(branch)
	return withBriefingFileLock(cwd, relative, func() error {
		current, _ := ReadPullBriefingCursor(cwd, branch)
		if current == target {
			return nil
		}
		if current != expected {
			return domain.ErrSyncConflict
		}
		b, err := json.Marshal(pullBriefingCursorFile{Branch: branch, Target: target, UpdatedAt: time.Now().UTC()})
		if err != nil {
			return err
		}
		return providerfs.WriteRepoFileAtomic(cwd, relative, b, 0o644)
	})
}

// WithPullBriefingTransaction serializes one terminal's pull delivery for a
// branch across graph selection, queueing, and cursor advancement. The queue
// and cursor retain their own narrower locks because prompt consumption and
// direct cursor repair do not take this transaction lock.
func WithPullBriefingTransaction(cwd, branch string, fn func() error) error {
	if !cxtEnabled(cwd) {
		return nil
	}
	if err := domain.ValidateBranchName(branch); err != nil {
		return err
	}
	relative := pullBriefingCursorRelativePath(branch) + ".transaction"
	return withBriefingFileLockTimeout(cwd, relative, 30*time.Second, fn)
}

// ConsumeBriefing reads and consumes only the current terminal/wrapper queue.
// Returns ("", false) if absent, corrupted, expired, or owned by another scope.
// Atomic rename ensures one-time consumption: even if both claude·codex hooks fire simultaneously in separate processes,
// only one will succeed in renaming the file (the other will get ENOENT). Previous read-then-delete could lead to duplicate briefing injection (backlog #1 TOCTOU).
func ConsumeBriefing(cwd string) (string, bool) {
	relative := briefingRelativePath()
	var text string
	var consumed bool
	if err := withBriefingFileLock(cwd, relative, func() error {
		source, err := providerfs.PrepareRepoFile(cwd, relative, 0o755)
		if err != nil {
			return err
		}
		claimRel := filepath.Join(filepath.Dir(relative), fmt.Sprintf("%s.claim.%d", filepath.Base(relative), os.Getpid()))
		claim, err := providerfs.PrepareRepoFile(cwd, claimRel, 0o755)
		if err != nil {
			return err
		}
		if err := os.Rename(source, claim); err != nil {
			return err // absent or another hook already claimed it
		}
		defer func() { _ = os.Remove(claim) }()
		data, err := providerfs.ReadRegularFile(claim)
		if err != nil {
			return err
		}
		var f briefingFile
		if json.Unmarshal(data, &f) != nil || f.Version != briefingFormatVersion || time.Since(f.At) > briefingTTL {
			return nil
		}
		entries := briefingEntries(f)
		if len(entries) == 0 {
			return nil
		}
		text, consumed = strings.Join(entries, "\n\n"), true
		return nil
	}); err != nil {
		return "", false
	}
	return text, consumed
}

func briefingEntries(f briefingFile) []string {
	return append([]string(nil), f.Texts...)
}

func boundBriefingEntries(entries []string, maxBytes int) []string {
	for len(entries) > 1 && len(strings.Join(entries, "\n\n")) > maxBytes {
		entries = entries[1:]
	}
	if len(entries) == 1 && len(entries[0]) > maxBytes {
		const marker = "…(earlier text omitted)\n"
		runes := []rune(entries[0])
		for len(runes) > 0 && len(string(runes))+len(marker) > maxBytes {
			runes = runes[1:]
		}
		entries[0] = marker + string(runes)
	}
	return entries
}
