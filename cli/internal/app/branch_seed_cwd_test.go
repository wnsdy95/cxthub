package app

import (
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type branchSeedGit struct {
	repo domain.Repo
}

func (g branchSeedGit) CurrentRepo(context.Context, string) (domain.Repo, error) {
	return g.repo, nil
}

func (g branchSeedGit) CurrentBranch(context.Context, string) (string, error) {
	return "main", nil
}

func TestBranchSeedUsesTargetWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	targetCwd := t.TempDir()
	repo := domain.Repo{
		ID:            "repo-1",
		LocalPath:     targetCwd,
		DefaultBranch: "main",
	}
	source := domain.CIRDocument{
		Envelope: domain.Envelope{
			CIRVersion:      "1",
			SourceProvider:  domain.ProviderCodex,
			SessionOriginID: "source-session",
			Cwd:             "/old/repository/path",
			GitBranch:       "main",
			Fidelity:        domain.FidelityFull,
		},
		Events: []domain.Event{{
			Seq:    0,
			Kind:   domain.EventMessage,
			Role:   "user",
			Ts:     time.Now().UTC().Format(time.RFC3339),
			Blocks: []domain.ContentBlock{{Type: "text", Text: "continue"}},
		}},
	}
	head, err := store.PutDoc(ctx, domain.SessionDoc{CIR: source})
	if err != nil {
		t.Fatalf("put source doc: %v", err)
	}
	if err := store.PutSnapshot(ctx, domain.Snapshot{
		ID:       head,
		RepoID:   repo.ID,
		Branch:   "main",
		DocHash:  head,
		Provider: domain.ProviderCodex,
		Fidelity: domain.FidelityFull,
	}); err != nil {
		t.Fatalf("put source snapshot: %v", err)
	}
	if err := store.PutRef(ctx, domain.Ref{
		Kind: domain.RefBranch, Name: "main", RepoID: repo.ID, Target: head,
	}); err != nil {
		t.Fatalf("put source ref: %v", err)
	}

	service := NewBranchSeedService(
		branchSeedGit{repo: repo},
		store,
		memory.NewRuleDistiller(),
		nil,
		nil,
	)
	out, err := service.Seed(ctx, inbound.SeedInput{
		Cwd:        targetCwd,
		FromBranch: "main",
		NewBranch:  "feature",
		Provider:   domain.ProviderCodex,
	})
	if err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	seed, err := store.GetDoc(ctx, out.SnapshotID)
	if err != nil {
		t.Fatalf("get seed doc: %v", err)
	}
	if seed.CIR.Envelope.Cwd != targetCwd {
		t.Fatalf("seed cwd = %q, want target %q", seed.CIR.Envelope.Cwd, targetCwd)
	}
}
