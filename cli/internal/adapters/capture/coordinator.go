package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// CaptureCoordinator is a common adjuster for auto/manual capture (capture path).
//
// It receives both auto (hook) and manual (MCP/CLI/slash) paths, debounces, grows, and locks them before converging to inbound.SaveSession.Save.
//
// State files (local-only, re-creatable, push non-target — capture path simplified by repo unit):
//
//	<repo>/.cxt/capture/<provider>-<session-scope>.last
//	<repo>/.cxt/capture/<provider>-<session-scope>.cursor
//	<repo>/.cxt/capture/<provider>-<session-scope>.turn
//	<repo>/.cxt/capture/<provider>-<session-scope>.baseline
//	<repo>/.cxt/capture/<provider>-<session-scope>.lock
//
// Safety contract (invariant, capture path):
//   - Always exit 0 from hook path calls (errors reported only to stderr — main ensures).
//   - Original session file is read-only (never modified/deleted).
//   - Heavy tasks (remote push, distillation) are forbidden from hook path synchronous execution.
//   - No-op silently if ErrNoActiveSession.
type CaptureCoordinator struct {
	save     inbound.SaveSession
	identity domain.TeamIdentity
}

// NewCaptureCoordinator creates a CaptureCoordinator.
// save/identity are passed to the constructor via cmd/cxt (composition root).
func NewCaptureCoordinator(save inbound.SaveSession, identity domain.TeamIdentity) *CaptureCoordinator {
	return &CaptureCoordinator{save: save, identity: identity}
}

func repositoryStateRoot(cwd string) string {
	if root, ok := gitctx.ContextRoot(context.Background(), cwd); ok {
		return root
	}
	return cwd
}

// captureStateDir returns the capture sidecar directory (creates if not exists).
func captureStateDir(cwd string) (string, error) {
	return providerfs.EnsureRepoDir(cwd, filepath.Join(".cxt", "capture"), 0o755)
}

// captureStateBase isolates concurrent desktop/CLI sessions that share one
// repository and provider. Official session IDs are opaque and are not
// guaranteed globally unique, so the exact worktree is always part of the
// scope. Neither value is exposed in a filename.
func captureStateBase(ctx context.Context, provider domain.ProviderKind, cwd, sessionID string) string {
	worktree := cwd
	if roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd); err == nil {
		worktree = roots.WorktreeRoot
	} else if abs, err := filepath.Abs(cwd); err == nil {
		worktree = abs
	}
	scope := strings.TrimSpace(sessionID)
	sum := sha256.Sum256([]byte(string(provider) + "\x00" + filepath.Clean(worktree) + "\x00" + scope))
	return fmt.Sprintf("%s-%x", provider, sum[:12])
}

// captureCursor is a growth detector cursor. No-op if session file hasn't grown beyond the cursor.
type captureCursor struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// staleLockAge considers locks older than this age as orphan (crash residue) and takes them.
const staleLockAge = 2 * time.Minute

// acquireLock obtains an O_CREATE|O_EXCL lock file. Retries once if already exists after stale check.
func acquireLock(path string) bool {
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return true
		}
		fi, serr := os.Stat(path)
		if serr != nil || time.Since(fi.ModTime()) < staleLockAge {
			return false // another process is capturing — no-op (capture path)
		}
		_ = os.Remove(path) // stale lock removal
	}
	return false
}

// MarkBaseline records the baseline state at the SessionStart event (capture path). Does not commit.
// If sessionPath is empty, detects the active session (silently no-op if none found).
// Clears residual .turn hints from the previous session at the session boundary.
func (c *CaptureCoordinator) MarkBaseline(ctx context.Context, provider domain.ProviderKind, cwd, sessionPath, sessionID string) error {
	repoRoot, enabled := gitctx.ContextRoot(ctx, cwd)
	if !enabled {
		return nil
	}
	dir, err := captureStateDir(repoRoot)
	if err != nil {
		return err
	}
	base := captureStateBase(ctx, provider, cwd, sessionID)
	statePath := func(ext string) string { return filepath.Join(dir, base+ext) }
	_ = os.Remove(statePath(".turn"))
	path := sessionPath
	if path == "" {
		src, err := sourceFor(provider)
		if err != nil {
			return nil
		}
		if path, err = src.LocateActiveSession(ctx, cwd); err != nil {
			return nil // ErrNoActiveSession included — no baseline (next capture is full read)
		}
	}
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	b, _ := json.Marshal(map[string]interface{}{"path": path, "size": size, "at": time.Now().UTC().Format(time.RFC3339)})
	return providerfs.WriteRegularFileAtomic(statePath(".baseline"), b, 0o644)
}

