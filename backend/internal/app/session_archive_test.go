package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestSessionArchivePreservesPublicationAndPending(t *testing.T) {
	ctx := systemTestContext()
	service, storage := newFsckSvc(t)
	repo := hh(t.Name())
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	base := domain.Snapshot{ID: hh("committed"), DocHash: hh("committed"), RepoID: repo, Branch: "main", SessionID: "working", Provider: domain.ProviderCodex, CreatedAt: time.Unix(100, 0)}
	capture := base
	capture.ID, capture.DocHash, capture.Message = hh("tail"), hh("tail"), "hook: work"
	capture.Parents, capture.CreatedAt = []domain.ContentHash{base.ID}, time.Unix(200, 0)
	for _, snapshot := range []domain.Snapshot{base, capture} {
		if err := storage.PutSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base.ID}, ""); err != nil {
		t.Fatal(err)
	}
	pending := domain.Pending{RepoID: repo, SessionID: capture.SessionID, Provider: capture.Provider, Branch: "main", Target: capture.ID}
	if err := service.PutPending(ctx, repo, pending.SessionID, pending); err != nil {
		t.Fatal(err)
	}
	before, err := service.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetSessionArchived(ctx, repo, base.ID, true); err != nil {
		t.Fatal(err)
	}
	full, err := service.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := service.GetPendingView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.ArchivedSessions) != 1 || len(full.ArchivedSessions[0].SnapshotIDs) != 2 || full.ArchivedSessions[0].LatestSnapshotID != capture.ID {
		t.Fatalf("archive did not cover committed and pending snapshots: %+v", full.ArchivedSessions)
	}
	if !reflect.DeepEqual(full.ArchivedSessions, patch.ArchivedSessions) {
		t.Fatal("full and live archive contracts disagree")
	}
	if full.Revision.Graph <= before.Revision.Graph {
		t.Fatal("archive did not invalidate repository readers")
	}
	before.Graph.Revision = full.Graph.Revision
	if !reflect.DeepEqual(before.Graph, full.Graph) || !reflect.DeepEqual(before.Refs, full.Refs) || !reflect.DeepEqual(before.Pending, full.Pending) || !reflect.DeepEqual(before.Snapshots, full.Snapshots) || !reflect.DeepEqual(before.Semantics, full.Semantics) {
		t.Fatal("archive changed publication, pending pointers, original records, or PR semantics")
	}
	if err := service.SetSessionArchived(ctx, repo, capture.ID, true); err != nil {
		t.Fatal(err)
	}
	records, err := storage.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != 1 || records[0].ArchivedAt != full.ArchivedSessions[0].ArchivedAt {
		t.Fatalf("repeat archive replaced original receipt: %+v %v", records, err)
	}
	if err := service.SetSessionArchived(ctx, repo, capture.ID, false); err != nil {
		t.Fatal(err)
	}
	restored, err := service.GetRepositoryView(ctx, repo)
	if err != nil || len(restored.ArchivedSessions) != 0 || !reflect.DeepEqual(restored.Snapshots, before.Snapshots) {
		t.Fatalf("restore lost data: %v", err)
	}
}

