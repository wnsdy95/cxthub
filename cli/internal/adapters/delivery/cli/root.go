// Package cli contains the cxt CLI driver (domain model).
//
// Registers user subcommands under the single entry point Run. Each command maps flags → DTO
// and calls the corresponding inbound port, rendering the result to stdout.
//
// Implemented subcommands: setup / init(=repo) / remote / add / commit / save / list(=log) /
// switch / checkout / fork / load / stash / push / pull / memorize(=memory) / tag /
// config / login / logout / secrets / settings / hooks / claude·codex(agent wrapper) /
// git-hook(internal).
// Unimplemented: memory load(memory is always distill). diff is CLI non-target (web "compare" uses server Diff).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/secretscrypto"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Container is a bundle of inbound ports used by the CLI driver + author identifier.
type Container struct {
	Init     inbound.InitRepo
	Save     inbound.SaveSession
	Fork     inbound.ForkSession
	Checkout inbound.CheckoutSession
	Load     inbound.LoadSession
	List     inbound.ListSessions
	Memorize inbound.Memorize
	Sync     inbound.SyncRepo
	Seed     inbound.SeedBranch
	Tag      inbound.TagRef
	Stash    inbound.StashSession
	// PRMerges resolves incoming Git commits to merged provider PRs so post-merge
	// can promote the source branch context into the checked-out base timeline.
	PRMerges outbound.PullRequestMergeResolver
	// Settings is a remote client for setting bundles (outbound direct — thin utility path).
	Settings interface {
		PullSettings(ctx context.Context, repoID, kind string) (domain.SettingsBundle, error)
	}
	// SettingsObjects is a local settings object store access (for replacement/backup/restore).
	SettingsObjects interface {
		PutSettingsObject(ctx context.Context, bundle domain.SettingsBundle) (domain.ContentHash, error)
		GetSettingsObject(ctx context.Context, hash domain.ContentHash) (domain.SettingsBundle, error)
	}
	// Repack re-packs local doc storage into chunk CAS (legacy monolithic conversion + orphan chunk cleanup).
	Repack func() (converted int, saved int64, err error)
	// Identity is an author identifier used for snapshot author attribution (env CXT_NAME/EMAIL/TEAM).
	Identity domain.TeamIdentity
}

