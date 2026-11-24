// Package cli contains the cxt CLI driver.
//
// Registers user subcommands under the single entry point Run. Each command maps flags → DTO
// and calls the corresponding inbound port, rendering the result to stdout.
//
// Commands: setup / init(=repo) / remote / login / logout / config / secrets / settings / hooks /
// segments and its verbs (push, log, remove, allow, doctor, refresh, archive, unarchive, resend,
// fork), each of which is also a top-level command / git-hook (internal).
package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// Container is the bundle of ports and wiring the CLI driver uses.
type Container struct {
	DeviceLoginClient       func(string) (outbound.DeviceLoginClient, error)
	OpenLoginBrowser        func(string)
	ResolveConnection       func(context.Context, string) (domain.RepositoryConnection, error)
	PrepareRemoteConnection func(context.Context, string, string, string) (PreparedRemoteConnection, error)
	// SetCaptureIdentity saves the identity that conversation documents are sent in, once the server confirmed it.
	SetCaptureIdentity func(context.Context, string, domain.DocumentIdentity) error
	SetRemoteEndpoint  func(context.Context, remotecfg.Observation, string, string) error
	// ResolveRepo identifies the repository of a folder without registering or changing it.
	ResolveRepo     func(context.Context, string) (domain.Repo, error)
	WithSecretsLock func(context.Context, string, func() error) error
	Init            inbound.InitRepo
	// Connection registers the repository of a folder with its server.
	Connection inbound.RepositoryConnector
	// Settings is a remote client for setting bundles (outbound direct — thin utility path).
	Settings interface {
		PullSettings(ctx context.Context, repoID, kind string) (domain.SettingsBundle, error)
	}
	// SettingsObjects is a local settings object store access (for replacement/backup/restore).
	SettingsObjects interface {
		PutSettingsObject(ctx context.Context, bundle domain.SettingsBundle) (domain.ContentHash, error)
		GetSettingsObject(ctx context.Context, hash domain.ContentHash) (domain.SettingsBundle, error)
	}
	// Segments reads agent session records and Git for the read-only segment report.
	Segments inbound.SegmentReporter
	// SegmentUpload uploads that report to the repository's server, one report per session.
	SegmentUpload inbound.SegmentUploader
	// SegmentCare finds and puts right what is wrong between the sessions on
	// the server and the records on this computer.
	SegmentCare inbound.SegmentCare
	// SessionFork starts new work from a commit: a Git branch, a session
	// that continues a conversation, or both.
	SessionFork inbound.SessionForker
	// SegmentCollector is the same upload when a hook woke it: sessions since
	// automatic reading began, and only what changed since its last run.
	SegmentCollector inbound.SegmentCollector
	// WakeSegmentCollector starts a detached collector; process lifecycle
	// wiring, absent in embedded and test drivers.
	WakeSegmentCollector func(string)
	// WakeSegmentCollectorAfterPush starts the detached wait for the process
	// that pushes to end, which then wakes the collector; the pre-push hook
	// uses it. Absent like WakeSegmentCollector.
	WakeSegmentCollectorAfterPush func(cwd string, pushing int)
	// WithSegmentCollectorLock lets one collector run per repository and
	// keeps one request for another run.
	WithSegmentCollectorLock func(ctx context.Context, cwd string, use func(another func() bool) error) (bool, error)
	// SegmentCollectorState reads what the collector last did.
	SegmentCollectorState func(ctx context.Context, cwd string) (domain.CollectorState, error)
}

// segmentVerbs are the session commands that are also top-level commands:
// `cxt push` is `cxt segments push`.
var segmentVerbs = map[string]bool{"push": true, "log": true, "remove": true, "allow": true, "doctor": true, "refresh": true,
	"archive": true, "unarchive": true, "resend": true, "fork": true}

