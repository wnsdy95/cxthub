package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type finalCASAdoptionStore struct {
	*storage.FileStore
	beforeCAS func(context.Context, domain.ContentHash, outbound.RemoteObservation)
	calls     int
}

func (s *finalCASAdoptionStore) CompareAndSwapRemoteObservation(ctx context.Context, expected domain.ContentHash, next outbound.RemoteObservation) error {
	s.calls++
	if s.beforeCAS != nil {
		s.beforeCAS(ctx, expected, next)
	}
	return s.FileStore.CompareAndSwapRemoteObservation(ctx, expected, next)
}

type finalCASAdoptionRemote struct {
	metadataOnlyProgressRemote
	history []domain.HistoryEvent
}

func (r *finalCASAdoptionRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	return r.history, nil
}
func (r *finalCASAdoptionRemote) PushHistoryEvent(context.Context, domain.HistoryEvent) error {
	return errors.New("unexpected history push")
}

// Include all durable synthetic state, even the immutable bodies and acquired
// metadata caches. Lock-file contents are synchronization, not repository data.
func finalCASFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	base := filepath.Join(root, ".cxt")
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if rel == "locks" {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Extend the real FileStore/app adoption path used by
// TestP4RootReceiverAndBatchAdoption. Only the metadata peer and timing hook are
// synthetic; the hook induces an actual FileStore CAS conflict/cancellation.
func TestRootAdoptionFinalObservationFailureNoAdvance(t *testing.T) {
	for _, identity := range []domain.DocumentIdentity{domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy} {
		for _, failure := range []string{"stale-CAS", "cancel-at-CAS"} {
			t.Run(fmt.Sprintf("identity=%s/%s", identity, failure), func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				base := storage.NewFileStore(root)
				repo := domain.HashContent([]byte(t.Name()))
				legacy := pullDoc(t, "retained local baseline")
				if _, err := base.PutDoc(ctx, legacy); err != nil {
					t.Fatal(err)
				}
				old := domain.Snapshot{ID: legacy.Hash, DocHash: legacy.Hash, RepoID: repo, Branch: "main"}
				if err := base.PutSnapshot(ctx, old); err != nil {
					t.Fatal(err)
				}
				oldRef := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: old.ID}
				if err := base.PutRef(ctx, oldRef); err != nil {
					t.Fatal(err)
				}
				oldEvent := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo, Branch: "main", BranchID: domain.LegacyContextBranchID(repo, "main"), Kind: "position", Source: old.ID, Target: old.ID, MemoryPinned: true, CreatedAt: time.Unix(1, 0).UTC()}
				if err := base.PutHistoryEvent(ctx, oldEvent); err != nil {
					t.Fatal(err)
				}
				observation := outbound.RemoteObservation{Version: 1, RepoID: repo, Remote: "configured", Snapshots: []domain.Snapshot{old}, Refs: []domain.Ref{oldRef}, History: []domain.HistoryEvent{oldEvent}}
				if err := base.CompareAndSwapRemoteObservation(ctx, "", observation); err != nil {
					t.Fatal(err)
				}
				state, err := domain.SnapshotStateHash(old)
				if err != nil {
					t.Fatal(err)
				}
				cursor := map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry{old.ID: {LocalState: state, RemoteState: domain.HashContent([]byte("prior remote projection"))}}
				if err := base.SaveRemoteSnapshotStateCursor(ctx, repo, cursor); err != nil {
					t.Fatal(err)
				}

				representation, chunks, materialized := p4AppRoot(t)
				if identity == domain.DocumentIdentityLegacy {
					materialized = pullDoc(t, "new legacy adoption")
				}
				incoming := domain.Snapshot{ID: materialized.Hash, DocHash: materialized.Hash, DocIdentity: identity, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{old.ID}}
				if identity == domain.DocumentIdentityRootV1 {
					for hash, body := range chunks {
						if err := base.PutChunk(ctx, hash, body); err != nil {
							t.Fatal(err)
						}
					}
					receiver := &pullDocumentReceiver{store: base}
					if err := receiver.ReceiveRoot(ctx, representation); err != nil {
						t.Fatal(err)
					}
					if !receiver.verified[incoming.DocumentRef()] {
						t.Fatal("root receiver did not verify the exact reference")
					}
				} else if _, err := base.PutDoc(ctx, materialized); err != nil {
					t.Fatal(err)
				}
				// Seed meaningful acquisition progress separately from adoption. A
				// failed final CAS must neither erase it nor promote it to authority.
				if _, err := base.AppendMetadataCheckpoint(ctx, "", repo, "configured", []domain.Snapshot{old, incoming}, nil); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(incoming)
				if err != nil {
					t.Fatal(err)
				}
				checkpoint := &domain.CatalogCheckpoint{Version: 1, RepoID: repo, Epoch: "00000000-0000-0000-0000-000000000001", Sequence: 2}
				page := domain.CatalogPage{Version: 1, RepoID: repo, Epoch: checkpoint.Epoch, Through: 2, Mode: "baseline", Checkpoint: checkpoint, Entries: []domain.CatalogEntry{
					{Sequence: 1, Kind: "protocol", Key: repo, Value: json.RawMessage(`{"context_protocol":0}`)},
					{Sequence: 2, Kind: "snapshot", Key: incoming.ID, Value: raw},
				}}
				if _, err := base.AppendCatalogPage(ctx, "", repo, "configured", page); err != nil {
					t.Fatal(err)
				}
				ref := oldRef
				ref.Target = incoming.ID
				event := oldEvent
				event.ID, event.Target, event.CreatedAt = strings.Repeat("2", 32), incoming.ID, time.Unix(2, 0).UTC()
				remote := &finalCASAdoptionRemote{metadataOnlyProgressRemote: metadataOnlyProgressRemote{snaps: []domain.Snapshot{incoming}, refs: []domain.Ref{ref}}, history: []domain.HistoryEvent{oldEvent, event}}
				store := &finalCASAdoptionStore{FileStore: base}
				before := finalCASFiles(t, root)
				var atFailure map[string]string
				var winner outbound.RemoteObservation
				pullCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				store.beforeCAS = func(bound context.Context, expected domain.ContentHash, next outbound.RemoteObservation) {
					if !reflect.DeepEqual(before, finalCASFiles(t, root)) {
						t.Fatal("snapshot/ref/history/cursor/cache advanced before final observation fence")
					}
					if len(next.History) != 2 || len(next.Refs) != 1 || next.Refs[0].Target != incoming.ID {
						t.Fatal("final fence did not receive complete proposed adoption")
					}
					found := false
					for _, snap := range next.Snapshots {
						found = found || snap.DocumentRef() == incoming.DocumentRef()
					}
					if !found {
						t.Fatal("final fence lost requested identity")
					}
					if err := base.VerifyStoredDocReference(bound, incoming.DocumentRef()); err != nil {
						t.Fatal("not a valid current body at final boundary", err)
					}
					if _, err := base.GetSnapshot(bound, incoming.ID); !errors.Is(err, domain.ErrNotFound) {
						t.Fatal("metadata published before final fence", err)
					}
					var err error
					winner, err = base.ReadRemoteObservation(bound, repo, "configured")
					if err != nil {
						t.Fatal(err)
					}
					if failure == "stale-CAS" {
						winner.Snapshots[0].Message = "concurrent observation winner"
						if err := base.CompareAndSwapRemoteObservation(bound, expected, winner); err != nil {
							t.Fatal(err)
						}
						winner, err = base.ReadRemoteObservation(bound, repo, "configured")
						if err != nil {
							t.Fatal(err)
						}
					} else {
						cancel()
					}
					atFailure = finalCASFiles(t, root)
				}
				svc := newTestSyncService(store, remote, nil)
				out, err := svc.Pull(pullCtx, inbound.SyncInput{RepoID: repo})
				want := domain.ErrSyncConflict
				if failure == "cancel-at-CAS" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || store.calls != 1 || !reflect.DeepEqual(out, inbound.SyncOutput{}) {
					t.Fatal("did not fail at actual final observation CAS", out, err, store.calls)
				}
				if atFailure == nil || !reflect.DeepEqual(atFailure, finalCASFiles(t, root)) {
					t.Fatal("failed final observation changed durable snapshots/refs/history/cursor/cache")
				}
				got, err := base.ReadRemoteObservation(ctx, repo, "configured")
				if err != nil || !reflect.DeepEqual(got, winner) {
					t.Fatal("failed operation replaced observation winner", err)
				}
				if err := base.VerifyStoredDocReference(ctx, incoming.DocumentRef()); err != nil {
					t.Fatal("reusable exact body lost", err)
				}
				// A fresh operation consumes the same current immutable progress,
				// retries from the winning revision, and really adopts the root.
				store.beforeCAS = nil
				if _, err := svc.Pull(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
					t.Fatal("fresh retry failed", err)
				}
				adopted, err := base.GetSnapshot(ctx, incoming.ID)
				if err != nil || adopted.DocumentRef() != incoming.DocumentRef() {
					t.Fatal("retry lost exact identity", err)
				}
				gotRef, err := base.GetRef(ctx, repo, domain.RefBranch, "main")
				if err != nil || gotRef.Target != incoming.ID {
					t.Fatal("retry failed to advance ref", err)
				}
				history, err := base.ListHistoryEvents(ctx, repo)
				if err != nil || len(history) != 2 {
					t.Fatal("retry failed to adopt history", err, history)
				}
			})
		}
	}
}
