package app

import (
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPutPendingRejectsCrossProviderCollisionBeforeGC(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh("pending provider collision")
	old, other := hh("claude capture"), hh("codex capture")
	for _, snap := range []domain.Snapshot{
		{ID: old, DocHash: old, RepoID: repo, SessionID: "same-native-id", Provider: domain.ProviderClaude, Message: domain.HookMessagePrefix + " capture"},
		{ID: other, DocHash: other, RepoID: repo, SessionID: "same-native-id", Provider: domain.ProviderCodex, Message: domain.HookMessagePrefix + " capture"},
	} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	p := domain.Pending{SessionID: "same-native-id", Provider: domain.ProviderClaude, Branch: "main", Target: old}
	if err := svc.PutPending(ctx, repo, p.SessionID, p); err != nil {
		t.Fatal(err)
	}
	p.Provider, p.Target = domain.ProviderCodex, other
	if err := svc.PutPending(ctx, repo, p.SessionID, p); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("cross-provider collision accepted: %v", err)
	}
	got, err := st.ListPendings(ctx, repo)
	if err != nil || len(got) != 1 || got[0].Target != old || got[0].Provider != domain.ProviderClaude {
		t.Errorf("original pending pointer lost: %+v err=%v", got, err)
	}
	for _, id := range []domain.ContentHash{old, other} {
		if _, err := st.GetSnapshot(ctx, repo, id); err != nil {
			t.Errorf("collision garbage-collected capture %s: %v", id, err)
		}
	}
}
