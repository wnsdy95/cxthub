package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestReplacePendingRejectsCrossProviderSessionCollision(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(t.TempDir())
	first := domain.Pending{SessionID: "same-native-id", Provider: domain.ProviderClaude, Branch: "main", Target: securityHash("1"), Dismissed: true}
	if err := st.PutPending(ctx, first); err != nil {
		t.Fatal(err)
	}
	other := first
	other.Provider, other.Target = domain.ProviderCodex, securityHash("2")
	previous, err := st.ReplacePending(ctx, other)
	if !errors.Is(err, domain.ErrSyncConflict) || previous != "" {
		t.Errorf("cross-provider collision accepted: previous=%s err=%v", previous, err)
	}
	got, err := st.ListPendings(ctx, "")
	if err != nil || len(got) != 1 || got[0] != first {
		t.Fatalf("collision modified the original pointer: %+v err=%v", got, err)
	}
}

func TestReplacePendingSameProviderCanMoveBranches(t *testing.T) {
	ctx := context.Background()
	st := NewFileStore(t.TempDir())
	p := domain.Pending{SessionID: "native-session-moving-worktrees", Provider: domain.ProviderClaude, Branch: "main", Target: securityHash("1"), Dismissed: true}
	if err := st.PutPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	old := p.Target
	p.Branch, p.Target, p.Dismissed = "feature/other-worktree", securityHash("2"), false
	previous, err := st.ReplacePending(ctx, p)
	if err != nil || previous != old {
		t.Fatalf("same native session cannot move worktrees: previous=%s err=%v", previous, err)
	}
	got, err := st.ListPendings(ctx, "")
	if err != nil || len(got) != 1 || got[0].Target != p.Target || got[0].Branch != p.Branch || !got[0].Dismissed {
		t.Fatalf("per-session replacement changed identity or lost dismissal: %+v err=%v", got, err)
	}
}
