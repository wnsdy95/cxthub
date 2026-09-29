package capture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// StagingSource is an explicitly resolved native session, never a latest-file
// provider selector. Its native identity is checked again when Stage freezes it.
type StagingSource struct {
	Provider  domain.ProviderKind
	Path      string
	SessionID string
}

type StagingSources struct {
	Selection string
	Worktree  string
	Sessions  []StagingSource
	// Complete is false when discovery stopped before every candidate could be
	// classified. Callers must not stage a partial result returned with an error.
	Complete bool
	Gaps     []StagingSourceGap
}

type StagingSourceGap struct {
	Provider domain.ProviderKind
	Path     string
	Reason   string
}

// ResolveStagingSources implements bare add / add . / add <provider>. It always
// enumerates all eligible sources in this exact Git worktree, even when called
// by a wrapper or desktop app. An owning-session-only operation must use a
// separately explicit session selector; inherited environment IDs do not narrow
// an all-sources request. Archived provider files and nested/peer worktrees are
// excluded. Filesystem, ledger and identity errors fail the complete selection.
func ResolveStagingSources(ctx context.Context, cwd string, providers []domain.ProviderKind) (StagingSources, error) {
	result := StagingSources{Selection: "all_eligible_in_worktree", Sessions: []StagingSource{}}
	roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd)
	if err != nil {
		return result, err
	}
	result.Worktree = roots.WorktreeRoot
	if len(providers) == 0 {
		providers = []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex}
	}
	selected := map[domain.ProviderKind]bool{}
	for _, provider := range providers {
		if provider != domain.ProviderClaude && provider != domain.ProviderCodex {
			return result, domain.ErrUnsupportedProvider
		}
		selected[provider] = true
	}
	ledger, err := stagingCaptureLedger(roots.SharedRoot)
	if err != nil {
		return result, err
	}
	identities := map[string]string{}
	add := func(path string, provider domain.ProviderKind, info fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		// Same eligibility rule as CaptureExcluded, but reading the complete ledger
		// once propagates corruption and avoids a per-file mutation/lock in discovery.
		if entry, ok := ledger[path]; ok && (entry.Superseded || info.Size() <= entry.Size) {
			return nil
		}
		session, nativeCwd, identityErr := stagingNativeIdentity(path, provider)
		incomplete := func(err error) error {
			result.Gaps = append(result.Gaps, StagingSourceGap{Provider: provider, Path: path, Reason: err.Error()})
			return fmt.Errorf("staging source coverage incomplete for %s %q: %w", provider, path, err)
		}
		if !filepath.IsAbs(nativeCwd) {
			if identityErr != nil {
				return incomplete(identityErr)
			}
			return incomplete(fmt.Errorf("%w: source cwd is not absolute", ErrSessionIdentityMismatch))
		}
		// Codex stores every project's rollouts under one global root. A decoded
		// native cwd can exclude an unrelated project even if its session ID is
		// corrupt. Unparseable/ambiguous metadata cannot prove exclusion.
		nativeReal, err := filepath.EvalSymlinks(nativeCwd)
		if err != nil {
			lexical, relErr := filepath.Rel(roots.WorktreeRoot, filepath.Clean(nativeCwd))
			if os.IsNotExist(err) && relErr == nil && (lexical == ".." || strings.HasPrefix(lexical, ".."+string(filepath.Separator))) {
				return nil
			}
			return incomplete(fmt.Errorf("resolve source worktree: %w", err))
		}
		rel, err := filepath.Rel(roots.WorktreeRoot, nativeReal)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil
		}
		nativeRoots, err := gitctx.ResolveRepositoryRoots(ctx, nativeReal)
		if err != nil {
			return err
		}
		if nativeRoots.WorktreeRoot != roots.WorktreeRoot {
			return nil
		}
		if identityErr != nil {
			return incomplete(identityErr)
		}
		if !providerfs.IsProviderSessionPath(path) {
			return fmt.Errorf("unsafe provider session path %q", path)
		}
		key := string(provider) + "\x00" + session
		if prior, ok := identities[key]; ok && prior != path {
			return fmt.Errorf("ambiguous %s native session %q in this worktree", provider, session)
		}
		identities[key] = path
		result.Sessions = append(result.Sessions, StagingSource{Provider: provider, Path: path, SessionID: session})
		return nil
	}
	if selected[domain.ProviderClaude] {
		root, err := claudeProjectsDir()
		if err != nil {
			return result, err
		}
		if info, statErr := os.Lstat(root); statErr == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return result, fmt.Errorf("unsafe Claude session root")
		} else if statErr != nil && !os.IsNotExist(statErr) {
			return result, statErr
		}
		dirs, err := os.ReadDir(root)
		if err != nil && !os.IsNotExist(err) {
			return result, err
		}
		for _, dir := range dirs {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if !dir.IsDir() || dir.Type()&os.ModeSymlink != 0 {
				continue
			}
			// Project names are only a candidate optimization; native cwd below is the
			// authority, including encoding collisions and nested Git repositories.
			prefix := providerfs.EncodeCwd(roots.WorktreeRoot)
			requested, _ := filepath.Abs(cwd)
			requestedPrefix := providerfs.EncodeCwd(requested)
			if dir.Name() != prefix && !strings.HasPrefix(dir.Name(), prefix+"-") && dir.Name() != requestedPrefix && !strings.HasPrefix(dir.Name(), requestedPrefix+"-") {
				continue
			}
			entries, err := os.ReadDir(filepath.Join(root, dir.Name()))
			if err != nil {
				return result, err
			}
			for _, entry := range entries {
				if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".jsonl") {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return result, err
				}
				if err := add(filepath.Join(root, dir.Name(), entry.Name()), domain.ProviderClaude, info); err != nil {
					return result, err
				}
			}
		}
	}
	if selected[domain.ProviderCodex] {
		root, err := codexSessionsDir()
		if err != nil {
			return result, err
		}
		if info, err := os.Lstat(root); os.IsNotExist(err) {
		} else if err != nil {
			return result, err
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("unsafe Codex session root")
		} else {
			err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
					return nil
				}
				if !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
					return nil
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				return add(path, domain.ProviderCodex, info)
			})
			if err != nil {
				return result, err
			}
		}
	}
	sort.Slice(result.Sessions, func(i, j int) bool {
		a, b := result.Sessions[i], result.Sessions[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		return a.Path < b.Path
	})
	result.Complete = true
	if len(result.Sessions) == 0 {
		return result, domain.ErrNoActiveSession
	}
	return result, nil
}

