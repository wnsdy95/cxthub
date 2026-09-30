// Package main is the entry point for the cxt client binary.
//
// It acts as a composition root, branching to mcp/hook/CLI entry points based on arguments.
// It is the sole point for creating and wiring adapters and injecting use-case services (DI).
// (domain model Rule 5: cmd/cxt can import everything. DI is only here.)
//
// cxt is client-specific: the central server (serve) has its own backend module,
// and this binary acts as a negotiator (adapters/backendclient) syncing with that server via REST (push/pull).
//
// Subcommands (domain model, client-specific):
//
//	cxt mcp --local              → start the offline-development stdio MCP helper
//	cxt hook --provider X --event Y → hook event handler (auto capture)
//	cxt init|repo|save|list|fork|checkout|load|memorize|memory|push|pull → user CLI commands
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capturejournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	delivhook "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/hook"
	delivmcp "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	adaptersession "github.com/wnsdy95/cxthub/cli/internal/adapters/session"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/sessionnotice"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
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
	// Help and usage errors must return before adapter construction: some
	// commands materialize provider sessions or contact the remote immediately.
	if handled, err := delivcli.PreflightArgs(args); handled || err != nil {
		return err
	}
	if args[1] == "version" || args[1] == "--version" {
		fmt.Println("cxt", version)
		return nil
	}
	if args[1] == "sync" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		return delivcli.RunSyncStatus(context.Background(), cwd, slices.Contains(args[2:], "--json"), os.Stdout)
	}
	if args[1] == "doctor" || (args[1] == "branch" && len(args) > 2 && args[2] == "operations") {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return delivcli.RunDiagnostics(ctx, cwd, args[1:], os.Stdout, inspectCaptureRecovery)
	}
	if args[1] == "capture" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		ctx := context.Background()
		state := gitctx.InspectContextRoot(ctx, cwd)
		if !state.Initialized {
			return fmt.Errorf("no initialized local replica; run cxt doctor")
		}
		store := storage.NewFileStore(state.Root)
		repo, err := remotecfg.Wrap(state.Root, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
		if err != nil {
			return err
		}
		service := app.NewCaptureRecoveryService(capturejournal.New(state.Root, cwd), store)
		c := &delivcli.Container{CaptureRecovery: service, History: app.NewContextHistoryService(store, store), List: app.NewListSessionsService(store)}
		return delivcli.RunCaptureRecovery(ctx, c, cwd, string(repo.ID), args[2:], os.Stdout)
	}
	if args[1] == "repair" && slices.Contains(args[2:], "--from-server") {
		return runRepair(args[2:])
	}
	// Process-level configuration resolves the shared repo root and environment
	// overrides. Persisted remotes and authentication are loaded lazily by their
	// adapters in buildContainer.
	cfg := loadConfig()

	// composition root: creates and wires all adapters and services.
	var ctr container
	if delivcli.ReadOnlyInvocation(args) && args[1] != "mcp" {
		ctr = buildReadContainer(cfg)
	} else {
		ctr = buildContainer(cfg)
	}

	// subcommand branching
	if len(args) < 2 {
		return ctr.cliHandler.Run(ctr.clictr, args)
	}
	switch args[1] {
	case "mcp":
		// PreflightArgs requires --local before adapter construction. The product
		// MCP is the OAuth-protected independent cxt-mcp endpoint, not this process.
		return ctr.mcpServer.Run()
	case "hook":
		// hook safety contract (capture path): capture failures must not block agent sessions —
		// errors should only be reported to stderr and always exit 0.
		provider, event := parseHookFlags(args[2:])
		if err := ctr.hookHandler.Run(provider, event); err != nil {
			fmt.Fprintf(os.Stderr, "cxt hook: %v\n", err)
		}
		return nil
	default:
		return ctr.cliHandler.Run(ctr.clictr, args)
	}
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
	mcpServer   *delivmcp.Server
	hookHandler *delivhook.Handler
	cliHandler  struct {
		Run func(*delivcli.Container, []string) error
	}
	clictr *delivcli.Container
}

