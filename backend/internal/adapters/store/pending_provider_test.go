package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestFSStorePendingProviderCollisionAndBranchMove(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte("pending provider repo"))
	p := domain.Pending{RepoID: repo, SessionID: "same-native-id", Provider: domain.ProviderClaude, Branch: "main", Target: securityHash("1"), Dismissed: true}
	if err := st.PutPending(ctx, repo, p); err != nil {
		t.Fatal(err)
	}
	other := p
	other.Provider, other.Target = domain.ProviderCodex, securityHash("2")
	previous, err := st.ReplacePending(ctx, repo, other)
	if !errors.Is(err, domain.ErrConflict) || previous != "" {
		t.Errorf("provider collision accepted: previous=%s err=%v", previous, err)
	}
	got, err := st.ListPendings(ctx, repo)
	if err != nil || len(got) != 1 || got[0] != p {
		t.Fatalf("original pending modified: %+v err=%v", got, err)
	}
	old := p.Target
	p.Branch, p.Target, p.Dismissed = "feature/other-worktree", securityHash("3"), false
	previous, err = st.ReplacePending(ctx, repo, p)
	if err != nil || previous != old {
		t.Fatalf("same-provider branch move rejected: previous=%s err=%v", previous, err)
	}
	got, err = st.ListPendings(ctx, repo)
	if err != nil || len(got) != 1 || got[0].Target != p.Target || got[0].Branch != p.Branch || !got[0].Dismissed {
		t.Fatalf("branch move changed identity or dismissal: %+v err=%v", got, err)
	}
}
