package app

import (
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPendingReplayDoesNotManufactureLiveActivity(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh("activity")
	snap := domain.Snapshot{ID: hh("old capture"), DocHash: hh("old capture"), RepoID: repo, SessionID: "native-session", Provider: domain.ProviderCodex}
	snap.CreatedAt = time.Now().Add(-24 * time.Hour).UTC()
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	at := snap.CreatedAt
	p := domain.Pending{Provider: snap.Provider, Target: snap.ID, UpdatedAt: time.Now(), ActivityAt: &at}
	for i := 0; i < 2; i++ {
		if err := svc.PutPending(ctx, repo, snap.SessionID, p); err != nil {
			t.Fatal(err)
		}
		got, err := svc.ListPendings(ctx, repo)
		if err != nil || len(got) != 1 {
			t.Fatalf("%+v %v", got, err)
		}
		if !got[0].UpdatedAt.Equal(snap.CreatedAt) || got[0].ActivityAt == nil || !got[0].ActivityAt.Equal(at) {
			t.Fatalf("replay refreshed activity: %+v", got[0])
		}
	}
	future := time.Now().Add(time.Hour)
	p.ActivityAt = &future
	if err := svc.PutPending(ctx, repo, snap.SessionID, p); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.ListPendings(ctx, repo)
	if got[0].ActivityAt != nil {
		t.Fatal("future clock generated live status")
	}
	// Legacy server pointers can still carry their last sync time. Project the
	// actual capture without writing or removing immutable history.
	p.RepoID, p.SessionID, p.UpdatedAt, p.ActivityAt = repo, snap.SessionID, time.Now(), nil
	if err := st.PutPending(ctx, repo, p); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.ListPendings(ctx, repo)
	if !got[0].UpdatedAt.Equal(snap.CreatedAt) || got[0].ActivityAt != nil {
		t.Fatalf("legacy replay appeared current: %+v", got[0])
	}
}

func TestLateLiveCaptureCannotRewindPendingPointer(t *testing.T) {
	ctx := systemTestContext()
	svc, st := newFsckSvc(t)
	repo := hh("late activity")
	newer := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderCodex, "first", "second"))
	older := putPendingGCCapture(t, st, repo, pendingGCCIR(domain.ProviderCodex, "first"))
	now, before := time.Now().UTC(), time.Now().Add(-time.Minute).UTC()
	p := domain.Pending{Provider: domain.ProviderCodex, Target: newer.ID, ActivityAt: &now}
	if err := svc.PutPending(ctx, repo, newer.SessionID, p); err != nil {
		t.Fatal(err)
	}
	p.Target, p.ActivityAt = older.ID, &before
	if err := svc.PutPending(ctx, repo, newer.SessionID, p); err == nil {
		t.Fatal("late capture rewound pointer")
	}
	got, _ := svc.ListPendings(ctx, repo)
	if len(got) != 1 || got[0].Target != newer.ID {
		t.Fatalf("pointer changed: %+v", got)
	}
	assertPendingGCCapture(t, st, newer)
	assertPendingGCCapture(t, st, older)
}