// buildRepositoryAdapters constructs passive repository readers and a lazy client.
// No registration, capture discovery, replay or provider initialization occurs here.
func buildRepositoryAdapters(cfg config) (*storage.FileStore, *backendclient.BackendClient, outbound.GitContext) {
	// --- driven adapters (outbound implementations) ---
	// Local store: repo root .cxt/ content-addressed file store (client-only).
	store := storage.NewWorktreeFileStore(cfg.RepoRoot, cfg.GitDir, cfg.GitBranch, cfg.GitCommit)
	if cacheDir, err := os.UserCacheDir(); err == nil {
		store.EnableDocVerificationCache(filepath.Join(cacheDir, "cxthub", "doc-verification", "key-v1"))
	}
	// Remote sync: central server REST client (net/http stdlib). Server role is backend module.
	// Like git, origin remote URL is the destination — server address is derived from URL at request time.
	// (Immediate registration after remote add also works), otherwise CXT_REMOTE env fallback.
	endpoint := func() string {
		if origin, ok := remotecfg.Origin(cfg.RepoRoot); ok {
			if base, err := remotecfg.APIBase(origin); err == nil {
				return base
			}
		}
		return cfg.RemoteEndpoint
	}
	// Token also lazy interpreted: CXT_TOKEN(CI) > ~/.cxt/auth.json[host] (saved by cxt login).
	token := func() string {
		if cfg.RemoteToken != "" {
			return cfg.RemoteToken
		}
		if u, err := url.Parse(endpoint()); err == nil && u.Host != "" {
			return authcfg.Token(u.Host)
		}
		return ""
	}
	remote := backendclient.NewBackendClient(endpoint, token, cfg.Identity)
	// Pull delta: Injects local chunk store, avoiding download of existing chunks.
	remote.SetChunkLocal(store)
	return store, remote, remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter())
}

// buildReadContainer deliberately omits command-side services. Read-only
// dispatch cannot reach capture, checkout, sync mutation or branch replay.
func buildReadContainer(cfg config) container {
	store, remote, gitCtx := buildRepositoryAdapters(cfg)
	history := app.NewHistoryQueryService(gitCtx, gitctx.NewGitContextAdapter(), store, remote)
	working := app.NewWorkingStateService(gitCtx, gitctx.NewGitContextAdapter(), store, store, history).WithAppliedPullReader(store, remote.SyncRemoteIdentity())
	c := &delivcli.Container{
		ResolveRepo:  gitCtx.CurrentRepo,
		List:         app.NewListSessionsService(store),
		Queries:      app.NewLocalRefQueryService(gitCtx, store),
		HistoryQuery: history,
		WorkingState: working,
		ContextDiff:  working,
		CodePosition: gitctx.NewGitContextAdapter(),
		ListIndexStashes: func(ctx context.Context, cwd string) ([]domain.StagingStash, error) {
			repo, err := gitCtx.CurrentRepo(ctx, cwd)
			if err != nil {
				return nil, err
			}
			return store.ListIndexStashes(ctx, repo.ID)
		},
		Settings: remote,
	}
	out := container{clictr: c}
	out.cliHandler.Run = delivcli.Run
	return out
}