func TestSessionArchiveProtectsCaptureAndTracksNewCapture(t *testing.T) {
	ctx := systemTestContext()
	service, storage := newFsckSvc(t)
	repo := hh(t.Name())
	old := putPendingGCCapture(t, storage, repo, pendingGCCIR(domain.ProviderClaude, "first"))
	if err := service.PutPending(ctx, repo, old.SessionID, domain.Pending{Target: old.ID, Provider: old.Provider, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetSessionArchived(ctx, repo, old.ID, true); err != nil {
		t.Fatal(err)
	}
	next := putPendingGCCapture(t, storage, repo, pendingGCCIR(domain.ProviderClaude, "first", "next"))
	if err := service.PutPending(ctx, repo, old.SessionID, domain.Pending{Target: next.ID, Provider: old.Provider, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	assertPendingGCCapture(t, storage, old)
	assertPendingGCCapture(t, storage, next)
	if err := service.DeletePending(ctx, repo, old.SessionID); err != nil {
		t.Fatal(err)
	}
	report, err := service.Fsck(ctx, repo)
	if err != nil || report.Reachable != 2 || len(report.Unreachable) != 0 {
		t.Fatalf("archived session lost retention roots: %+v %v", report, err)
	}
	records, err := storage.ListSessionArchives(ctx, repo)
	if err != nil || len(records) != 1 {
		t.Fatalf("capture or pending resolution erased archive: %+v %v", records, err)
	}
}

func TestSessionArchiveAuthorizationAndIsolation(t *testing.T) {
	storage := store.NewFSStore(t.TempDir())
	fixture := makeTeamFixture(t, storage)
	service := NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	system := systemTestContext()
	repo := domain.Repo{ID: hh(t.Name()), RepositoryID: fixture.repository.ID, DefaultBranch: "main"}
	if _, err := storage.PutRepo(system, repo); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.Snapshot{ID: hh("authorization"), DocHash: hh("authorization"), RepoID: repo.ID, Provider: domain.ProviderCodex, SessionID: "shared"}
	if err := storage.PutSnapshot(system, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, role := range []domain.MemberRole{domain.RoleViewer, domain.RolePuller, domain.RoleMember, domain.RoleMaintainer} {
		if err := storage.AddMember(system, domain.Membership{RepositoryID: fixture.repository.ID, UserID: fixture.member.ID, Role: role, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		actor := inbound.WithRepositoryActor(context.Background(), fixture.member.ID)
		for _, archived := range []bool{true, false} {
			err := service.SetSessionArchived(actor, repo.ID, snapshot.ID, archived)
			if role == domain.RoleMaintainer && err != nil {
				t.Fatalf("maintainer rejected: %v", err)
			}
			if role != domain.RoleMaintainer && !errors.Is(err, domain.ErrForbidden) {
				t.Fatalf("%s changed archive=%v: %v", role, archived, err)
			}
		}
	}
	owner := inbound.WithRepositoryActor(context.Background(), fixture.owner.ID)
	if err := service.SetSessionArchived(owner, repo.ID, snapshot.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := service.SetSessionArchived(context.Background(), repo.ID, snapshot.ID, false); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("anonymous restore accepted: %v", err)
	}
	if err := service.SetSessionArchived(owner, repo.ID, hh("missing"), true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("nonexistent snapshot archived: %v", err)
	}
	if err := service.SetSessionArchived(owner, hh("other-repository"), snapshot.ID, false); err == nil {
		t.Fatal("cross-repository restoration accepted")
	}
}

type pausedArchiveGCStore struct {
	*store.FSStore
	ready  chan struct{}
	resume chan struct{}
}

func (storage *pausedArchiveGCStore) CaptureSupersedes(ctx context.Context, repo, old, next domain.ContentHash, provider domain.ProviderKind, session string) (bool, error) {
	close(storage.ready)
	<-storage.resume
	return storage.FSStore.CaptureSupersedes(ctx, repo, old, next, provider, session)
}

func TestSessionArchiveWinsConcurrentCaptureGC(t *testing.T) {
	ctx := systemTestContext()
	storage := store.NewFSStore(t.TempDir())
	paused := &pausedArchiveGCStore{FSStore: storage, ready: make(chan struct{}), resume: make(chan struct{})}
	service := NewService(paused, paused, nil, gitengine.NewEngine(paused), nil)
	repo := hh(t.Name())
	old := putPendingGCCapture(t, storage, repo, pendingGCCIR(domain.ProviderClaude, "first"))
	if err := service.PutPending(ctx, repo, old.SessionID, domain.Pending{Target: old.ID, Provider: old.Provider, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	next := putPendingGCCapture(t, storage, repo, pendingGCCIR(domain.ProviderClaude, "first", "next"))
	finished := make(chan error, 1)
	go func() {
		finished <- service.PutPending(ctx, repo, old.SessionID, domain.Pending{Target: next.ID, Provider: next.Provider, Branch: "main"})
	}()
	select {
	case <-paused.ready:
	case <-time.After(5 * time.Second):
		close(paused.resume)
		t.Fatal("capture GC did not reach transcript verification")
	}
	archiveErr := service.SetSessionArchived(ctx, repo, old.ID, true)
	close(paused.resume)
	if archiveErr != nil {
		t.Fatal(archiveErr)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	assertPendingGCCapture(t, storage, old)
	assertPendingGCCapture(t, storage, next)
}
