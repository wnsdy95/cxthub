package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func latestMainRuntime(t *testing.T) (*agentSelectionFixture, *domain.HistoryQueryResult) {
	t.Helper()
	f := newAgentSelectionFixture(t)
	f.writePosition(t)
	selectionGit(t, f.cwd, "switch", "-qc", "feature/older-code")
	id := domain.HashContent([]byte("latest authorized main"))
	v := &domain.HistoryQueryResult{Version: 1, ServerChecked: true, Complete: true, Position: id,
		StateHash: domain.HashContent([]byte("latest-main-state")), Revision: &domain.RepositoryRevision{Graph: 1, Evidence: 1},
		Selection: domain.HistorySelection{Branch: "main", CodeCommit: strings.Repeat("b", 40)},
		Snapshots: []domain.Snapshot{{ID: id, DocHash: id, RepoID: f.repo}}}
	f.runtime.history = selectionHistoryFunc(func(_ context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
		f.historyRequests = append(f.historyRequests, in)
		if !in.Server || !in.ServerTip || in.Branch != "main" || in.Position != "" || in.Ref != "" {
			t.Fatalf("source came from local selection: %+v", in)
		}
		return *v, nil
	})
	return f, v
}

func TestRuntimeInjectionAlwaysUsesServerMain(t *testing.T) {
	for _, provider := range []domain.ProviderKind{domain.ProviderCodex, domain.ProviderClaude} {
		t.Run(string(provider), func(t *testing.T) {
			f, v := latestMainRuntime(t)
			ctx := context.Background()
			before, err := f.runtime.store.ReadCheckoutState(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			// No local main ref exists. An explicit old source/pin must not override injection.
			p, err := f.runtime.PrepareAgentContext(ctx, inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: provider, Branch: "feature/older-code", SnapshotID: f.position.Snapshot, MemoryPin: &domain.AgentMemoryPin{SnapshotID: f.position.Snapshot, MemoryHash: f.position.MemoryHash}})
			if err != nil {
				t.Fatal(err)
			}
			selected := p.Content.Selection
			if selected.SourcePolicy != domain.AgentSourceLatestMain || selected.Branch != "main" || selected.SnapshotID != v.Position || selected.CodeCommit != v.Selection.CodeCommit || selected.MemoryPin != nil {
				t.Fatal("wrong input source", selected)
			}
			if selected.WorkingPosition == nil || selected.WorkingPosition.Branch != "feature/older-code" || selected.DeliveryCodeCommit() != f.code || selected.DeliveryBranch() != "feature/older-code" {
				t.Fatal("source replaced working position", selected)
			}
			if err := p.ValidateIdentity(); err != nil {
				t.Fatal(err)
			}
			if err := f.runtime.validateAgentDelivery(ctx, f.cwd, selected); err != nil {
				t.Fatal(err)
			}
			for _, req := range f.requests {
				if req.Branch != "main" || req.SnapshotID != v.Position || req.CodeCommit != v.Selection.CodeCommit || req.MemoryHash != "" {
					t.Fatal("memory not from main", req)
				}
			}
			after, err := f.runtime.store.ReadCheckoutState(ctx, f.repo)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("injection moved working cursor", err)
			}
		})
	}
}

func TestRuntimeLatestMainRejectsConcurrentSourceOrWorkingChanges(t *testing.T) {
	for _, change := range []string{"main tip", "main code", "main revision", "worktree branch", "worktree code", "memory pin"} {
		t.Run(change, func(t *testing.T) {
			f, v := latestMainRuntime(t)
			ctx := context.Background()
			p, err := f.runtime.PrepareAgentContext(ctx, inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "main tip":
				v.Position = domain.HashContent([]byte("new main"))
				v.Snapshots[0].ID = v.Position
				v.Snapshots[0].DocHash = v.Position
			case "main code":
				v.Selection.CodeCommit = strings.Repeat("c", 40)
			case "main revision":
				v.Revision = &domain.RepositoryRevision{Graph: 2, Evidence: 1}
			case "worktree branch":
				selectionGit(t, f.cwd, "switch", "-qc", "another")
			case "worktree code":
				selectionGit(t, f.cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "next")
			case "memory pin":
				f.position.MemoryHash = ""
				f.writePosition(t)
			}
			err = f.runtime.validateAgentDelivery(ctx, f.cwd, p.Content.Selection)
			if !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatalf("%s accepted: %v", change, err)
			}
		})
	}
}

func TestRuntimeLatestMainHasNoLocalFallback(t *testing.T) {
	for _, cause := range []error{domain.ErrNotFound, errors.New("access revoked"), errors.New("offline")} {
		t.Run(cause.Error(), func(t *testing.T) {
			f, _ := latestMainRuntime(t)
			f.runtime.history = selectionHistoryFunc(func(context.Context, inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				return domain.HistoryQueryResult{}, cause
			})
			_, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex})
			if !errors.Is(err, cause) || len(f.requests) != 0 {
				t.Fatal("local pin used after source failure", err)
			}
		})
	}
}

// A same-SHA switch must not give working_position one branch and its hash another.
type switchingSelectionGit struct {
	outbound.GitContext
	code     outbound.CodePosition
	branches []string
}

func (g *switchingSelectionGit) CurrentBranch(ctx context.Context, cwd string) (string, error) {
	if len(g.branches) > 0 {
		b := g.branches[0]
		g.branches = g.branches[1:]
		return b, nil
	}
	return g.GitContext.CurrentBranch(ctx, cwd)
}
func (g *switchingSelectionGit) CurrentCommit(ctx context.Context, cwd string) (string, error) {
	return g.code.CurrentCommit(ctx, cwd)
}
func TestRuntimeLatestMainRejectsSplitWorkingBranchObservation(t *testing.T) {
	f, _ := latestMainRuntime(t)
	g := f.runtime.git
	f.runtime.git = &switchingSelectionGit{GitContext: g, code: g.(outbound.CodePosition), branches: []string{"old", "new", "new"}}
	_, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd, Provider: domain.ProviderCodex})
	if !errors.Is(err, domain.ErrSelectionChanged) || len(f.historyRequests) != 0 {
		t.Fatal("mixed branch observation accepted", err)
	}
}

func TestRuntimeLatestMainResolvesOmittedProviderFromServer(t *testing.T) {
	f, v := latestMainRuntime(t)
	v.Snapshots[0].Provider = domain.ProviderCodex
	p, err := f.runtime.PrepareAgentContext(context.Background(), inbound.PrepareAgentContextInput{Cwd: f.cwd})
	if err != nil || p.Provider != domain.ProviderCodex {
		t.Fatal("default provider did not come from server main", p.Provider, err)
	}
}
