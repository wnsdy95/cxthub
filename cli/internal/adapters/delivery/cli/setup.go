// setup.go — cxt setup: single command for new team member onboarding (idempotent — re-run = status check).
//
//	cxt setup [remote-url] [--no-login] [--git-remote <name>]
//
// Completes in one go after cloning: .cxt → login (device flow) → verified remote registration → git hooks
// → agent hooks (Claude repo setup, Codex global merge) → team basic settings pull.
// Each step reports with a checklist (✓/✗/⚠), and completed steps are not modified.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
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

// agentHookEvents are the events at which a tool runs `cxt hook`.
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
// setupGitRemote returns the value of --git-remote among the arguments, and
// whether the flag was given.
func setupGitRemote(args []string) (string, bool) {
	for index, arg := range args {
		name, value, inline := splitFlagValue(arg)
		if name != "--git-remote" {
			continue
		}
		if !inline && index+1 < len(args) {
			value = args[index+1]
		}
		return value, true
	}
	return "", false
}

func runSetup(ctx context.Context, c *Container, cwd string, rest []string) error {
	remoteURL := firstPositional(rest)
	ok := func(f string, a ...interface{}) { fmt.Printf("  ✓ "+f+"\n", a...) }
	bad := func(f string, a ...interface{}) { fmt.Printf("  ✗ "+f+"\n", a...) }
	warn := func(f string, a ...interface{}) { fmt.Printf("  ⚠ "+f+"\n", a...) }

	// 0) The Git remote cxt follows, before anything reads it: its address
	// names the repository. It is the one named with --git-remote, else the
	// one set before, else origin, else the repository's only remote.
	if name, named := setupGitRemote(rest); named {
		if err := gitctx.FollowRemote(ctx, cwd, name); err != nil {
			return err
		}
	}
	followed, followedExists := gitctx.FollowOnlyRemote(ctx, cwd)

	// 1) .cxt store (git repo required — otherwise, fail here).
	var out inbound.InitOutput
	var err error
	if state := gitctx.InspectContextRoot(ctx, cwd); state.Initialized {
		repo, rerr := resolveLocalRepo(ctx, c, cwd)
		err = rerr
		out = inbound.InitOutput{RepoID: repo.ID, LocalStorePath: filepath.Join(state.Root, ".cxt")}
	} else {
		out, err = c.Init.Init(ctx, inbound.InitInput{Cwd: cwd})
	}
	if err != nil {
		return fmt.Errorf(".cxt initialization failed: %w", err)
	}
	ok(".cxt store (repo %s)", shortHash(out.RepoID))
	if followedExists {
		ok("Git remote %s (cxt reads from it what was pushed)", followed)
	} else {
		warn("this repository has no Git remote named %s, so cxt cannot see what was pushed — name the one to follow: cxt setup --git-remote <name>", followed)
	}
	_, _ = githooks.EnsureGitignore(cwd)
	_ = githooks.EnsureExcluded(cwd)

	// Verify a new connection only after login, so private repository aliases
	// cannot create a different URL-derived identity before authentication.
	if remoteURL != "" && !flagPresent(rest, "--no-login") {
		binding, err := remotecfg.ResolveEndpoint(ctx, cwd, remoteURL)
		if err != nil {
			return err
		}
		base := binding.Endpoint
		server, err := url.Parse(base)
		if err != nil {
			return err
		}
		if authcfg.Token(server.Host) == "" && os.Getenv("CXT_TOKEN") == "" {
			if err = c.deviceLogin(ctx, base, server.Host); err != nil {
				return fmt.Errorf("login before repository connection: %w", err)
			}
		}
	}
	// 3) Preserve existing connections, including a migrated display alias.
	if remoteURL != "" {
		if cur, has := remotecfg.Origin(cwd); has {
			matches, err := setupRemoteMatches(ctx, c, cur, remoteURL)
			if err != nil {
				return err
			}
			if matches {
				ok("remote origin already registered = %s", cur)
			} else {
				return fmt.Errorf("requested repository differs from configured origin (%s); preserved without attachment — change: cxt remote remove origin then setup", cur)
			}
		} else if rerr := runRemote(ctx, c, cwd, []string{"add", "origin", remoteURL}); rerr != nil {
			// Git origin mismatch is server rejection — invalid folder, so stop setup.
			var he *backendclient.HTTPError
			if errors.As(rerr, &he) && he.Code == "git_origin_mismatch" {
				return fmt.Errorf("this folder cannot connect to %s — %w", remoteURL, he)
			}
			return fmt.Errorf("remote registration failed: %w", rerr)
		}
	}
	origin, hasOrigin := remotecfg.Origin(cwd)
	if hasOrigin {
		ok("remote origin = %s", origin)
	} else {
		warn("remote not registered — repository connection: cxt setup https://<host>/<owner>/<repository>")
	}

	// 4) Login (device flow). Keep token if already present.
	if hasOrigin && !flagPresent(rest, "--no-login") {
		if _, host, aerr := remoteAPIBase(cwd); aerr == nil {
			if authcfg.Token(host) != "" {
				ok("login (existing token: %s)", host)
			} else if base, host2, berr := remoteAPIBase(cwd); berr == nil {
				fmt.Println("  … login (device flow) — approve in browser")
				if lerr := c.deviceLogin(ctx, base, host2); lerr != nil {
					bad("Login failed: %v (try cxt login later)", lerr)
				} else {
					ok("Login completed")
				}
			}
		}
	}

	// 2) Git hooks. They only wake the reading of the tools' session records.
	if installed, herr := githooks.Install(cwd); herr == nil {
		ok("git hooks %d (commit·checkout·merge·rewrite·push; each only wakes the reading of your sessions)", len(installed))
	} else {
		bad("git hook installation failed: %v", herr)
	}

	// 5) Agent hooks — the same events exist in the Claude Code/Codex
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
	connectGitHubAt, githubMissing := "", false
	if hasOrigin && authTokenPresent(cwd) {
		if conn, cerr := c.Connection.Connect(ctx, cwd); cerr == nil {
			// The connection with GitHub: the server reads through it what
			// was pushed.
			if c.GitHub != nil {
				connection, gerr := c.GitHub.ReadGitHubConnection(ctx, string(conn.Repo.ID))
				mark, line := setupGitHubLine(connection, gerr)
				if mark != "" {
					fmt.Printf("  %s %s\n", mark, line)
				}
				connectGitHubAt, githubMissing = connection.ConnectURL, mark == setupMissing
			}
			// The server derives RepositoryID from the URL's namespace/repository
			// prefix. A missing binding is a legacy/local state worth surfacing.
			if conn.Repo.RepositoryID == "" {
				warn("connection has no repository ownership record and will not appear on the web — check <owner>/<repository> in the URL: %s", origin)
			}
			applied := 0
			keep := rememberSettings(ctx, c, cwd, "before cxt setup")
			for _, kind := range []string{"claude", "agents", "codex"} {
				if n, aerr := applySettings(ctx, c, cwd, string(conn.Repo.ID), kind); aerr == nil && n > 0 {
					applied += n
				}
			}
			if applied > 0 {
				keep()
				ok("Team default settings pulled (%d files applied; the folders as they were: cxt settings list)", applied)
			} else {
				ok("No team default settings (upload possible in web About ⚙)")
			}
		} else {
			warn("Team setup pull deferred: %v", cerr)
		}
	}

	if githubMissing {
		return fmt.Errorf("not complete: this repository is not connected to GitHub — an owner of the repository connects it at %s; everything on this computer is set up", connectGitHubAt)
	}
	fmt.Println("Complete — use Claude Code/Codex as usual; your sessions are read from their own records and shown with your commits once you push them. Check with: cxt doctor")
	return nil
}