// CanonicalArgs rewrites a top-level session command to its `segments` form,
// so that both spellings are parsed and run by the same code.
func CanonicalArgs(args []string) []string {
	if len(args) < 2 || !segmentVerbs[args[1]] {
		return args
	}
	out := append([]string{args[0], "segments"}, args[1:]...)
	return out
}

// Run is the CLI entry point. Parses args(=os.Args) to execute the corresponding subcommand.
func Run(c *Container, args []string) error {
	args = CanonicalArgs(args)
	if handled, err := PreflightArgs(args); handled || err != nil {
		return err
	}
	cmd := args[1]
	parsed := parsedCommand{name: cmd}
	if cmd != "git-hook" {
		var err error
		parsed, _, err = parseCommand(cmd, args[2:])
		if err != nil {
			return err
		}
	}

	ctx := context.Background()
	rest := args[2:]
	cwd, _ := os.Getwd()
	if cmd == "segments" {
		var reporter inbound.SegmentReporter
		var uploader inbound.SegmentUploader
		if c != nil {
			reporter, uploader = c.Segments, c.SegmentUpload
		}
		var care inbound.SegmentCare
		if c != nil {
			care = c.SegmentCare
		}
		switch parsed.first() {
		case "push":
			return RunSegmentsPush(ctx, uploader, cwd, parsed.positionals[1:], parsed.has("--json"), os.Stdout)
		case "log":
			return RunSegmentsLog(ctx, care, cwd, parsed.positionals[1:], parsed.has("--json"), os.Stdout)
		case "remove":
			return RunSegmentsRemove(ctx, care, cwd, parsed.positionals[1:], parsed.has("--yes"), parsed.has("--json"), os.Stdout)
		case "allow":
			return RunSegmentsAllow(ctx, care, cwd, parsed.positionals[1:], parsed.has("--json"), os.Stdout)
		case "doctor":
			return RunSegmentsDoctor(ctx, care, cwd, parsed.has("--json"), os.Stdout)
		case "refresh":
			return RunSegmentsRefresh(ctx, care, cwd, os.Stdout)
		case "archive", "unarchive":
			return RunSegmentsArchive(ctx, care, cwd, parsed.positionals[1:], parsed.first() == "archive", os.Stdout)
		case "resend":
			return RunSegmentsResend(ctx, uploader, cwd, parsed.positionals[1:], parsed.has("--json"), os.Stdout)
		case "fork":
			var forker inbound.SessionForker
			if c != nil {
				forker = c.SessionFork
			}
			return RunSegmentsFork(ctx, forker, cwd, parsed.positionals[1:], parsed.flags["--at"], parsed.flags["--branch"], parsed.flags["--to"], parsed.has("--json"), os.Stdout)
		}
		return RunSegments(ctx, reporter, cwd, parsed.has("--json"), os.Stdout)
	}

	switch cmd {
	case "init", "repo": // 'cxt repo create <url>' also routes to init
		remote := parsed.flags["--remote"]
		if cmd == "repo" {
			remote = parsed.last() // repo create <github-url>
		}
		out, err := c.Init.Init(ctx, inbound.InitInput{Cwd: cwd, RemoteURL: remote})
		if err != nil {
			return err
		}
		fmt.Printf("cxt is set up here: %s (repo %s)\n", out.LocalStorePath, shortHash(out.RepoID))
		if added, gerr := githooks.EnsureGitignore(cwd); gerr == nil && len(added) > 0 {
			fmt.Printf(".gitignore now keeps the files of cxt out of Git: %s\n", strings.Join(added, " "))
		}
		_ = githooks.EnsureExcluded(cwd) // .git/info/exclude local augmentation (user can remove from .gitignore and it persists)
		if n, created := capture.GenerateFromEnv(cwd); created {
			fmt.Printf(".cxtsecrets created — %d values extracted from .env (automatically masked during context storage)\n", n)
		}
		// git hook auto-install — "using git means cxt comes with it" (--no-hooks to opt out).
		if !parsed.has("--no-hooks") {
			if installed, herr := githooks.Install(cwd); herr == nil {
				fmt.Printf("installed git hooks: %s (each only wakes the reading of your sessions)\n", strings.Join(installed, ", "))
			} else {
				fmt.Printf("hint: git hooks not installed (%v) — run cxt hooks install inside the Git repository\n", herr)
			}
		}
		// --remote (or repo create <url>) continues execution to register origin —
		// previously, this value was silently ignored (review #1).
		if remote != "" {
			if rerr := runRemote(ctx, c, cwd, []string{"add", "origin", remote}); rerr != nil {
				return rerr
			}
		}
		if _, ok := remotecfg.Origin(cwd); !ok {
			fmt.Println("hint: to connect to team server → cxt setup https://<host>/<owner>/<repository>")
		}
		return nil

	case "setup":
		// Onboarding single command (idempotent): init→git hook→remote→login→agent hook→team setting pull.
		return runSetup(ctx, c, cwd, rest)

	case "remote":
		return runRemote(ctx, c, cwd, rest)

	case "config":
		// cxt config <key> [value] — local preferences; the root document identity requires fresh confirmation.
		key := parsed.first()
		val := parsed.last()
		hasVal := len(parsed.positionals) == 2
		switch key {
		case "capture.identity":
			if hasVal {
				if val == "default" || val == "legacy" {
					val = ""
				}
				identity := domain.DocumentIdentity(val)
				if err := identity.Validate(); err != nil {
					return err
				}
				if c.SetCaptureIdentity == nil {
					return fmt.Errorf("capture identity configuration unavailable")
				}
				if err := c.SetCaptureIdentity(ctx, cwd, identity); err != nil {
					return err
				}
			}
			identity, err := remotecfg.CaptureIdentity(ctx, cwd)
			if err != nil {
				return err
			}
			display := string(identity)
			if identity == domain.DocumentIdentityLegacy {
				display = "legacy"
			}
			fmt.Printf("capture.identity = %s\n", display)
			return nil
		case "segments.auto":
			// Whether hooks wake the session report collector (on|off). "default" returns to on.
			if hasVal {
				if val == "default" {
					val = ""
				}
				if err := remotecfg.SetSegmentsAuto(ctx, cwd, val); err != nil {
					return err
				}
			}
			if remotecfg.SegmentsAuto(cwd) {
				fmt.Println("segments.auto = on (default)")
			} else {
				fmt.Println("segments.auto = off")
			}
			return nil
		case "secrets.scrub":
			// Pattern scrub tier (off|standard|strict). Use "default" to return to standard.
			if hasVal {
				if val == "default" {
					val = ""
				}
				if err := remotecfg.SetSecretsScrub(ctx, cwd, val); err != nil {
					return err
				}
			}
			cur := remotecfg.SecretsScrub(cwd)
			if cur == "" {
				cur = "standard (default)"
			}
			fmt.Printf("secrets.scrub = %s\n", cur)
			return nil
		case "secrets.redact":
			// Masking replacement text customization. "default" to return to default.
			if hasVal {
				if val == "default" {
					val = ""
				}
				if err := remotecfg.SetSecretsRedact(ctx, cwd, val); err != nil {
					return err
				}
			}
			cur := remotecfg.SecretsRedact(cwd)
			if cur == "" {
				cur = capture.RedactedToken + " (default)"
			}
			fmt.Printf("secrets.redact = %s\n", cur)
			return nil
		case "secrets.minlen":
			if hasVal {
				n, cerr := strconv.Atoi(val)
				if cerr != nil {
					return fmt.Errorf("secrets.minlen must be a number: %q", val)
				}
				if err := remotecfg.SetSecretsMinLen(ctx, cwd, n); err != nil {
					return err
				}
			}
			cur := remotecfg.SecretsMinLen(cwd)
			if cur == 0 {
				fmt.Println("secrets.minlen = 4 (default)")
			} else {
				fmt.Printf("secrets.minlen = %d\n", cur)
			}
			return nil
		default:
			return fmt.Errorf("supported keys: segments.auto | capture.identity | secrets.scrub | secrets.redact | secrets.minlen")
		}

	case "login":
		// Default is device flow (browser approval — token does not pass through screen/clipboard, device_login.go).
		// `cxt login <token>` is manual fallback (web account settings ⚙ issued token), CI is CXT_TOKEN.
		tok := parsed.first()
		if tok == "" {
			tok = parsed.flags["-t"]
		}
		base, host, err := loginTarget(cwd, parsed.flags["--server"])
		if err != nil {
			return err
		}
		if tok != "" {
			return c.loginWithToken(ctx, base, host, tok)
		}
		return c.deviceLogin(ctx, base, host)

	case "logout":
		if err := requireRemote(cwd); err != nil {
			return err
		}
		_, host, err := remoteAPIBase(cwd)
		if err != nil {
			return err
		}
		if err := authcfg.Delete(host); err != nil {
			return err
		}
		fmt.Printf("✓ %s logout (local token deletion)\n", host)
		return nil

	case "secrets":
		// Share .cxtsecrets with end-to-end encryption: push encrypts the local file for the server;
		// pull decrypts server ciphertext into local .cxtsecrets. The passphrase never reaches the server.
		sub := parsed.first()
		if sub != "push" && sub != "pull" {
			return fmt.Errorf("usage: cxt secrets push|pull [-p <team passphrase>] [--remember] [--rotate] [--force (pull only)]")
		}
		if c.WithSecretsLock == nil {
			return fmt.Errorf("secrets lock unavailable")
		}
		if err := requireRemote(cwd); err != nil {
			return err
		}
		roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd)
		if err != nil {
			return err
		}
		cwd = roots.WorktreeRoot
		conn, err := c.Connection.Connect(ctx, cwd)
		if err != nil {
			return err
		}
		repoID := string(conn.Repo.ID)
		// Passphrase precedence: -p > CXT_SECRETS_PASSPHRASE > ~/.cxt/credentials.json.
		// On success, --remember stores it for this repository (credential store, secrets_cred.go).
		pass := parsed.flags["-p"]
		if pass == "" {
			pass = os.Getenv("CXT_SECRETS_PASSPHRASE")
		}
		fromStore := false
		if pass == "" {
			if pass = loadStoredPassphrase(repoID); pass != "" {
				fromStore = true
			}
		}
		if pass == "" {
			return fmt.Errorf("passphrase required: -p <passphrase> (after --remember stores it in ~/.cxt/credentials.json, -p may be omitted; the passphrase is never sent to the server)")
		}
		remember := func() {
			if fromStore || !parsed.has("--remember") {
				return
			}
			if err := storePassphrase(repoID, pass); err == nil {
				fmt.Println("✓ passphrase saved (~/.cxt/credentials.json, 0600) — future commands may omit -p")
			}
		}
		remote, ok := c.Settings.(secretsRemote)
		if !ok {
			return fmt.Errorf("secrets client unavailable")
		}
		if err := syncSecrets(ctx, c.WithSecretsLock, remote, cwd, repoID, sub, pass, parsed.has("--rotate"), parsed.has("--force")); err != nil {
			return err
		}
		remember()
		return nil

	case "settings":
		// Receive team default settings bundles and overwrite local .claude/.agents/.codex (upload via web About ⚙).
		switch parsed.first() {
		case "list":
			cur1, cur2, cur3, err := inspectSettingsHashes(cwd)
			if err != nil {
				return err
			}
			fmt.Printf("current: claude=%s agents=%s codex=%s\n", shortHash(cur1), shortHash(cur2), shortHash(cur3))
			backups := loadBackups(cwd)
			if len(backups) == 0 {
				fmt.Println("(No backup — one is kept each time cxt settings pull or cxt setup replaces a settings folder)")
				return nil
			}
			for i, b := range backups {
				fmt.Printf("backup@{%d}: %s claude=%s agents=%s codex=%s — %s\n",
					i, b.At.Local().Format("01-02 15:04"), shortHash(b.Claude), shortHash(b.Agents), shortHash(b.Codex), b.Note)
			}
			return nil
		case "restore":
			idx := 0
			if pos := parsed.positionals; len(pos) > 1 {
				fmt.Sscanf(pos[1], "%d", &idx)
			}
			return restoreSettingsBackup(ctx, c, cwd, idx)
		case "pull":
			// Continue with the existing logic.
		default:
			return fmt.Errorf("Usage: cxt settings pull|list|restore [n]")
		}
		if err := requireRemote(cwd); err != nil {
			return err
		}
		repo, err := c.Connection.Connect(ctx, cwd)
		if err != nil {
			return err
		}
		applied := 0
		keep := rememberSettings(ctx, c, cwd, "before settings pull")
		for _, kind := range []string{"claude", "agents", "codex"} {
			n, aerr := applySettings(ctx, c, cwd, string(repo.Repo.ID), kind)
			if aerr != nil {
				if errors.Is(aerr, domain.ErrNotFound) {
					continue
				}
				if applied > 0 {
					keep()
					return fmt.Errorf("settings pull %s failed after applying %d file(s); earlier bundles remain applied: %w", kind, applied, aerr)
				}
				return fmt.Errorf("settings pull %s: %w", kind, aerr)
			}
			if n > 0 {
				fmt.Printf("applied .%s/ — %d files overwritten\n", kind, n)
				applied += n
			}
		}
		if applied > 0 {
			keep()
			fmt.Println("the folders as they were are kept: cxt settings list, cxt settings restore")
		}
		if applied == 0 {
			fmt.Println("no team settings to apply — upload claude/agents/codex folder to web About ⚙")
		}
		return nil

	case "hooks":
		switch parsed.first() {
		case "install":
			state := gitctx.InspectContextRoot(ctx, cwd)
			if state.GitRepository && state.Exists {
				_ = githooks.EnsureIgnored(state.Root)
			}
			if !state.GitRepository || !state.Initialized {
				return fmt.Errorf("cxt is not set up in this repository — run 'cxt setup' or 'cxt init' first")
			}
			installed, err := githooks.Install(cwd)
			if err != nil {
				return err
			}
			fmt.Printf("installed git hooks: %s\n", strings.Join(installed, ", "))
			return nil
		case "uninstall":
			if err := githooks.Uninstall(cwd); err != nil {
				return err
			}
			fmt.Println("removed the git hooks of cxt (your own hooks are back in place)")
			return nil
		default:
			return fmt.Errorf("usage: cxt hooks install|uninstall")
		}

	case "git-hook":
		// git hook script entry point (fail-open) — not for direct invocation.
		return runGitHook(ctx, c, cwd, rest)

	default:
		return unknownCommandError(cmd)
	}
}

