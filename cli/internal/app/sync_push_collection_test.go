package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Model a remote whose GC can collect an unreferenced pending prefix between
// negotiation and metadata publication. The parent was not in the original
// upload set, so recovery must recheck the entire offered dependency graph.
type collectingPushRemote struct {
	lazyPushRemote
	docs, snapshots            map[domain.ContentHash]bool
	parent                     domain.ContentHash
	current                    domain.ContentHash
	mode                       string
	publications, negotiations int
	cancel                     context.CancelFunc
}

func (r *collectingPushRemote) NegotiatePushObjects(_ context.Context, _ string, snapshots, docs []domain.ContentHash) (outbound.PushObjectWants, error) {
	r.negotiations++
	var wants outbound.PushObjectWants
	for _, id := range snapshots {
		if !r.snapshots[id] {
			wants.Snapshots = append(wants.Snapshots, id)
		}
	}
	for _, id := range docs {
		if !r.docs[id] {
			wants.Docs = append(wants.Docs, id)
		}
	}
	return wants, nil
}

var errCollected = errors.New("a publication prerequisite disappeared")
var errUnrelatedPublication = errors.New("metadata validation failed")

func (r *collectingPushRemote) Push(_ context.Context, _ string, snapshots []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, _, _ bool) error {
	for _, doc := range docs {
		r.docs[doc.Hash] = true
		r.objectDocs = append(r.objectDocs, doc)
	}
	if len(snapshots) > 0 {
		r.publications++
		if r.mode == "unrelated" {
			return errUnrelatedPublication
		}
		if r.publications == 1 || r.mode == "repeated" {
			if r.mode == "conflict" {
				r.snapshots[r.current] = true
			}
			if r.mode == "current" {
				delete(r.docs, r.current)
			} else {
				if r.mode != "snapshot-only" {
					delete(r.docs, r.parent)
				}
				delete(r.snapshots, r.parent)
			}
			if r.cancel != nil {
				r.cancel()
			}
			return errCollected
		}
		incoming := map[domain.ContentHash]bool{}
		for _, snap := range snapshots {
			incoming[snap.ID] = true
		}
		for _, snap := range snapshots {
			if r.mode == "conflict" && snap.ID == r.current {
				return errUnrelatedPublication
			}
			if !r.docs[snap.DocHash] {
				return errCollected
			}
			for _, parent := range snap.ReachabilityParents() {
				if !incoming[parent] && !r.snapshots[parent] {
					return errCollected
				}
			}
		}
		for _, snap := range snapshots {
			r.snapshots[snap.ID] = true
		}
	}
	if len(refs) > 0 {
		for _, ref := range refs {
			if !r.snapshots[ref.Target] {
				return errCollected
			}
		}
		r.refCalls++
	}
	return nil
}

func TestPushRecoversCollectedPrerequisitesBeforeRefs(t *testing.T) {
	for _, mode := range []string{"once", "current", "snapshot-only", "repeated", "unrelated", "canceled", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			base, repo, ids := lazyPushFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			remote := &collectingPushRemote{docs: map[domain.ContentHash]bool{ids[0]: true, ids[1]: true}, snapshots: map[domain.ContentHash]bool{ids[0]: true}, parent: ids[0], current: ids[1], mode: mode}
			if mode == "canceled" {
				remote.cancel = cancel
			}
			counting := &docReadCountingStore{SessionStore: base}
			_, err := newTestSyncService(counting, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo})
			if mode == "once" || mode == "current" || mode == "snapshot-only" {
				if err != nil {
					t.Fatal(err)
				}
				wantReads, wantID := 1, ids[0]
				wantPublications := 3 // failed child, recovered parent, then child
				if mode == "current" {
					wantID = ids[1]
					wantPublications = 2 // only the current document disappeared
				}
				if mode == "snapshot-only" {
					wantReads = 0
				}
				if remote.refCalls != 1 || remote.publications != wantPublications || len(counting.reads) != wantReads || (wantReads > 0 && counting.reads[0] != wantID) {
					t.Fatalf("refs=%d publications=%d reads=%v", remote.refCalls, remote.publications, counting.reads)
				}
				return
			}
			if err == nil || remote.refCalls != 0 {
				t.Fatalf("failure advanced refs: %v / %d", err, remote.refCalls)
			}
			if mode == "conflict" {
				if !errors.Is(err, errUnrelatedPublication) || remote.publications != 3 {
					t.Fatalf("recovery hid metadata error: %v / %d", err, remote.publications)
				}
				return
			}
			if mode == "repeated" {
				if !errors.Is(err, errCollected) || remote.publications != 3 || len(counting.reads) != 2 {
					t.Fatalf("unbounded or lost recovery: %v, publications=%d reads=%v", err, remote.publications, counting.reads)
				}
			} else if remote.publications != 1 || len(counting.reads) != 0 {
				t.Fatalf("unrelated failure retried: publications=%d reads=%v", remote.publications, counting.reads)
			}
		})
	}
}
