package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// DeliverSessionHandoff treats the queued body only as a pending request. The
// caller must prepare fresh input and write it inside deliver. Failed delivery
// leaves the request retryable; acknowledgement never removes a replacement.
func DeliverSessionHandoff(ctx context.Context, cwd, sessionID string, deliver func() error) (bool, error) {
	return deliverSessionHandoff(ctx, cwd, sessionID, func(string) error { return deliver() })
}

// ConsumeSessionHandoff retains the immediate-consumption helper for archive
// callers. Provider hooks use DeliverSessionHandoff and never emit this body.
func ConsumeSessionHandoff(cwd, sessionID string) (string, bool) {
	var text string
	delivered, err := deliverSessionHandoff(context.Background(), cwd, sessionID, func(body string) error {
		text = body
		return nil
	})
	return text, delivered && err == nil
}

func deliverSessionHandoff(ctx context.Context, cwd, sessionID string, deliver func(string) error) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	repoRoot, enabled := gitctx.ContextRoot(ctx, cwd)
	if !enabled {
		return false, ctx.Err()
	}
	scopes := handoffScopes(ctx, cwd, []string{sessionID})
	if roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd); err == nil {
		fallback := "worktree\x00" + roots.WorktreeRoot
		if len(scopes) == 0 || scopes[len(scopes)-1] != fallback {
			scopes = append(scopes, fallback)
		}
	}
	for _, scope := range scopes {
		if delivered, err := deliverHandoffAt(ctx, repoRoot, handoffRelativePath(scope), deliver); delivered || err != nil {
			return delivered, err
		}
	}
	return false, ctx.Err()
}

func deliverHandoffAt(ctx context.Context, repoRoot, relative string, deliver func(string) error) (bool, error) {
	// Avoid creating queue directories on hooks with no pending request.
	if _, _, err := readHandoffFile(repoRoot, relative); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var delivered bool
	// Serialize consumers separately from writers. Preparation can involve slow
	// authorized reads; a Git hook must still be able to queue its replacement.
	err := withBriefingFileLock(repoRoot, relative+".delivery", func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var data []byte
		var original os.FileInfo
		err := withBriefingFileLock(repoRoot, relative, func() error {
			var err error
			data, original, err = readHandoffFile(repoRoot, relative)
			return err
		})
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		var payload handoffFile
		if json.Unmarshal(data, &payload) != nil || payload.Version != handoffFormatVersion ||
			time.Since(payload.At) > handoffTTL || len(payload.Text) > handoffMaxBytes || strings.TrimSpace(payload.Text) == "" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := deliver(payload.Text); err != nil {
			return err
		}
		delivered = true
		return withBriefingFileLock(repoRoot, relative, func() error {
			current, info, err := readHandoffFile(repoRoot, relative)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !os.SameFile(original, info) || !bytes.Equal(data, current) {
				return nil // A newer request belongs to the next hook.
			}
			path, err := providerfs.PrepareRepoFile(repoRoot, relative, 0o755)
			if err != nil {
				return err
			}
			return os.Remove(path)
		})
	})
	return delivered, err
}

func readHandoffFile(repoRoot, relative string) ([]byte, os.FileInfo, error) {
	f, err := providerfs.OpenRepoFile(repoRoot, relative)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	// JSON escaping can expand each byte of the bounded text sixfold.
	data, err := io.ReadAll(io.LimitReader(f, 6*handoffMaxBytes+4096))
	return data, info, err
}
