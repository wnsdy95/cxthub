// Package cli contains the cxt CLI driver (domain model).
//
// Registers user subcommands under the single entry point Run. Each command maps flags → DTO
// and calls the corresponding inbound port, rendering the result to stdout.
//
// Implemented subcommands: setup / init(=repo) / remote / add / commit / save / list(=log) /
// switch / checkout / fork / load / stash / push / pull / memorize(=memory) / tag /
// config / login / logout / secrets / settings / hooks / claude·codex(agent wrapper) /
// git-hook(internal).
// There is no separate CLI memory-load command: `cxt memorize` writes memory,
// while MCP memory_load reads it. CLI diff compares captured events with the frozen index.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/githooks"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Container is a bundle of inbound ports used by the CLI driver + author identifier.
type Container struct {
	// WakeHistoricalSync is process lifecycle wiring, absent in embedded/test drivers.
	WakeHistoricalSync     func(string)
	ProviderLaunch         ProviderLaunchHooks
	PrepareAgent           inbound.PrepareAgentContext
	ApplySelectedPull      func(context.Context, string) (outbound.SelectedPullReceipt, error)
	PreviewRemoteRepair    func(context.Context, string, string, domain.ContentHash, string) (outbound.RemoteRepairPlan, error)
	ApplyRemoteRepair      func(context.Context, string, domain.ContentHash, outbound.RemoteRepairPlan) (outbound.RemoteRepairReceipt, error)
	CaptureRecovery        inbound.CaptureRecovery
	ResolveConnection      func(context.Context, string) (domain.RepositoryConnection, error)
	ResolveSyncDestination func(context.Context, string, string) (SyncDestination, error)
	// ResolveRepo identifies a configured replica without registering or mutating it.
	ResolveRepo      func(context.Context, string) (domain.Repo, error)
	Init             inbound.InitRepo
	Save             inbound.SaveSession
	Fork             inbound.ForkSession
	Branches         inbound.BranchLifecycle
	Checkout         inbound.CheckoutSession
	Load             inbound.LoadSession
	List             inbound.ListSessions
	HistoryQuery     inbound.HistoryQuery
	WorkingState     inbound.WorkingStateQuery
	ContextDiff      inbound.ContextDiffQuery
	Staging          inbound.Staging
	IndexStash       inbound.StagingStash
	ListIndexStashes func(context.Context, string) ([]domain.StagingStash, error)
	CodePosition     outbound.CodePosition
	Memorize         inbound.Memorize
	Sync             inbound.SyncRepo
	Seed             inbound.SeedBranch
	Tag              inbound.TagRef
	Queries          inbound.LocalRefQueries
	Stash            inbound.StashSession
	Handoff          inbound.BranchHandoff
	History          inbound.ContextHistory
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
	if handled, err := PreflightArgs(args); handled || err != nil {
		return err
	}
	if intent, recognized, err := ParseLaunchIntent(args[1:]); recognized {
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("provider launch composition unavailable")
		}
		return RunProviderLaunch(context.Background(), cwd, intent, c.ProviderLaunch)
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
	if cmd == "sync" {
		return RunSyncStatus(ctx, cwd, parsed.has("--json"), os.Stdout)
	}
	if cmd == "capture" {
		root := cxtRepoRoot(ctx, cwd)
		repo, err := remotecfg.Wrap(root, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
		if err != nil {
			return err
		}
		return RunCaptureRecovery(ctx, c, cwd, string(repo.ID), rest, os.Stdout)
	}
	if cmd == "doctor" || (cmd == "branch" && parsed.first() == "operations") {
		return RunDiagnostics(ctx, cwd, args[1:], os.Stdout)
	}
	if cmd == "branch" && parsed.first() == "recover" {
		if err := confirmOrphanRecovery(ctx, c, cwd, parsed.last()); err != nil {
			return err
		}
		return replayBranchCommand(ctx, c, cwd)
	}
	if cmd == "branch" && parsed.first() == "replay" {
		return replayBranchCommand(ctx, c, cwd)
	}
	if cmd == "push" || cmd == "pull" {
		fmt.Fprintf(os.Stderr, "%s: checking queued branch and PR operations\n", cmd)
	}
	if (parsed.effect == commandContextWrite || parsed.effect == commandSync) && c.History != nil {
		if err := replayBranchOperations(ctx, c, cwd); err != nil {
			hookWarn("branch operation remains queued: %v", err)
		}
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
		fmt.Printf("initialized cxt store: %s (repo %s)\n", out.LocalStorePath, shortHash(out.RepoID))
		if added, gerr := githooks.EnsureGitignore(cwd); gerr == nil && len(added) > 0 {
			fmt.Printf(".gitignore adds cxt commit prohibition list: %s\n", strings.Join(added, " "))
		}
		_ = githooks.EnsureExcluded(cwd) // .git/info/exclude local augmentation (user can remove from .gitignore and it persists)
		if n, created := capture.GenerateFromEnv(cwd); created {
			fmt.Printf(".cxtsecrets created — %d values extracted from .env (automatically masked during context storage)\n", n)
		}
		// git hook auto-install — "using git means cxt comes with it" (--no-hooks to opt out).
		if !parsed.has("--no-hooks") {
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
			fmt.Println("hint: to connect to team server → cxt setup https://<host>/<owner>/<repository>")
		}
		return nil

	case "setup":
		// Onboarding single command (idempotent): init→git hook→remote→login→agent hook→team setting pull.
		return runSetup(ctx, c, cwd, rest)

	case "remote":
		return runRemote(ctx, c, cwd, rest)

	case "branch":
		if parsed.first() == "" || parsed.first() == "list" {
			var refs []domain.Ref
			var err error
			if c.Queries != nil {
				refs, err = c.Queries.Refs(ctx, cwd)
			} else {
				var out inbound.ListOutput
				out, err = c.List.List(ctx, inbound.ListInput{})
				refs = out.Refs
			}
			if err != nil {
				return err
			}
			refs = append([]domain.Ref(nil), refs...)
			sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
			head := ""
			for _, ref := range refs {
				if ref.Kind == domain.RefHEAD {
					head = ref.Symbolic
				}
			}
			for _, ref := range refs {
				if ref.Kind != domain.RefBranch {
					continue
				}
				mark := " "
				if ref.Name == head {
					mark = "*"
				}
				fmt.Printf("%s %s\t%s\n", mark, ref.Name, shortHash(ref.Target))
			}
			return nil
		}
		action := parsed.first()
		branch := ""
		pos := parsed.positionals
		if len(pos) > 1 {
			branch = pos[1]
		}
		switch action {
		case "archive":
			if branch == "" {
				return fmt.Errorf("usage: cxt branch archive <name>")
			}
			if gitOut(cwd, "show-ref", "--verify", "refs/heads/"+branch) != "" {
				return fmt.Errorf("Git branch %q still exists — delete it first, then archive its context", branch)
			}
			out, err := c.Branches.Archive(ctx, inbound.BranchArchiveInput{Cwd: cwd, Branch: branch})
			if err != nil {
				return err
			}
			if out.LocalOnly {
				fmt.Printf("detached local tracking branch %q (server branch retained)\n", out.Branch)
			} else {
				fmt.Printf("archived context branch %q at %s (history preserved)\n", out.Branch, shortHash(out.Target))
			}
			spawnBranchStateSync(cwd)
			return nil
		case "restore":
			if branch == "" {
				return fmt.Errorf("usage: cxt branch restore <name> [--provider claude|codex] [--mode full|reconstructed|memory]")
			}
			out, err := c.Checkout.Checkout(ctx, inbound.CheckoutInput{
				From: branch, TargetProvider: parsed.flags["--provider"], Mode: loadModeOr(cwd, parsed.flags["--mode"]), Cwd: cwd,
			})
			if err != nil {
				return err
			}
			printRestore(out.Branch, out.Fidelity, out.ResumeCmd, out.WrittenPath)
			if out.ActivatedBranch {
				spawnBranchStateSync(cwd)
			}
			return nil
		default:
			return fmt.Errorf("usage: cxt branch archive|restore <name>")
		}

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
		return runStage(ctx, c, cwd, parsed)

	case "commit":
		return runStagedCommit(ctx, c, cwd, parsed)

	case "restore":
		return runUnstage(ctx, c, cwd, parsed)

	case "switch":
		// git switch equivalent: switch <branch> = checkout, switch -c <new> = checkout -b.
		out, err := manualCheckout(ctx, c, inbound.CheckoutInput{
			From:      parsed.first(),
			NewBranch: parsed.flags["-c"],
			Mode:      loadModeOr(cwd, parsed.flags["--mode"]),
			Cwd:       cwd,
		})
		if err != nil {
			return err
		}
		printRestore(out.Branch, out.Fidelity, out.ResumeCmd, out.WrittenPath)
		if out.ActivatedBranch {
			spawnBranchStateSync(cwd)
		}
		return nil

	case "config":
		// cxt config <key> [value] — checkout.mode | load.mode | secrets.redact | secrets.minlen | secrets.scrub.
		key := parsed.first()
		val := parsed.last()
		hasVal := len(parsed.positionals) == 2
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
		tok := parsed.first()
		if tok == "" {
			tok = parsed.flags["-t"]
		}
		base, host, err := loginTarget(cwd, parsed.flags["--server"])
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
		// Reference reachability audit (read-only): The server calculates the
		// reachability set for all refs and pending sessions, then reports
		// unreferenced snapshots, missing parents, and roots. No changes are made.
		if err := requireRemote(cwd); err != nil {
			return err
		}
		repo, cerr := resolveReadRepo(ctx, c, cwd)
		if cerr != nil {
			return cerr
		}
		rep, ferr := c.Settings.(interface {
			Fsck(ctx context.Context, repoID string) (backendclient.FsckReport, error)
		}).Fsck(ctx, string(repo.ID))
		if ferr != nil {
			return ferr
		}
		fmt.Print(formatFsckReport(rep))
		return nil

	case "reflog":
		// Reference movement log (read-only): Each ref's movement history, newest first. Tips that became unreachable due to ref movements can be recovered from the old column.
		if err := requireRemote(cwd); err != nil {
			return err
		}
		repo, cerr := resolveReadRepo(ctx, c, cwd)
		if cerr != nil {
			return cerr
		}
		entries, ferr := c.Settings.(interface {
			Reflog(ctx context.Context, repoID string) ([]backendclient.RefLogEntry, error)
		}).Reflog(ctx, string(repo.ID))
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
		sub := parsed.first()
		if sub != "push" && sub != "pull" {
			return fmt.Errorf("usage: cxt secrets push|pull [-p <team passphrase>] [--remember] [--rotate] [--force (pull only)]")
		}
		if err := requireRemote(cwd); err != nil {
			return err
		}
		roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd)
		if err != nil {
			return err
		}
		cwd = roots.WorktreeRoot
		conn, err := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
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
		if err := syncSecrets(ctx, remote, cwd, repoID, sub, pass, parsed.has("--rotate"), parsed.has("--force")); err != nil {
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
		repo, err := c.Sync.Connect(ctx, inbound.SyncInput{Cwd: cwd})
		if err != nil {
			return err
		}
		applied := 0
		for _, kind := range []string{"claude", "agents", "codex"} {
			n, aerr := applySettings(ctx, c, cwd, string(repo.Repo.ID), kind)
			if aerr != nil {
				if errors.Is(aerr, domain.ErrNotFound) {
					continue
				}
				if applied > 0 {
					return fmt.Errorf("settings pull %s failed after applying %d file(s); earlier bundles remain applied: %w", kind, applied, aerr)
				}
				return fmt.Errorf("settings pull %s: %w", kind, aerr)
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
		switch parsed.first() {
		case "install":
			state := gitctx.InspectContextRoot(ctx, cwd)
			if state.GitRepository && state.Exists {
				_ = githooks.EnsureIgnored(state.Root)
			}
			if !state.GitRepository || !state.Initialized {
				return fmt.Errorf("cxt repository is not initialized — run 'cxt init' first")
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
			fmt.Println("removed cxt git hooks (previous user hooks restored)")
			return nil
		default:
			return fmt.Errorf("usage: cxt hooks install|uninstall")
		}

	case "git-hook":
		// git hook script entry point (fail-open) — not for direct invocation.
		return runGitHook(ctx, c, cwd, rest)

	case "save":
		if err := reconcileCompletedPRPosition(ctx, c, cwd); err != nil {
			return err
		}
		target, err := commandCapture(ctx, cwd, parsed.flags["--provider"])
		if err != nil {
			return err
		}
		out, err := c.Save.Save(ctx, inbound.SaveInput{Cwd: cwd, Provider: target.Provider, SessionPath: target.SessionPath, Message: parsed.flags["-m"], Author: c.Identity})
		if err != nil {
			return err
		}
		fmt.Printf("saved snapshot %s on branch %q\n", shortHash(out.SnapshotID), out.Branch)
		// Automatically memorize the commit path (audit finding #5: consistency across storage paths). Best-effort.
		if mout, merr := c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: target.Provider, Ref: string(out.SnapshotID)}); merr == nil {
			fmt.Printf("memorized → %s (included in next push)\n", shortHash(mout.MemoryHash))
		}
		return nil

	case "status":
		if c.WorkingState == nil {
			return fmt.Errorf("working state query unavailable")
		}
		out, err := c.WorkingState.Status(ctx, cwd)
		if err != nil {
			return err
		}
		if parsed.has("--json") {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		printWorkingState(out)
		return nil

	case "diff":
		if c.ContextDiff == nil {
			return fmt.Errorf("context diff query unavailable")
		}
		out, err := c.ContextDiff.Diff(ctx, inbound.ContextDiffInput{Cwd: cwd, Staged: parsed.has("--staged")})
		if err != nil {
			return err
		}
		if parsed.has("--json") {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		printContextDiff(out)
		return nil

	case "list", "log":
		if c.HistoryQuery == nil {
			return fmt.Errorf("history query service unavailable")
		}
		out, err := c.HistoryQuery.QueryHistory(ctx, inbound.HistoryQueryInput{Cwd: cwd, Ref: parsed.first(), Branch: parsed.flags["--branch"], All: parsed.has("--all"), Retained: parsed.has("--retained"), Server: parsed.has("--server")})
		if err != nil {
			return err
		}
		if parsed.has("--json") {
			return json.NewEncoder(os.Stdout).Encode(out)
		}
		fmt.Printf("history: %s · %s · %d snapshots\n", out.Selection.Source, out.Selection.Scope, len(out.Snapshots))
		if !out.Complete {
			fmt.Printf("coverage incomplete: %d missing ancestors; inspect --json for server review reasons\n", len(out.Missing))
		}
		if len(out.Snapshots) == 0 {
			fmt.Println("(no snapshots in this scope)")
			return nil
		}
		for _, snap := range out.Snapshots {
			fmt.Printf("%s  %-20s  %s\n", shortHash(snap.ID), snap.Branch, snap.Message)
		}
		return nil

	case "checkout":
		out, err := manualCheckout(ctx, c, inbound.CheckoutInput{
			From:           parsed.first(),
			NewBranch:      parsed.flags["-b"],
			TargetProvider: parsed.flags["--provider"],
			Mode:           loadModeOr(cwd, parsed.flags["--mode"]),
			Cwd:            cwd,
		})
		if err != nil {
			return err
		}
		printRestore(out.Branch, out.Fidelity, out.ResumeCmd, out.WrittenPath)
		if out.ActivatedBranch {
			spawnBranchStateSync(cwd)
		}
		return nil

	case "fork":
		// fork = checkout -b (branch + restore). NewBranch is --as.
		out, err := manualCheckout(ctx, c, inbound.CheckoutInput{
			From:           parsed.first(),
			NewBranch:      parsed.flags["--as"],
			TargetProvider: parsed.flags["--provider"],
			Mode:           loadModeOr(cwd, parsed.flags["--mode"]),
			Cwd:            cwd,
		})
		if err != nil {
			return err
		}
		fmt.Printf("forked → branch %q (head %s)\n", out.Branch, shortHash(out.Head))
		printRestore(out.Branch, out.Fidelity, out.ResumeCmd, out.WrittenPath)
		return nil

	case "load":
		if parsed.has("--output") || parsed.has("--context-budget") {
			return runAgentArtifact(ctx, c, cwd, parsed)
		}
		mode := ""
		if !parsed.has("--work-state") {
			mode = loadModeOr(cwd, parsed.flags["--mode"])
		}
		out, err := c.Load.Load(ctx, inbound.LoadInput{
			Ref:            parsed.first(),
			TargetProvider: parsed.flags["--provider"],
			Mode:           mode,
			WorkStatePath:  parsed.flags["--work-state"],
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
		selected, remoteName, selectedRef, err := syncDestination(ctx, c, cwd, parsed)
		if err != nil {
			return err
		}
		c = selected
		if selectedRef == "" {
			replaySavedPRDiscovery(ctx, c, cwd)
		}
		if selectedRef == "" {
			if err := replayRewriteHistory(ctx, c, cwd); err != nil {
				return fmt.Errorf("rewritten context associations remain pending: %w", err)
			}
		}
		force := parsed.has("--force") || parsed.has("-f")
		appendDiverged := parsed.has("--append")
		defer wakeHistoricalSync(c, cwd)
		out, err := c.Sync.Push(ctx, inbound.SyncInput{Cwd: cwd, Ref: selectedRef, Force: force, Append: appendDiverged, ForegroundOnly: len(parsed.positionals) == 0 && !parsed.has("--wait-history"), Progress: syncProgressPrinter(os.Stderr)})
		if err != nil {
			if errors.Is(err, domain.ErrSyncConflict) {
				if strings.Contains(err.Error(), "memory attachment") {
					return fmt.Errorf("! [rejected] %w (memory fork)\nhint: Preview the specific memory pointer with 'cxt repair --preview --snapshot <hash> --reason <text> --output <file>', then apply its exact plan ID.\nhint: Then run 'cxt memorize' and 'cxt push' to project the local session again", err)
				}
				return fmt.Errorf("! [rejected] %w (non-fast-forward)\nhint: Remote commit not found in local. Use 'cxt push --append' to rebase (amend) onto remote head,\nor 'cxt pull' followed by push again.\nhint: To force overwrite, use 'cxt push --force' (remote history may be lost)", err)
			}
			return err
		}
		fmt.Printf("pushed %d snapshot(s), %d ref(s) → %s\n", out.Pushed, len(out.NewRefs), remoteName)
		if out.BackfillPending > 0 {
			fmt.Printf("retained history: %d snapshot(s) queued for background upload; inspect with 'cxt sync status' or wait with 'cxt push --wait-history'\n", out.BackfillPending)
		}
		if appendDiverged {
			// Server grafted remote head onto local ancestry — pull will reflect in local history.
			fmt.Println("appended: rebased onto remote head — 'cxt pull' will connect local history")
		}
		return nil

	case "pull":
		selected, remoteName, selectedRef, err := syncDestination(ctx, c, cwd, parsed)
		if err != nil {
			return err
		}
		c = selected
		if selectedRef == "" {
			replaySavedPRDiscovery(ctx, c, cwd)
		}
		force := parsed.has("--force") || parsed.has("-f")
		out, err := c.Sync.Pull(ctx, inbound.SyncInput{Cwd: cwd, Ref: selectedRef, Force: force, Progress: syncProgressPrinter(os.Stderr)})
		if err != nil {
			return err
		}
		fmt.Printf("pulled %d snapshot(s), %d ref(s) from %s\n", out.Pulled, len(out.NewRefs), remoteName)
		if len(out.Conflicts) > 0 {
			return fmt.Errorf("%w: ! [conflict] %s — merge canceled (local kept)\nhint: Preview a specific pointer with 'cxt repair --preview --ref <branch> --reason <text> --output <file>' before applying its exact plan ID", domain.ErrSyncConflict, strings.Join(out.Conflicts, ", "))
		}
		if selectedRef == "" {
			if err := reconcileCompletedPRPosition(ctx, c, cwd); err != nil {
				return err
			}
		}
		if c.ApplySelectedPull == nil {
			return fmt.Errorf("selected-code pull application unavailable")
		}
		receipt, err := c.ApplySelectedPull(ctx, cwd)
		if err != nil {
			return fmt.Errorf("remote observation updated; selected context was not applied: %w", err)
		}
		fmt.Printf("applied selected-code context and memory receipt %s (provider conversation unchanged)\n", receipt.Plan.ID)
		return nil

	case "fetch":
		selected, remoteName, selectedRef, err := syncDestination(ctx, c, cwd, parsed)
		if err != nil {
			return err
		}
		c = selected
		out, err := c.Sync.Pull(ctx, inbound.SyncInput{Cwd: cwd, Ref: selectedRef, FetchOnly: true, Progress: syncProgressPrinter(os.Stderr)})
		if err != nil {
			return err
		}
		fmt.Printf("fetched %d snapshot(s) from %s; working context, memory and branch refs unchanged\n", out.Pulled, remoteName)
		return nil

	case "repair":
		return runRemoteRepair(ctx, c, cwd, parsed)

	case "stash":
		if parsed.has("--staged") {
			return runIndexStash(ctx, c, cwd, parsed)
		}
		// git stash equivalent: save active session and return to branch head (commit chain) context.
		switch parsed.first() {
		case "", "push":
			target, err := commandCapture(ctx, cwd, parsed.flags["--provider"])
			if err != nil {
				return err
			}
			out, err := c.Stash.Stash(ctx, inbound.StashInput{Cwd: cwd, Provider: target.Provider, SessionPath: target.SessionPath, Message: parsed.flags["-m"], Author: c.Identity})
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
			var entries []domain.StashEntry
			var err error
			if c.Queries != nil {
				entries, err = c.Queries.StashList(ctx, cwd)
			} else {
				entries, err = c.Stash.StashList(ctx, cwd)
			}
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
			return fmt.Errorf("usage: cxt stash [push [-m msg] [--provider claude|codex]|pop|list]")
		}

	case "memorize", "memory":
		// Current branch head context distilled (compressed memory) → attached to snapshot.
		// Push sends the attached memory to the server with the raw data.
		claims, err := readMemoryClaims(parsed.flags["--claims"])
		if err != nil {
			return err
		}
		out, err := c.Memorize.Memorize(ctx, inbound.MemorizeInput{Cwd: cwd, Provider: parsed.flags["--provider"], Ref: parsed.first(), Claims: claims})
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
		name := parsed.first()
		if name == "" {
			var tags []domain.Ref
			var err error
			if c.Queries != nil {
				tags, err = c.Queries.Tags(ctx, cwd)
			} else {
				tags, err = c.Tag.Tags(ctx, cwd)
			}
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
		if pos := parsed.positionals; len(pos) > 1 {
			ref = pos[1]
		}
		out, err := c.Tag.Tag(ctx, inbound.TagInput{Cwd: cwd, Name: name, Ref: ref})
		if err != nil {
			return err
		}
		fmt.Printf("tag %q → %s (push to server)\n", out.Name, shortHash(out.Target))
		return nil

	default:
		return unknownCommandError(cmd)
	}
}

// formatFsckReport deliberately distinguishes reachability from integrity.
// An unreachable object is still stored and may be a superseded capture or
// intentionally detached history. A missing parent is the structural
// corruption class that fsck must call out as an error.
func formatFsckReport(rep backendclient.FsckReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Snapshots %d · Reach %d · Roots %d · Unreachable %d · Missing %d\n",
		rep.Total, rep.Reachable, len(rep.Roots), len(rep.Unreachable), len(rep.DanglingParents))
	for _, u := range rep.Unreachable {
		fmt.Fprintf(&b, "  unreachable (unreferenced): %s\n", u)
	}
	for _, d := range rep.DanglingParents {
		fmt.Fprintf(&b, "  corrupt (missing parent): %s → %s\n", d.Snapshot, d.Missing)
	}

	if len(rep.Unreachable) == 0 && len(rep.DanglingParents) == 0 {
		fmt.Fprintln(&b, "  ✓ No issues — all snapshots are referenced and no parents are missing")
		return b.String()
	}
	if len(rep.Unreachable) > 0 {
		fmt.Fprintln(&b, "  note: unreachable snapshots are preserved; they may be superseded captures or intentionally detached history")
	}
	if len(rep.DanglingParents) == 0 {
		fmt.Fprintln(&b, "  ✓ No missing-parent corruption")
	}
	return b.String()
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
	return fmt.Errorf("no origin to push/pull — first connect your repository URL:\n  cxt remote add origin https://<host>/<owner>/<repository>")
}

// Server audits identify the replica but never register it as a side effect.
func resolveReadRepo(ctx context.Context, c *Container, cwd string) (domain.Repo, error) {
	if c.ResolveRepo == nil {
		return domain.Repo{}, fmt.Errorf("read-only repository resolver unavailable")
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
	remotes, err := remotecfg.Load(cwd)
	if err != nil {
		return err
	}
	pos := positionals(rest)
	sub := firstPositional(rest)
	switch sub {
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
		canonicalURL, err = resolvedRepositoryURL(ctx, c, canonicalURL)
		if err != nil {
			return err
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
				// (unlike connection failure, "auto-registration on server start" does not apply: this folder is not a git connected to this repository repo.)
				delete(remotes, name)
				_ = remotecfg.Save(cwd, remotes)
				return fmt.Errorf("connection rejected — %w", he)
			case cerr != nil:
				fmt.Printf("⚠ Unable to connect to server (%v)\n  settings saved — will auto-register on first push when server is up.\n", cerr)
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

const usageText = `cxt — Git-style version control for coding-agent sessions
usage: cxt <command> [flags]

  Capture recovery:
  capture list [--json]      inspect durable capture attempts without replay or network
  capture show <id>          show evidence and expected fingerprint
  capture retry <id> --expect <hash>
                            retry publication using only frozen capture outcomes
  capture resolve <id> --expect <hash>
                            record a verified later successful capture
  capture acknowledge <id> --expect <hash> --reason <text>
                            retain an explicitly acknowledged gap, not successful capture

  Getting started:
  setup [remote-url]        initialize everything: repository → Git hooks → remote → login → agent hooks → team settings
                            (safe to rerun; use --no-login to skip login)
  init [--no-hooks] [--remote <url>]
                            initialize the local context store and install Git hooks
  repo create <url>         initialize and connect a server repository
  login [token] [--server url] authenticate before connecting, or with origin
  logout                    remove the saved origin credential

  Agent commands:
  claude [args...]          run Claude Code with branch context seeding
  codex [args...]           run Codex with branch context seeding
  mcp --local               start the offline-development MCP helper on stdio
  hook --provider P --event E
                            process a provider lifecycle event (integration use)

  Git-integrated commands:
  remote add origin <url>   connect the server repository URL (the context equivalent of Git origin)
  remote [-v]               list configured remotes
  remote remove <name>      remove a configured remote
  branch [list]             list local context branches
  branch archive <name>     hide a deleted Git branch pointer while preserving all context history
  branch restore <name>     restore an archived context branch and record a new active generation
  add [claude|codex|.]      freeze matching worktree sources in the additive index
  commit [-m msg]           publish only the frozen index; --resume retries its operation
  restore --staged <key>|.  unstage frozen sources without deleting their archives
  status [--json]           inspect worktree selection, memory, index and stored captures
  diff [--staged] [--json]   compare stored captures/index against selected history
  checkout [<ref>] [-b new] [--provider claude|codex] [--mode full|reconstructed|memory]
                            restore or branch context (also run automatically by Git checkout)
  switch [<branch>] [-c new] [--mode full|reconstructed|memory]
                            alias for checkout (equivalent to Git switch)
  push [remote [branch]] [--force|--append] [--wait-history]
                            publish current context; retain and retry historical uploads
  fetch [remote [branch]]   observe remote state without applying refs or memory
  pull [remote [branch]]    synchronize and apply the selected-code projection
  tag [<name> [ref]]        list or create immutable tags (equivalent to Git tags)
  stash [push|pop|list]     save or restore a session (push accepts --provider)

  Context commands:
  save [-m msg] [--provider claude|codex]
                            create a single-provider snapshot without commit staging or remote pending sync
  list | log [ref]          inspect HEAD/ref ancestry (--all, --retained, --server, --json)
  fork <ref> --as <branch> [--provider claude|codex] [--mode full|reconstructed|memory]
                            fork and restore a context branch
  load [<ref>] [--provider claude|codex] [--mode full|reconstructed|memory]
                            restore a snapshot (current head when ref is omitted)
  memorize | memory [<ref>] [--provider claude|codex] [--claims <json-file>]
                            distill context into reusable memory

  Configuration and maintenance:
  settings pull|list|restore [n]
                            apply team defaults, list backups, or restore a backup
  secrets push|pull [-p <pw>] [--remember] [--rotate] [--force (pull only)]
                            share .cxtsecrets with end-to-end encryption
  hooks install|uninstall   manage Git hooks manually
  config <key> [value]      inspect or set checkout, load, boundary, capture, or scrub behavior
  sync status [--json]      inspect local upload queues without reading objects
  doctor [--json]           inspect the local replica and Git journal without writes
  branch operations         inspect durable local operations (--json available)
  branch replay             retry verified operations and queue server synchronization
  branch recover <id> --confirm-orphan  recover an explicitly confirmed unborn branch
  repair --from-server       restore verified server objects, preserving local-only data
  fsck                      audit repository integrity
  reflog                    view the server ref-move log
  repack                    reclaim duplicate prefix storage through chunk CAS
  version | --version       print the cxt version
  help | -h | --help        show this help`

func printUsage() {
	fmt.Println(usageText)
}

// loadModeOr interprets the priority of load fidelity:
// --mode flag (per invocation) > local load.mode (checkout-specific) > server personal setting (account global) > structured memory input.
func loadModeOr(cwd, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := remotecfg.LoadMode(cwd); v != "" {
		return v
	}
	return serverLoadMode(cwd)
}

// flagVal finds a value in internal argv, excluding option values and literals.
func flagVal(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		flagName, inlineValue, inline := splitFlagValue(args[i])
		if flagName == name && inline {
			return inlineValue
		}
		if args[i] == name {
			if i+1 >= len(args) {
				return ""
			}
			return args[i+1]
		}
		if flagConsumesValue(args[i]) && !inline {
			i++
		}
	}
	return ""
}

// lastPositional returns the last argument that is not a flag (e.g., repo create <url> returns url).
func lastPositional(args []string) string {
	if pos := positionals(args); len(pos) > 0 {
		return pos[len(pos)-1]
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
