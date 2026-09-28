package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type dependencyPublicationRemote struct {
	outbound.RemoteSync
	accepted map[domain.ContentHash]bool
	order    []domain.ContentHash
	failed   domain.ContentHash
	calls    int
}

func (r *dependencyPublicationRemote) Push(ctx context.Context, _ string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendDiverged bool) error {
	r.calls++
	if len(snaps) != 1 || len(docs) != 0 || len(refs) != 0 || force || appendDiverged {
		return errors.New("unbounded or mixed publication")
	}
	s := snaps[0]
	for _, parent := range s.Parents {
		if !r.accepted[parent] {
			return errors.New("missing natural parent")
		}
	}
	if s.ID == r.failed {
		return errors.New("publication interrupted")
	}
	r.accepted[s.ID] = true
	r.order = append(r.order, s.ID)
	return nil
}
func publicationSnaps() []domain.Snapshot {
	id := func(s string) domain.ContentHash { return domain.HashContent([]byte(s)) }
	// Newest-first input and a stale overlay pointing toward a child. Only the
	// immutable natural parents are the metadata creation dependency graph.
	return []domain.Snapshot{
		{ID: id("tip"), DocHash: id("tip"), Parents: []domain.ContentHash{id("left"), id("right")}},
		{ID: id("right"), DocHash: id("right"), Parents: []domain.ContentHash{id("root")}},
		{ID: id("left"), DocHash: id("left"), Parents: []domain.ContentHash{id("root")}},
		{ID: id("root"), DocHash: id("root"), GraftParents: []domain.ContentHash{id("tip")}},
	}
}
func TestSnapshotPublicationBoundsTransactionsAndAcknowledgesOnlyAcceptedObjects(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		snaps := publicationSnaps()
		r := &dependencyPublicationRemote{accepted: map[domain.ContentHash]bool{}}
		if interrupted {
			r.failed = snaps[2].ID
		}
		svc := newTestSyncService(nil, r, nil)
		var counts []int
		err := svc.pushSelectedObjects(context.Background(), "fixture", snaps, snaps, nil, func(p inbound.SyncProgress) {
			if p.Phase == "publish-snapshots" {
				counts = append(counts, p.Completed)
			}
		})
		if (err != nil) != interrupted {
			t.Fatal(err)
		}
		if r.order[0] != snaps[3].ID || r.order[1] != snaps[1].ID {
			t.Fatal("parents not published first", r.order)
		}
		if interrupted {
			if !reflect.DeepEqual(counts, []int{0, 1, 2}) || r.accepted[snaps[0].ID] {
				t.Fatal("unacknowledged child counted", counts)
			}
		} else if !reflect.DeepEqual(counts, []int{0, 1, 2, 3, 4}) || r.order[3] != snaps[0].ID {
			t.Fatal(counts, r.order)
		}
	}
}
func TestSnapshotPublicationRejectsCyclesAndDuplicatesBeforeAnyRequest(t *testing.T) {
	base := publicationSnaps()
	for _, snaps := range [][]domain.Snapshot{{base[0], base[0]}, {{ID: base[0].ID, Parents: []domain.ContentHash{base[1].ID}}, {ID: base[1].ID, Parents: []domain.ContentHash{base[0].ID}}}} {
		r := &dependencyPublicationRemote{accepted: map[domain.ContentHash]bool{}}
		svc := newTestSyncService(nil, r, nil)
		if err := svc.pushSelectedObjects(context.Background(), "fixture", snaps, snaps, nil); !errors.Is(err, domain.ErrHashMismatch) || r.calls != 0 {
			t.Fatal(err, r.calls)
		}
	}
}
func TestSnapshotPublicationHandlesLargeChainWithoutRecursiveTraversal(t *testing.T) {
	snaps := make([]domain.Snapshot, 20000)
	for i := range snaps {
		snaps[i].ID = domain.HashContent([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		if i > 0 {
			snaps[i-1].Parents = []domain.ContentHash{snaps[i].ID}
		}
	}
	out, err := orderSnapshotPublication(snaps)
	if err != nil || len(out) != len(snaps) || out[0].ID != snaps[len(snaps)-1].ID || out[len(out)-1].ID != snaps[0].ID {
		t.Fatal("chain order", err)
	}
}

type failedSnapshotPublication struct {
	outbound.RemoteSync
	accepted int
}

func (r *failedSnapshotPublication) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendDiverged bool) error {
	if len(snaps) > 0 {
		r.accepted++
		if r.accepted == 2 {
			return errors.New("snapshot unavailable")
		}
	}
	return r.RemoteSync.Push(ctx, repo, snaps, docs, refs, force, appendDiverged)
}
func TestPartialSnapshotPublicationNeverPublishesGraftsOrRefs(t *testing.T) {
	svc, remote, root, _ := setupPushOrder(t)
	svc.remote = &failedSnapshotPublication{RemoteSync: remote}
	if _, err := svc.Push(context.Background(), inbound.SyncInput{Cwd: root}); err == nil {
		t.Fatal("failed snapshot accepted")
	}
	for _, event := range remote.events {
		if event == "refs" || event == "graft" {
			t.Fatal("published pointers after partial metadata")
		}
	}
}
