package app

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type fetchInventoryStore struct {
	*storage.FileStore
	negotiating   bool
	manifests     int
	snapshotReads map[domain.ContentHash]int
	haveReads     map[domain.ContentHash]int
	verifications map[domain.ContentHash]int
	badSnapshot   domain.ContentHash
}

func (s *fetchInventoryStore) reset() {
	s.negotiating, s.manifests = true, 0
	s.snapshotReads = map[domain.ContentHash]int{}
	s.haveReads = map[domain.ContentHash]int{}
	s.verifications = map[domain.ContentHash]int{}
}

func (s *fetchInventoryStore) Manifest(ctx context.Context, repo string) (domain.Manifest, error) {
	s.manifests++
	return s.FileStore.Manifest(ctx, repo)
}

func (s *fetchInventoryStore) GetSnapshot(ctx context.Context, id domain.ContentHash) (domain.Snapshot, error) {
	if s.negotiating {
		s.snapshotReads[id]++
	}
	if id == s.badSnapshot {
		return domain.Snapshot{}, domain.ErrHashMismatch
	}
	return s.FileStore.GetSnapshot(ctx, id)
}

func (s *fetchInventoryStore) HasDoc(ctx context.Context, id domain.ContentHash) (bool, error) {
	if s.negotiating {
		s.haveReads[id]++
	}
	return s.FileStore.HasDoc(ctx, id)
}

func (s *fetchInventoryStore) VerifyStoredDoc(ctx context.Context, id domain.ContentHash) error {
	s.verifications[id]++
	return s.FileStore.VerifyStoredDoc(ctx, id)
}

type fetchInventoryRemote struct {
	*reviewTransferRemote
	store  *fetchInventoryStore
	states map[domain.ContentHash]domain.ContentHash
}

func (r *fetchInventoryRemote) PullSelectedBranchTo(ctx context.Context, repo string, req domain.BranchPullRequest, states map[domain.ContentHash]domain.ContentHash, haves []domain.ContentHash, receiver outbound.PullDocumentReceiver) (domain.BranchPullPlan, []domain.Snapshot, error) {
	// Count only inventory reads before negotiation, not later validation reads.
	r.store.negotiating = false
	r.states = maps.Clone(states)
	plan, snaps, err := r.reviewTransferRemote.PullSelectedBranchTo(ctx, repo, req, states, haves, receiver)
	if err != nil {
		return plan, nil, err
	}
	have := map[domain.ContentHash]bool{}
	for _, id := range haves {
		have[id] = true
	}
	// Model the streaming adapter's existing-body check. Missing inventory hints
	// must not cause a download or bypass verification of already stored bodies.
	for _, id := range plan.SnapshotIndex {
		if have[id] {
			continue
		}
		ok, err := receiver.HasVerifiedDoc(ctx, id)
		if err != nil {
			return plan, nil, err
		}
		if !ok {
			return plan, nil, errors.New("unexpected synthetic body download")
		}
	}
	return plan, snaps, nil
}

func assertFetchInventory(t *testing.T, st *fetchInventoryStore, ids ...domain.ContentHash) {
	t.Helper()
	want := map[domain.ContentHash]int{}
	for _, id := range ids {
		want[id]++
	}
	if st.manifests != 0 || !maps.Equal(st.snapshotReads, want) || !maps.Equal(st.haveReads, want) {
		t.Errorf("negotiation: Manifest calls=%d snapshot reads=%v document stats=%v; want only %v", st.manifests, st.snapshotReads, st.haveReads, want)
	}
}

func assertFetchBodiesVerified(t *testing.T, st *fetchInventoryStore, f reviewTransferFixture) {
	t.Helper()
	want := map[domain.ContentHash]int{f.target: 1, f.source: 1}
	if !maps.Equal(st.verifications, want) {
		t.Errorf("document verification calls=%v, want once per planned document: %v", st.verifications, want)
	}
}

