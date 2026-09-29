package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func runAgentArtifact(ctx context.Context, c *Container, cwd string, p parsedCommand) error {
	if c.PrepareAgent == nil || c.HistoryQuery == nil || c.ResolveRepo == nil {
		return fmt.Errorf("agent context preparation unavailable")
	}
	// Never overwrite an existing output, including symlinks. Provider files and
	// the selected HEAD remain untouched by this inspectable handoff.
	if _, err := os.Lstat(p.flags["--output"]); err == nil {
		return fmt.Errorf("output already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	repo, err := c.ResolveRepo(ctx, cwd)
	if err != nil {
		return err
	}
	view, err := c.HistoryQuery.QueryHistory(ctx, inbound.HistoryQueryInput{Cwd: cwd, Ref: p.first(), Server: true})
	if err != nil {
		return err
	}
	provider := p.flags["--provider"]
	if provider == "" {
		for _, snap := range view.Snapshots {
			if snap.ID == view.Position {
				provider = snap.Provider
				break
			}
		}
	}
	if provider == "" {
		return fmt.Errorf("select --provider claude or codex")
	}
	policy := domain.MemoryInputPolicy()
	if p.has("--context-budget") {
		budget, err := domain.ParseHistoryBudget(p.flags["--context-budget"])
		if err != nil {
			return err
		}
		policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: budget, Source: "explicit_artifact"}
	}
	value, err := c.PrepareAgent.PrepareAgentContext(ctx, inbound.PrepareAgentContextInput{RepoID: repo.ID, Cwd: cwd, Branch: view.Selection.Branch, SnapshotID: view.Position, Provider: provider, Model: p.flags["--model"], Policy: policy, WorkStatePath: p.flags["--work-state"], ArtifactOnly: true})
	if err != nil {
		return err
	}
	raw, err := value.Artifact()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p.flags["--output"], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Printf("prepared artifact %s → %s\n", value.ID, p.flags["--output"])
	fmt.Printf("budget %d; selected %d (%s); provider acceptance unverified\n", value.Policy.BudgetTokens, value.Usage.Tokens, value.Usage.Tokenizer)
	return nil
}
