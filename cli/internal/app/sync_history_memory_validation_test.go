package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type reviewHistoryMemoryRemote struct {
	*pinnedHistoryPullRemote
	failHash domain.ContentHash
	failErr  error
}

func (r *reviewHistoryMemoryRemote) PullMemoryObject(ctx context.Context, repo string, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if hash == r.failHash {
		r.objectReads = append(r.objectReads, hash)
		return domain.MemoryDigest{}, r.failErr
	}
	return r.causalPullRemote.PullMemoryObject(ctx, repo, hash)
}

// Real application/FileStore, synthetic remote only. Every failure must leave
// the prior successful observation, existing snapshot/ref and history intact.
func TestPullValidatesHistoricalMemoryClosure(t *testing.T) {
	for _, mode := range []string{
		"fresh_pin", "cached_pin", "cached_pin_missing_parent", "missing_parent", "corrupt_parent",
		"foreign_parent", "explicit_owner_contradicts_source", "staged_tip_wrong_explicit_owner",
		"repeated_completed_pin_wrong_owner", "parent_cancellation", "pinned_empty", "empty_owner", "invalid_owner",
	} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repo := string(domain.HashContent([]byte("review historical-memory closure")))
			doc, other := pullDoc(t, "review memory source"), pullDoc(t, "other valid owner")
			store := storage.NewFileStore(t.TempDir())
			for _, d := range []domain.SessionDoc{doc, other} {
				if _, err := store.PutDoc(ctx, d); err != nil {
					t.Fatal(err)
				}
				if err := store.PutSnapshot(ctx, domain.Snapshot{ID: d.Hash, DocHash: d.Hash, RepoID: repo}); err != nil {
					t.Fatal(err)
				}
			}
			localRef := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: doc.Hash, BranchID: "local-identity"}
			if err := store.PutRef(ctx, localRef); err != nil {
				t.Fatal(err)
			}
			localSnapshot, err := store.GetSnapshot(ctx, doc.Hash)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := store.ReadRemoteObservation(ctx, repo, "configured")
			if err != nil {
				t.Fatal(err)
			}
			previous.Snapshots = []domain.Snapshot{localSnapshot}
			previous.Refs = []domain.Ref{localRef}
			if err := store.CompareAndSwapRemoteObservation(ctx, previous.Revision, previous); err != nil {
				t.Fatal(err)
			}
			previous, err = store.ReadRemoteObservation(ctx, repo, "configured")
			if err != nil {
				t.Fatal(err)
			}
			hashOf := func(d domain.MemoryDigest) domain.ContentHash {
				h, err := domain.MemoryDigestHash(d)
				if err != nil {
					t.Fatal(err)
				}
				return h
			}
			ancestor := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "historical ancestor"}
			if mode == "foreign_parent" {
				ancestor.SnapshotID = other.Hash
			}
			ancestorHash := hashOf(ancestor)
			pin := domain.MemoryDigest{SnapshotID: doc.Hash, PreviousMemoryHash: ancestorHash, Summary: "historical pin"}
			if mode == "empty_owner" {
				pin.SnapshotID, pin.PreviousMemoryHash = "", ""
			}
			if mode == "invalid_owner" {
				pin.SnapshotID, pin.PreviousMemoryHash = "not-a-content-hash", ""
			}
			pinHash := hashOf(pin)
			current := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "independent current attachment"}
			currentHash := hashOf(current)
			snapshot := localSnapshot
			snapshot.MemoryHash = currentHash
			event := domain.HistoryEvent{ID: "11111111111111111111111111111111", RepoID: repo, BranchID: "main-identity", Branch: "main", Kind: "position", Source: doc.Hash, Target: doc.Hash, MemoryHash: pinHash, MemoryPinned: true, CreatedAt: time.Unix(100, 0).UTC()}
			base := &pinnedHistoryPullRemote{causalPullRemote: &causalPullRemote{snapshot: snapshot, doc: doc, latest: current, objects: map[domain.ContentHash]domain.MemoryDigest{pinHash: pin, ancestorHash: ancestor}}, history: []domain.HistoryEvent{event}}
			remote := &reviewHistoryMemoryRemote{pinnedHistoryPullRemote: base}
			var wantErr error
			cached := mode == "cached_pin" || mode == "cached_pin_missing_parent"
			if cached {
				if h, err := store.PutMemory(ctx, pin); err != nil || h != pinHash {
					t.Fatalf("cache pin: %s %v", h, err)
				}
			}
			switch mode {
			case "missing_parent", "cached_pin_missing_parent":
				delete(base.objects, ancestorHash)
				wantErr = domain.ErrNotFound
			case "corrupt_parent":
				changed := ancestor
				changed.Summary = "tampered"
				base.objects[ancestorHash] = changed
				wantErr = domain.ErrHashMismatch
			case "foreign_parent":
				wantErr = domain.ErrHashMismatch
			case "explicit_owner_contradicts_source":
				base.history[0].MemorySource = other.Hash
				wantErr = domain.ErrHashMismatch
			case "staged_tip_wrong_explicit_owner":
				base.history[0].MemoryHash, base.history[0].MemorySource = currentHash, other.Hash
				wantErr = domain.ErrHashMismatch
			case "repeated_completed_pin_wrong_owner":
				bad := event
				bad.ID = "22222222222222222222222222222222"
				bad.MemorySource = other.Hash
				base.history = append(base.history, bad)
				wantErr = domain.ErrHashMismatch
			case "parent_cancellation":
				remote.failHash, remote.failErr, wantErr = ancestorHash, context.Canceled, context.Canceled
			case "pinned_empty":
				base.history[0].MemoryHash = ""
			case "empty_owner", "invalid_owner":
				base.history[0].Source = ""
				wantErr = domain.ErrHashMismatch
			}
			_, err = newTestSyncService(store, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo, FetchOnly: true})
			if wantErr != nil {
				// Owner syntax may be reported as invalid hash or hash mismatch;
				// either is acceptable, but successful observation is not.
				ownerSyntax := mode == "empty_owner" || mode == "invalid_owner"
				if err == nil || (!ownerSyntax && !errors.Is(err, wantErr)) {
					t.Fatalf("error=%v want %v", err, wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after, rerr := store.ReadRemoteObservation(ctx, repo, "configured")
			if rerr != nil {
				t.Fatal(rerr)
			}
			if wantErr != nil && !reflect.DeepEqual(after, previous) {
				t.Fatal("failed fetch changed successful observation")
			}
			if wantErr == nil {
				if after.Revision == previous.Revision || len(after.History) != 1 || !reflect.DeepEqual(after.History[0], base.history[0]) {
					t.Fatal("successful fetch did not retain exact raw pin")
				}
				if mode != "pinned_empty" {
					for _, h := range []domain.ContentHash{pinHash, ancestorHash} {
						if _, err := store.GetMemory(ctx, h); err != nil {
							t.Fatalf("missing offline ancestor %s: %v", h, err)
						}
					}
					wantReads := []domain.ContentHash{pinHash, ancestorHash}
					if cached {
						wantReads = []domain.ContentHash{ancestorHash}
					}
					if !reflect.DeepEqual(base.objectReads, wantReads) {
						t.Fatalf("reads=%v want=%v", base.objectReads, wantReads)
					}
				} else if len(base.objectReads) != 0 {
					t.Fatal("empty pin fetched historical memory")
				}
			}
			gotRef, rerr := store.GetRef(ctx, repo, domain.RefBranch, "main")
			gotSnapshot, serr := store.GetSnapshot(ctx, doc.Hash)
			gotHistory, herr := store.ListHistoryEvents(ctx, repo)
			if rerr != nil || serr != nil || herr != nil || gotRef != localRef || !reflect.DeepEqual(gotSnapshot, localSnapshot) || len(gotHistory) != 0 {
				t.Fatalf("fetch adopted local state: refErr=%v snapErr=%v historyErr=%v", rerr, serr, herr)
			}
		})
	}
}