func TestScopedFetchInventoryNegotiation(t *testing.T) {
	for _, mode := range []string{"cold", "warm", "other-endpoint"} {
		t.Run(mode, func(t *testing.T) {
			f := reviewTransferSetup(t, mode != "cold")
			unrelated := domain.HashContent([]byte("unobserved local snapshot"))
			if err := f.st.PutSnapshot(context.Background(), domain.Snapshot{RepoID: f.repo, ID: unrelated, DocHash: unrelated}); err != nil {
				t.Fatal(err)
			}
			st := &fetchInventoryStore{FileStore: f.st}
			st.reset()
			remote := &fetchInventoryRemote{reviewTransferRemote: f.remote, store: st}
			svc := newTestSyncService(st, remote, nil)
			if mode == "other-endpoint" {
				svc = svc.WithRemoteIdentity("different-exact-endpoint")
			}
			if _, err := svc.Pull(context.Background(), inbound.SyncInput{RepoID: f.repo, Ref: "feature", FetchOnly: true}); err != nil {
				t.Fatal(err)
			}
			if mode == "warm" {
				assertFetchInventory(t, st, f.target, f.source)
				if !maps.Equal(remote.states, f.remote.plan.SnapshotStates) || remote.metadataCount != 0 {
					t.Fatal("warm fetch did not reuse the exact observed metadata")
				}
			} else {
				assertFetchInventory(t, st)
				if len(remote.states) != 0 || remote.metadataCount != 2 {
					t.Fatal("scoped fetch reused an ordinary or another endpoint's observation")
				}
			}
			assertFetchBodiesVerified(t, st, f)
			f.assertUnadopted(t)
			if mode == "other-endpoint" {
				after, err := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
				if err != nil || after.Revision != f.before.Revision {
					t.Fatal("fetch changed another endpoint's observation", err)
				}
			}
		})
	}
}

func TestScopedFetchInventoryRejectsInvalidInputs(t *testing.T) {
	for _, mode := range []string{"body", "metadata", "denied", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			f := reviewTransferSetupAt(t, true, root)
			st := &fetchInventoryStore{FileStore: f.st}
			st.reset()
			remote := &fetchInventoryRemote{reviewTransferRemote: f.remote, store: st}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var wantErr error
			switch mode {
			case "body":
				f.st.EnableDocVerificationCache(filepath.Join(t.TempDir(), "private", "key"))
				if err := f.st.VerifyStoredDoc(ctx, f.target); err != nil {
					t.Fatal(err)
				}
				hex := strings.TrimPrefix(string(f.target), "sha256:")
				if _, err := os.Stat(filepath.Join(root, ".cxt", "doc-verification", hex+".json")); err != nil {
					t.Fatal("authenticated warm receipt missing", err)
				}
				if err := os.WriteFile(filepath.Join(root, ".cxt", "objects", "docs", hex), []byte("tampered stored document"), 0600); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				st.badSnapshot = f.target
				wantErr = domain.ErrHashMismatch
			case "denied":
				wantErr = errors.New("synthetic plan denied")
				f.remote.planErr = wantErr
			case "cancel":
				wantErr = context.Canceled
				f.remote.onPlan = cancel
			}
			_, err := newTestSyncService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: f.repo, Ref: "feature", FetchOnly: true})
			if err == nil || wantErr != nil && !errors.Is(err, wantErr) {
				t.Fatalf("fetch error=%v, want rejection %v", err, wantErr)
			}
			if mode == "body" && st.verifications[f.target] != 1 {
				t.Fatal("tampered body was not checked against its warm receipt")
			}
			if st.manifests != 0 {
				t.Error("scoped failure scanned the full manifest")
			}
			if f.remote.broadCalls != 0 || f.remote.planCalls != 1 {
				t.Fatal("scoped error changed transfer scope")
			}
			if mode == "denied" && len(st.verifications) != 0 {
				t.Fatal("denied plan accessed document bodies")
			}
			after, err := f.st.ReadScopedRemoteObservation(context.Background(), f.repo, f.remote.SyncRemoteIdentity(), "feature")
			if err != nil || after.Revision != f.before.Revision {
				t.Fatal("failed fetch advanced observation", err)
			}
			f.assertUnadopted(t)
		})
	}
}

