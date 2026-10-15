package capture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type codexSessionCandidate struct {
	path     string
	header   codexSessionHeader
	worktree string
	excluded bool
}

func selectCodexSession(candidates []codexSessionCandidate) (string, error) {
	if len(candidates) == 0 {
		return "", domain.ErrNoActiveSession
	}
	if len(candidates) == 1 {
		return candidates[0].path, nil
	}
	ambiguous := fmt.Errorf("ambiguous exact Codex session %q: native rollout generations do not identify one current file", candidates[0].header.Payload.ID)
	latest := -1
	var latestTime time.Time
	seen := make(map[time.Time]bool)
	ordinary := 0
	for index, candidate := range candidates {
		if providerfs.CodexRolloutGeneration(candidate.path) == "" {
			ordinary++
			if ordinary > 1 {
				return "", ambiguous
			}
		}
		if candidate.worktree != candidates[0].worktree {
			return "", ambiguous
		}
		created, err := time.Parse(time.RFC3339Nano, candidate.header.Payload.Timestamp)
		created = created.UTC()
		if err != nil || created.IsZero() || seen[created] {
			return "", ambiguous
		}
		seen[created] = true
		if latest < 0 || created.After(latestTime) {
			latest, latestTime = index, created
		}
	}
	if providerfs.CodexRolloutGeneration(candidates[latest].path) == "" {
		return "", ambiguous
	}
	return candidates[latest].path, nil
}

func locateCodexSession(ctx context.Context, sessionID, stateRoot string, requireEligible bool, scope func(string) (string, bool)) (string, error) {
	if !providerfs.ValidSessionID(sessionID) {
		return "", domain.ErrNoActiveSession
	}
	root, err := codexSessionsDir()
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		return "", err
	}
	retired, err := filepath.Glob(filepath.Join(root, "*", "*", "*", "rollout-*.jsonl.superseded"))
	if err != nil {
		return "", err
	}
	matches = append(matches, retired...)
	var candidates []codexSessionCandidate
	for _, path := range matches {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !sameNativeSessionID(providerfs.SessionIDFromPath(path), sessionID) || !providerfs.IsProviderSessionPath(path) {
			continue
		}
		header, err := readCodexSessionHeader(path)
		if err != nil {
			return "", fmt.Errorf("verify exact Codex session %q: %w", sessionID, err)
		}
		worktree, included := scope(header.Payload.Cwd)
		if !included || !filepath.IsAbs(header.Payload.Cwd) {
			continue
		}
		if !sameNativeSessionID(header.Payload.ID, sessionID) {
			return "", fmt.Errorf("%w: Codex filename and metadata IDs disagree", ErrSessionIdentityMismatch)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		candidates = append(candidates, codexSessionCandidate{path: path, header: header, worktree: worktree,
			excluded: requireEligible && (strings.HasSuffix(path, ".superseded") || providerfs.CaptureExcluded(stateRoot, path, info.Size()))})
	}
	selected, err := selectCodexSession(candidates)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if candidate.path == selected && candidate.excluded {
			return "", domain.ErrNoActiveSession
		}
	}
	return selected, nil
}

func canonicalSessionCwd(cwd string) string {
	if !filepath.IsAbs(cwd) {
		return ""
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func codexSessionWorktree(ctx context.Context, cwd string) string {
	if !filepath.IsAbs(cwd) {
		return ""
	}
	if roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd); err == nil {
		return roots.WorktreeRoot
	}
	return canonicalSessionCwd(cwd)
}

func (c *CodexCaptureSource) ValidateCurrentSession(ctx context.Context, path string) error {
	header, err := readCodexSessionHeader(path)
	if err != nil {
		return err
	}
	filenameID := providerfs.SessionIDFromPath(path)
	if (filenameID != "" && !sameNativeSessionID(filenameID, header.Payload.ID)) || (strings.Contains(filepath.Base(path), "_") && filenameID == "") {
		return fmt.Errorf("%w: invalid Codex filename and metadata binding", ErrSessionIdentityMismatch)
	}
	if !providerfs.ValidSessionID(header.Payload.ID) {
		return nil
	}
	wanted := codexSessionWorktree(ctx, header.Payload.Cwd)
	owner, ownerErr := gitctx.ResolveRepositoryRoots(ctx, header.Payload.Cwd)
	current, err := locateCodexSession(ctx, header.Payload.ID, "", false, func(nativeCwd string) (string, bool) {
		if ownerErr == nil {
			roots, err := gitctx.ResolveRepositoryRoots(ctx, nativeCwd)
			return roots.WorktreeRoot, err == nil && roots.SharedRoot == owner.SharedRoot
		}
		native := codexSessionWorktree(ctx, nativeCwd)
		return native, wanted != "" && native == wanted
	})
	if err != nil {
		return err
	}
	if current != path {
		currentDir, err := filepath.EvalSymlinks(filepath.Dir(current))
		if err != nil {
			return err
		}
		pathDir, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return err
		}
		if currentDir != pathDir || filepath.Base(current) != filepath.Base(path) {
			return fmt.Errorf("%w: capture refers to an older Codex rollout generation", ErrSessionIdentityMismatch)
		}
	}
	return nil
}