// The marks of a checklist line that setupGitHubLine answers.
const (
	setupDone    = "✓"
	setupMissing = "✗"
	setupToKnow  = "⚠"
)

// setupGitHubLine is what cxt setup says of the repository's connection with
// GitHub: the mark of the line and the line. A repository that is not
// connected is missing something, and setup does not end as complete. A server
// without a GitHub App, or one that does not say, gets no line.
func setupGitHubLine(connection domain.GitHubRepositoryConnection, err error) (string, string) {
	switch {
	case err != nil:
		return setupToKnow, fmt.Sprintf("the server did not say whether this repository is connected to GitHub: %v", err)
	case connection.State == domain.GitHubRepositoryConnected:
		return setupDone, "GitHub connected (the server reads what is pushed through the GitHub App)"
	case connection.State == domain.GitHubRepositoryNotConnected:
		return setupMissing, "GitHub is not connected — an owner of the repository connects it at " + connection.ConnectURL
	}
	return "", ""
}

func setupRemoteMatches(ctx context.Context, c *Container, existing, requested string) (bool, error) {
	canonical, err := remotecfg.CanonicalURL(requested)
	if err != nil {
		return false, err
	}
	if remotecfg.RepoIDFor(existing) == remotecfg.RepoIDFor(canonical) {
		return true, nil // An unchanged connection also works offline.
	}
	stable, err := resolvedRepositoryURL(ctx, c, canonical)
	if err != nil {
		return false, err
	}
	return remotecfg.RepoIDFor(existing) == remotecfg.RepoIDFor(stable), nil
}

// authTokenPresent indicates the presence of a stored token on the origin host (validity is determined by the first request).
func authTokenPresent(cwd string) bool {
	_, host, err := remoteAPIBase(cwd)
	return err == nil && authcfg.Token(host) != ""
}