func TestScopedFetchInventoryPreservesUnexaminedCursor(t *testing.T) {
	f := reviewTransferSetup(t, true)
	ctx := context.Background()
	other := domain.HashContent([]byte("another branch's unexamined snapshot"))
	if err := f.st.PutSnapshot(ctx, domain.Snapshot{RepoID: f.repo, ID: other, DocHash: other}); err != nil {
		t.Fatal(err)
	}
	// This cursor is stale locally. Selected fetch must retain it without reading
	// that branch's metadata or advertising it as a validated omission.
	passive := domain.RemoteSnapshotStateCursorEntry{
		LocalState:  domain.HashContent([]byte("old local state")),
		RemoteState: domain.HashContent([]byte("other remote projection")),
	}
	local, err := f.st.GetSnapshot(ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	local.GraftSeq = 1
	if err := f.st.ReconcileGraftState(ctx, local); err != nil {
		t.Fatal(err)
	}
	localState, err := domain.SnapshotStateHash(local)
	if err != nil {
		t.Fatal(err)
	}
	cursorKey := string(domain.HashContent([]byte(f.repo + "\x00" + f.remote.SyncRemoteIdentity())))
	if err := f.st.SaveRemoteSnapshotStateCursor(ctx, cursorKey, map[domain.ContentHash]domain.RemoteSnapshotStateCursorEntry{
		other:    passive,
		f.target: {LocalState: localState, RemoteState: f.remote.plan.SnapshotStates[f.target]},
	}); err != nil {
		t.Fatal(err)
	}
	st := &fetchInventoryStore{FileStore: f.st}
	remote := &fetchInventoryRemote{reviewTransferRemote: f.remote, store: st}
	for _, mutated := range []bool{false, true} {
		if mutated {
			local.GraftSeq++
			if err := f.st.ReconcileGraftState(ctx, local); err != nil {
				t.Fatal(err)
			}
		}
		st.reset()
		before := remote.metadataCount
		if _, err := newTestSyncService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: f.repo, Ref: "feature", FetchOnly: true}); err != nil {
			t.Fatal(err)
		}
		assertFetchInventory(t, st, f.target, f.source)
		assertFetchBodiesVerified(t, st, f)
		if _, advertised := remote.states[other]; advertised {
			t.Error("unexamined cursor was advertised")
		}
		wantMetadata := 0
		if mutated {
			wantMetadata = 1
			if _, advertised := remote.states[f.target]; advertised {
				t.Error("local mutation failed to invalidate the selected cursor")
			}
		} else if remote.states[f.target] != f.remote.plan.SnapshotStates[f.target] {
			t.Error("matching current local state did not reuse the selected cursor")
		}
		if remote.metadataCount-before != wantMetadata {
			t.Errorf("mutated=%v metadata delta=%d, want %d", mutated, remote.metadataCount-before, wantMetadata)
		}
		entries, err := f.st.LoadRemoteSnapshotStateCursor(ctx, cursorKey)
		if err != nil || entries[other] != passive {
			t.Fatal("selected fetch discarded an unexamined cursor", err)
		}
	}
	current, err := f.st.GetSnapshot(ctx, f.target)
	if err != nil || current.GraftSeq != 2 {
		t.Fatal("fetch adopted remote metadata over local mutation", err)
	}
	f.assertUnadopted(t)
}

func TestFetchInventoryBroadFetchUnchanged(t *testing.T) {
	f := newRemoteStateCursorFixture(t)
	st := &fetchInventoryStore{FileStore: f.store}
	st.reset()
	svc := newTestSyncService(st, f.remote, nil)
	for i, want := range []int{1, 0} {
		out, err := svc.Pull(f.ctx, inbound.SyncInput{RepoID: f.repoID, FetchOnly: true})
		if err != nil || out.Pulled != want {
			t.Fatalf("bare fetch %d: %+v %v, want %d snapshots", i, out, err, want)
		}
	}
	if st.manifests != 2 || !maps.Equal(st.verifications, map[domain.ContentHash]int{f.id: 1}) {
		t.Fatalf("bare fetch changed: Manifest calls=%d document checks=%v", st.manifests, st.verifications)
	}
}