func TestMemoryAttachmentRejectsInvalidSelfOwner(t *testing.T) {
	for _, owner := range []domain.ContentHash{"", "not-a-content-hash"} {
		d := domain.MemoryDigest{SnapshotID: owner, Summary: "invalid self-owner"}
		h, err := domain.MemoryDigestHash(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateMemoryAttachmentObject(d, h, owner); err == nil {
			t.Errorf("accepted invalid expected owner %q despite valid digest bytes", owner)
		}
	}
}

// Cache values are deliberately verified under their original owner first.
// This tests the per-use owner guard, not insertion of unverified fake bytes.
func TestMemoryLoaderChecksVerifiedCacheOwner(t *testing.T) {
	owner, other := domain.HashContent([]byte("verified owner")), domain.HashContent([]byte("other owner"))
	d := domain.MemoryDigest{SnapshotID: owner, Summary: "verified digest"}
	h, err := domain.MemoryDigestHash(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMemoryAttachmentObject(d, h, owner); err != nil {
		t.Fatal(err)
	}
	for _, cache := range []string{"loaded", "staged"} {
		for _, match := range []bool{true, false} {
			t.Run(cache+map[bool]string{true: "_owner_matches", false: "_owner_differs"}[match], func(t *testing.T) {
				loader := &memoryPullLoader{ctx: context.Background(), snapshotID: owner}
				if !match {
					loader.snapshotID = other
				}
				if cache == "loaded" {
					loader.loaded = map[domain.ContentHash]domain.MemoryDigest{h: d}
				} else {
					loader.staged = map[domain.ContentHash]domain.MemoryDigest{h: d}
				}
				got, err := loader.load(h)
				if match {
					if err != nil || !reflect.DeepEqual(got, d) {
						t.Fatalf("verified cache lost: %v", err)
					}
				} else if !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("foreign owner accepted: %v", err)
				}
			})
		}
	}
}
