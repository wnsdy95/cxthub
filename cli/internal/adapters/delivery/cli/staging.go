package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func runStage(ctx context.Context, c *Container, cwd string, p parsedCommand) error {
	if c.Staging == nil {
		return fmt.Errorf("frozen staging unavailable")
	}
	var providers []domain.ProviderKind
	for _, name := range p.positionals {
		if name == "." {
			providers = nil
			break
		}
		providers = append(providers, name)
	}
	sources, err := capture.ResolveStagingSources(ctx, cwd, providers)
	if err != nil {
		return err
	}
	in := inbound.StageInput{Cwd: cwd, ExpectedRevision: domain.ContentHash(p.flags["--expect"])}
	for _, source := range sources.Sessions {
		in.Sessions = append(in.Sessions, inbound.StageSession{Provider: source.Provider, Path: source.Path, SessionID: source.SessionID})
	}
	index, err := c.Staging.Stage(ctx, in)
	if err != nil {
		return err
	}
	if p.has("--json") {
		return json.NewEncoder(os.Stdout).Encode(index)
	}
	fmt.Printf("staged %d frozen source(s) in this worktree; revision %s\n", len(index.Entries), index.Revision)
	for _, entry := range index.Entries {
		fmt.Printf("  %s %s events [%d,%d)\n", entry.Key, entry.Provider, entry.StartEvent, entry.Events)
	}
	return nil
}

func runStagedCommit(ctx context.Context, c *Container, cwd string, p parsedCommand) error {
	if c.Staging == nil {
		return fmt.Errorf("frozen staging unavailable; run cxt add before cxt commit")
	}
	var out domain.StagingCommit
	var err error
	if p.has("--resume") {
		out, err = c.Staging.ResumeCommit(ctx, cwd, p.flags["--resume"])
	} else {
		out, err = c.Staging.Commit(ctx, inbound.StagingCommitInput{Cwd: cwd, Message: p.flags["-m"], Author: c.Identity, ExpectedRevision: domain.ContentHash(p.flags["--expect"])})
	}
	if err != nil {
		return err
	}
	if p.has("--json") {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("committed frozen index → %s; operation %s\n", shortHash(out.Position.Snapshot), out.ID)
	fmt.Println("locally finalized; run cxt push to publish (server acceptance not yet checked)")
	return nil
}

func runUnstage(ctx context.Context, c *Container, cwd string, p parsedCommand) error {
	if c.Staging == nil {
		return fmt.Errorf("frozen staging unavailable")
	}
	in := inbound.UnstageInput{Cwd: cwd, ExpectedRevision: domain.ContentHash(p.flags["--expect"]), All: p.first() == "."}
	if !in.All {
		for _, key := range p.positionals {
			in.Keys = append(in.Keys, domain.ContentHash(key))
		}
	}
	index, err := c.Staging.Unstage(ctx, in)
	if err != nil {
		return err
	}
	if p.has("--json") {
		return json.NewEncoder(os.Stdout).Encode(index)
	}
	fmt.Printf("%d frozen source(s) remain staged; original conversations preserved\n", len(index.Entries))
	return nil
}

func runIndexStash(ctx context.Context, c *Container, cwd string, p parsedCommand) error {
	if p.first() == "list" {
		if c.ListIndexStashes == nil {
			return fmt.Errorf("index stash query unavailable")
		}
		entries, err := c.ListIndexStashes(ctx, cwd)
		if err != nil {
			return err
		}
		if p.has("--json") {
			return json.NewEncoder(os.Stdout).Encode(entries)
		}
		for _, entry := range entries {
			fmt.Printf("%s %d source(s) applied=%t\n", entry.ID, len(entry.Index.Entries), entry.Applied)
		}
		return nil
	}
	if c.IndexStash == nil {
		return fmt.Errorf("index stash unavailable")
	}
	expected := domain.ContentHash(p.flags["--expect"])
	if p.first() == "pop" {
		index, err := c.IndexStash.PopIndex(ctx, cwd, p.flags["--id"], expected)
		if err != nil {
			return err
		}
		if p.has("--json") {
			return json.NewEncoder(os.Stdout).Encode(index)
		}
		fmt.Printf("restored %d frozen source(s); revision %s\n", len(index.Entries), index.Revision)
		return nil
	}
	out, err := c.IndexStash.StashIndex(ctx, cwd, expected)
	if err != nil {
		return err
	}
	if p.has("--json") {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("saved frozen index %s (%d source(s))\n", out.ID, len(out.Index.Entries))
	return nil
}

func manualCheckout(ctx context.Context, c *Container, in inbound.CheckoutInput) (inbound.CheckoutOutput, error) {
	checked, ok := c.Checkout.(inbound.CodeCheckedCheckout)
	if !ok || c.CodePosition == nil || c.ResolveRepo == nil {
		return inbound.CheckoutOutput{}, fmt.Errorf("code-checked checkout unavailable; use cxt load for reference-only context")
	}
	repo, err := c.ResolveRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}
	in.RepoID = repo.ID
	code, err := c.CodePosition.CurrentCommit(ctx, in.Cwd)
	if err != nil {
		return inbound.CheckoutOutput{}, err
	}
	return checked.CheckoutAtCode(ctx, in, code)
}
