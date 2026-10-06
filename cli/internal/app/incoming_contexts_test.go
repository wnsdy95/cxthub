package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type incomingCatalogRemote struct {
	*reviewTransferRemote
	catalog    []domain.Snapshot
	catalogErr error
	reads      int
}

func (r *incomingCatalogRemote) ReadSnapshotCatalog(context.Context, string) ([]domain.Snapshot, error) {
	r.reads++
	return r.catalog, r.catalogErr
}

func TestIncomingCatalogDiscoveryPreservesLocalAndDoesNotImportMetadata(t *testing.T) {
	f := reviewTransferSetup(t, true)
	ctx := context.Background()
	local, err := f.st.ListSnapshots(ctx, f.repo, "")
	if err != nil {
		t.Fatal(err)
	}
	extra := domain.HashContent([]byte("remote-only body deliberately absent"))
	remoteSnap := domain.Snapshot{ID: extra, DocHash: extra, RepoID: f.repo, Branch: "other", Message: "candidate [git aaaa]", CreatedAt: time.Unix(10, 0)}
	conflicting := local[0]
	conflicting.Message = "remote label must not overwrite existing local metadata"
	r := &incomingCatalogRemote{reviewTransferRemote: f.remote, catalog: []domain.Snapshot{conflicting, remoteSnap}}
	svc := newTestSyncService(f.st, r, nil)
	got, supported, err := svc.DiscoverIncomingSnapshots(ctx, inbound.SyncInput{RepoID: f.repo})
	if err != nil || !supported || len(got) != 3 || got[0].ID != extra {
		t.Fatalf("got=%+v supported=%v err=%v", got, supported, err)
	}
	for _, snap := range got {
		if snap.ID == local[0].ID && !reflect.DeepEqual(snap, local[0]) {
			t.Fatal("local label replaced")
		}
	}
	if _, err := f.st.GetSnapshot(ctx, extra); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("discovery imported unverified snapshot")
	}
	if has, _ := f.st.HasDoc(ctx, extra); has {
		t.Fatal("discovery imported unrelated body")
	}
	if r.planCalls+r.broadCalls+r.memoryCalls+r.settingsCalls != 0 {
		t.Fatal("discovery hydrated objects")
	}
	f.assertUnadopted(t)
}

func TestIncomingCatalogCapabilityAndFailureBoundaries(t *testing.T) {
	for _, mode := range []string{"legacy", "unsupported", "denied", "canceled", "capability-lost"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewTransferSetup(t, true)
			r := &incomingCatalogRemote{reviewTransferRemote: f.remote, catalog: f.remote.snapshots}
			switch mode {
			case "legacy":
				r.capability.BranchPlanVersion = 0
			case "unsupported":
				r.capability.BranchPlanVersion = 2
			case "denied":
				r.catalogErr = errors.New("forbidden")
			case "canceled":
				r.catalogErr = context.Canceled
			}
			svc := newTestSyncService(f.st, r, nil)
			_, supported, err := svc.DiscoverIncomingSnapshots(context.Background(), inbound.SyncInput{RepoID: f.repo})
			if mode == "legacy" {
				if supported || err != nil || r.reads != 0 {
					t.Fatal("legacy not explicitly reported")
				}
			} else if mode == "capability-lost" {
				if !supported || err != nil {
					t.Fatal(err)
				}
				r.capability.BranchPlanVersion = 0
				_, err = svc.Pull(context.Background(), inbound.SyncInput{RepoID: f.repo, Ref: "feature", FetchOnly: true, RequireBranchPlan: true})
				if !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatalf("capability loss: %v", err)
				}
			} else if err == nil {
				t.Fatal("discovery failure accepted")
			}
			if r.broadCalls+r.planCalls != 0 {
				t.Fatal("failed discovery broadened")
			}
			f.assertUnadopted(t)
		})
	}
}
