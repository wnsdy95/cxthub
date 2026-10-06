package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type memorySelectionUnsupportedRemote struct {
	*strictPublicationRemote
	unsupported bool
	attempts    []string
}

func (r *memorySelectionUnsupportedRemote) PushHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	r.attempts = append(r.attempts, e.ID)
	if e.MemorySelectionParent != "" && r.unsupported {
		return &backendclient.HTTPError{Status: 404, Code: "not_found"}
	}
	return r.strictPublicationRemote.PushHistoryEvent(ctx, e)
}
func TestMemorySelectionUnsupportedServerLeavesExactLocalEventPending(t *testing.T) {
	f := newStrictPublication(t)
	before := f.event(30, "position", "R", "feature", f.b)
	before.Source, before.MemoryPinned, before.WorktreeID = before.Target, true, strings.Repeat("d", 32)
	after := before
	after.ID = strings.Repeat("e", 32)
	digest := domain.MemoryDigest{SnapshotID: f.b, Summary: "frozen-memory"}
	var err error
	after.MemoryHash, err = f.st.PutMemory(f.ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	f.r.snaps = []domain.ContentHash{f.a, f.b, f.x}
	f.r.memoryObjects[after.MemoryHash] = digest
	after.MemorySelectionParent = before.ID
	after.CreatedAt = before.CreatedAt.Add(-time.Hour)
	f.put(before)
	f.put(after)
	local, err := f.st.ListHistoryEvents(f.ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	remote := &memorySelectionUnsupportedRemote{strictPublicationRemote: f.r, unsupported: true}
	f.svc.remote = remote
	err = f.svc.pushSelectedHistory(f.ctx, f.repo, local)
	var he *backendclient.HTTPError
	if !errors.As(err, &he) || he.Status != 404 || !strings.Contains(err.Error(), "remains pending") {
		t.Fatalf("unsupported relation was acknowledged: %v", err)
	}
	afterFailure, err := f.st.ListHistoryEvents(f.ctx, f.repo)
	if err != nil || !reflect.DeepEqual(afterFailure, local) {
		t.Fatal("failure changed durable local history", err)
	}
	for _, e := range remote.accepted {
		if e.ID == after.ID {
			t.Fatal("unsupported relation accepted")
		}
	}
	remote.unsupported = false
	remote.attempts = nil
	if err := f.svc.pushSelectedHistory(f.ctx, f.repo, afterFailure); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remote.attempts, []string{after.ID}) {
		t.Fatalf("retry changed pending set: %v", remote.attempts)
	}
	for _, e := range remote.accepted {
		if e.ID == after.ID {
			if !reflect.DeepEqual(e, after) {
				t.Fatal("retry changed immutable payload")
			}
			return
		}
	}
	t.Fatal("retry did not send pending relation")
}