// requireRemote checks if the destination is set before push/pull.
// Fails with a warning if neither origin remote (recommended) nor CXT_REMOTE is set.
// remoteAPIBase fetches the REST base and host from origin (or CXT_REMOTE) for login/logout.
func remoteAPIBase(cwd string) (base, host string, err error) {
	observed, err := remotecfg.Observe(context.Background(), cwd)
	if err != nil {
		return "", "", err
	}
	binding, err := observed.OriginEndpoint(strings.TrimRight(strings.TrimSpace(os.Getenv("CXT_REMOTE")), "/"))
	if err != nil {
		return "", "", err
	}
	base = binding.Endpoint
	if _, err := remotecfg.CanonicalEndpoint(base); err != nil {
		return "", "", err
	}
	u, perr := url.Parse(base)
	if perr != nil || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("origin or CXT_REMOTE server address is not a safe http(s) URL")
	}
	return base, u.Host, nil
}

func requireRemote(cwd string) error {
	if _, ok := remotecfg.Origin(cwd); ok {
		return nil
	}
	if os.Getenv("CXT_REMOTE") != "" {
		return nil
	}
	return fmt.Errorf("no origin to push/pull — first connect your repository URL:\n  cxt remote add origin https://<host>/<owner>/<repository>")
}

// Server audits identify the replica but never register it as a side effect.
func resolveLocalRepo(ctx context.Context, c *Container, cwd string) (domain.Repo, error) {
	if c == nil || c.ResolveRepo == nil {
		return domain.Repo{}, fmt.Errorf("repository resolver unavailable")
	}
	return c.ResolveRepo(ctx, cwd)
}

