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

type collectedMemoryRemote struct {
	*causalPushRemote
	missing        bool
	failure        error
	restoreFailure error
	restores       int
}

func (r *collectedMemoryRemote) NegotiatePushObjects(context.Context, string, []domain.ContentHash, []domain.ContentHash) (outbound.PushObjectWants, error) {
	return outbound.PushObjectWants{}, nil // present at negotiation, collected later
}
func (r *collectedMemoryRemote) PushMemory(ctx context.Context, repo string, d domain.MemoryDigest) error {
	if r.failure != nil {
		return r.failure
	}
	if r.missing {
		return memoryStatusError(404)
	}
	return r.causalPushRemote.PushMemory(ctx, repo, d)
}
func (r *collectedMemoryRemote) PublishMemoryArchive(ctx context.Context, repo string, snap domain.Snapshot, doc domain.SessionDoc, root domain.MemoryDigest) error {
	r.restores++
	if r.restoreFailure != nil {
		return r.restoreFailure
	}
	if snap.ID != root.SnapshotID || doc.Hash != snap.DocHash || snap.RepoID != repo || root.PreviousMemoryHash != "" {
		return domain.ErrHashMismatch
	}
	r.missing = false
	return r.causalPushRemote.PushMemory(ctx, repo, root)
}

func TestPushRestoresCollectedMemoryArchiveBeforeRefs(t *testing.T) {
	for _, mode := range []string{"restored", "restore conflict", "server failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte(t.Name())))
			doc := pullDoc(t, "locally retained capture")
			if _, err := st.PutDoc(ctx, doc); err != nil {
				t.Fatal(err)
			}
			root := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "root"}
			rootHash := putMemoryObject(t, ctx, st, root)
			latest := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "next", PreviousMemoryHash: rootHash}
			latestHash := putMemoryObject(t, ctx, st, latest)
			if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo, MemoryHash: latestHash}); err != nil {
				t.Fatal(err)
			}
			if err := st.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: doc.Hash}); err != nil {
				t.Fatal(err)
			}
			remote := &collectedMemoryRemote{causalPushRemote: &causalPushRemote{objects: map[domain.ContentHash]domain.MemoryDigest{}}, missing: true}
			if mode == "restore conflict" {
				remote.restoreFailure = domain.ErrSyncConflict
			}
			if mode == "server failure" {
				remote.failure = memoryStatusError(503)
			}
			_, err := newTestSyncService(st, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo})
			if mode == "restored" {
				if err != nil || remote.current != latestHash || remote.refPushes != 1 || remote.restores != 1 {
					t.Fatalf("restore failed: current=%s refs=%d restore=%d err=%v", remote.current, remote.refPushes, remote.restores, err)
				}
				if len(remote.pushes) != 2 || remote.pushes[0] != rootHash || remote.pushes[1] != latestHash {
					t.Fatalf("causal suffix: %v", remote.pushes)
				}
			} else {
				if err == nil || remote.refPushes != 0 {
					t.Fatalf("failure published refs: %v %d", err, remote.refPushes)
				}
				if mode == "restore conflict" && !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatal(err)
				}
				if mode == "server failure" && remote.restores != 0 {
					t.Fatal("retried unrelated failure")
				}
			}
		})
	}
}