// Run is the CLI entry point. Parses args(=os.Args) to execute the corresponding subcommand.
func Run(c *Container, args []string) error {
	if len(args) < 2 {
		printUsage()
		return nil
	}
	cmd := args[1]
	switch cmd {
	case "-h", "--help", "help":
		printUsage()
		return nil
	}

	ctx := context.Background()
	rest := args[2:]
	cwd, _ := os.Getwd()

	switch cmd {
	case "init", "repo": // 'cxt repo create <url>' also routes to init
		remote := flagVal(rest, "--remote")
		if cmd == "repo" {
			remote = lastPositional(rest) // repo create <github-url>
		}
		out, err := c.Init.Init(ctx, inbound.InitInput{Cwd: cwd, RemoteURL: remote})
		if err != nil {
			return err
		}
		fmt.Printf("initialized cxt store: %s (repo %s)\n", out.LocalStorePath, shortHash(out.RepoID))
		if added, gerr := githooks.EnsureGitignore(cwd); gerr == nil && len(added) > 0 {
			fmt.Printf(".gitignore adds cxt commit prohibition list: %s\n", strings.Join(added, " "))
		}
		_ = githooks.EnsureExcluded(cwd) // .git/info/exclude local augmentation (user can remove from .gitignore and it persists)
		if n, created := capture.GenerateFromEnv(cwd); created {
			fmt.Printf(".cxtsecrets created — %d values extracted from .env (automatically masked during context storage)\n", n)
		}
		// git hook auto-install — "using git means cxt comes with it" (--no-hooks to opt out).
		if !flagPresent(rest, "--no-hooks") {
			if installed, herr := githooks.Install(cwd); herr == nil {
				fmt.Printf("installed git hooks: %s (git commit/checkout/push automatically triggers cxt)\n", strings.Join(installed, ", "))
			} else {
				fmt.Printf("hint: git hooks not installed (%v) — connect to cxt hooks install in git repo\n", herr)
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
			fmt.Println("hint: to connect to team server → cxt setup https://<host>/<username>/<workspace>")
		}
		return nil

	case "setup":
		// Onboarding single command (idempotent): init→git hook→remote→login→agent hook→team setting pull.
		return runSetup(ctx, c, cwd, rest)

	case "claude", "codex":
		// Agent wrapper (default execution path): fresh start seeds the current branch context, and automatically restarts with the seed session on context switch.
		return runAgentWrapper(ctx, c, cwd, cmd, rest)

	case "remote":
		return runRemote(ctx, c, cwd, rest)

	case "repack":
		// Repack large transcript and memory objects into their storage-only chunk
		// CAS forms. Content identities and the wire protocol remain unchanged.
		if c.Repack == nil {
			return fmt.Errorf("repack unsupported build")
		}
		n, saved, err := c.Repack()
		if err != nil {
			return err
		}
		fmt.Printf("repacked %d object(s), reclaimed %.1f MB\n", n, float64(saved)/1e6)
		return nil

	case "add":
		// git add response: stages the session provider to be included in the next commit. No arguments/"." = default (all active session providers — commit staging default). Prevents the reversal of capturing a narrower scope than staging.
		var provs []string
		for _, a := range rest {
			if strings.HasPrefix(a, "-") || a == "." {
				continue
			}
			if a != domain.ProviderClaude && a != domain.ProviderCodex {
				return fmt.Errorf("unknown provider %q (claude|codex)", a)
			}
			provs = append(provs, a)
		}
		if len(provs) == 0 {
			provs = []string{domain.ProviderClaude, domain.ProviderCodex}
		}
		if err := remotecfg.SetStagedProviders(cwd, provs); err != nil {
			return err
		}
		fmt.Printf("staged for next commit: %s\n", strings.Join(provs, ", "))
		return nil

	case "commit":
		// git commit response: snapshots the active session of the staged (or default) provider.
		msg := flagVal(rest, "-m")
		n, lastErr := snapshotForCommit(ctx, c, cwd, msg)
		if n == 0 {
			if lastErr != nil {
				return lastErr // e.g., not a git repository — pass the cause unchanged
			}
			return fmt.Errorf("no active session to snapshot (agent session must have run in this directory)")
		}
		return nil

	case "switch":
		// git switch equivalent: switch <branch> = checkout, switch -c <new> = checkout -b.
		out, err := c.Checkout.Checkout(ctx, inbound.CheckoutInput{
			From:      firstPositional(rest),
			NewBranch: flagVal(rest, "-c"),
			Mode:      modeOr(cwd, rest),
			Cwd:       cwd,
		})
		if err != nil {
			return err
		}
		printRestore(out.Branch, out.Fidelity, out.ResumeCmd, out.WrittenPath)
		return nil

	case "config":
		// cxt config <key> [value] — checkout.mode | load.mode | secrets.redact | secrets.minlen | secrets.scrub.
		key := firstPositional(rest)
		val := lastPositional(rest)
		hasVal := val != key && val != ""
		switch key {
		case "checkout.mode":
			if hasVal {
				if err := remotecfg.SetCheckoutMode(cwd, val); err != nil {
					return err
				}
			}
			fmt.Printf("checkout.mode = %s\n", remotecfg.CheckoutMode(cwd))
			return nil
		case "load.mode":
			// load/checkout/fork default fidelity (full|reconstructed|memory). "default" to disable.
			if hasVal {
				if val == "default" {
					val = ""
				}
				if err := remotecfg.SetLoadMode(cwd, val); err != nil {
					return err
				}
			}
			cur := remotecfg.LoadMode(cwd)
			if cur == "" {
				cur = "full (default)"
			}
			fmt.Printf("load.mode = %s\n", cur)
			return nil
		case "boundary.enforce":
			// session isolation process termination policy on switch (kill|none). "default" to disable (kill).
			if hasVal {
				if val == "default" {
					val = ""
				}
				if err := remotecfg.SetBoundaryEnforce(cwd, val); err != nil {
					return err
				}
			}
			fmt.Printf("boundary.enforce = %s\n", remotecfg.BoundaryEnforce(cwd))
			return nil
		case "capture.debounce":
			// hook Stop capture debounce window (seconds). "default"/0 to disable (60 seconds).
			if hasVal {
				if val == "default" {
					val = "0"
				}
				sec := 0
				if _, serr := fmt.Sscanf(val, "%d", &sec); serr != nil {
					return fmt.Errorf("capture.debounce must be an integer in seconds: %q", val)
				}
				if err := remotecfg.SetCaptureDebounce(cwd, sec); err != nil {
					return err
				}
			}
			fmt.Printf("capture.debounce = %s\n", remotecfg.CaptureDebounce(cwd))
			return nil
		case "secrets.scrub":
			// Pattern scrub tier (off|standard|strict). Use "default" to return to standard.
			if hasVal {
				if val == "default" {
					val = ""
				}
				if err := remotecfg.SetSecretsScrub(cwd, val); err != nil {
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
				if err := remotecfg.SetSecretsRedact(cwd, val); err != nil {
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
				if err := remotecfg.SetSecretsMinLen(cwd, n); err != nil {
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
			return fmt.Errorf("supported keys: checkout.mode | load.mode | boundary.enforce | capture.debounce | secrets.scrub | secrets.redact | secrets.minlen")
		}

	case "login":
		// Default is device flow (browser approval — token does not pass through screen/clipboard, device_login.go).
		// `cxt login <token>` is manual fallback (web account settings ⚙ issued token), CI is CXT_TOKEN.
		tok := firstPositional(rest)
		if tok == "" {
			tok = flagVal(rest, "-t")
		}
		if err := requireRemote(cwd); err != nil {
			return err
		}
		base, host, err := remoteAPIBase(cwd)
		if err != nil {
			return err
		}
		if tok != "" {
			return loginWithToken(ctx, base, host, tok)
		}
		return deviceLogin(ctx, base, host)

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

	case "fsck":
		// Reference reachability audit (read-only): The server calculates the reachability set for all refs and reports orphans, missing parents, and roots. No changes are made.
		if err := requireRemote(cwd); err != nil {
			return err
		}
		conn, cerr := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
		if cerr != nil {
			return cerr
		}
		rep, ferr := c.Settings.(interface {
			Fsck(ctx context.Context, repoID string) (backendclient.FsckReport, error)
		}).Fsck(ctx, string(conn.Repo.ID))
		if ferr != nil {
			return ferr
		}
		fmt.Printf("Snapshots %d · Reach %d · Roots %d · Orphans %d · Missing %d\n",
			rep.Total, rep.Reachable, len(rep.Roots), len(rep.Unreachable), len(rep.DanglingParents))
		for _, u := range rep.Unreachable {
			fmt.Printf("  orphan (unreachable): %s\n", u)
		}
		for _, d := range rep.DanglingParents {
			fmt.Printf("  corrupt (missing parent): %s → %s\n", d.Snapshot, d.Missing)
		}
		if len(rep.Unreachable) == 0 && len(rep.DanglingParents) == 0 {
			fmt.Println("  ✓ No issues — all snapshots are reachable from refs")
		}
		return nil

	case "reflog":
		// Reference movement log (read-only): Each ref's movement history, newest first. Tips that became unreachable due to ref movements can be recovered from the old column.
		if err := requireRemote(cwd); err != nil {
			return err
		}
		conn, cerr := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
		if cerr != nil {
			return cerr
		}
		entries, ferr := c.Settings.(interface {
			Reflog(ctx context.Context, repoID string) ([]backendclient.RefLogEntry, error)
		}).Reflog(ctx, string(conn.Repo.ID))
		if ferr != nil {
			return ferr
		}
		if len(entries) == 0 {
			fmt.Println("No reference movement log")
			return nil
		}
		for _, e := range entries {
			old := e.Old
			if old == "" {
				old = "(new)"
			}
			fmt.Printf("%s %s/%s  %s → %s\n", e.CreatedAt, e.Kind, e.Name, old, e.New)
		}
		return nil

	case "secrets":
		// Share .cxtsecrets with end-to-end encryption: push encrypts the local file for the server;
		// pull decrypts server ciphertext into local .cxtsecrets. The passphrase never reaches the server.
		sub := firstPositional(rest)
		if sub != "push" && sub != "pull" {
			return fmt.Errorf("usage: cxt secrets push|pull [-p <team passphrase>] [--remember] [--rotate]")
		}
		if err := requireRemote(cwd); err != nil {
			return err
		}
		conn, err := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
		if err != nil {
			return err
		}
		repoID := string(conn.Repo.ID)
		// Passphrase precedence: -p > CXT_SECRETS_PASSPHRASE > ~/.cxt/credentials.json.
		// On success, --remember stores it for this repository (credential store, secrets_cred.go).
		pass := flagVal(rest, "-p")
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
			if fromStore || !hasFlag(rest, "--remember") {
				return
			}
			if err := storePassphrase(repoID, pass); err == nil {
				fmt.Println("✓ passphrase saved (~/.cxt/credentials.json, 0600) — future commands may omit -p")
			}
		}
		if sub == "push" {
			rotate := hasFlag(rest, "--rotate")
			if verr := secretscrypto.ValidatePassphrase(pass); verr != nil {
				// Enforce the format only when rotation introduces a new passphrase. A legacy team passphrase
				// must not block ordinary secret refreshes; the server fingerprint check still enforces consistency.
				if rotate {
					return verr
				}
				fmt.Fprintf(os.Stderr, "warning: %v — the new format is required for passphrase replacement (--rotate)\n", verr)
			}
			plain, rerr := providerfs.ReadRepoFile(cwd, ".cxtsecrets")
			if rerr != nil {
				return fmt.Errorf(".cxtsecrets not found — create with cxt init or write manually")
			}
			env, eerr := secretscrypto.Encrypt(pass, string(plain), repoID)
			if eerr != nil {
				return eerr
			}
			env.UpdatedAt = time.Now().UTC()
			raw, _ := json.Marshal(env)
			// rotate CAS: send server's current envelope fingerprint to expect — if another save
			// interferes, the server rejects with 409, preventing stale decrypted secrets from
			// overwriting team members' updates.
			expect := ""
			if rotate {
				if cur, cerr := c.Settings.(interface {
					PullSecrets(ctx context.Context, repoID string) ([]byte, error)
				}).PullSecrets(ctx, repoID); cerr == nil {
					var curEnv struct {
						Fingerprint string `json:"fingerprint"`
					}
					_ = json.Unmarshal(cur, &curEnv)
					expect = curEnv.Fingerprint
				}
			}
			if err := c.Settings.(interface {
				PushSecrets(ctx context.Context, repoID string, raw []byte, rotate bool, expect string) error
			}).PushSecrets(ctx, repoID, raw, rotate, expect); err != nil {
				if strings.Contains(err.Error(), "401") {
					return fmt.Errorf("%v\nhint: login required for secret upload — generate token in Web Account Settings ⚙ and run `cxt login <token>`", err)
				}
				if strings.Contains(err.Error(), "passphrase_mismatch") {
					return fmt.Errorf("already set with a different team passphrase — use the passphrase shared by the team.\nTo rotate, upload the existing secret with the new passphrase and add `--rotate`")
				}
				if strings.Contains(err.Error(), "rotate_conflict") {
					return fmt.Errorf("secret updated during rotation — run `cxt secrets pull` to get the latest and retry rotation")
				}
				return err
			}
			if rotate {
				fmt.Println("✓ Team passphrase rotated — share the new passphrase with team members")
			} else {
				fmt.Println("✓ Encrypted and uploaded — server cannot see plaintext (E2E)")
			}
			remember()
			return nil
		}
		raw, perr := c.Settings.(interface {
			PullSecrets(ctx context.Context, repoID string) ([]byte, error)
		}).PullSecrets(ctx, repoID)
		if perr != nil {
			if strings.Contains(perr.Error(), "401") {
				return fmt.Errorf("login required — create a token in Web Account Settings ⚙, then run cxt login <token>")
			}
			if strings.Contains(perr.Error(), "403") {
				return fmt.Errorf("Insufficient permissions — Secret pull requires puller or higher role (request from owner)")
			}
			return fmt.Errorf("No secret on server — Set via web About ⚙ or cxt secrets push")
		}
		var env secretscrypto.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return err
		}
		plain, derr := secretscrypto.Decrypt(pass, env, repoID)
		if derr != nil {
			return derr
		}
		if err := providerfs.WriteRepoFileAtomic(cwd, ".cxtsecrets", []byte(plain), 0o600); err != nil {
			return err
		}
		fmt.Printf("✓ .cxtsecrets saved (%d bytes) — Used for automatic masking during context storage\n", len(plain))
		remember() // Save only after successful decryption — to prevent typo passphrase leakage
		return nil

	case "settings":
		// Receive team default settings bundles and overwrite local .claude/.agents/.codex (upload via web About ⚙).
		switch firstPositional(rest) {
		case "list":
			cur1, cur2, cur3 := currentSettingsHashes(ctx, c, cwd)
			fmt.Printf("current: claude=%s agents=%s codex=%s\n", shortHash(cur1), shortHash(cur2), shortHash(cur3))
			backups := loadBackups(cwd)
			if len(backups) == 0 {
				fmt.Println("(No backup — Folder is automatically replaced by branch move/rollback)")
				return nil
			}
			for i, b := range backups {
				fmt.Printf("backup@{%d}: %s claude=%s agents=%s codex=%s — %s\n",
					i, b.At.Local().Format("01-02 15:04"), shortHash(b.Claude), shortHash(b.Agents), shortHash(b.Codex), b.Note)
			}
			return nil
		case "restore":
			idx := 0
			if pos := positionals(rest); len(pos) > 1 {
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
		repo, err := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
		if err != nil {
			return err
		}
		applied := 0
		for _, kind := range []string{"claude", "agents", "codex"} {
			n, aerr := applySettings(ctx, c, cwd, string(repo.Repo.ID), kind)
			if aerr != nil {
				continue // no-op (404) — next type
			}
			if n > 0 {
				fmt.Printf("applied .%s/ — %d files overwritten\n", kind, n)
				applied += n
			}
		}
		if applied == 0 {
			fmt.Println("no team settings to apply — upload claude/agents/codex folder to web About ⚙")
		}
		return nil

	case "hooks":
		switch firstPositional(rest) {
		case "install":
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
			fmt.Println("removed cxt git hooks (previous user hooks restored)")
			return nil
		default:
			return fmt.Errorf("usage: cxt hooks install|uninstall")
		}

	case "git-hook":
		// git hook script entry point (fail-open) — not for direct invocation.
		return runGitHook(ctx, c, cwd, rest)

	case "save":
		provider := flagVal(rest, "--provider")
		if provider == "" {
			provider = domain.ProviderClaude
		}
		out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: provider, Message: flagVal(rest, "-m"), Author: c.Identity})
		if err != nil {
			return err
		}
		fmt.Printf("saved snapshot %s on branch %q\n", shortHash(out.SnapshotID), out.Branch)
		// Automatically memorize the commit path (audit finding #5: consistency across storage paths). Best-effort.
		if mout, merr := c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: provider, Ref: string(out.SnapshotID)}); merr == nil {
			fmt.Printf("memorized → %s (included in next push)\n", shortHash(mout.MemoryHash))
		}
		return nil

	case "list", "log":
		out, err := c.List.List(ctx, inbound.ListInput{RepoID: "", Branch: flagVal(rest, "--branch")})
		if err != nil {
			return err
		}
		if len(out.Snapshots) == 0 {
			fmt.Println("(no snapshots yet — run 'cxt save')")
			return nil
		}
		for _, s := range out.Snapshots {
			fmt.Printf("%s  %-20s  %s\n", shortHash(s.ID), s.Branch, s.Message)
		}
		return nil

	case "checkout":
		out, err := c.Checkout.Checkout(ctx, inbound.CheckoutInput{
			From:           firstPositional(rest),
			NewBranch:      flagVal(rest, "-b"),
			TargetProvider: flagVal(rest, "--provider"),
			Mode:           modeOr(cwd, rest),
			Cwd:            cwd,
		})
		if err != nil {
			return err
		}
		printRestore(out.Branch, out.Fidelity, out.ResumeCmd, out.WrittenPath)
		return nil

	case "fork":
		// fork = checkout -b (branch + restore). NewBranch is --as.
		out, err := c.Checkout.Checkout(ctx, inbound.CheckoutInput{
			From:           firstPositional(rest),
			NewBranch:      flagVal(rest, "--as"),
			TargetProvider: flagVal(rest, "--provider"),
			Mode:           modeOr(cwd, rest),
			Cwd:            cwd,
		})
		if err != nil {
			return err
		}
		fmt.Printf("forked → branch %q (head %s)\n", out.Branch, shortHash(out.Head))
		printRestore(out.Branch, out.Fidelity, out.ResumeCmd, out.WrittenPath)
		return nil

	case "load":
		out, err := c.Load.Load(ctx, inbound.LoadInput{
			Ref:            firstPositional(rest),
			TargetProvider: flagVal(rest, "--provider"),
			Mode:           modeOr(cwd, rest),
			Cwd:            cwd,
		})
		if err != nil {
			return err
		}
		printRestore("", out.Fidelity, out.ResumeCmd, out.WrittenPath)
		if out.TrimmedEvents > 0 {
			fmt.Printf("  → Context window budget reduced to recent history (omitted %d old events)\n", out.TrimmedEvents)
		}
		return nil

	case "push":
		if err := requireRemote(cwd); err != nil {
			return err
		}
		force := flagPresent(rest, "--force") || flagPresent(rest, "-f")
		appendDiverged := flagPresent(rest, "--append")
		out, err := c.Sync.Push(ctx, inbound.SyncInput{Cwd: cwd, Force: force, Append: appendDiverged})
		if err != nil {
			if strings.Contains(err.Error(), "sync conflict") {
				if strings.Contains(err.Error(), "memory attachment") {
					return fmt.Errorf("! [rejected] %s (memory fork)\nhint: Run 'cxt pull --force' to adopt the remote memory pointer; immutable local memory and raw sessions are retained.\nhint: Then run 'cxt memorize' and 'cxt push' to project the local session again", strings.TrimPrefix(err.Error(), "sync conflict: "))
				}
				return fmt.Errorf("! [rejected] %s (non-fast-forward)\nhint: Remote commit not found in local. Use 'cxt push --append' to rebase (amend) onto remote head,\nor 'cxt pull' followed by push again.\nhint: To force overwrite, use 'cxt push --force' (remote history may be lost)", strings.TrimPrefix(err.Error(), "sync conflict: "))
			}
			return err
		}
		fmt.Printf("pushed %d snapshot(s), %d ref(s) → origin\n", out.Pushed, len(out.NewRefs))
		if appendDiverged {
			// Server grafted remote head onto local ancestry — pull will reflect in local history.
			fmt.Println("appended: rebased onto remote head — 'cxt pull' will connect local history")
		}
		return nil

	case "pull":
		if err := requireRemote(cwd); err != nil {
			return err
		}
		force := flagPresent(rest, "--force") || flagPresent(rest, "-f")
		out, err := c.Sync.Pull(ctx, inbound.SyncInput{Cwd: cwd, Force: force})
		if err != nil {
			return err
		}
		fmt.Printf("pulled %d snapshot(s), %d ref(s) from origin\n", out.Pulled, len(out.NewRefs))
		if len(out.Conflicts) > 0 {
			return fmt.Errorf("! [conflict] %s — merge canceled (local kept)\nhint: To adopt remote state, use 'cxt pull --force'", strings.Join(out.Conflicts, ", "))
		}
		return nil

	case "stash":
		// git stash equivalent: save active session and return to branch head (commit chain) context.
		switch firstPositional(rest) {
		case "", "push":
			out, err := c.Stash.Stash(ctx, inbound.StashInput{Cwd: cwd, Message: flagVal(rest, "-m"), Author: c.Identity})
			if err != nil {
				if err == domain.ErrNoActiveSession {
					return fmt.Errorf("no active session to stash (Git equivalent: \"No local changes to save\")")
				}
				return err
			}
			fmt.Printf("Saved context stash@{0} on %s: %s\n", out.Branch, shortHash(out.StashID))
			if out.RestoredHead {
				fmt.Printf("cxt: returned to %q head (commit-chain) context\n", out.Branch)
				if out.ResumeCmd != "" {
					fmt.Printf("  → resume: %s\n", out.ResumeCmd)
				}
			}
			return nil
		case "pop":
			out, err := c.Stash.StashPop(ctx, cwd)
			if err != nil {
				if err == domain.ErrNotFound {
					return fmt.Errorf("stash is empty")
				}
				return err
			}
			fmt.Printf("Dropped stash@{0} (%s) — context restored [fidelity: %s]\n", shortHash(out.Entry.Snapshot), out.Fidelity)
			if out.ResumeCmd != "" {
				fmt.Printf("  → resume: %s\n", out.ResumeCmd)
			}
			return nil
		case "list":
			entries, err := c.Stash.StashList(ctx, cwd)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Println("(no stash)")
				return nil
			}
			for i, e := range entries {
				fmt.Printf("stash@{%d}: On %s: %s (%s)\n", i, e.Branch, e.Message, shortHash(e.Snapshot))
			}
			return nil
		default:
			return fmt.Errorf("usage: cxt stash [push [-m msg]|pop|list]")
		}

	case "memorize", "memory":
		// Current branch head context distilled (compressed memory) → attached to snapshot.
		// Push sends the attached memory to the server with the raw data.
		out, err := c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: flagVal(rest, "--provider"), Ref: firstPositional(rest)})
		if err != nil {
			if err == domain.ErrNotFound {
				return fmt.Errorf("no snapshot to distill — create a snapshot first using git commit (or cxt commit)")
			}
			return err
		}
		fmt.Printf("memorized %s → memory %s (included in next push)\n", shortHash(out.SnapshotID), shortHash(out.MemoryHash))
		return nil

	case "tag":
		// git tag handling: cxt tag → list, cxt tag <name> [ref] → create (immutable).
		name := firstPositional(rest)
		if name == "" {
			tags, err := c.Tag.Tags(ctx, cwd)
			if err != nil {
				return err
			}
			if len(tags) == 0 {
				fmt.Println("(No tag — cxt tag <name> [ref])")
				return nil
			}
			for _, t := range tags {
				fmt.Printf("%s\t%s\n", t.Name, shortHash(t.Target))
			}
			return nil
		}
		var ref string
		if pos := positionals(rest); len(pos) > 1 {
			ref = pos[1]
		}
		out, err := c.Tag.Tag(ctx, inbound.TagInput{Cwd: cwd, Name: name, Ref: ref})
		if err != nil {
			return err
		}
		fmt.Printf("tag %q → %s (push to server)\n", out.Name, shortHash(out.Target))
		return nil

	default:
		return fmt.Errorf("%q: unknown command. Supported: %s", cmd, strings.Join(publicCommandNames, "|"))
	}
}

