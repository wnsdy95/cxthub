package app

import (
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type pinnedHistoryPullRemote struct {
	*causalPullRemote
	history []domain.HistoryEvent
}

func (r *pinnedHistoryPullRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return r.history, nil
}
func (r *pinnedHistoryPullRemote) PushHistoryEvent(context.Context, domain.HistoryEvent) error {
	panic("fetch wrote history")
}

func TestPullRetainsHistoricalPinAncestryOutsideCurrentAttachment(t *testing.T) {
	ctx := context.Background()
	repo := string(domain.HashContent([]byte("historical pin ancestry")))
	doc := pullDoc(t, "historical memory pin")
	st := storage.NewFileStore(t.TempDir())
	ancestor := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "old ancestor"}
	ancestorHash, err := domain.MemoryDigestHash(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	pinned := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: ancestorHash, Summary: "historical pin"}
	pinnedHash, err := domain.MemoryDigestHash(pinned)
	if err != nil {
		t.Fatal(err)
	}
	current := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "current independent attachment"}
	currentHash, err := domain.MemoryDigestHash(current)
	if err != nil {
		t.Fatal(err)
	}
	remote := &pinnedHistoryPullRemote{
		causalPullRemote: &causalPullRemote{
			snapshot: domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, Branch: "main", MemoryHash: currentHash},
			doc:      doc, latest: current,
			objects: map[domain.ContentHash]domain.MemoryDigest{pinnedHash: pinned, ancestorHash: ancestor},
		},
		history: []domain.HistoryEvent{{ID: "11111111111111111111111111111111", RepoID: repo, BranchID: "main-identity", Branch: "main", Kind: "birth", Target: doc.Hash, MemoryHash: pinnedHash, MemoryPinned: true, CreatedAt: time.Unix(100, 0)}},
	}
	if _, err := newTestSyncService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: true}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []domain.ContentHash{currentHash, pinnedHash, ancestorHash} {
		if _, err := st.GetMemory(ctx, hash); err != nil {
			t.Fatalf("successful fetch omitted memory dependency %s: %v", hash, err)
		}
	}
}
