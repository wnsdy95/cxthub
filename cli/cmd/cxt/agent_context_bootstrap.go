package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/codec"
	delivcli "github.com/wnsdy95/cxthub/cli/internal/adapters/delivery/cli"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	sessionadapter "github.com/wnsdy95/cxthub/cli/internal/adapters/session"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type bootstrapGitPosition struct {
	Branch, Commit, GitDir string
	Unborn                 bool
}

// bootstrapGit reads the specified worktree, ignoring a parent hook's exported
// repository selectors. A failed rev-parse alone is never proof of unborn HEAD.
func bootstrapGit(ctx context.Context, cwd string) (bootstrapGitPosition, error) {
	var p bootstrapGitPosition
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			switch name {
			case "GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_NAMESPACE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		out, err := cmd.Output()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return strings.TrimSpace(string(out)), err
	}
	var err error
	p.GitDir, err = run("rev-parse", "--absolute-git-dir")
	if err != nil {
		return p, err
	}
	ref, err := run("symbolic-ref", "--quiet", "HEAD")
	if err != nil || !strings.HasPrefix(ref, "refs/heads/") {
		return p, fmt.Errorf("%w: empty bootstrap requires a Git branch", domain.ErrAgentContextUnavailable)
	}
	p.Branch = strings.TrimPrefix(ref, "refs/heads/")
	if err := domain.ValidateBranchName(p.Branch); err != nil {
		return p, err
	}
	p.Commit, err = run("rev-parse", "--verify", "HEAD^{commit}")
	if err == nil {
		if !domain.ValidGitOID(p.Commit) {
			return p, domain.ErrHashMismatch
		}
		return p, nil
	}
	if ctx.Err() != nil {
		return p, ctx.Err()
	}
	// Missing symbolic branch ref (exit 1) distinguishes unborn from a corrupt
	// object/ref, permission error, detached HEAD or an arbitrary command failure.
	_, missing := run("show-ref", "--verify", "--quiet", ref)
	var exit *exec.ExitError
	if !errors.As(missing, &exit) || exit.ExitCode() != 1 {
		return p, err
	}
	p.Commit, p.Unborn = "", true
	return p, nil
}

// Only a pristine local context replica is eligible. Existing/pinned/staged
// context follows the normal selection path; it cannot downgrade to bootstrap.
func (r runtimeAgentPreparer) bootstrapLocal(ctx context.Context, cfg config, repo string) (domain.ContentHash, bool, error) {
	state, err := r.store.ReadCheckoutState(ctx, repo)
	if err != nil {
		return "", false, err
	}
	if p := state.Position; p != nil && (p.Snapshot != "" || p.SharedTarget != "" || p.MemoryHash != "" || p.MemorySource != "" || p.MemoryPinned || p.Selection != nil || p.Rewound || p.Orphan) {
		return "", false, nil
	}
	refs, err := r.store.ListRefs(ctx, repo)
	if err != nil {
		return "", false, err
	}
	for _, ref := range refs {
		if ref.Kind != domain.RefHEAD || ref.Target != "" {
			return "", false, nil
		}
	}
	snaps, err := r.store.ListSnapshots(ctx, repo, "")
	if err != nil || len(snaps) != 0 {
		return "", false, err
	}
	// ListHistoryEvents recovers journals under a write lock. Bootstrap only
	// needs absence, so inspect the history directory without replaying writes.
	historyPath := filepath.Join(cfg.RepoRoot, ".cxt", "history")
	if info, err := os.Lstat(historyPath); err == nil {
		if !info.IsDir() {
			return "", false, domain.ErrHashMismatch
		}
		entries, err := os.ReadDir(historyPath)
		if err != nil || len(entries) != 0 {
			return "", false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	pending, err := r.store.ListPendings(ctx, repo)
	if err != nil || len(pending) != 0 {
		return "", false, err
	}
	index, _, err := r.store.ReadStaging(ctx, repo)
	if err != nil || len(index.Entries) != 0 || index.Sequence != 0 {
		return "", false, err
	}
	// A first-run cursor is the only persisted worktree state allowed. This
	// also refuses pins/index stashes in another worktree without reconstructing
	// or recovering their journals. Unknown records and symlinks fail closed.
	worktrees := filepath.Join(cfg.RepoRoot, ".cxt", "worktrees")
	allowed := filepath.Join(worktrees, index.WorktreeID, "position.json")
	pinned := false
	err = filepath.WalkDir(worktrees, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == worktrees {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return domain.ErrHashMismatch
		}
		if !entry.IsDir() && path != allowed {
			pinned = true
		}
		return ctx.Err()
	})
	if err != nil || pinned {
		return "", false, err
	}
	for _, name := range []string{"checkout-transition.json", "working-commit.json"} {
		if _, err := os.Lstat(filepath.Join(cfg.RepoRoot, ".cxt", name)); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return "", false, err
			}
			return "", false, fmt.Errorf("%w: unfinished local operation prevents empty bootstrap", domain.ErrAgentContextUnavailable)
		}
	}
	raw, err := json.Marshal(state)
	return domain.HashContent(raw), true, err
}