// resolvedRepositoryURL validates a display/legacy address against the server's
// immutable content identity before callers save or compare a connection.
func resolvedRepositoryURL(ctx context.Context, c *Container, rawURL string) (string, error) {
	canonical, err := remotecfg.CanonicalURL(rawURL)
	if err != nil || c.ResolveConnection == nil {
		return canonical, err
	}
	resolved, err := c.ResolveConnection(ctx, canonical)
	if err != nil {
		return "", fmt.Errorf("repository identity could not be verified; remote was not saved (use cxt setup <url> or cxt login --server <server-url> first): %w", err)
	}
	return ValidatedRepositoryURL(canonical, resolved)
}

// ValidatedRepositoryURL preserves the server/identity checks for both setup
// resolution and a frozen remote-add connection.
func ValidatedRepositoryURL(canonical string, resolved domain.RepositoryConnection) (string, error) {
	var err error
	canonical, err = remotecfg.CanonicalURL(canonical)
	if err != nil {
		return "", err
	}
	stable, err := remotecfg.CanonicalURL(resolved.RemoteURL)
	requested, _ := url.Parse(canonical)
	resolvedURL, _ := url.Parse(stable)
	if err != nil || resolvedURL == nil || resolvedURL.Scheme != requested.Scheme || !strings.EqualFold(resolvedURL.Host, requested.Host) || remotecfg.RepoIDFor(stable) != resolved.RepoID || resolved.RepositoryID == "" {
		return "", fmt.Errorf("repository connection returned an invalid identity or a different server; remote was not saved")
	}
	return stable, nil
}

