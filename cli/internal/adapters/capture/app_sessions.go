package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

const (
	appSessionVersion = 1
	appSessionTTL     = 24 * time.Hour
)

// AppSession is one provider session that official lifecycle hooks have seen
// in this exact worktree. The registry is local-only and stores no transcript.
type AppSession struct {
	Provider  domain.ProviderKind
	SessionID string
	Path      string
}

type appSessionFile struct {
	Version   int                 `json:"version"`
	Provider  domain.ProviderKind `json:"provider"`
	SessionID string              `json:"session_id"`
	Path      string              `json:"path"`
	Worktree  string              `json:"worktree"`
	UpdatedAt time.Time           `json:"updated_at"`
}

func appSessionRelativePath(provider domain.ProviderKind, worktree, sessionID string) string {
	sum := sha256.Sum256([]byte(string(provider) + "\x00" + worktree + "\x00" + sessionID))
	return filepath.Join(".cxt", "app-sessions", fmt.Sprintf("%x.json", sum[:16]))
}

func appSessionRoots(ctx context.Context, cwd string) (string, string, bool) {
	root, enabled := gitctx.ContextRoot(ctx, cwd)
	if !enabled {
		return "", "", false
	}
	worktree := cwd
	if roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd); err == nil {
		worktree = roots.WorktreeRoot
	} else if abs, err := filepath.Abs(cwd); err == nil {
		worktree = abs
	}
	return root, filepath.Clean(worktree), true
}

func validHookSessionID(sessionID string) bool {
	if sessionID == "" || len(sessionID) > 256 || strings.TrimSpace(sessionID) != sessionID {
		return false
	}
	for _, r := range sessionID {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validAppSessionPath(path string) bool {
	if strings.HasSuffix(path, ".superseded") || !providerfs.IsProviderSessionPath(path) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// TrackAppSession records only identity/path/liveness metadata from an
// official provider hook. An empty path refreshes an existing entry but never
// invents a filesystem location.
func TrackAppSession(cwd string, provider domain.ProviderKind, sessionID, path string) error {
	if provider != domain.ProviderClaude && provider != domain.ProviderCodex || !validHookSessionID(sessionID) {
		return nil
	}
	ctx := context.Background()
	root, worktree, enabled := appSessionRoots(ctx, cwd)
	if !enabled {
		return nil
	}
	relative := appSessionRelativePath(provider, worktree, sessionID)
	path = strings.TrimSpace(path)
	if path == "" {
		if raw, err := providerfs.ReadRepoFile(root, relative); err == nil {
			var prior appSessionFile
			if json.Unmarshal(raw, &prior) == nil && prior.Provider == provider && prior.SessionID == sessionID {
				path = prior.Path
			}
		}
	}
	if !validAppSessionPath(path) {
		return nil
	}
	payload, err := json.Marshal(appSessionFile{
		Version: appSessionVersion, Provider: provider, SessionID: sessionID,
		Path: path, Worktree: worktree, UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return providerfs.WriteRepoFileAtomic(root, relative, payload, 0o600)
}

// EndAppSession removes only the liveness pointer. The provider transcript and
// all cxt snapshots remain untouched.
func EndAppSession(cwd string, provider domain.ProviderKind, sessionID string) {
	if !validHookSessionID(sessionID) {
		return
	}
	if root, worktree, enabled := appSessionRoots(context.Background(), cwd); enabled {
		_ = providerfs.RemoveRepoFile(root, appSessionRelativePath(provider, worktree, sessionID))
	}
}

// ActiveAppSessions returns valid, non-stale sessions for this worktree. It is
// the branch-switch ownership source for desktop/IDE apps; old provider files
// outside this registry are archive candidates, not live conversations.
func ActiveAppSessions(cwd string) []AppSession {
	ctx := context.Background()
	root, worktree, enabled := appSessionRoots(ctx, cwd)
	if !enabled {
		return nil
	}
	dir, err := providerfs.EnsureRepoDir(root, filepath.Join(".cxt", "app-sessions"), 0o755)
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	now := time.Now()
	out := make([]AppSession, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		relative := filepath.Join(".cxt", "app-sessions", entry.Name())
		raw, err := providerfs.ReadRepoFile(root, relative)
		if err != nil {
			continue
		}
		var state appSessionFile
		age := time.Duration(0)
		if json.Unmarshal(raw, &state) == nil {
			age = now.Sub(state.UpdatedAt)
		}
		valid := state.Version == appSessionVersion &&
			(state.Provider == domain.ProviderClaude || state.Provider == domain.ProviderCodex) &&
			validHookSessionID(state.SessionID) &&
			age >= -time.Minute && age <= appSessionTTL &&
			validAppSessionPath(state.Path)
		if !valid {
			_ = providerfs.RemoveRepoFile(root, relative)
			continue
		}
		if state.Worktree != worktree {
			continue
		}
		out = append(out, AppSession{Provider: state.Provider, SessionID: state.SessionID, Path: state.Path})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}
