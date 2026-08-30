// setup.go — cxt setup: single command for new team member onboarding (idempotent — re-run = status check).
//
//	cxt setup [remote-url] [--no-login]
//
// Completes in one go after cloning: .cxt initialization → git hooks → remote registration → login (device flow)
// → agent hooks (Claude repo setup, Codex global merge) → team basic settings pull.
// Each step reports with a checklist (✓/✗/⚠), and completed steps are not modified.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// cxtHookEntry is an item in the agent hook settings (similar to Claude Code and Codex hooks.json).
type cxtHookEntry struct {
	Matcher string `json:"matcher,omitempty"`
	Hooks   []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Timeout int    `json:"timeout,omitempty"`
	} `json:"hooks"`
}

// agentHookEvents are the target events for registration by provider (capture path).
func agentHookEvents(_ string) map[string]string {
	return map[string]string{
		"SessionStart":     "startup|resume",
		"UserPromptSubmit": "",
		"Stop":             "",
		"SessionEnd":       "",
	}
}

// mergeAgentHooks merges the cxt item in the hooks settings JSON (preserves existing user hooks).
// Returns: (change made, error). Determination is based on whether the command includes "cxt hook --provider <p>".
func mergeAgentHooks(path, provider string) (bool, error) {
	root := map[string]json.RawMessage{}
	if data, err := providerfs.ReadRegularFile(path); err == nil {
		if jerr := json.Unmarshal(data, &root); jerr != nil {
			return false, fmt.Errorf("%s parsing failed (manual verification required): %w", path, jerr)
		}
	}
	hooks := map[string][]json.RawMessage{}
	if raw, ok := root["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return false, fmt.Errorf("%s hooks parsing failed: %w", path, err)
		}
	}
	marker := fmt.Sprintf("cxt hook --provider %s", provider)
	changed := false
	for event, matcher := range agentHookEvents(provider) {
		exists := false
		for _, raw := range hooks[event] {
			var e cxtHookEntry
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			for _, h := range e.Hooks {
				if h.Type == "command" && strings.Contains(h.Command, marker) {
					exists = true
				}
			}
		}
		if exists {
			continue
		}
		entry := cxtHookEntry{Matcher: matcher}
		entry.Hooks = append(entry.Hooks, struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout,omitempty"`
		}{Type: "command", Command: fmt.Sprintf("cxt hook --provider %s --event %s", provider, event), Timeout: 10})
		raw, _ := json.Marshal(entry)
		hooks[event] = append(hooks[event], raw)
		changed = true
	}
	if !changed {
		return false, nil
	}
	rawHooks, _ := json.Marshal(hooks)
	root["hooks"] = rawHooks
	out, _ := json.MarshalIndent(root, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, providerfs.WriteRegularFileAtomic(path, append(out, '\n'), 0o644)
}

