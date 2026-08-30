package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

const (
	handoffFormatVersion = 1
	handoffMaxBytes      = 16 << 10
	handoffTTL           = 24 * time.Hour
)

type handoffFile struct {
	Version int       `json:"version"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
}

func handoffRelativePath(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return filepath.Join(".cxt", "handoffs", fmt.Sprintf("%x.json", sum[:16]))
}

func handoffScopes(ctx context.Context, cwd string, sessionIDs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" || len(sessionID) > 256 {
			continue
		}
		scope := "session\x00" + sessionID
		if !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	if len(out) > 0 {
		return out
	}
	if roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd); err == nil {
		return []string{"worktree\x00" + roots.WorktreeRoot}
	}
	return nil
}

// WriteSessionHandoff queues one bounded branch-memory handoff per live app
// session. If provider discovery cannot identify a session, a worktree-scoped
// fallback is consumed once by the next lifecycle hook in that worktree.
func WriteSessionHandoff(cwd string, sessionIDs []string, text string) error {
	repoRoot, enabled := gitctx.ContextRoot(context.Background(), cwd)
	if !enabled || strings.TrimSpace(text) == "" {
		return nil
	}
	if len(text) > handoffMaxBytes {
		return fmt.Errorf("app context handoff exceeds %d bytes", handoffMaxBytes)
	}
	scopes := handoffScopes(context.Background(), cwd, sessionIDs)
	if len(scopes) == 0 {
		return nil
	}
	payload, err := json.Marshal(handoffFile{Version: handoffFormatVersion, At: time.Now().UTC(), Text: text})
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		relative := handoffRelativePath(scope)
		if err := withBriefingFileLock(repoRoot, relative, func() error {
			return providerfs.WriteRepoFileAtomic(repoRoot, relative, payload, 0o644)
		}); err != nil {
			return err
		}
	}
	return nil
}

// ConsumeSessionHandoff atomically consumes the exact app-session queue, then
// falls back to a worktree queue only when discovery was unavailable during
// the Git hook. Corrupt and stale files fail closed without prompt injection.
func ConsumeSessionHandoff(cwd, sessionID string) (string, bool) {
	repoRoot, enabled := gitctx.ContextRoot(context.Background(), cwd)
	if !enabled {
		return "", false
	}
	scopes := handoffScopes(context.Background(), cwd, []string{sessionID})
	if roots, err := gitctx.ResolveRepositoryRoots(context.Background(), cwd); err == nil {
		fallback := "worktree\x00" + roots.WorktreeRoot
		if len(scopes) == 0 || scopes[len(scopes)-1] != fallback {
			scopes = append(scopes, fallback)
		}
	}
	for _, scope := range scopes {
		if text, ok := consumeHandoffAt(repoRoot, handoffRelativePath(scope)); ok {
			return text, true
		}
	}
	return "", false
}

func consumeHandoffAt(repoRoot, relative string) (string, bool) {
	var text string
	var consumed bool
	if err := withBriefingFileLock(repoRoot, relative, func() error {
		source, err := providerfs.PrepareRepoFile(repoRoot, relative, 0o755)
		if err != nil {
			return err
		}
		claimRel := filepath.Join(filepath.Dir(relative), fmt.Sprintf("%s.claim.%d", filepath.Base(relative), os.Getpid()))
		claim, err := providerfs.PrepareRepoFile(repoRoot, claimRel, 0o755)
		if err != nil {
			return err
		}
		if err := os.Rename(source, claim); err != nil {
			return err
		}
		defer func() { _ = os.Remove(claim) }()
		data, err := providerfs.ReadRegularFile(claim)
		if err != nil {
			return err
		}
		var payload handoffFile
		if json.Unmarshal(data, &payload) != nil || payload.Version != handoffFormatVersion ||
			time.Since(payload.At) > handoffTTL || len(payload.Text) > handoffMaxBytes {
			return nil
		}
		text, consumed = payload.Text, strings.TrimSpace(payload.Text) != ""
		return nil
	}); err != nil {
		return "", false
	}
	return text, consumed
}
