package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type metadataCheckpointCalls struct {
	manifests, plans, metadata, documents, memories int
}

// Only the HTTP peer is synthetic. Discovery, transfer, validation, checkpoint
// persistence, and observation publication use the production adapters.
type metadataCheckpointFixture struct {
	mu        sync.Mutex
	calls     metadataCheckpointCalls
	root      string
	key       string
	repo      string
	url       string
	store     *storage.FileStore
	snaps     []domain.Snapshot
	docs      map[domain.ContentHash]domain.SessionDoc
	memories  map[domain.ContentHash]domain.MemoryDigest
	ref       domain.Ref
	localRefs []domain.Ref
}

func newMetadataCheckpointFixture(t *testing.T) *metadataCheckpointFixture {
	t.Helper()
	f := &metadataCheckpointFixture{
		root: t.TempDir(), key: filepath.Join(t.TempDir(), "private", "key"),
		repo: string(domain.HashContent([]byte(t.Name()))),
		docs: map[domain.ContentHash]domain.SessionDoc{}, memories: map[domain.ContentHash]domain.MemoryDigest{},
	}
	for _, text := range []string{"checkpoint parent transcript", "checkpoint child transcript"} {
		doc := pullDoc(t, text)
		f.docs[doc.Hash] = doc
		snap := domain.Snapshot{RepoID: f.repo, ID: doc.Hash, DocHash: doc.Hash, Branch: "feature"}
		if len(f.snaps) != 0 {
			snap.Parents = []domain.ContentHash{f.snaps[0].ID}
		}
		f.snaps = append(f.snaps, snap)
	}
	f.ref = domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "feature", Target: f.snaps[1].ID}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Error(err)
			}
		}
		man := domain.Manifest{RepoID: f.repo, Refs: []domain.Ref{f.ref}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
		for _, snap := range f.snaps {
			state, err := domain.SnapshotStateHash(snap)
			if err != nil {
				t.Error(err)
				http.Error(w, "invalid fixture snapshot", http.StatusInternalServerError)
				return
			}
			man.SnapshotIndex = append(man.SnapshotIndex, snap.ID)
			man.SnapshotStates[snap.ID] = state
		}
		base := "/repos/" + f.repo
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base:
			write(map[string]any{"id": f.repo, "default_branch": "feature", "branch_pull_version": domain.BranchPullVersion})
		case r.Method == http.MethodGet && r.URL.Path == base+"/manifest":
			f.calls.manifests++
			write(man)
		case r.Method == http.MethodGet && r.URL.Path == base+"/history":
			write([]domain.HistoryEvent{})
		case r.Method == http.MethodPost && r.URL.Path == base+"/pull/branch-plan":
			var req domain.BranchPullRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Version != domain.BranchPullVersion || req.Branch != f.ref.Name {
				t.Errorf("unexpected branch request: %+v, %v", req, err)
				http.Error(w, "invalid branch request", http.StatusBadRequest)
				return
			}
			f.calls.plans++
			write(domain.BranchPullPlan{Version: domain.BranchPullVersion, RepoID: f.repo, Branch: f.ref.Name, SelectedRef: f.ref, Refs: man.Refs, SnapshotIndex: man.SnapshotIndex, SnapshotStates: man.SnapshotStates})
		case r.Method == http.MethodPost && r.URL.Path == base+"/pull/objects":
			var req struct {
				SnapshotWants    []domain.ContentHash `json:"snapshot_wants"`
				DocWants         []domain.ContentHash `json:"doc_wants"`
				DocManifestWants []domain.ContentHash `json:"doc_manifest_wants"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
				http.Error(w, "invalid object request", http.StatusBadRequest)
				return
			}
			if len(req.SnapshotWants) != 0 {
				f.calls.metadata++
				if len(req.DocWants)+len(req.DocManifestWants) != 0 {
					t.Error("metadata discovery requested document bodies")
				}
				var snaps []domain.Snapshot
				for _, id := range req.SnapshotWants {
					for _, snap := range f.snaps {
						if snap.ID == id {
							snaps = append(snaps, snap)
						}
					}
				}
				write(map[string]any{"snapshots": snaps})
				return
			}
			f.calls.documents++
			wants := append(req.DocManifestWants, req.DocWants...)
			if len(wants) == 0 {
				t.Error("unexpected empty document request")
			}
			var docs []domain.SessionDoc
			for _, id := range wants {
				doc, ok := f.docs[id]
				if !ok {
					t.Errorf("unexpected document request: %s", id)
				}
				docs = append(docs, doc)
			}
			write(map[string]any{"docs": docs})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"/memory-objects/"):
			f.calls.memories++
			write(f.memories[domain.ContentHash(strings.TrimPrefix(r.URL.Path, base+"/memory-objects/"))])
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"/memories/"):
			f.calls.memories++
			id := domain.ContentHash(strings.TrimPrefix(r.URL.Path, base+"/memories/"))
			for _, snap := range f.snaps {
				if snap.ID == id {
					write(f.memories[snap.MemoryHash])
					return
				}
			}
			t.Errorf("unexpected memory owner: %s", id)
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected HTTP request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	f.url = server.URL
	f.reopen()
	var err error
	f.localRefs, err = f.store.ListRefs(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *metadataCheckpointFixture) reopen() *backendclient.BackendClient {
	// Use a real worktree-aware store so an absent working position is meaningful.
	f.store = storage.NewWorktreeFileStore(f.root, filepath.Join(f.root, ".git"), "feature", strings.Repeat("a", 40))
	f.store.EnableDocVerificationCache(f.key)
	client := backendclient.NewBackendClient(func() string { return f.url }, func() string { return "" }, domain.TeamIdentity{})
	client.SetMetadataCheckpointStore(f.store)
	return client
}

func (f *metadataCheckpointFixture) takeCalls() metadataCheckpointCalls {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = metadataCheckpointCalls{}
	return calls
}

func assertCheckpointSnapshots(t *testing.T, got, want []domain.Snapshot) {
	t.Helper()
	states := func(snaps []domain.Snapshot) map[domain.ContentHash]domain.ContentHash {
		out := map[domain.ContentHash]domain.ContentHash{}
		for _, snap := range snaps {
			state, err := domain.SnapshotStateHash(snap)
			if err != nil {
				t.Fatal(err)
			}
			out[snap.ID] = state
		}
		return out
	}
	if len(got) != len(want) || !reflect.DeepEqual(states(got), states(want)) {
		t.Fatalf("snapshot selection or metadata changed: got %+v, want %+v", got, want)
	}
}

func (f *metadataCheckpointFixture) discover(t *testing.T, client *backendclient.BackendClient) {
	t.Helper()
	got, err := client.ReadSnapshotCatalog(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointSnapshots(t, got, f.snaps)
	checkpoint, err := f.store.ReadMetadataCheckpoint(context.Background(), f.repo, client.SyncRemoteIdentity())
	if err != nil || checkpoint.Revision == "" {
		t.Fatalf("metadata acquisition was not checkpointed: %+v, %v", checkpoint, err)
	}
	assertCheckpointSnapshots(t, checkpoint.Snapshots, f.snaps)
}

func (f *metadataCheckpointFixture) observations(t *testing.T, client *backendclient.BackendClient) []outbound.RemoteObservation {
	t.Helper()
	full, err := f.store.ReadRemoteObservation(context.Background(), f.repo, client.SyncRemoteIdentity())
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := f.store.ReadScopedRemoteObservation(context.Background(), f.repo, client.SyncRemoteIdentity(), "feature")
	if err != nil {
		t.Fatal(err)
	}
	return []outbound.RemoteObservation{full, scoped}
}

func (f *metadataCheckpointFixture) assertUnobserved(t *testing.T, client *backendclient.BackendClient) {
	t.Helper()
	for _, observation := range f.observations(t, client) {
		if observation.Revision != "" || len(observation.Snapshots)+len(observation.Refs)+len(observation.History) != 0 {
			t.Fatalf("unverified metadata published an observation: %+v", observation)
		}
	}
}

func (f *metadataCheckpointFixture) assertNoAdoption(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	refs, err := f.store.ListRefs(ctx, f.repo)
	if err != nil || !reflect.DeepEqual(refs, f.localRefs) {
		t.Fatalf("local refs adopted: %+v, %v", refs, err)
	}
	if position, err := f.store.GetWorkingPosition(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("working position adopted: %+v, %v", position, err)
	}
	history, err := f.store.ListHistoryEvents(ctx, f.repo)
	if err != nil || len(history) != 0 {
		t.Fatalf("local history adopted: %+v, %v", history, err)
	}
}

func (f *metadataCheckpointFixture) assertObserved(t *testing.T, client *backendclient.BackendClient, branch string, out inbound.SyncOutput, wantPulled int) {
	t.Helper()
	if out.Pulled != wantPulled || len(out.Conflicts) != 0 || !reflect.DeepEqual(out.FetchedRefs, []domain.Ref{f.ref}) {
		t.Fatalf("incomplete fetch result: %+v", out)
	}
	assertCheckpointSnapshots(t, out.FetchedSnapshots, f.snaps)
	for i, observation := range f.observations(t, client) {
		selected := i == 0 && branch == "" || i == 1 && branch != ""
		if !selected {
			if observation.Revision != "" {
				t.Fatalf("fetch published outside its observation scope: %+v", observation)
			}
			continue
		}
		if observation.Revision == "" || !reflect.DeepEqual(observation.Refs, []domain.Ref{f.ref}) {
			t.Fatalf("validated fetch did not publish refs: %+v", observation)
		}
		assertCheckpointSnapshots(t, observation.Snapshots, f.snaps)
	}
	f.assertNoAdoption(t)
}

func TestMetadataCheckpointDiscoveryAndPullDoNotPublish(t *testing.T) {
	f := newMetadataCheckpointFixture(t)
	client := f.reopen()
	f.discover(t, client)
	if calls := f.takeCalls(); calls != (metadataCheckpointCalls{manifests: 1, metadata: 1}) {
		t.Fatalf("cold discovery performed unexpected I/O: %+v", calls)
	}
	f.assertUnobserved(t, client)
	f.assertNoAdoption(t)

	client = f.reopen()
	f.discover(t, client)
	if calls := f.takeCalls(); calls != (metadataCheckpointCalls{manifests: 1}) {
		t.Fatalf("warm discovery did not reuse persisted metadata: %+v", calls)
	}
	// No verified negotiation haves exist. Every snapshot must still be returned,
	// even though discovery has already acquired all of its metadata.
	snaps, docs, refs, err := client.Pull(context.Background(), f.repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointSnapshots(t, snaps, f.snaps)
	if len(docs) != len(f.docs) || !reflect.DeepEqual(refs, []domain.Ref{f.ref}) {
		t.Fatalf("checkpoint suppressed selected objects: docs=%d refs=%+v", len(docs), refs)
	}
	for _, doc := range docs {
		if err := domain.ValidateSessionDocHash(doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.docs[doc.Hash]; !ok {
			t.Fatalf("unexpected document: %s", doc.Hash)
		}
	}
	if calls := f.takeCalls(); calls != (metadataCheckpointCalls{manifests: 1, documents: 1}) {
		t.Fatalf("warm pull did not separate metadata reuse from body transfer: %+v", calls)
	}
	f.assertUnobserved(t, client)
	f.assertNoAdoption(t)
	for _, snap := range f.snaps {
		if _, err := f.store.GetSnapshot(context.Background(), snap.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("metadata-only acquisition adopted snapshot %s: %v", snap.ID, err)
		}
	}
}

func TestMetadataCheckpointFetchRejectsCorruptStoredDocument(t *testing.T) {
	for _, branch := range []string{"", "feature"} {
		t.Run(map[bool]string{true: "repository", false: "selected-branch"}[branch == ""], func(t *testing.T) {
			f := newMetadataCheckpointFixture(t)
			ctx := context.Background()
			client := f.reopen()
			for _, snap := range f.snaps {
				if _, err := f.store.PutDoc(ctx, f.docs[snap.DocHash]); err != nil {
					t.Fatal(err)
				}
				if err := f.store.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
				if err := f.store.VerifyStoredDoc(ctx, snap.DocHash); err != nil {
					t.Fatal(err)
				}
			}
			f.discover(t, client)
			f.takeCalls()
			f.assertUnobserved(t, client)
			id := f.snaps[1].DocHash
			hex := strings.TrimPrefix(string(id), "sha256:")
			if _, err := os.Stat(filepath.Join(f.root, ".cxt", "doc-verification", hex+".json")); err != nil {
				t.Fatal("fixture did not warm document verification", err)
			}
			path := filepath.Join(f.root, ".cxt", "objects", "docs", hex)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			corrupt, err := domain.CanonicalBytes(pullDoc(t, "substituted stored transcript").CIR)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			client = f.reopen()
			input := inbound.SyncInput{RepoID: f.repo, Ref: branch, FetchOnly: true}
			if _, err := newTestSyncService(f.store, client, nil).Pull(ctx, input); !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("cached metadata hid a corrupt stored document: %v", err)
			}
			calls := f.takeCalls()
			if calls.metadata != 0 || calls.documents != 0 || calls.manifests+calls.plans != 1 {
				t.Fatalf("cached document failure performed unexpected I/O: %+v", calls)
			}
			f.assertUnobserved(t, client)
			f.assertNoAdoption(t)

			// Repair only the fixture body; ordinary fetch must now validate the
			// complete cached selection and publish its first verified observation.
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			client = f.reopen()
			out, err := newTestSyncService(f.store, client, nil).Pull(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if calls := f.takeCalls(); calls.metadata != 0 || calls.documents != 0 || calls.manifests+calls.plans != 1 {
				t.Fatalf("repair discarded cached acquisition progress: %+v", calls)
			}
			f.assertObserved(t, client, branch, out, len(f.snaps))
		})
	}
}

func TestMetadataCheckpointFetchValidatesChangedMemory(t *testing.T) {
	for _, branch := range []string{"", "feature"} {
		t.Run(map[bool]string{true: "repository", false: "selected-branch"}[branch == ""], func(t *testing.T) {
			f := newMetadataCheckpointFixture(t)
			ctx := context.Background()
			client := f.reopen()
			f.assertUnobserved(t, client)
			var previous domain.ContentHash
			for _, summary := range []string{"first memory attachment", "changed memory on the same document"} {
				before := f.observations(t, client)
				digest := domain.MemoryDigest{SnapshotID: f.snaps[1].ID, Summary: summary, PreviousMemoryHash: previous}
				hash, err := domain.MemoryDigestHash(digest)
				if err != nil {
					t.Fatal(err)
				}
				bad := digest
				bad.Summary = "tampered attachment body"
				f.mu.Lock()
				f.snaps[1].MemoryHash = hash
				f.memories[hash] = bad
				f.mu.Unlock()
				f.discover(t, client)
				if calls := f.takeCalls(); calls != (metadataCheckpointCalls{manifests: 1, metadata: 1}) {
					t.Fatalf("changed metadata did not require fresh acquisition: %+v", calls)
				}
				if got := f.observations(t, client); !reflect.DeepEqual(got, before) {
					t.Fatal("discovery published unvalidated memory metadata")
				}
				client = f.reopen()
				input := inbound.SyncInput{RepoID: f.repo, Ref: branch, FetchOnly: true}
				if _, err := newTestSyncService(f.store, client, nil).Pull(ctx, input); !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("cached snapshot bypassed memory validation: %v", err)
				}
				wantDocuments := 0
				if previous == "" {
					wantDocuments = len(f.docs)
				}
				if calls := f.takeCalls(); calls.metadata != 0 || calls.documents != wantDocuments || calls.memories != 1 || calls.manifests+calls.plans != 1 {
					t.Fatalf("memory failure did not reuse metadata and validate bodies: %+v", calls)
				}
				if got := f.observations(t, client); !reflect.DeepEqual(got, before) {
					t.Fatal("failed memory validation advanced an observation")
				}
				if _, err := f.store.GetMemory(ctx, hash); !errors.Is(err, domain.ErrNotFound) {
					t.Fatalf("unverified memory was stored: %v", err)
				}
				f.assertNoAdoption(t)
				for _, snap := range f.snaps {
					if previous == "" {
						if _, err := f.store.GetSnapshot(ctx, snap.ID); !errors.Is(err, domain.ErrNotFound) {
							t.Fatalf("failed memory validation published snapshot %s: %v", snap.ID, err)
						}
					}
					if err := f.store.VerifyStoredDoc(ctx, snap.DocHash); err != nil {
						t.Fatal("verified body progress was lost", err)
					}
				}
				f.mu.Lock()
				f.memories[hash] = digest
				f.mu.Unlock()
				client = f.reopen()
				out, err := newTestSyncService(f.store, client, nil).Pull(ctx, input)
				if err != nil {
					t.Fatal(err)
				}
				if calls := f.takeCalls(); calls.metadata != 0 || calls.documents != 0 || calls.memories != 1 || calls.manifests+calls.plans != 1 {
					t.Fatalf("memory retry lost reusable progress: %+v", calls)
				}
				// On the second repository fetch the unchanged parent is omitted by
				// verified negotiation. A scoped fetch reconstructs it from its plan.
				wantPulled := len(f.snaps)
				if previous != "" {
					wantPulled = 1
				}
				f.assertObserved(t, client, branch, out, wantPulled)
				if _, err := f.store.GetMemory(ctx, hash); err != nil {
					t.Fatal("validated attachment was not stored", err)
				}
				if previous != "" {
					local, err := f.store.GetSnapshot(ctx, digest.SnapshotID)
					if err != nil || local.DocHash != digest.SnapshotID || local.MemoryHash != previous {
						t.Fatalf("fetch adopted changed memory over existing metadata: %+v, %v", local, err)
					}
				}
				previous = hash
			}
		})
	}
}