// runRemote is a git-like remote management command:
//
//	cxt remote [-v]                 list registered remotes
//	cxt remote add <name> <url>     add repo URL (usually origin)
//	cxt remote remove <name>        remove remote
//
// A single URL defines both the server address (scheme://host/api/v1) and RepoID (sha256(normalize(url))).
func runRemote(ctx context.Context, c *Container, cwd string, rest []string) error {
	observed, err := remotecfg.Observe(ctx, cwd)
	if err != nil {
		return err
	}
	remotes, err := observed.Remotes()
	if err != nil {
		return err
	}
	pos := positionals(rest)
	sub := firstPositional(rest)
	switch sub {
	case "set-endpoint":
		if len(pos) != 3 || c.SetRemoteEndpoint == nil {
			return fmt.Errorf("usage: cxt remote set-endpoint <name> <https-api-endpoint>")
		}
		if err := c.SetRemoteEndpoint(ctx, observed, pos[1], pos[2]); err != nil {
			return err
		}
		fmt.Println("Transport endpoint verified and saved; repository identity unchanged.")
		return nil
	case "add":
		if len(pos) != 3 {
			return fmt.Errorf("usage: cxt remote add <name> <url>  (e.g., cxt remote add origin https://cxthub.com/<owner>/<repository>)")
		}
		name, rawURL := pos[1], pos[2]
		canonicalURL, err := remotecfg.CanonicalURL(rawURL)
		if err != nil {
			return err
		}
		if existing, dup := remotes[name]; dup {
			return fmt.Errorf("remote %q is already registered as %s (change: remove then add)", name, existing)
		}
		var prepared PreparedRemoteConnection
		if c.PrepareRemoteConnection != nil {
			prepared, err = c.PrepareRemoteConnection(ctx, cwd, canonicalURL, remotes["origin"])
			if err != nil {
				return err
			}
			canonicalURL = prepared.URL
		} else {
			if name == "origin" {
				return fmt.Errorf("explicit remote connection unavailable; config unchanged")
			}
			canonicalURL, err = resolvedRepositoryURL(ctx, c, canonicalURL)
			if err != nil {
				return err
			}
		}
		var out inbound.ConnectOutput
		var connectErr error
		if name == "origin" {
			if prepared.Connect == nil || prepared.ValidateLocal == nil {
				return fmt.Errorf("incomplete remote connection; config unchanged")
			}
			out, connectErr = prepared.Connect(ctx)
			var he *backendclient.HTTPError
			if errors.As(connectErr, &he) && he.Code == "git_origin_mismatch" {
				return fmt.Errorf("connection rejected — %w", he)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := remotecfg.Add(ctx, observed, name, canonicalURL, prepared.ValidateLocal); err != nil {
			return err
		}
		fmt.Printf("remote %q → %s (repo %s)\n", name, canonicalURL, shortHash(remotecfg.RepoIDFor(canonicalURL)))
		if name == "origin" {
			switch {
			case connectErr != nil:
				fmt.Printf("⚠ Unable to connect to server (%v)\n  settings saved — the repository is registered the next time cxt setup runs with the server up.\n", connectErr)
			case out.Repo.RepositoryID != "":
				fmt.Printf("✓ Connected — server registration complete, bound to repository (%s). Visible on the web.\n", out.Repo.RepositoryID)
			default:
				fmt.Println("✓ Connected — server registration complete.")
				fmt.Println("⚠ Note: URL path does not match any repository (/<username>/<repository-slug>/…), so it will not be displayed in the web repository.")
			}
		}
		return nil
	case "remove", "rm":
		if len(pos) != 2 {
			return fmt.Errorf("usage: cxt remote remove <name>")
		}
		name := pos[1]
		if _, ok := remotes[name]; !ok {
			return fmt.Errorf("remote %q not found", name)
		}
		if err := remotecfg.Remove(ctx, observed, name); err != nil {
			return err
		}
		fmt.Printf("removed remote %q\n", name)
		return nil
	case "":
		if len(remotes) == 0 {
			fmt.Println("(No registered remotes — cxt remote add origin <url>)")
			return nil
		}
		verbose := flagPresent(rest, "-v")
		for name, u := range remotes {
			if verbose {
				binding, err := observed.ResolveEndpoint(u)
				if err != nil {
					return err
				}
				fmt.Printf("%s\t%s (repo %s)\n", name, u, shortHash(remotecfg.RepoIDFor(u)))
				if binding.Version != 0 {
					fmt.Printf("  endpoint\t%s (verified repository %s)\n", binding.Endpoint, binding.RepositoryID)
				}
			} else {
				fmt.Println(name)
			}
		}
		return nil
	default:
		return fmt.Errorf("cxt remote %q: unsupported subcommand (add|remove)", sub)
	}
}

// flagPresent checks if the name flag exists in args (a valueless boolean flag).
func flagPresent(args []string, name string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		if args[i] == name {
			return true
		}
		_, _, inline := splitFlagValue(args[i])
		if flagConsumesValue(args[i]) && !inline {
			i++
		}
	}
	return false
}

// positionals returns all positional arguments in order, excluding flags.
func positionals(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return append(out, args[i+1:]...)
		}
		if strings.HasPrefix(args[i], "-") {
			_, _, inline := splitFlagValue(args[i])
			if flagConsumesValue(args[i]) && !inline {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// firstPositional returns the first argument that is not a flag (e.g., checkout <ref>).
func firstPositional(args []string) string {
	if pos := positionals(args); len(pos) > 0 {
		return pos[0]
	}
	return ""
}

var publicCommandNames = publicCommands()

const usageText = `cxt — your coding-agent sessions, kept with the Git commits they produced
usage: cxt <command> [flags]

  Getting started:
  setup [remote-url]        set everything up in this folder: .cxt → login → repository connection → Git hooks → agent hooks → team settings
                            (safe to rerun; use --no-login to skip login)
  init [--no-hooks] [--remote <url>]
                            set up .cxt in this folder and install the Git hooks
  repo create <url>         set up .cxt and connect a server repository
  login [token] [--server url] authenticate before connecting, or with origin
  logout                    remove the saved origin credential
  remote add origin <url>   connect the server repository URL
  remote set-endpoint <name> <api-url>  verify and save another transport endpoint
  remote [-v]               list configured remotes
  remote remove <name>      remove a configured remote

  Sessions (cxt reads what Claude Code and Codex recorded, and Git; hooks only wake that reading):
  segments [--json]         show the sessions of this folder cut at their commits, without writes
  doctor [--json]           read this computer and the server and say what does not fit, each with the command that puts it right; changes nothing
  push [claude|codex <session>] [--json]
                            upload the sessions to the server now, or the one session named; a copy of what was sent is kept under .cxt/sessions
  log [claude|codex <session>] [--json]
                            show what .cxt/sessions keeps about the uploads: every session with its last upload, or one session's whole log
  remove claude|codex <session> --yes [--json]
                            remove a session from the server for every reader, with the sessions it spawned; it is not uploaded again until it is allowed again
  allow claude|codex <session> [--json]
                            take back the removal of a session and upload it again
  resend claude|codex <session> [--json]
                            send the copy kept under .cxt/sessions again, for a session the tool no longer holds
  archive|unarchive claude|codex <session>
                            hide a session from lists, the search and the summaries on the server, or bring it back
  refresh                   ask the server to read the repository from GitHub again, so that a push it has not seen opens what it explains
  fork --at <commit> --branch <name>
                            make a Git branch at a commit; a session started on it is given the summaries along it
  fork claude|codex <session> [--at <commit>] [--to claude|codex] [--branch <name>] [--json]
                            continue a session's conversation, up to the commit, as a new session of a tool in this folder
                            (every command of this group is also cxt segments <command>)

  Configuration:
  settings pull|list|restore [n]
                            apply team defaults, list backups, or restore a backup
  secrets push|pull [-p <pw>] [--remember] [--rotate] [--force (pull only)]
                            share .cxtsecrets with end-to-end encryption
  hooks install|uninstall   manage Git hooks manually
  config <key> [value]      inspect or set segments.auto, capture.identity, secrets.scrub, secrets.redact, secrets.minlen
  hook --provider P --event E
                            what the agent hooks run (integration use)
  version | --version       print the cxt version
  help | -h | --help        show this help`

func printUsage() {
	fmt.Println(usageText)
}

// shortHash returns the short notation (first 10 hex characters) of ContentHash.
func shortHash(h domain.ContentHash) string {
	hexPart := strings.TrimPrefix(string(h), "sha256:")
	if len(hexPart) > 10 {
		return hexPart[:10]
	}
	return hexPart
}

// applySettings applies server setting bundles (kind ∈ claude|agents|codex) to
// the repo root .claude/ or .agents/ directory (includes path traversal defense).
func applySettings(ctx context.Context, c *Container, cwd, repoID, kind string) (int, error) {
	bundle, err := c.Settings.PullSettings(ctx, repoID, kind)
	if err != nil {
		return 0, err
	}
	return capture.WriteSettingsDir(cwd, kind, "", bundle)
}
