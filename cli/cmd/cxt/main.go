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
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	delivhook "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/hook"
	delivmcp "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	adaptersession "github.com/wnsdy95/cxthub/cli/internal/adapters/session"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// version is injected by goreleaser as an ldflag during release builds (-X main.version=vX.Y.Z).
var version = "dev"

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "cxt: %v\n", err)
		os.Exit(1)
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
	// Process-level configuration resolves the shared repo root and environment
	// overrides. Persisted remotes and authentication are loaded lazily by their
	// adapters in buildContainer.
	cfg := loadConfig()

	// composition root: creates and wires all adapters and services.
	ctr := buildContainer(cfg)

	// subcommand branching
	if len(args) < 2 {
		return ctr.cliHandler.Run(ctr.clictr, args)
	}
	switch args[1] {
	case "mcp":
		// PreflightArgs requires --local before adapter construction. The product
		// MCP is the OAuth-protected remote cxtd endpoint, not this process.
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
	RepoRoot string
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
		RemoteEndpoint: os.Getenv("CXT_REMOTE"),
		RemoteToken:    os.Getenv("CXT_TOKEN"),
		Identity: domain.TeamIdentity{
			Name:  name,
			Email: email,
			Team:  os.Getenv("CXT_TEAM"),
		},
	}
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

// buildContainer creates all adapters/services and injects dependencies.
// DI is performed only within this function (domain model Rule 5).
func buildContainer(cfg config) container {
	// --- driven adapters (outbound implementations) ---
	// Local store: repo root .cxt/ content-addressed file store (client-only).
	store := storage.NewFileStore(cfg.RepoRoot)
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
	gitCtx := remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter())
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
	saveSvc := app.NewSaveSessionService(gitCtx, captures, codecs, store)
	forkSvc := app.NewForkSessionService(store)
	branchLifecycleSvc := app.NewBranchLifecycleService(gitCtx, store)
	loadSvc := app.NewLoadSessionService(store, codecs, materializers, memSources, distiller, memSinks)
	checkoutSvc := app.NewCheckoutSessionService(forkSvc, loadSvc, store)
	listSvc := app.NewListSessionsService(store)
	memorizeSvc := app.NewMemorizeService(gitCtx, captures, codecs, memSources, distiller, store)
	handoffSvc := app.NewBranchHandoffService(store)
	syncSvc := app.NewSyncRepoService(store, remote, gitCtx)
	seedSvc := app.NewBranchSeedService(gitCtx, store, distiller, codecs, materializers, memSources)
	tagSvc := app.NewTagService(gitCtx, store)
	stashSvc := app.NewStashService(gitCtx, captures, codecs, store, loadSvc)

	// --- capture coordinator ---
	coord := capture.NewCaptureCoordinator(saveSvc, cfg.Identity)

	// --- driving adapters (delivery; client-specific: mcp/hook/cli) ---
	// The explicit --local MCP helper is a read-only offline projection. The
	// product MCP runs remotely in cxtd against shared cloud storage.
	mcpSrv := delivmcp.NewServer(gitCtx, store, remote)
	hookHdl := delivhook.NewHandler(coord)
	clictr := &delivcli.Container{
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
		PRMerges:        gitctx.NewGitHubPRMergeResolver(),
		Settings:        remote,
		SettingsObjects: store,
		Repack:          store.RepackObjects,
		Identity:        cfg.Identity,
	}

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
