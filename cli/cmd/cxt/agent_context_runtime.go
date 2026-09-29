package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	sessionadapter "github.com/wnsdy95/cxthub/cli/internal/adapters/session"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type cloudAgentDocuments struct {
	remote *backendclient.BackendClient
	repo   string
}

func (r cloudAgentDocuments) GetDoc(ctx context.Context, hash domain.ContentHash) (domain.SessionDoc, error) {
	return r.remote.FetchAgentDocument(ctx, r.repo, hash)
}

func (r cloudAgentDocuments) ReadAgentHistoryPage(ctx context.Context, hash domain.ContentHash, req domain.AgentHistoryPageRequest) (domain.AgentHistoryPage, error) {
	return r.remote.ReadAgentHistoryPage(ctx, r.repo, hash, req)
}

// No model window override can supply missing tokenizer/host evidence. Explicit
// history remains inspectable with load --output while native delivery fails.
type unverifiedAgentHost struct{}

func (unverifiedAgentHost) AgentCapability(context.Context, domain.ProviderKind, string) (domain.AgentHostCapability, error) {
	return domain.AgentHostCapability{}, fmt.Errorf("%w: this build has no verified host/tokenizer combination for strict history; use cxt load --context-budget <budget> --output <file> to prepare an inspectable artifact", domain.ErrProviderCapabilityUnknown)
}

type runtimeAgentPreparer struct {
	git     outbound.GitContext
	store   *storage.FileStore
	remote  *backendclient.BackendClient
	history inbound.HistoryQuery
}

func (r runtimeAgentPreparer) PrepareAgentContext(ctx context.Context, in inbound.PrepareAgentContextInput) (domain.AgentContextPackage, error) {
	repo, err := r.git.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return domain.AgentContextPackage{}, err
	}
	if in.RepoID != "" && in.RepoID != repo.ID {
		return domain.AgentContextPackage{}, domain.ErrHashMismatch
	}
	in.RepoID = repo.ID
	in, err = r.selectAgentContext(ctx, in)
	if err != nil {
		return domain.AgentContextPackage{}, err
	}
	work, scope, err := importPersonalWork(ctx, r.remote, repo.ID, in.Cwd, in.WorkStatePath, in.PersonalScope)
	if err != nil {
		return domain.AgentContextPackage{}, err
	}
	in.PersonalScope = scope
	service := app.NewAgentContextService(r.history, r.remote, cloudAgentDocuments{r.remote, repo.ID}, work, app.ConservativeAgentTokenCounter{}, unverifiedAgentHost{})
	p, err := service.PrepareAgentContext(ctx, in)
	if err != nil {
		return domain.AgentContextPackage{}, err
	}
	if err := r.validateAgentWorktree(ctx, in.Cwd, in.RepoID, in.WorktreeStateHash); err != nil {
		return domain.AgentContextPackage{}, err
	}
	return p, nil
}

func runtimeConfigAt(ctx context.Context, base config, cwd string) (config, error) {
	roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd)
	if err != nil {
		return base, err
	}
	base.RepoRoot = roots.SharedRoot
	base.GitDir = configGitValue(cwd, "rev-parse", "--absolute-git-dir")
	base.GitBranch = configGitValue(cwd, "symbolic-ref", "--short", "HEAD")
	base.GitCommit = configGitValue(cwd, "rev-parse", "HEAD")
	return base, nil
}
func runtimeAgentLoader(cfg config) (runtimeAgentPreparer, *app.LoadSessionService) {
	store, remote, git := buildRepositoryAdapters(cfg)
	history := app.NewHistoryQueryService(git, gitctx.NewGitContextAdapter(), store, remote)
	p := runtimeAgentPreparer{git, store, remote, history}
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec(), domain.ProviderClaude: codec.NewClaudeCodec()}
	materializers := map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: sessionadapter.NewCodexMaterializer(), domain.ProviderClaude: sessionadapter.NewClaudeMaterializer()}
	loader := app.NewLoadSessionService(store, codecs, materializers, nil, nil, nil).WithAgentContext(p).WithAgentCodePosition(gitctx.NewGitContextAdapter())
	return p, loader
}
func providerLaunchHooks(base config) delivcli.ProviderLaunchHooks {
	// Each preparation rebinds --cd and branch transitions to their own worktree.
	// The supervisor invokes callbacks serially; receipts use that exact root.
	receiptRoot := base.RepoRoot
	return delivcli.ProviderLaunchHooks{
		Prepare: func(ctx context.Context, req delivcli.ProviderLaunchRequest) (delivcli.PreparedProviderLaunch, error) {
			var result delivcli.PreparedProviderLaunch
			cfg, err := runtimeConfigAt(ctx, base, req.Cwd)
			if err != nil {
				return result, err
			}
			receiptRoot = cfg.RepoRoot
			details, err := req.ArgumentDetails()
			if err != nil {
				return result, err
			}
			policy := domain.MemoryInputPolicy()
			if req.Intent.Pull {
				policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: req.Intent.ContextBudget, Source: "explicit_cli"}
			}
			preparer, loader := runtimeAgentLoader(cfg)
			if bootstrap, handled, err := preparer.prepareEmptyBootstrap(ctx, cfg, req); handled {
				return bootstrap, err
			}
			p, out, err := loader.PrepareAgentDelivery(ctx, inbound.PrepareAgentContextInput{Cwd: req.Cwd, Provider: req.Intent.Provider, Model: details.Model, Policy: policy, WorkStatePath: req.Intent.WorkStatePath})
			if err != nil {
				return result, err
			}
			id := providerfs.SessionIDFromPath(out.WrittenPath)
			if !providerfs.ValidSessionID(id) {
				return result, fmt.Errorf("invalid materialized session identity")
			}
			args, err := req.ResumeArguments(id)
			if err != nil {
				return result, err
			}
			raw, err := p.Artifact()
			if err != nil {
				return result, err
			}
			if err = providerfs.WriteRepoFileDurable(cfg.RepoRoot, filepath.Join(".cxt", "input-packages", strings.TrimPrefix(string(p.ID), "sha256:")+".json"), raw, 0600); err != nil {
				return result, err
			}
			measurement := "conservative_bound"
			if p.Usage.Exact {
				measurement = "exact"
			}
			return delivcli.PreparedProviderLaunch{Args: args, SessionID: id, PackageHash: p.ID, CodeCommit: p.Content.Selection.CodeCommit, SourceRevision: string(p.Content.Selection.ContextStateHash), SelectedTokens: p.Usage.Tokens, TokenMeasurement: measurement, Capability: p.Capability,
				Validate: func(ctx context.Context) error {
					return preparer.validateAgentDelivery(ctx, req.Cwd, p.Content.Selection)
				},
			}, nil
		},
		Record: func(ctx context.Context, receipt delivcli.ProviderLaunchReceipt) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			raw, err := json.Marshal(struct {
				At      time.Time                      `json:"at"`
				Receipt delivcli.ProviderLaunchReceipt `json:"receipt"`
			}{time.Now().UTC(), receipt})
			if err != nil {
				return err
			}
			id := strings.TrimPrefix(string(domain.HashContent(raw)), "sha256:")
			return providerfs.WriteRepoFileDurable(receiptRoot, filepath.Join(".cxt", "delivery-receipts", id+".json"), raw, 0600)
		},
	}
}
