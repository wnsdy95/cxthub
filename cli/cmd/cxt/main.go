// Package main is the entry point for the cxt client binary.
//
// It acts as a composition root, branching to the hook and CLI entry points based on arguments.
// It is the sole point for creating and wiring adapters and injecting use-case services (DI).
// (domain model Rule 5: cmd/cxt can import everything. DI is only here.)
//
// cxt is client-specific: the central server (serve) has its own backend module,
// and this binary talks to that server over REST (adapters/backendclient).
//
// Entry points:
//
//	cxt hook --provider X --event Y → what an agent tool runs at the events of a session
//	cxt git-hook <name>             → what a Git hook runs; it only wakes the collector
//	cxt <command>                   → user commands (see cxt help)
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	delivcli "github.com/wnsdy95/cxthub-cli/internal/adapters/delivery/cli"
	delivhook "github.com/wnsdy95/cxthub-cli/internal/adapters/delivery/hook"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// version is injected by goreleaser as an ldflag during release builds (-X main.version=vX.Y.Z).
var version = "dev"

func main() {
	if err := run(os.Args); err != nil {
		os.Exit(delivcli.WriteCommandFailure(os.Stderr, os.Args, err))
	}
}

// run parses arguments and branches to the appropriate entry point.
func run(args []string) error {
	// A session command is the same at the top level and under `segments`.
	args = delivcli.CanonicalArgs(args)
	// Help and usage errors must return before adapter construction.
	if handled, err := delivcli.PreflightArgs(args); handled || err != nil {
		return err
	}
	if args[1] == "version" || args[1] == "--version" {
		fmt.Println("cxt", version)
		return nil
	}
	if args[1] == "segments" {
		// Reading session records needs no store or provider adapter, so this
		// path builds neither. The report itself cannot write on this
		// computer; push adds the repository's server, writes branch IDs into
		// Git's own branch configuration and keeps a copy of what it sent
		// under .cxt/sessions.
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		positionals := segmentPositionals(args[2:])
		asJSON := slices.Contains(args[2:], "--json")
		sub := ""
		if len(positionals) > 0 {
			sub = positionals[0]
		}
		switch sub {
		case "push":
			// The same readers plus the repository's server: still no local store.
			uploader, err := newSegmentUploader(loadConfig(), "")
			if err != nil {
				return err
			}
			return delivcli.RunSegmentsPush(ctx, uploader, cwd, positionals[1:], asJSON, os.Stdout)
		case "log":
			// What .cxt keeps is read from this computer alone.
			return delivcli.RunSegmentsLog(ctx, newSegmentCare(), cwd, positionals[1:], asJSON, os.Stdout)
		case "fork":
			return delivcli.RunSegmentsFork(ctx, newSessionFork(loadConfig()), cwd, positionals[1:], segmentFlagValue(args[2:], "--at"), segmentFlagValue(args[2:], "--branch"),
				segmentFlagValue(args[2:], "--to"), asJSON, os.Stdout)
		case "resend":
			// The copy under .cxt is read, and nothing of the tools' records.
			uploader, err := newSegmentUploader(loadConfig(), "")
			if err != nil {
				return err
			}
			return delivcli.RunSegmentsResend(ctx, uploader, cwd, positionals[1:], asJSON, os.Stdout)
		case "remove", "allow", "doctor", "refresh", "archive", "unarchive":
			care, err := newSegmentCareWithServer(loadConfig())
			if err != nil {
				return err
			}
			switch sub {
			case "remove":
				return delivcli.RunSegmentsRemove(ctx, care, cwd, positionals[1:], slices.Contains(args[2:], "--yes"), asJSON, os.Stdout)
			case "allow":
				return delivcli.RunSegmentsAllow(ctx, care, cwd, positionals[1:], asJSON, os.Stdout)
			case "doctor":
				return delivcli.RunSegmentsDoctor(ctx, care, cwd, asJSON, os.Stdout)
			case "refresh":
				return delivcli.RunSegmentsRefresh(ctx, care, cwd, os.Stdout)
			}
			return delivcli.RunSegmentsArchive(ctx, care, cwd, positionals[1:], sub == "archive", os.Stdout)
		}
		reporter, err := newSegmentReporter("")
		if err != nil {
			return err
		}
		return delivcli.RunSegments(ctx, reporter, cwd, asJSON, os.Stdout)
	}
	// Process-level configuration resolves the shared repo root and environment
	// overrides. Persisted remotes and authentication are loaded lazily by their
	// adapters in buildContainer.
	cfg := loadConfig()

	// composition root: creates and wires all adapters and services.
	ctr := buildContainer(cfg)
	if args[1] == "hook" {
		// Hook safety contract: a failure must not block an agent session —
		// errors are only reported to stderr and the exit status is always 0.
		provider, event := parseHookFlags(args[2:])
		if err := ctr.hookHandler.Run(provider, event); err != nil {
			fmt.Fprintf(os.Stderr, "cxt hook: %v\n", err)
		}
		return nil
	}
	return ctr.cliHandler.Run(ctr.clictr, args)
}