// runSetup runs the cxt setup.
func runSetup(ctx context.Context, c *Container, cwd string, rest []string) error {
	url := firstPositional(rest)
	ok := func(f string, a ...interface{}) { fmt.Printf("  ✓ "+f+"\n", a...) }
	bad := func(f string, a ...interface{}) { fmt.Printf("  ✗ "+f+"\n", a...) }
	warn := func(f string, a ...interface{}) { fmt.Printf("  ⚠ "+f+"\n", a...) }

	// 1) .cxt store (git repo required — otherwise, fail here).
	out, err := c.Init.Init(ctx, inbound.InitInput{Cwd: cwd})
	if err != nil {
		return fmt.Errorf(".cxt initialization failed: %w", err)
	}
	ok(".cxt store (repo %s)", shortHash(out.RepoID))
	_, _ = githooks.EnsureGitignore(cwd)
	_ = githooks.EnsureExcluded(cwd)

	// 2) git hooks — wiring for "cxt follows git".
	if installed, herr := githooks.Install(cwd); herr == nil {
		ok("git hooks %d (commit·checkout·merge·push·ref-tx·rewrite; stash detected as ref-tx)", len(installed))
	} else {
		bad("git hook installation failed: %v", herr)
	}

	// 3) remote origin (idempotent: re-register if not the same URL).
	if url != "" {
		if cur, has := remotecfg.Origin(cwd); has {
			if remotecfg.RepoIDFor(cur) == remotecfg.RepoIDFor(url) {
				ok("remote origin already registered = %s", cur)
			} else {
				warn("remote origin is registered with a different URL (%s) — change: cxt remote remove origin then setup", cur)
			}
		} else if rerr := runRemote(ctx, c, cwd, []string{"add", "origin", url}); rerr != nil {
			// Git origin mismatch is server rejection — invalid folder, so stop setup.
			var he *backendclient.HTTPError
			if errors.As(rerr, &he) && he.Code == "git_origin_mismatch" {
				return fmt.Errorf("this folder cannot connect to %s — %w", url, he)
			}
			bad("remote registration failed: %v", rerr)
		}
	}
	origin, hasOrigin := remotecfg.Origin(cwd)
	if hasOrigin {
		ok("remote origin = %s", origin)
	} else {
		warn("remote not registered — workspace connection: cxt setup https://<host>/<username>/<workspace>")
	}

	// 4) Login (device flow). Keep token if already present.
	if hasOrigin && !flagPresent(rest, "--no-login") {
		if _, host, aerr := remoteAPIBase(cwd); aerr == nil {
			if authcfg.Token(host) != "" {
				ok("login (existing token: %s)", host)
			} else if base, host2, berr := remoteAPIBase(cwd); berr == nil {
				fmt.Println("  … login (device flow) — approve in browser")
				if lerr := deviceLogin(ctx, base, host2); lerr != nil {
					bad("Login failed: %v (try cxt login later)", lerr)
				} else {
					ok("Login completed")
				}
			}
		}
	}

	// 5) Agent hooks — the same lifecycle capture works in Claude Code/Codex
	// desktop clients as well as their CLIs. Claude settings are project-scoped;
	// Codex performs a global merge shared by its app and CLI clients.
	claudePath := filepath.Join(cwd, ".claude", "settings.json")
	if changed, merr := mergeAgentHooks(claudePath, "claude"); merr != nil {
		bad("claude hook(%s): %v", claudePath, merr)
	} else if changed {
		ok("claude hook registered (.claude/settings.json — git commit to apply to team)")
	} else {
		ok("claude hook (.claude/settings.json — already registered)")
	}
	if home, herr := os.UserHomeDir(); herr == nil {
		codexPath := filepath.Join(home, ".codex", "hooks.json")
		if _, serr := os.Stat(filepath.Join(home, ".codex")); serr != nil {
			warn("codex not installed (~/.codex missing) — run cxt setup again if using codex")
		} else if changed, merr := mergeAgentHooks(codexPath, "codex"); merr != nil {
			bad("codex hook(%s): %v", codexPath, merr)
		} else {
			if changed {
				ok("codex hook registered (~/.codex/hooks.json — existing hooks merged)")
			} else {
				ok("codex hook (~/.codex/hooks.json — already registered)")
			}
			warn("codex requires one-time approval: codex run → /hooks → cxt item approval")
		}
	}

	// 6) Team default settings pull(.claude/.agents/.codex) — only when login and origin exist.
	if hasOrigin && authTokenPresent(cwd) {
		if conn, cerr := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd}); cerr == nil {
			// If URL's /<username>/<workspace>/ does not match the server workspace,
			// the repo is stored as unowned and will not appear on the web — a silent trap, so a warning.
			if conn.Repo.WorkspaceID == "" {
				warn("repository is not bound to a workspace and will not appear on the web — check <username>/<workspace> in the URL: %s", origin)
			}
			applied := 0
			for _, kind := range []string{"claude", "agents", "codex"} {
				if n, aerr := applySettings(ctx, c, cwd, string(conn.Repo.ID), kind); aerr == nil && n > 0 {
					applied += n
				}
			}
			if applied > 0 {
				ok("Team default settings pulled (%d files applied)", applied)
			} else {
				ok("No team default settings (upload possible in web About ⚙)")
			}
		} else {
			warn("Team setup pull deferred: %v", cerr)
		}
	}

	fmt.Println("Complete — use Claude Code/Codex in the CLI or supported desktop apps; commits, switches, and pushes remain ordinary git")
	return nil
}

// authTokenPresent indicates the presence of a stored token on the origin host (validity is determined by the first request).
func authTokenPresent(cwd string) bool {
	_, host, err := remoteAPIBase(cwd)
	return err == nil && authcfg.Token(host) != ""
}