// buildContainer creates all adapters/services and injects dependencies.
// The read-only composition above uses the same passive repository adapters.
func buildContainer(cfg config) container {
	store, remote, gitCtx := buildRepositoryAdapters(cfg)
	// Masking policy loader injection (capture ← remotecfg circular prevention — DI here only).
	capture.LoadScrubOptions = func(repoRoot string) capture.ScrubOptions {
		return capture.ScrubOptions{
			Redact: remotecfg.SecretsRedact(repoRoot),
			MinLen: remotecfg.SecretsMinLen(repoRoot),
			Tier:   capture.ScrubTier(remotecfg.SecretsScrub(repoRoot)),
		}
	}
	// Repository identity: an origin URL reanchors the local store to its URL-derived RepoID,
	// so clients configured with the same origin address the same repository.
	claudeCodec := codec.NewClaudeCodec()
	codexCodec := codec.NewCodexCodec()
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{
		domain.ProviderClaude: claudeCodec,
		domain.ProviderCodex:  codexCodec,
	}
	claudeCap := capture.NewClaudeCapture()
	codexCap := capture.NewCodexCapture()
	captures := map[domain.ProviderKind]outbound.CaptureSource{
		domain.ProviderClaude: claudeCap,
		domain.ProviderCodex:  codexCap,
	}

	// Memory adapter (compatibility rules): MemorySource/MemorySink registry + self-distillation MemoryDistiller.
	memSources := map[domain.ProviderKind]outbound.MemorySource{
		domain.ProviderClaude: memory.NewClaudeMemorySource(),
		domain.ProviderCodex:  memory.NewCodexMemorySource(),
	}
	memSinks := map[domain.ProviderKind]outbound.MemorySink{
		domain.ProviderClaude: memory.NewClaudeMemorySink(),
		domain.ProviderCodex:  memory.NewCodexMemorySink(),
	}
	var distiller outbound.MemoryDistiller = memory.NewRuleDistiller()

	// Session materializer(compatibility rules): full-context recovery native resume synthesis.
	materializers := map[domain.ProviderKind]outbound.SessionMaterializer{
		domain.ProviderClaude: adaptersession.NewClaudeMaterializer(),
		domain.ProviderCodex:  adaptersession.NewCodexMaterializer(),
	}

	// --- use-case services (inbound implementation, outbound injection) ---
	initSvc := app.NewInitRepoService(gitCtx, store)
	saveSvc := app.NewSaveSessionService(gitCtx, captures, codecs, store, capture.NewSessionCapture(store), storage.NewSyncOutbox())
	stagingSvc := app.NewStagingService(gitCtx, gitctx.NewGitContextAdapter(), store, store, captures, codecs, capture.NewSessionCapture(store), storage.NewSyncOutbox())
	forkSvc := app.NewForkSessionService(store)
	branchLifecycleSvc := app.NewBranchLifecycleService(gitCtx, store)
	prompts := app.NewMemoryPromptService(remote, gitctx.NewGitContextAdapter(), store)
	loadSvc := app.NewLoadSessionService(store, codecs, materializers, memSources, distiller, memSinks).WithMemoryPrompts(prompts)
	checkoutSvc := app.NewCheckoutSessionService(forkSvc, loadSvc, store).WithCodePosition(gitctx.NewGitContextAdapter())
	listSvc := app.NewListSessionsService(store)
	memorizeSvc := app.NewMemorizeService(gitCtx, captures, codecs, memSources, distiller, store)
	handoffSvc := app.NewBranchHandoffService(store)
	syncSvc := app.NewSyncRepoService(store, remote, gitCtx, storage.NewSyncOutbox())
	seedSvc := app.NewBranchSeedService(gitCtx, store, distiller, codecs, materializers, memSources).WithMemoryPrompts(prompts)
	tagSvc := app.NewTagService(gitCtx, store)
	stashSvc := app.NewStashService(gitCtx, captures, codecs, store, loadSvc, capture.NewSessionCapture(store))

	// --- capture coordinator ---
	coord := capture.NewCaptureCoordinator(saveSvc, cfg.Identity)

	// --- driving adapters (delivery; client-specific: mcp/hook/cli) ---
	// The explicit --local MCP helper is a read-only offline projection. The
	// product MCP runs in the independent cxt-mcp server against shared cloud storage.
	mcpSrv := delivmcp.NewServer(gitCtx, store, remote)
	notices := app.NewSessionNoticeService(sessionnotice.NewSelectionReader(cfg.RepoRoot, cfg.GitDir, store), store)
	hookHdl := delivhook.NewHandler(coord).WithLiveObservation().WithSessionNotices(notices)
	history := app.NewHistoryQueryService(gitCtx, gitctx.NewGitContextAdapter(), store, remote)
	working := app.NewWorkingStateService(gitCtx, gitctx.NewGitContextAdapter(), store, store, history).WithAppliedPullReader(store, remote.SyncRemoteIdentity())
	clictr := &delivcli.Container{
		ProviderLaunch:     providerLaunchHooks(cfg),
		WakeHistoricalSync: delivcli.SpawnHistoricalSync,
		ResolveRepo:        gitCtx.CurrentRepo,
		Queries:            app.NewLocalRefQueryService(gitCtx, store),
		HistoryQuery:       history,
		WorkingState:       working,
		ContextDiff:        working,
		CodePosition:       gitctx.NewGitContextAdapter(),
		Staging:            stagingSvc,
		IndexStash:         stagingSvc,
		ListIndexStashes:   stagingSvc.ListIndexStashes,
		ResolveConnection: func(ctx context.Context, raw string) (domain.RepositoryConnection, error) {
			base, err := remotecfg.APIBase(raw)
			if err != nil {
				return domain.RepositoryConnection{}, err
			}
			client := backendclient.NewBackendClient(func() string { return base }, func() string {
				return tokenForSyncDestination(cfg, base)
			}, cfg.Identity)
			return client.ResolveRepositoryConnection(ctx, raw)
		},
		Init:            initSvc,
		Save:            saveSvc,
		Fork:            forkSvc,
		Branches:        branchLifecycleSvc,
		Checkout:        checkoutSvc,
		Load:            loadSvc,
		List:            listSvc,
		Memorize:        memorizeSvc,
		Sync:            syncSvc,
		Seed:            seedSvc,
		Tag:             tagSvc,
		Stash:           stashSvc,
		Handoff:         handoffSvc,
		History:         app.NewContextHistoryService(store, store),
		CaptureRecovery: app.NewCaptureRecoveryService(capturejournal.New(cfg.RepoRoot, cfg.RepoRoot), store),
		PRMerges:        gitctx.NewGitHubPRMergeResolver(),
		Settings:        remote,
		SettingsObjects: store,
		Repack:          store.RepackObjects,
		Identity:        cfg.Identity,
	}
	preparer := runtimeAgentPreparer{gitCtx, store, remote, history}
	loadSvc.WithAgentContext(preparer).WithAgentCodePosition(gitctx.NewGitContextAdapter())
	seedSvc.WithAgentContext(preparer)
	handoffSvc.WithAgentContext(preparer, gitctx.NewGitContextAdapter())
	clictr.PrepareAgent = preparer
	clictr.ApplySelectedPull = selectedPullApplication(store, remote, gitCtx)
	clictr.ResolveSyncDestination = namedSyncDestination(cfg, store, gitCtx)
	remoteRepair := app.NewRemoteRepairService(syncSvc, store, gitctx.NewGitContextAdapter())
	clictr.PreviewRemoteRepair = func(ctx context.Context, cwd, ref string, snapshot domain.ContentHash, reason string) (outbound.RemoteRepairPlan, error) {
		in := app.RemoteRepairInput{Cwd: cwd, Reason: reason, Snapshot: snapshot, RefName: ref}
		if ref != "" {
			in.RefKind = domain.RefBranch
		}
		return remoteRepair.Preview(ctx, in)
	}
	clictr.ApplyRemoteRepair = remoteRepair.Apply

	return container{
		mcpServer:   mcpSrv,
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

func inspectCaptureRecovery(ctx context.Context, cwd string) ([]domain.CaptureRecoveryStatus, error) {
	state := gitctx.InspectContextRoot(ctx, cwd)
	if !state.Initialized {
		return nil, nil
	}
	repo, err := remotecfg.Wrap(state.Root, gitctx.NewGitContextAdapter()).CurrentRepo(ctx, cwd)
	if err != nil {
		return nil, err
	}
	service := app.NewCaptureRecoveryService(capturejournal.New(state.Root, cwd), storage.NewCaptureEvidenceReader(state.Root))
	return service.Inspect(ctx, string(repo.ID))
}
