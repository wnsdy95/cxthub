package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type collectingPullRemote struct {
	outbound.RemoteSync
	t         *testing.T
	collector *SaveSessionService
	old       domain.Snapshot
	next      domain.ContentHash
}

func (r *collectingPullRemote) Pull(ctx context.Context, repo string, _ map[domain.ContentHash]domain.ContentHash, haves []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	advertised := false
	for _, id := range haves {
		if id == r.old.ID {
			advertised = true
		}
	}
	if !advertised {
		r.t.Fatal("fixture did not advertise the old capture")
	}
	// Another capture replaces the pending leaf while the network read is in
	// flight. The server correctly omits the doc the client advertised.
	r.collector.gcHookLeaf(ctx, repo, r.old.ID, r.next)
	return []domain.Snapshot{r.old}, nil, nil, nil
}

func TestPullRetainsAdvertisedDocumentsUntilBatchIsInstalled(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	peer := storage.NewFileStore(root)
	repo := string(domain.HashContent([]byte(t.Name())))
	put := func(texts ...string) domain.Snapshot {
		t.Helper()
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "session", Fidelity: domain.FidelityFull}}
		for i, text := range texts {
			cir.Events = append(cir.Events, domain.Event{Kind: domain.EventMessage, Seq: i, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
		}
		id, err := st.PutDoc(ctx, domain.SessionDoc{CIR: cir})
		if err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Provider: domain.ProviderCodex, SessionID: "session", Message: domain.HookMessagePrefix + " capture"}
		if err = st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return snap
	}
	old, next := put("first"), put("first", "second")
	collector := NewSaveSessionService(nil, nil, nil, peer)
	remote := &collectingPullRemote{t: t, collector: collector, old: old, next: next.ID}
	_, err := NewSyncRepoService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: true})
	if err != nil {
		t.Fatalf("advertised object vanished during pull: %v", err)
	}
	if _, err = st.GetDoc(ctx, old.ID); err != nil {
		t.Fatalf("validated batch lost its doc: %v", err)
	}
	// The deferral survives a collector restart and is replayed by a later
	// capture, even though that capture no longer names the old pending target.
	restarted := storage.NewFileStore(root)
	jobs, err := restarted.CaptureCollections(ctx, repo, 32)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("collection deferral lost: %+v %v", jobs, err)
	}
	latest := put("first", "second", "third")
	NewSaveSessionService(nil, nil, nil, restarted).gcHookLeaf(ctx, repo, next.ID, latest.ID)
	if _, err = st.GetDoc(ctx, old.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("read reservation leaked: %v", err)
	}
	if jobs, err = restarted.CaptureCollections(ctx, repo, 32); err != nil || len(jobs) != 0 {
		t.Fatalf("completed deferral remains: %+v %v", jobs, err)
	}
}
