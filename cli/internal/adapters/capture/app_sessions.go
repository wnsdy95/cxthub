package capture

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	NativeID  string              `json:"native_id,omitempty"`
	NativeCwd string              `json:"native_cwd,omitempty"`
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

var ErrSessionIdentityMismatch = errors.New("provider session identity mismatch")

type codexSessionHeader struct {
	Type    string `json:"type"`
	Payload struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"payload"`
}

// Read only the native header, even for a long-lived, very large rollout.
func readCodexSessionHeader(path string) (codexSessionHeader, error) {
	var header codexSessionHeader
	f, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return header, err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	sc.Buffer(make([]byte, 4096), 1<<20)
	if !sc.Scan() {
		return header, fmt.Errorf("missing Codex session header")
	}
	if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
		return header, err
	}
	if header.Type != "session_meta" || !validHookSessionID(header.Payload.ID) || header.Payload.Cwd == "" {
		return header, fmt.Errorf("incomplete Codex session metadata")
	}
	return header, nil
}

func sameNativeSessionID(a, b string) bool {
	if providerfs.ValidSessionID(a) && providerfs.ValidSessionID(b) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// ValidateHookSession rejects inherited parent IDs paired with child paths
// before any registry, capture state, or briefing can be changed. A native
// filename and any header identity must agree. Opaque hook IDs are aliases;
// an existing alias must keep its original native file/identity.
func ValidateHookSession(cwd string, provider domain.ProviderKind, sessionID, path string) error {
	if sessionID == "" || path == "" {
		return nil
	}
	if !validHookSessionID(sessionID) || !validAppSessionPath(path) {
		return fmt.Errorf("%w: invalid %s session identity/path", ErrSessionIdentityMismatch, provider)
	}
	var root string
	var err error
	switch provider {
	case domain.ProviderCodex:
		root, err = codexSessionsDir()
	case domain.ProviderClaude:
		root, err = claudeProjectsDir()
	default:
		return domain.ErrUnsupportedProvider
	}
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: path belongs to another provider", ErrSessionIdentityMismatch)
	}
	filenameID := providerfs.SessionIDFromPath(path)
	if providerfs.ValidSessionID(sessionID) && filenameID != "" && !sameNativeSessionID(filenameID, sessionID) {
		return fmt.Errorf("%w: %s hook ID %q does not match native filename ID %q", ErrSessionIdentityMismatch, provider, sessionID, filenameID)
	}
	if provider == domain.ProviderCodex {
		header, err := readCodexSessionHeader(path)
		if err != nil {
			return fmt.Errorf("validate Codex session header: %w", err)
		}
		if (providerfs.ValidSessionID(sessionID) && !sameNativeSessionID(header.Payload.ID, sessionID)) || (filenameID != "" && !sameNativeSessionID(header.Payload.ID, filenameID)) {
			return fmt.Errorf("%w: Codex hook ID %q does not match native metadata ID %q", ErrSessionIdentityMismatch, sessionID, header.Payload.ID)
		}
		_, nativeWorktree, _ := appSessionRoots(context.Background(), header.Payload.Cwd)
		_, hookWorktree, _ := appSessionRoots(context.Background(), cwd)
		if nativeWorktree == "" || nativeWorktree != hookWorktree {
			return fmt.Errorf("%w: Codex native cwd does not match hook worktree", ErrSessionIdentityMismatch)
		}
		return validateAliasBinding(cwd, provider, sessionID, path, header.Payload.ID)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	real, _ := filepath.EvalSymlinks(abs)
	_, worktree, _ := appSessionRoots(context.Background(), cwd)
	if rel != providerfs.EncodeCwd(abs) && rel != providerfs.EncodeCwd(real) && (worktree == "" || rel != providerfs.EncodeCwd(worktree)) {
		return fmt.Errorf("%w: Claude native project path does not match hook worktree", ErrSessionIdentityMismatch)
	}
	// Claude has no dedicated session header; early records may be file-history
	// metadata. A canonical filename is sufficient until a sessionId appears.
	f, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	sc.Buffer(make([]byte, 4096), 1<<20)
	for n := 0; n < 32 && sc.Scan(); n++ {
		var row struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &row) == nil && row.SessionID != "" {
			if (providerfs.ValidSessionID(sessionID) && !sameNativeSessionID(row.SessionID, sessionID)) || (filenameID != "" && !sameNativeSessionID(row.SessionID, filenameID)) {
				return fmt.Errorf("%w: Claude hook ID %q does not match native record ID %q", ErrSessionIdentityMismatch, sessionID, row.SessionID)
			}
			if row.Cwd != "" {
				_, nativeWorktree, _ := appSessionRoots(context.Background(), row.Cwd)
				if nativeWorktree == "" || nativeWorktree != worktree {
					return fmt.Errorf("%w: Claude native cwd does not match hook worktree", ErrSessionIdentityMismatch)
				}
			}
			return validateAliasBinding(cwd, provider, sessionID, path, row.SessionID)
		}
	}
	if filenameID == "" && providerfs.ValidSessionID(sessionID) {
		return fmt.Errorf("%w: no native Claude identity", ErrSessionIdentityMismatch)
	}
	if filenameID == "" {
		filenameID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return validateAliasBinding(cwd, provider, sessionID, path, filenameID)
}

func validateAliasBinding(cwd string, provider domain.ProviderKind, sessionID, path, nativeID string) error {
	if providerfs.ValidSessionID(sessionID) {
		return nil
	}
	root, worktree, enabled := appSessionRoots(context.Background(), cwd)
	if !enabled {
		return nil
	}
	raw, err := providerfs.ReadRepoFile(root, appSessionRelativePath(provider, worktree, sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var prior appSessionFile
	if json.Unmarshal(raw, &prior) != nil || prior.Provider != provider || prior.SessionID != sessionID || prior.Worktree != worktree {
		return fmt.Errorf("%w: invalid registered opaque alias", ErrSessionIdentityMismatch)
	}
	if prior.Path != path || (prior.NativeID != "" && !sameNativeSessionID(prior.NativeID, nativeID)) {
		return fmt.Errorf("%w: opaque hook alias %q is already bound to another native session", ErrSessionIdentityMismatch, sessionID)
	}
	return nil
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
	// Serialize first registration and refresh so concurrent hooks cannot both
	// establish different native bindings for the same opaque alias.
	lockPath, err := providerfs.PrepareRepoFile(root, filepath.Join(".cxt", "app-sessions", "registry.lock"), 0700)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("app session registry is busy; retry hook: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
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
	if err := ValidateHookSession(cwd, provider, sessionID, path); err != nil {
		return err
	}
	nativeID := providerfs.SessionIDFromPath(path)
	if provider == domain.ProviderCodex {
		header, err := readCodexSessionHeader(path)
		if err != nil {
			return err
		}
		nativeID = header.Payload.ID
	}
	if provider == domain.ProviderClaude && nativeID == "" {
		f, err := providerfs.OpenRegularFile(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
		sc.Buffer(make([]byte, 4096), 1<<20)
		for n := 0; n < 32 && sc.Scan(); n++ {
			var row struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(sc.Bytes(), &row) == nil && row.SessionID != "" {
				nativeID = row.SessionID
				break
			}
		}
	}
	if nativeID == "" {
		nativeID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	nativeCwd, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(appSessionFile{
		Version: appSessionVersion, Provider: provider, SessionID: sessionID,
		Path: path, Worktree: worktree, NativeCwd: nativeCwd, NativeID: nativeID, UpdatedAt: time.Now().UTC(),
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
	return activeAppSessions(cwd, false)
}

// LocateRegisteredAppSession is for a command with an explicit native session
// identity (app environment or owning wrapper). Cross-worktree capture is
// allowed only within the same Git repository. Background discovery and branch
// switching continue to use the exact-worktree ActiveAppSessions boundary.
func LocateRegisteredAppSession(cwd string, provider domain.ProviderKind, sessionID string) (string, error) {
	if !validHookSessionID(sessionID) {
		return "", domain.ErrNoActiveSession
	}
	path := ""
	for _, session := range activeAppSessions(cwd, true) {
		if session.Provider != provider || session.SessionID != sessionID {
			continue
		}
		if path != "" && path != session.Path {
			return "", fmt.Errorf("ambiguous registered provider session")
		}
		path = session.Path
	}
	if path == "" {
		return "", domain.ErrNoActiveSession
	}
	return path, nil
}

// LocateCodexCommandSession is only for a command carrying an explicit native
// ID. Search that filename across dates, verify its header ID and repository,
// and reject ambiguity. Background discovery remains worktree-scoped.
func LocateCodexCommandSession(ctx context.Context, cwd, sessionID string) (string, error) {
	if !providerfs.ValidSessionID(sessionID) {
		return "", domain.ErrNoActiveSession
	}
	commandRoots, err := gitctx.ResolveRepositoryRoots(ctx, cwd)
	if err != nil {
		return "", err
	}
	root, err := codexSessionsDir()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*-"+sessionID+".jsonl"))
	if err != nil {
		return "", err
	}
	selected := ""
	for _, path := range matches {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !validAppSessionPath(path) {
			continue
		}
		header, err := readCodexSessionHeader(path)
		if err != nil {
			return "", fmt.Errorf("verify exact Codex session %q: %w", sessionID, err)
		}
		if !sameNativeSessionID(header.Payload.ID, sessionID) {
			return "", fmt.Errorf("%w: Codex filename ID %q does not match native metadata ID %q", ErrSessionIdentityMismatch, sessionID, header.Payload.ID)
		}
		nativeRoots, err := gitctx.ResolveRepositoryRoots(ctx, header.Payload.Cwd)
		if err != nil || nativeRoots.SharedRoot != commandRoots.SharedRoot {
			continue
		}
		if err := ValidateHookSession(header.Payload.Cwd, domain.ProviderCodex, sessionID, path); err != nil {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if providerfs.CaptureExcluded(commandRoots.SharedRoot, path, info.Size()) {
			continue
		}
		if selected != "" && selected != path {
			return "", fmt.Errorf("ambiguous exact Codex session %q in shared Git repository", sessionID)
		}
		selected = path
	}
	if selected == "" {
		return "", domain.ErrNoActiveSession
	}
	return selected, nil
}

func activeAppSessions(cwd string, relatedWorktrees bool) []AppSession {
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
		// Older clients could store a parent ID with a child's transcript. Do
		// not expose or refresh it; a correctly identified hook can replace it.
		nativeCwd := state.NativeCwd
		if nativeCwd == "" {
			nativeCwd = state.Worktree
		}
		_, nativeWorktree, _ := appSessionRoots(ctx, nativeCwd)
		if nativeWorktree != state.Worktree || ValidateHookSession(nativeCwd, state.Provider, state.SessionID, state.Path) != nil {
			continue
		}
		if state.Worktree != worktree {
			if !relatedWorktrees {
				continue
			}
			roots, err := gitctx.ResolveRepositoryRoots(ctx, state.Worktree)
			if err != nil || roots.SharedRoot != root {
				continue
			}
		}
		if relatedWorktrees {
			var path string
			var err error
			if state.Provider == domain.ProviderCodex {
				// Native cwd may use a symlink spelling (e.g. /var vs /private/var).
				// Verify the canonical worktree before using its native spelling.
				nativeCwd, ok := rolloutCwd(state.Path)
				if !ok {
					continue
				}
				nativeRoot, nativeWorktree, enabled := appSessionRoots(ctx, nativeCwd)
				if !enabled || nativeRoot != root || nativeWorktree != state.Worktree {
					continue
				}
				path, err = NewCodexCapture().LocateSession(ctx, nativeCwd, state.SessionID)
			} else {
				path, err = NewClaudeCapture().LocateSession(ctx, state.Worktree, state.SessionID)
			}
			if err != nil || path != state.Path {
				continue
			}
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