// prepareEmptyBootstrap is invoked only by the implicit default fresh launch
// hook, never by load/checkout, explicit history, handoff or wrapper transitions.
func (r runtimeAgentPreparer) prepareEmptyBootstrap(ctx context.Context, cfg config, req delivcli.ProviderLaunchRequest) (delivcli.PreparedProviderLaunch, bool, error) {
	var result delivcli.PreparedProviderLaunch
	if req.Intent.Pull || req.Intent.ContextBudget != 0 || req.Intent.WorkStatePath != "" || req.Transition != nil {
		return result, false, nil
	}
	details, err := req.ArgumentDetails()
	if err != nil {
		return result, true, err
	}
	if details.Mode != "fresh interactive session" {
		return result, false, nil
	}
	repo, err := r.git.CurrentRepo(ctx, req.Cwd)
	if err != nil {
		return result, true, err
	}
	local, eligible, err := r.bootstrapLocal(ctx, cfg, repo.ID)
	if err != nil || !eligible {
		return result, eligible || err != nil, err
	}
	server, err := r.remote.VerifyEmptyRepository(ctx, repo.ID)
	if errors.Is(err, domain.ErrRepositoryHasContext) {
		return result, false, nil // Continue with the authorized latest-main path.
	}
	if err != nil {
		return result, true, err
	}
	git, err := bootstrapGit(ctx, req.Cwd)
	if err != nil {
		return result, true, err
	}
	if filepath.Clean(git.GitDir) != filepath.Clean(cfg.GitDir) {
		return result, true, domain.ErrSelectionChanged
	}
	proof := domain.AgentBootstrapProof{Server: server, Branch: git.Branch, CodeCommit: git.Commit, Unborn: git.Unborn, WorktreeStateHash: local}
	if err := proof.Validate(); err != nil {
		return result, true, err
	}
	validateLocal := func(ctx context.Context) error {
		currentRepo, err := r.git.CurrentRepo(ctx, req.Cwd)
		if err != nil {
			return err
		}
		if currentRepo.ID != repo.ID {
			return domain.ErrSelectionChanged
		}
		currentGit, err := bootstrapGit(ctx, req.Cwd)
		if err != nil {
			return err
		}
		if currentGit != git {
			return domain.ErrCodePositionMismatch
		}
		current, empty, err := r.bootstrapLocal(ctx, cfg, repo.ID)
		if err != nil {
			return err
		}
		if !empty || current != local {
			return domain.ErrSelectionChanged
		}
		return nil
	}
	validate := func(ctx context.Context) error {
		if err := validateLocal(ctx); err != nil {
			return err
		}
		current, err := r.remote.VerifyEmptyRepository(ctx, repo.ID)
		if err != nil {
			return err
		}
		if current != server {
			return domain.ErrSelectionChanged
		}
		return validateLocal(ctx) // Git/worktree can move during the authorized read.
	}
	// Bootstrap is a distinct artifact, with no invented snapshot or memory root.
	input := struct {
		Kind   string                     `json:"kind"`
		Notice string                     `json:"notice"`
		Proof  domain.AgentBootstrapProof `json:"proof"`
	}{"verified_empty_repository", "The server-authorized repository catalog contains no snapshots or context history at this revision. No prior project memory or personal work was loaded. This starts the first conversation; server state may change after verification.", proof}
	content, err := json.Marshal(input)
	if err != nil {
		return result, true, err
	}
	prompt := "[cxt context package v1]\n" + string(content)
	if len([]byte(prompt)) > domain.DefaultMemoryContextTokens {
		return result, true, domain.ErrContextBudgetExceeded
	}
	artifact := struct {
		Version  int                        `json:"version"`
		Kind     string                     `json:"kind"`
		Policy   domain.InputPolicy         `json:"policy"`
		Proof    domain.AgentBootstrapProof `json:"proof"`
		Prompt   string                     `json:"prompt"`
		Usage    domain.AgentTokenUsage     `json:"usage"`
		Delivery string                     `json:"delivery"`
	}{1, input.Kind, domain.MemoryInputPolicy(), proof, prompt, domain.AgentTokenUsage{Tokens: len([]byte(prompt)), Tokenizer: domain.UTF8ByteBoundCounter}, "prepared"}
	raw, err := json.Marshal(artifact)
	if err != nil {
		return result, true, err
	}
	hash := domain.HashContent(raw)
	if err := validate(ctx); err != nil {
		return result, true, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := domain.NewSessionID()
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: req.Intent.Provider, SessionOriginID: id, Cwd: req.Cwd, GitBranch: git.Branch, CapturedAt: now, Fidelity: domain.FidelityMemory}, Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Seq: 0, Ts: now, Blocks: []domain.ContentBlock{{Type: "text", Text: prompt}}}}}
	var encoder outbound.ProviderCodec
	var materializer outbound.SessionMaterializer
	switch req.Intent.Provider {
	case domain.ProviderClaude:
		encoder, materializer = codec.NewClaudeCodec(), sessionadapter.NewClaudeMaterializer()
	case domain.ProviderCodex:
		encoder, materializer = codec.NewCodexCodec(), sessionadapter.NewCodexMaterializer()
	default:
		return result, true, domain.ErrUnsupportedProvider
	}
	native, err := encoder.Encode(ctx, cir, req.Intent.Provider)
	if err != nil {
		return result, true, err
	}
	if err := validateLocal(ctx); err != nil {
		return result, true, err
	}
	path, resume, err := materializer.Materialize(ctx, native, req.Cwd)
	if err != nil {
		return result, true, fmt.Errorf("%w: bootstrap materialization: %w", domain.ErrDeliveryFailed, err)
	}
	id = providerfs.SessionIDFromPath(path)
	if path == "" || resume == "" || !providerfs.ValidSessionID(id) {
		return result, true, domain.ErrDeliveryFailed
	}
	args, err := req.ResumeArguments(id)
	if err != nil {
		return result, true, err
	}
	if err := providerfs.WriteRepoFileDurable(cfg.RepoRoot, filepath.Join(".cxt", "input-packages", strings.TrimPrefix(string(hash), "sha256:")+".json"), raw, 0600); err != nil {
		return result, true, err
	}
	return delivcli.PreparedProviderLaunch{Args: args, SessionID: id, PackageHash: hash, CodeCommit: git.Commit, SourceRevision: string(server.StateHash), SelectedTokens: artifact.Usage.Tokens, TokenMeasurement: "conservative_bound", Capability: "verified_empty_repository", Bootstrap: &proof, Validate: validate}, true, nil
}