// MarkTurn records the turn boundary at the UserPromptSubmit event (capture path). Does not commit.
// The hint (previous user prompt) is used as the Message in the next capture snapshot.
func (c *CaptureCoordinator) MarkTurn(ctx context.Context, provider domain.ProviderKind, cwd, sessionID, hint string) error {
	repoRoot, enabled := gitctx.ContextRoot(ctx, cwd)
	if !enabled {
		return nil
	}
	dir, err := captureStateDir(repoRoot)
	if err != nil {
		return err
	}
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return nil
	}
	if nl := strings.IndexByte(hint, '\n'); nl >= 0 {
		hint = hint[:nl]
	}
	if len(hint) > 120 {
		hint = hint[:120]
	}
	base := captureStateBase(ctx, provider, cwd, sessionID)
	return providerfs.WriteRegularFileAtomic(filepath.Join(dir, base+".turn"), []byte(hint), 0o644)
}

// RequestCapture requests a capture (capture path commit/flush path).
// If debounce=true, applies the marker file mtime gate ( leading + mtime gate),
// and if force=true (SessionEnd flush/manual), ignores debouncing. Growth gate ( cheap gate) and
// a file lock is always applied. Save performs final content-hash deduplication.
// The returned captured indicates whether actual storage occurred (gate no-op is false) — caller should
// only proceed with subsequent work (pending-sync spawn, etc.) if storage happened.
func (c *CaptureCoordinator) RequestCapture(ctx context.Context, provider domain.ProviderKind, cwd, sessionPath, sessionID string, debounce, force bool) (captured bool, err error) {
	repoRoot, enabled := gitctx.ContextRoot(ctx, cwd)
	if !enabled {
		return false, nil
	}
	dir, err := captureStateDir(repoRoot)
	if err != nil {
		return false, err
	}
	base := captureStateBase(ctx, provider, cwd, sessionID)
	statePath := func(ext string) string { return filepath.Join(dir, base+ext) }
	lock := statePath(".lock")
	if !acquireLock(lock) {
		return false, nil
	}
	defer os.Remove(lock)

	last := statePath(".last")
	if debounce && !force {
		if fi, serr := os.Stat(last); serr == nil && time.Since(fi.ModTime()) < remotecfg.CaptureDebounce(repoRoot) {
			return false, nil
		}
	}

	path := sessionPath
	if path == "" {
		src, serr := sourceFor(provider)
		if serr != nil {
			return false, serr
		}
		if path, serr = src.LocateActiveSession(ctx, cwd); serr != nil {
			if errors.Is(serr, domain.ErrNoActiveSession) {
				return false, nil // inactive session: no-op
			}
			return false, serr
		}
	}
	fi, serr := os.Stat(path)
	if serr != nil {
		return false, nil // session file lost — no-op
	}
	size := fi.Size()

	// Growth gate: no-op if the same file did not grow after the last capture.
	cursorPath := statePath(".cursor")
	var cur captureCursor
	if b, rerr := providerfs.ReadRegularFile(cursorPath); rerr == nil {
		_ = json.Unmarshal(b, &cur)
	}
	if cur.Path == path && size <= cur.Size {
		return false, nil
	}

	// Snapshot Message: Turn hint (if any) > Event base.
	msg := "checkpoint"
	turnPath := statePath(".turn")
	if b, rerr := providerfs.ReadRegularFile(turnPath); rerr == nil && len(strings.TrimSpace(string(b))) > 0 {
		msg = strings.TrimSpace(string(b))
	}

	_, err = c.save.Save(ctx, inbound.SaveInput{
		Cwd:         cwd,
		Provider:    provider,
		SessionPath: path,
		Message:     domain.HookMessagePrefix + msg,
		Author:      c.identity,
		Pending:     true,
	})
	if err != nil {
		if errors.Is(err, domain.ErrNoActiveSession) {
			return false, nil // Isolation/growth-inhibiting gate — no-op
		}
		return false, err
	}
	_ = os.Remove(turnPath) // Hint is consumed once
	if b, merr := json.Marshal(captureCursor{Path: path, Size: size}); merr == nil {
		_ = providerfs.WriteRegularFileAtomic(cursorPath, b, 0o644)
	}
	now := time.Now()
	if werr := providerfs.WriteRegularFileAtomic(last, nil, 0o644); werr == nil {
		_ = os.Chtimes(last, now, now)
	}
	return true, nil
}

// sourceFor returns a CaptureSource for a provider (internal to coordinator —
// uses the same adapter as the registry of the assembly root, but for shallow discovery of hook paths).
func sourceFor(provider domain.ProviderKind) (interface {
	LocateActiveSession(context.Context, string) (string, error)
}, error) {
	switch provider {
	case domain.ProviderClaude:
		return NewClaudeCapture(), nil
	case domain.ProviderCodex:
		return NewCodexCapture(), nil
	default:
		return nil, domain.ErrUnsupportedProvider
	}
}