func stagingCaptureLedger(root string) (map[string]providerfs.LedgerEntry, error) {
	raw, err := providerfs.ReadRepoFile(root, filepath.Join(".cxt", "session-ledger.json"))
	if os.IsNotExist(err) {
		return map[string]providerfs.LedgerEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ledger struct {
		Sessions map[string]providerfs.LedgerEntry `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil, fmt.Errorf("invalid capture exclusion ledger: %w", err)
	}
	if ledger.Sessions == nil {
		return nil, fmt.Errorf("invalid capture exclusion ledger: sessions missing")
	}
	for path, entry := range ledger.Sessions {
		if !filepath.IsAbs(path) || entry.Size < 0 {
			return nil, fmt.Errorf("invalid capture exclusion ledger entry")
		}
	}
	return ledger.Sessions, nil
}

func stagingNativeIdentity(path string, provider domain.ProviderKind) (string, string, error) {
	filenameID := providerfs.SessionIDFromPath(path)
	if provider == domain.ProviderCodex {
		h, err := readCodexSessionHeader(path)
		if err != nil {
			// Only a fully decoded session_meta record proves project ownership.
			// JSON type/syntax errors may leave partially assigned struct fields.
			var syntax *json.SyntaxError
			var shape *json.UnmarshalTypeError
			if errors.As(err, &syntax) || errors.As(err, &shape) || h.Type != "session_meta" {
				return "", "", err
			}
			return h.Payload.ID, h.Payload.Cwd, err
		}
		if filenameID != "" && !sameNativeSessionID(filenameID, h.Payload.ID) {
			return h.Payload.ID, h.Payload.Cwd, ErrSessionIdentityMismatch
		}
		return h.Payload.ID, h.Payload.Cwd, nil
	}
	f, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, 1<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for n := 0; n < 512 && scanner.Scan(); n++ {
		var row struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return "", "", fmt.Errorf("invalid native identity record: %w", err)
		}
		if row.SessionID == "" {
			continue
		}
		if !validHookSessionID(row.SessionID) || row.Cwd == "" {
			return row.SessionID, row.Cwd, ErrSessionIdentityMismatch
		}
		if filenameID != "" && !sameNativeSessionID(filenameID, row.SessionID) {
			return row.SessionID, row.Cwd, ErrSessionIdentityMismatch
		}
		if filenameID == "" && strings.TrimSuffix(filepath.Base(path), ".jsonl") != row.SessionID {
			return row.SessionID, row.Cwd, ErrSessionIdentityMismatch
		}
		return row.SessionID, row.Cwd, nil
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	return "", "", fmt.Errorf("no verified native Claude session identity within header budget")
}
