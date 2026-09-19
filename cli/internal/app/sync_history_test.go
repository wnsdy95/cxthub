package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type historyPushRemote struct {
	outbound.RemoteSync
	target   domain.ContentHash
	calls    []string
	accepted []domain.HistoryEvent
}

func (r *historyPushRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return r.accepted, nil
}
func (r *historyPushRemote) RemoteManifest(context.Context, string) (domain.Manifest, error) {
	return domain.Manifest{Refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", Target: r.target}}}, nil
}
func (r *historyPushRemote) Push(_ context.Context, _ string, _ []domain.Snapshot, _ []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	if force || appendMode || len(refs) != 1 {
		return errors.New("unsafe prerequisite push")
	}
	r.calls = append(r.calls, "prerequisite")
	r.target = refs[0].Target
	return nil
}
func (r *historyPushRemote) PushHistoryEvent(_ context.Context, e domain.HistoryEvent) error {
	if e.Source != r.target {
		return errors.New("history before source publication")
	}
	r.calls = append(r.calls, "retention")
	r.target = e.Target
	r.accepted = append(r.accepted, e)
	return nil
}

func TestHistoryPushPublishesUnpushedSourceBeforeRetentionAndRejectsConcurrentWork(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("history push")))
	var ids []domain.ContentHash
	for _, label := range []string{"base", "unpublished", "continuation", "peer"} {
		id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{SessionOriginID: label}}})
		if err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main"}
		if len(ids) > 0 {
			snap.Parents = []domain.ContentHash{ids[0]}
		}
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	e := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: repo, BranchID: "main", Branch: "main", Kind: "advance", Source: ids[1], Target: ids[2], CreatedAt: time.Now().UTC()}
	if err := st.PutHistoryEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	for _, initial := range []domain.ContentHash{"", ids[0], ids[1], ids[3]} {
		r := &historyPushRemote{target: initial}
		svc := newTestSyncService(st, r, nil)
		err := svc.pushHistory(ctx, repo)
		if initial == ids[3] {
			if !errors.Is(err, domain.ErrSyncConflict) || len(r.calls) != 0 || r.target != initial {
				t.Fatalf("peer overwritten: %+v %v", r, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"prerequisite", "retention"}
		if initial == ids[1] {
			want = want[1:]
		}
		if !reflect.DeepEqual(r.calls, want) || r.target != ids[2] {
			t.Fatalf("publication order: %+v", r)
		}
		if err := svc.pushHistory(ctx, repo); err != nil || !reflect.DeepEqual(r.calls, want) {
			t.Fatalf("acknowledged replay changed ref: %+v %v", r, err)
		}
	}
}