// config contains process-level execution settings for the cxt client.
type config struct {
	// RepoRoot is the shared context root. Linked app worktrees resolve to the
	// primary working tree while their original cwd remains available to Git and
	// provider session discovery.
	RepoRoot  string
	GitDir    string
	GitBranch string
	GitCommit string
	// RemoteEndpoint is the REST base URL of the central server (e.g., https://cxthub.example.com/api/v1).
	RemoteEndpoint string
	// RemoteToken is the team bearer token (Authorization: Bearer cxt_team_<opaque>).
	RemoteToken string
	// Identity is the user identifier used in X-Cxt-Identity.
	Identity domain.TeamIdentity
}

// loadConfig loads the configuration.
// Remote (team server) connection is injected via environment variables:
//
//	CXT_REMOTE    Central server REST base (e.g., http://127.0.0.1:8080/api/v1)
//	CXT_TOKEN     Team bearer token (e.g., cxt_team_<opaque>)
//	CXT_NAME / CXT_EMAIL / CXT_TEAM   User identifier
func loadConfig() config {
	cwd, _ := os.Getwd()
	// .cxt is repository-wide. Desktop agents commonly create linked worktrees,
	// whose --show-toplevel differs even though --git-common-dir is shared.
	// If not a git repository, cwd is maintained (CurrentRepo later fails).
	repoRoot := cwd
	if roots, err := gitctx.ResolveRepositoryRoots(context.Background(), cwd); err == nil {
		repoRoot = roots.SharedRoot
	}
	// User identifier: CXT_NAME/EMAIL takes precedence, otherwise git config user.* (git is the source of truth —
	// code commits and context commits are attributed to the same author).
	gitCfg := func(key string) string {
		out, err := exec.Command("git", "-C", repoRoot, "config", "--get", key).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	name := os.Getenv("CXT_NAME")
	if name == "" {
		name = gitCfg("user.name")
	}
	email := os.Getenv("CXT_EMAIL")
	if email == "" {
		email = gitCfg("user.email")
	}
	return config{
		RepoRoot:       repoRoot,
		GitDir:         configGitValue(cwd, "rev-parse", "--absolute-git-dir"),
		GitBranch:      configGitValue(cwd, "symbolic-ref", "--short", "HEAD"),
		GitCommit:      configGitValue(cwd, "rev-parse", "--verify", "HEAD"),
		RemoteEndpoint: os.Getenv("CXT_REMOTE"),
		RemoteToken:    os.Getenv("CXT_TOKEN"),
		Identity: domain.TeamIdentity{
			Name:  name,
			Email: email,
			Team:  os.Getenv("CXT_TEAM"),
		},
	}
}

func configGitValue(cwd string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// container is the bundle of all services/handlers created in the composition root.
type container struct {
	hookHandler *delivhook.Handler
	cliHandler  struct {
		Run func(*delivcli.Container, []string) error
	}
	clictr *delivcli.Container
}

// buildRepositoryAdapters constructs passive repository readers and a lazy client.
// No registration or provider initialization occurs here.
func buildRepositoryAdapters(cfg config) (*storage.FileStore, *backendclient.BackendClient, outbound.GitContext) {
	// Local store: repo root .cxt/ (client-only).
	store := storage.NewWorktreeFileStore(cfg.RepoRoot, cfg.GitDir, cfg.GitBranch, cfg.GitCommit)
	remote := backendclient.NewResolvedBackendClient(func() (string, string, error) { return configuredTransport(cfg) }, cfg.Identity)
	return store, remote, remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter())
}

// injectScrubOptions gives capture the repository's masking policy. Capture
// cannot read remotecfg itself without an import cycle, so every composition
// that produces masked content injects the loader here.
func injectScrubOptions() {
	capture.LoadScrubOptions = func(repoRoot string) capture.ScrubOptions {
		return capture.ScrubOptions{
			Redact: remotecfg.SecretsRedact(repoRoot),
			MinLen: remotecfg.SecretsMinLen(repoRoot),
			Tier:   capture.ScrubTier(remotecfg.SecretsScrub(repoRoot)),
		}
	}
}

// buildContainer creates all adapters/services and injects dependencies.
func buildContainer(cfg config) container {
	store, remote, gitCtx := buildRepositoryAdapters(cfg)
	injectScrubOptions()

	// --- use-case services (inbound implementation, outbound injection) ---
	initSvc := app.NewInitRepoService(gitCtx, gitctx.NewSetupMarker())
	connectSvc := app.NewRepositoryConnectService(remote, gitCtx)

	// --- driving adapters (delivery; client-specific: hook/cli) ---
	hookHdl := delivhook.NewHandler().WithSegmentCollector(delivcli.SpawnSegmentCollector)
	// A session is given the most recent summary along its branch, read from
	// the server and from Git as they are when a hook wakes cxt.
	hookHdl.WithBranchSummaries(app.NewBranchSummaryService(gitCtx.CurrentRepo, gitctx.NewBranchPlace(), remote, gitctx.NewBranchSummaryGiven()).
		WithConnection(func(ctx context.Context, cwd string) bool {
			root, enabled := gitctx.ContextRoot(ctx, cwd)
			if !enabled {
				return false
			}
			_, connected := remotecfg.Origin(root)
			return connected || os.Getenv("CXT_REMOTE") != ""
		}))
	clictr := &delivcli.Container{
		DeviceLoginClient:  deviceLoginClient,
		OpenLoginBrowser:   openDeviceLoginBrowser,
		SetCaptureIdentity: setCaptureIdentity(cfg),
		SetRemoteEndpoint:  setRemoteEndpoint(cfg),
		ResolveRepo:        gitCtx.CurrentRepo,
		WithSecretsLock: func(ctx context.Context, cwd string, work func() error) error {
			return storage.NewFileStore(cwd).WithSecretsLock(ctx, work)
		},
		PrepareRemoteConnection: prepareRemoteConnection(cfg),
		ResolveConnection: func(ctx context.Context, raw string) (domain.RepositoryConnection, error) {
			observed, err := remotecfg.Observe(ctx, cfg.RepoRoot)
			if err != nil {
				return domain.RepositoryConnection{}, err
			}
			remotes, err := observed.Remotes()
			if err != nil {
				return domain.RepositoryConnection{}, err
			}
			binding, err := observed.ResolveEndpoint(raw)
			if err != nil {
				return domain.RepositoryConnection{}, err
			}
			base := binding.Endpoint
			token := tokenForObservedDestination(cfg, base, remotes["origin"])
			client := backendclient.NewBackendClient(func() string { return base }, func() string { return token }, cfg.Identity)
			return resolveEndpointConnection(ctx, client, binding)
		},
		Init:            initSvc,
		Connection:      connectSvc,
		Settings:        remote,
		SettingsObjects: store,
	}
	// The session report collector a hook wakes. A hook is never what it
	// reads: it reads what the tools and Git recorded, whatever woke it.
	if collector, files, err := newSegmentCollector(cfg); err == nil {
		clictr.SegmentCare = newSegmentCare()
		clictr.SegmentCollector = collector
		clictr.WithSegmentCollectorLock = files.WithCollectorLock
		clictr.SegmentCollectorState = files.LoadCollectorState
		clictr.WakeSegmentCollector = delivcli.SpawnSegmentCollector
		clictr.WakeSegmentCollectorAfterPush = delivcli.SpawnSegmentCollectorAfterPush
	}

	return container{
		hookHandler: hookHdl,
		cliHandler: struct {
			Run func(*delivcli.Container, []string) error
		}{Run: delivcli.Run},
		clictr: clictr,
	}
}

// parseHookFlags extracts the --provider / --event values after PreflightArgs
// has validated the hook invocation.
func parseHookFlags(args []string) (domain.ProviderKind, string) {
	var provider domain.ProviderKind
	var event string
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--provider":
			provider = args[i+1]
		case "--event":
			event = args[i+1]
		}
	}
	return provider, event
}
