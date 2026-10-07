package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestScopedBranchFetchMaterializesOnlyDeclaredServerGraph(t *testing.T) {
	for _, mode := range []string{"cold", "warm", "warm-missing-ancestor", "local-only-graft", "omitted-parent-already-local", "missing-memory", "denied"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repo := string(domain.HashContent([]byte("scoped service repo")))
			st := storage.NewFileStore(t.TempDir())
			doc := pullDoc(t, "selected branch body")
			unrelatedDoc := pullDoc(t, "other branch body")
			unrelated := domain.Snapshot{RepoID: repo, ID: unrelatedDoc.Hash, DocHash: unrelatedDoc.Hash}
			if _, err := st.PutDoc(ctx, unrelatedDoc); err != nil {
				t.Fatal(err)
			}
			if err := st.PutSnapshot(ctx, unrelated); err != nil {
				t.Fatal(err)
			}
			ancestor := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "ancestor"}
			ah, err := domain.MemoryDigestHash(ancestor)
			if err != nil {
				t.Fatal(err)
			}
			tip := domain.MemoryDigest{SnapshotID: doc.Hash, Summary: "tip", PreviousMemoryHash: ah}
			th, err := domain.MemoryDigestHash(tip)
			if err != nil {
				t.Fatal(err)
			}
			snap := domain.Snapshot{RepoID: repo, ID: doc.Hash, DocHash: doc.Hash, MemoryHash: th}
			if mode == "omitted-parent-already-local" {
				snap.Parents = []domain.ContentHash{unrelated.ID}
			}
			state, err := domain.SnapshotStateHash(snap)
			if err != nil {
				t.Fatal(err)
			}
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature", Target: snap.ID}
			stale := domain.Snapshot{RepoID: repo, ID: domain.HashContent([]byte("old candidate")), DocHash: domain.HashContent([]byte("old candidate")), GraftParents: []domain.ContentHash{snap.ID}}
			plan := domain.BranchPullPlan{Version: 1, RepoID: repo, Branch: "feature", SelectedRef: ref, Refs: []domain.Ref{ref}, SnapshotIndex: []domain.ContentHash{snap.ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{snap.ID: state}, AbsentRoots: []domain.ContentHash{stale.ID}}
			var capReads, planReads, metadata, bodies, memoryReads, authorizations atomic.Int32
			base := "/repos/" + repo
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == base:
					capReads.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": repo, "default_branch": "main", "branch_pull_version": 1})
				case r.Method == http.MethodPost && r.URL.Path == base+"/pull/branch-plan":
					planReads.Add(1)
					var request domain.BranchPullRequest
					if json.NewDecoder(r.Body).Decode(&request) != nil || request.Version != domain.BranchPullVersion || request.Branch != "feature" || !reflect.DeepEqual(request.ObservationRoots, []domain.ContentHash{stale.ID}) {
						t.Errorf("unexpected selected branch request: %+v", request)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if mode == "denied" {
						w.WriteHeader(403)
						return
					}
					_ = json.NewEncoder(w).Encode(plan)
				case r.URL.Path == base+"/pull/objects":
					var request struct {
						Snapshots    []domain.ContentHash `json:"snapshot_wants"`
						Docs         []domain.ContentHash `json:"doc_manifest_wants"`
						DocWants     []domain.ContentHash `json:"doc_wants"`
						Chunks       []domain.ContentHash `json:"chunk_wants"`
						ChunkFormats []string             `json:"chunk_formats_supported"`
						CIRVersions  []string             `json:"cir_versions_supported"`
					}
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					if r.Method != http.MethodPost || decoder.Decode(&request) != nil || !reflect.DeepEqual(request.CIRVersions, domain.SupportedCIRVersions()) {
						t.Error("bad wants")
						w.WriteHeader(400)
						return
					}
					switch {
					case len(request.Snapshots)+len(request.Docs)+len(request.DocWants)+len(request.Chunks)+len(request.ChunkFormats) == 0:
						// Permission probes carry no transfer wants and return no objects.
						authorizations.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{})
					case reflect.DeepEqual(request.Snapshots, []domain.ContentHash{snap.ID}) && len(request.Docs)+len(request.DocWants)+len(request.Chunks)+len(request.ChunkFormats) == 0:
						metadata.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": []domain.Snapshot{snap}})
					case reflect.DeepEqual(request.Docs, []domain.ContentHash{doc.Hash}) && len(request.Snapshots)+len(request.DocWants)+len(request.Chunks) == 0:
						bodies.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"docs": []domain.SessionDoc{doc}})
					default:
						t.Errorf("unexpected object request: %+v", request)
						w.WriteHeader(http.StatusBadRequest)
					}
				case r.Method == http.MethodGet && (r.URL.Path == base+"/memory-objects/"+string(ah) || r.URL.Path == base+"/memory-objects/"+string(th)):
					memoryReads.Add(1)
					if r.URL.Path == base+"/memory-objects/"+string(ah) {
						if mode == "missing-memory" {
							w.WriteHeader(404)
							return
						}
						_ = json.NewEncoder(w).Encode(ancestor)
					} else {
						_ = json.NewEncoder(w).Encode(tip)
					}
				default:
					t.Errorf("unscoped or mutable query %s", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			endpoint := remote.SyncRemoteIdentity()
			full, err := st.ReadRemoteObservation(ctx, repo, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			full.Snapshots = []domain.Snapshot{unrelated}
			if err := st.CompareAndSwapRemoteObservation(ctx, "", full); err != nil {
				t.Fatal(err)
			}
			full, err = st.ReadRemoteObservation(ctx, repo, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			warm := mode == "warm" || mode == "warm-missing-ancestor" || mode == "local-only-graft"
			if warm {
				if _, err := st.PutDoc(ctx, doc); err != nil {
					t.Fatal(err)
				}
				if mode != "warm-missing-ancestor" {
					if _, err := st.PutMemory(ctx, ancestor); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := st.PutMemory(ctx, tip); err != nil {
					t.Fatal(err)
				}
				local := snap
				if mode == "local-only-graft" {
					local.GraftParents = []domain.ContentHash{unrelated.ID}
					local.GraftSeq = 2
				}
				if err := st.PutSnapshot(ctx, local); err != nil {
					t.Fatal(err)
				}
				old, err := st.ReadScopedRemoteObservation(ctx, repo, endpoint, "feature")
				if err != nil {
					t.Fatal(err)
				}
				old.Snapshots = []domain.Snapshot{snap, stale}
				old.Refs = []domain.Ref{ref}
				if err := st.CompareAndSwapRemoteObservation(ctx, "", old); err != nil {
					t.Fatal(err)
				}
			}
			before, err := st.ReadScopedRemoteObservation(ctx, repo, endpoint, "feature")
			if err != nil {
				t.Fatal(err)
			}
			result, err := newTestSyncService(st, remote, nil).ResolveRemoteBranchObservation(ctx, inbound.SyncInput{RepoID: repo, ObservationRoots: []domain.ContentHash{stale.ID}}, "feature")
			failure := mode == "omitted-parent-already-local" || mode == "missing-memory" || mode == "denied"
			if failure {
				if err == nil {
					t.Fatal("incomplete/denied plan succeeded")
				}
				after, e := st.ReadScopedRemoteObservation(ctx, repo, endpoint, "feature")
				if e != nil || after.Revision != before.Revision {
					t.Fatalf("failed fetch recorded observation: %v", e)
				}
			} else {
				if err != nil || len(result.Snapshots) != 1 || !reflect.DeepEqual(result.Snapshots[0], snap) {
					t.Fatalf("wrong observed graph: %+v %v", result.Snapshots, err)
				}
				if _, err := st.GetMemory(ctx, ah); err != nil {
					t.Fatal(err)
				}
			}
			after, err := st.ReadRemoteObservation(ctx, repo, endpoint)
			if err != nil || after.Revision != full.Revision {
				t.Fatalf("full repair observation changed: %v", err)
			}
			if _, err := st.GetRef(ctx, repo, domain.RefBranch, "feature"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("fetch adopted branch: %v", err)
			}
			if mode == "local-only-graft" {
				local, err := st.GetSnapshot(ctx, snap.ID)
				if err != nil || local.GraftSeq != 2 {
					t.Fatalf("local graft changed: %+v %v", local, err)
				}
			}
			if capReads.Load() != 1 || planReads.Load() != 1 {
				t.Fatalf("capability/plan reads %d/%d", capReads.Load(), planReads.Load())
			}
			want := map[string][4]int32{
				"cold":                         {0, 1, 1, 2},
				"warm":                         {1, 0, 0, 0},
				"warm-missing-ancestor":        {1, 0, 0, 1},
				"local-only-graft":             {0, 1, 0, 0},
				"omitted-parent-already-local": {0, 1, 1, 0},
				"missing-memory":               {0, 1, 1, 2},
				"denied":                       {0, 0, 0, 0},
			}[mode]
			got := [4]int32{authorizations.Load(), metadata.Load(), bodies.Load(), memoryReads.Load()}
			if got != want {
				t.Fatalf("authorization/metadata/body/memory requests=%v, want %v", got, want)
			}
		})
	}
}
