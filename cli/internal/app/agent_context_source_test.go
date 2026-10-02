package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestAgentLatestMainSourceSeparatesWorkingPositionAndHistory(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "history"}[history], func(t *testing.T) {
			s, in, h, _, _ := agentServiceFixture(t)
			in.LatestMain = true
			in.ArtifactOnly = true
			in.Branch = "feature"
			in.SnapshotID = agentHash("older selected context")
			in.MemoryPin = &domain.AgentMemoryPin{}
			in.WorktreeStateHash = agentHash("worktree")
			in.WorkingPosition = &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("b", 40)}
			if history {
				in.Policy = domain.InputPolicy{Version: 1, Mode: "history", BudgetTokens: 100000, Source: "explicit"}
			}
			s.history = retryHistoryFunc(func(ctx context.Context, q inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
				if !q.ServerTip || q.Branch != "main" || q.Position != "" {
					t.Fatal("wrong source query", q)
				}
				return h.QueryHistory(ctx, q)
			})
			p, err := s.PrepareAgentContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if p.Content.Selection.SnapshotID != h.view.Position || p.Content.Selection.MemoryPin != nil || p.Content.Selection.CodeCommit != h.view.Selection.CodeCommit || p.Content.Selection.DeliveryCodeCommit() != in.WorkingPosition.CodeCommit {
				t.Fatal("mixed source/worktree", p.Content.Selection)
			}
			if history && len(p.Content.History) == 0 {
				t.Fatal("main history not selected")
			}
			if err := s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentLatestMainRevalidatesMemoryAndPermissionBeforeDelivery(t *testing.T) {
	for _, change := range []string{"permission", "memory revision", "context revision", "main moved during preparation"} {
		t.Run(change, func(t *testing.T) {
			s, in, h, m, _ := agentServiceFixture(t)
			in.LatestMain = true
			in.WorkingPosition = &domain.AgentWorkingPosition{Branch: "feature", CodeCommit: strings.Repeat("b", 40)}
			in.WorktreeStateHash = agentHash("worktree")
			if change == "main moved during preparation" {
				h.after = func(v *domain.HistoryQueryResult) { v.Selection.CodeCommit = strings.Repeat("c", 40) }
			}
			p, err := s.PrepareAgentContext(context.Background(), in)
			if change == "main moved during preparation" {
				if !errors.Is(err, domain.ErrSelectionChanged) {
					t.Fatal("mixed main accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "permission":
				m.failAfter = m.calls
			case "memory revision":
				m.rev.Evidence++
			case "context revision":
				h.view.Revision = &domain.RepositoryRevision{Graph: 6, Evidence: 4}
			}
			if err := s.ValidateLatestMain(context.Background(), in.Cwd, p.Content.Selection); err == nil {
				t.Fatal("stale or revoked source accepted")
			}
		})
	}
}
