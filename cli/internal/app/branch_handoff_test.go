package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestBranchHandoffUsesSharedPackageAndDoesNotFallbackOnServerFailure(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	target := putBranchSeedSnapshot(t, ctx, store, "repo", "feature", []domain.Event{agentMessage("user", "RAW ARCHIVE", 0)}, nil, &domain.MemoryDigest{Summary: "LOCAL FALLBACK"})
	packages := &agentPackageFixture{}
	svc := NewBranchHandoffService(store).WithAgentContext(packages, agentCodeFixture{})
	input := inbound.BranchHandoffInput{Cwd: "/fixture", FromBranch: "main", ToBranch: "feature", Target: target}
	text, err := svc.RenderBranchHandoff(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !domain.IsAgentContextPackageText(text) || strings.Contains(text, "RAW ARCHIVE") || strings.Contains(text, "LOCAL FALLBACK") || !packages.seen.ArtifactOnly || packages.seen.SnapshotID != target || packages.seen.Branch != "feature" || packages.seen.Cwd != "/fixture" {
		t.Fatalf("wrong handoff %s %+v", text, packages.seen)
	}
	packages.err = errors.New("authorization revoked")
	if text, err = svc.RenderBranchHandoff(ctx, input); err == nil || text != "" {
		t.Fatal("fell back to local memory after permission failure")
	}
}

func TestBranchHandoffIsBoundedMemoryOnly(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	repoID := "repo-app-handoff"
	const newest = "LATEST TARGET DECISION MUST SURVIVE"
	target := putBranchSeedSnapshot(t, ctx, store, repoID, "feature/app", []domain.Event{{
		Kind: domain.EventMessage,
		Role: "user",
		Blocks: []domain.ContentBlock{{
			Type: "text",
			Text: "RAW ARCHIVED CONVERSATION MUST NOT BE INJECTED",
		}},
	}}, nil, &domain.MemoryDigest{
		Summary: strings.Repeat("older bounded memory ", 3000) + newest,
		KeyFacts: []string{
			"Desktop app sessions stay owned by their provider.",
		},
		OpenTasks:          []string{"Verify the one-time app handoff."},
		TasksAuthoritative: true,
	})

	got, err := NewBranchHandoffService(store).RenderBranchHandoff(ctx, inbound.BranchHandoffInput{
		FromBranch: "main",
		ToBranch:   "feature/app",
		Target:     target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > appBranchHandoffMaxBytes {
		t.Fatalf("handoff = %d bytes, want <= %d", len(got), appBranchHandoffMaxBytes)
	}
	for _, want := range []string{appBranchHandoffPrefix, newest, "Desktop app sessions stay owned by their provider.", "Verify the one-time app handoff."} {
		if !strings.Contains(got, want) {
			t.Fatalf("handoff lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "RAW ARCHIVED CONVERSATION") {
		t.Fatalf("raw transcript leaked into handoff:\n%s", got)
	}
}

func TestBranchHandoffDoesNotReinjectNestedCxtSeed(t *testing.T) {
	ctx := context.Background()
	store := storage.NewFileStore(t.TempDir())
	target := putBranchSeedSnapshot(t, ctx, store, "repo-app-handoff-recursion", "feature/app", nil, nil, &domain.MemoryDigest{
		Summary: seedSummaryPrefix + " recursive generation that must stay archived",
		KeyFacts: []string{
			"The non-recursive project fact remains available.",
		},
	})

	got, err := NewBranchHandoffService(store).RenderBranchHandoff(ctx, inbound.BranchHandoffInput{
		FromBranch: "main",
		ToBranch:   "feature/app",
		Target:     target,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, seedSummaryPrefix) || strings.Contains(got, "recursive generation") {
		t.Fatalf("recursive cxt seed was reinjected:\n%s", got)
	}
	if !strings.Contains(got, "The non-recursive project fact remains available.") {
		t.Fatalf("safe structured fact was lost:\n%s", got)
	}
}
