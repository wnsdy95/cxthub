package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/agenttokens"
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

// Materialized delivery has no native window authority. The Codex deferred
// composition supplies a scoped reader; unsupported routes remain inspectable
// with load --output without claiming provider acceptance.
type unverifiedAgentHost struct{}

func (unverifiedAgentHost) AgentCapability(context.Context, domain.ProviderKind, string) (domain.AgentHostCapability, error) {
	return domain.AgentHostCapability{}, fmt.Errorf("%w: this delivery route has no verified native model/window and tokenizer binding for history; use cxt load --context-budget <budget> --output <file> to prepare an inspectable artifact", domain.ErrProviderCapabilityUnknown)
}

var runtimeAgentTokens = agenttokens.New()

type runtimeAgentPreparer struct {
	git          outbound.GitContext
	store        *storage.FileStore
	remote       *backendclient.BackendClient
	history      inbound.HistoryQuery
	capabilities outbound.AgentCapabilityReader
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
	if in.Provider == "" && in.LatestMain {
		view, err := r.history.QueryHistory(ctx, inbound.HistoryQueryInput{Cwd: in.Cwd, Server: true, ServerTip: true, Branch: "main"})
		if err != nil {
			return domain.AgentContextPackage{}, err
		}
		for _, snapshot := range view.Snapshots {
			if snapshot.ID == view.Position {
				in.Provider = snapshot.Provider
				break
			}
		}
		if in.Provider != domain.ProviderCodex && in.Provider != domain.ProviderClaude {
			return domain.AgentContextPackage{}, fmt.Errorf("%w: main has no supported default provider; select --provider claude or codex", domain.ErrUnsupportedProvider)
		}
	}
	work, scope, err := importPersonalWork(ctx, r.remote, repo.ID, in.Cwd, in.WorkStatePath, in.PersonalScope)
	if err != nil {
		return domain.AgentContextPackage{}, err
	}
	in.PersonalScope = scope
	capabilities := r.capabilities
	if capabilities == nil {
		capabilities = unverifiedAgentHost{}
	}
	service := app.NewAgentContextService(r.history, r.remote, cloudAgentDocuments{r.remote, repo.ID}, work, runtimeAgentTokens, app.MeasuredAgentCapabilities{Runtime: capabilities, Observations: r.store})
	p, err := service.PrepareAgentContext(ctx, in)
	if err != nil {
		return domain.AgentContextPackage{}, err
	}
	if err := r.validateAgentWorktree(ctx, in.Cwd, in.RepoID, in.WorktreeStateHash); err != nil {
		return domain.AgentContextPackage{}, err
	}
	if in.WorkingPosition != nil {
		code := r.git.(outbound.CodePosition) // selectAgentContext already verified this port.
		actual, err := code.CurrentCommit(ctx, in.Cwd)
		if err != nil {
			return domain.AgentContextPackage{}, err
		}
		if actual != in.WorkingPosition.CodeCommit {
			return domain.AgentContextPackage{}, domain.ErrCodePositionMismatch
		}
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
	p := runtimeAgentPreparer{git: git, store: store, remote: remote, history: history}
	codecs := map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderCodex: codec.NewCodexCodec(), domain.ProviderClaude: codec.NewClaudeCodec()}
	materializers := map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderCodex: sessionadapter.NewCodexMaterializer(), domain.ProviderClaude: sessionadapter.NewClaudeMaterializer()}
	loader := app.NewLoadSessionService(store, codecs, materializers, nil, nil, nil).WithAgentContext(p).WithAgentCodePosition(gitctx.NewGitContextAdapter())
	return p, loader
}
func providerLaunchHooks(base config) delivcli.ProviderLaunchHooks {
	// Each preparation rebinds --cd and branch transitions to their own worktree.
	// Lifecycle records use that exact root; native input records retain their
	// own immutable root while a replacement runtime is prepared.
	receiptRoot := base.RepoRoot
	var receiptMu sync.Mutex
	return delivcli.ProviderLaunchHooks{
		PrepareDeferred: func(ctx context.Context, req delivcli.ProviderLaunchRequest) (delivcli.DeferredProviderLaunch, error) {
			cfg, err := runtimeConfigAt(ctx, base, req.Cwd)
			if err != nil {
				return delivcli.DeferredProviderLaunch{}, err
			}
			receiptMu.Lock()
			receiptRoot = cfg.RepoRoot
			receiptMu.Unlock()
			return prepareNativeCodexDeferred(ctx, cfg, req)
		},
		Prepare: func(ctx context.Context, req delivcli.ProviderLaunchRequest) (delivcli.PreparedProviderLaunch, error) {
			var result delivcli.PreparedProviderLaunch
			cfg, err := runtimeConfigAt(ctx, base, req.Cwd)
			if err != nil {
				return result, err
			}
			receiptMu.Lock()
			receiptRoot = cfg.RepoRoot
			receiptMu.Unlock()
			details, err := req.ArgumentDetails()
			if err != nil {
				return result, err
			}
			var initialPrompt domain.AgentInitialPrompt
			policy := domain.MemoryInputPolicy()
			if req.Intent.Pull {
				initialPrompt, err = req.InitialPrompt()
				if err != nil {
					return result, err
				}
				policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: req.Intent.ContextBudget, Source: "explicit_cli"}
			}
			preparer, loader := runtimeAgentLoader(cfg)
			if bootstrap, handled, err := preparer.prepareEmptyBootstrap(ctx, cfg, req); handled {
				return bootstrap, err
			}
			p, out, err := loader.PrepareAgentDelivery(ctx, inbound.PrepareAgentContextInput{Cwd: req.Cwd, Provider: req.Intent.Provider, Model: details.Model, Policy: policy, WorkStatePath: req.Intent.WorkStatePath, InitialPrompt: initialPrompt})
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
			var budget *domain.AgentContextBudget
			if p.Budget != nil {
				copy := *p.Budget
				budget = &copy
			}
			return delivcli.PreparedProviderLaunch{Args: args, SessionID: id, PackageHash: p.ID, CodeCommit: p.Content.Selection.DeliveryCodeCommit(), SourceRevision: string(p.Content.Selection.ContextStateHash), SelectedTokens: p.Usage.Tokens, TokenMeasurement: measurement, Capability: p.Capability, Budget: budget, PromptReservation: p.InitialPromptReservation(),
				Validate: func(ctx context.Context) error {
					return preparer.validateAgentDelivery(ctx, req.Cwd, p.Content.Selection)
				},
			}, nil
		},
		Record: func(ctx context.Context, receipt delivcli.ProviderLaunchReceipt) error {
			receiptMu.Lock()
			root := receiptRoot
			receiptMu.Unlock()
			return recordProviderLaunchReceipt(ctx, root, receipt)
		},
	}
}

func recordProviderLaunchReceipt(ctx context.Context, root string, receipt delivcli.ProviderLaunchReceipt) error {
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
	return providerfs.WriteRepoFileDurable(root, filepath.Join(".cxt", "delivery-receipts", id+".json"), raw, 0600)
}