// requireRemote checks if the destination is set before push/pull.
// Fails with a warning if neither origin remote (recommended) nor CXT_REMOTE is set.
// remoteAPIBase fetches the REST base and host from origin (or CXT_REMOTE) for login/logout.
func remoteAPIBase(cwd string) (base, host string, err error) {
	if origin, ok := remotecfg.Origin(cwd); ok {
		if base, err = remotecfg.APIBase(origin); err != nil {
			return "", "", err
		}
	} else {
		base = strings.TrimRight(strings.TrimSpace(os.Getenv("CXT_REMOTE")), "/")
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
	return fmt.Errorf("no origin to push/pull — first connect your workspace URL:\n  cxt remote add origin https://<host>/<username>/<workspace>")
}

// runRemote is a git-like remote management command:
//
//	cxt remote [-v]                 list registered remotes
//	cxt remote add <name> <url>     add repo URL (usually origin)
//	cxt remote remove <name>        remove remote
//
// A single URL defines both the server address (scheme://host/api/v1) and RepoID (sha256(normalize(url))).
func runRemote(ctx context.Context, c *Container, cwd string, rest []string) error {
	remotes, err := remotecfg.Load(cwd)
	if err != nil {
		return err
	}
	sub := ""
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		sub = rest[0]
	}
	switch sub {
	case "add":
		if len(rest) < 3 {
			return fmt.Errorf("usage: cxt remote add <name> <url>  (e.g., cxt remote add origin https://cxthub.com/<username>/<workspace>)")
		}
		name, rawURL := rest[1], rest[2]
		canonicalURL, err := remotecfg.CanonicalURL(rawURL)
		if err != nil {
			return err
		}
		if existing, dup := remotes[name]; dup {
			return fmt.Errorf("remote %q is already registered as %s (change: remove then add)", name, existing)
		}
		remotes[name] = canonicalURL
		if err := remotecfg.Save(cwd, remotes); err != nil {
			return err
		}
		fmt.Printf("remote %q → %s (repo %s)\n", name, canonicalURL, shortHash(remotecfg.RepoIDFor(canonicalURL)))
		// if origin, register immediately on the server to confirm and display the connection status (visible on the web too).
		if name == "origin" {
			out, cerr := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
			var he *backendclient.HTTPError
			switch {
			case cerr != nil && errors.As(cerr, &he) && he.Code == "git_origin_mismatch":
				// definitive server rejection — rollback the saved remote and exit with failure.
				// (unlike connection failure, "auto-registration on server start" does not apply: this folder is not a git connected to this workspace repo.)
				delete(remotes, name)
				_ = remotecfg.Save(cwd, remotes)
				return fmt.Errorf("connection rejected — %w", he)
			case cerr != nil:
				fmt.Printf("⚠ Unable to connect to server (%v)\n  settings saved — will auto-register on first push when server is up.\n", cerr)
			case out.Repo.WorkspaceID != "":
				fmt.Printf("✓ Connected — server registration complete, bound to workspace (%s). Visible on the web.\n", out.Repo.WorkspaceID)
			default:
				fmt.Println("✓ Connected — server registration complete.")
				fmt.Println("⚠ Note: URL path does not match any workspace (/<username>/<workspace-slug>/…), so it will not be displayed in the web workspace.")
			}
		}
		return nil
	case "remove", "rm":
		if len(rest) < 2 {
			return fmt.Errorf("usage: cxt remote remove <name>")
		}
		name := rest[1]
		if _, ok := remotes[name]; !ok {
			return fmt.Errorf("remote %q not found", name)
		}
		delete(remotes, name)
		if err := remotecfg.Save(cwd, remotes); err != nil {
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
				fmt.Printf("%s\t%s (repo %s)\n", name, u, shortHash(remotecfg.RepoIDFor(u)))
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
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// printRestore renders the load/checkout results (fidelity, resume command, or record path).
func printRestore(branch, fidelity, resumeCmd, writtenPath string) {
	if branch != "" {
		fmt.Printf("checked out branch %q  [fidelity: %s]\n", branch, fidelity)
	} else {
		fmt.Printf("loaded  [fidelity: %s]\n", fidelity)
	}
	if resumeCmd != "" {
		fmt.Printf("  → resume:  %s\n", resumeCmd)
	}
	if writtenPath != "" {
		fmt.Printf("  → written: %s\n", writtenPath)
	}
}

// positionals returns all positional arguments in order, excluding flags.
func positionals(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			i++ // skip flag value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// firstPositional returns the first argument that is not a flag (e.g., checkout <ref>).
func firstPositional(args []string) string {
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			i++ // skip flag value
			continue
		}
		return args[i]
	}
	return ""
}

var publicCommandNames = []string{
	"setup", "init", "repo", "claude", "codex", "remote", "repack",
	"add", "commit", "switch", "config", "login", "logout", "fsck",
	"reflog", "secrets", "settings", "hooks", "save", "list", "log",
	"checkout", "fork", "load", "push", "pull", "stash", "memorize",
	"memory", "tag", "mcp", "hook", "version", "help",
}

const usageText = `cxt — Git-style version control for coding-agent sessions
usage: cxt <command> [flags]

  Getting started:
  setup [remote-url]        initialize everything: repository → Git hooks → remote → login → agent hooks → team settings
                            (safe to rerun; use --no-login to skip login)
  init [--no-hooks] [--remote <url>]
                            initialize the local context store and install Git hooks
  repo create <url>         initialize and connect a server repository
  login [token|-t token]    authenticate with the configured origin
  logout                    remove the saved origin credential

  Agent commands:
  claude [args...]          run Claude Code with branch context seeding
  codex [args...]           run Codex with branch context seeding
  mcp                       start the read-only MCP server on stdio
  hook --provider P --event E
                            process a provider lifecycle event (integration use)

  Git-integrated commands:
  remote add origin <url>   connect the server repository URL (the context equivalent of Git origin)
  remote [-v]               list configured remotes
  remote remove <name>      remove a configured remote
  add [claude|codex]        stage providers for the next commit (defaults to both)
  commit [-m msg]           capture the active session (also run automatically by the Git commit hook)
  checkout [<ref>] [-b new] [--provider claude|codex] [--mode full|reconstructed|memory]
                            restore or branch context (also run automatically by Git checkout)
  switch [<branch>] [-c new] [--mode full|reconstructed|memory]
                            alias for checkout (equivalent to Git switch)
  push [--force|--append]   synchronize local context to origin
  pull [--force]            synchronize origin context locally
  tag [<name> [ref]]        list or create immutable tags (equivalent to Git tags)
  stash [push|pop|list]     save or restore a session (equivalent to Git stash)

  Context commands:
  save [-m msg] [--provider claude|codex]
                            create a single-provider snapshot without commit staging or remote pending sync
  list | log [--branch B]   list snapshots
  fork <ref> --as <branch> [--provider claude|codex] [--mode full|reconstructed|memory]
                            fork and restore a context branch
  load [<ref>] [--provider claude|codex] [--mode full|reconstructed|memory]
                            restore a snapshot (current head when ref is omitted)
  memorize | memory [<ref>] [--provider claude|codex]
                            distill context into reusable memory

  Configuration and maintenance:
  settings pull|list|restore [n]
                            apply team defaults, list backups, or restore a backup
  secrets push|pull [-p <pw>] [--remember] [--rotate]
                            share .cxtsecrets with end-to-end encryption
  hooks install|uninstall   manage Git hooks manually
  config <key> [value]      inspect or set checkout, load, boundary, capture, or scrub behavior
  fsck                      audit repository integrity
  reflog                    view the server ref-move log
  repack                    reclaim duplicate prefix storage through chunk CAS
  version | --version       print the cxt version
  help | -h | --help        show this help`

func printUsage() {
	fmt.Println(usageText)
}

// flagVal finds the value after name in args. Returns empty string if not found.
// modeOr interprets the priority of load fidelity:
// --mode flag (per invocation) > local load.mode (checkout-specific) > server personal setting (account global) > full.
func modeOr(cwd string, rest []string) string {
	if v := flagVal(rest, "--mode"); v != "" {
		return v
	}
	if v := remotecfg.LoadMode(cwd); v != "" {
		return v
	}
	return serverLoadMode(cwd)
}

func flagVal(args []string, name string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

// lastPositional returns the last argument that is not a flag (e.g., repo create <url> returns url).
func lastPositional(args []string) string {
	for i := len(args) - 1; i >= 0; i-- {
		if !strings.HasPrefix(args[i], "-") {
			return args[i]
		}
	}
	return ""
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
