package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
			capReads, planReads, metadata, bodies, memoryReads := 0, 0, 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+repo):
					capReads++
					_ = json.NewEncoder(w).Encode(map[string]any{"id": repo, "default_branch": "main", "branch_pull_version": 1})
				case strings.HasSuffix(r.URL.Path, "/pull/branch-plan"):
					planReads++
					var request domain.BranchPullRequest
					if json.NewDecoder(r.Body).Decode(&request) != nil || !reflect.DeepEqual(request.ObservationRoots, []domain.ContentHash{stale.ID}) {
						t.Error("candidate roots not forwarded")
					}
					if mode == "denied" {
						w.WriteHeader(403)
						return
					}
					_ = json.NewEncoder(w).Encode(plan)
				case strings.HasSuffix(r.URL.Path, "/pull/objects"):
					var request struct {
						Snapshots []domain.ContentHash `json:"snapshot_wants"`
						Docs      []domain.ContentHash `json:"doc_manifest_wants"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Error("bad wants")
						w.WriteHeader(400)
						return
					}
					if len(request.Snapshots) > 0 {
						metadata++
						if !reflect.DeepEqual(request.Snapshots, []domain.ContentHash{snap.ID}) {
							t.Errorf("unexpected snapshots %v", request.Snapshots)
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": []domain.Snapshot{snap}})
					} else {
						bodies++
						if !reflect.DeepEqual(request.Docs, []domain.ContentHash{doc.Hash}) {
							t.Errorf("unexpected bodies %v", request.Docs)
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"docs": []domain.SessionDoc{doc}})
					}
				case strings.Contains(r.URL.Path, "/memory-objects/"):
					memoryReads++
					if strings.HasSuffix(r.URL.Path, string(ah)) {
						if mode == "missing-memory" {
							w.WriteHeader(404)
							return
						}
						_ = json.NewEncoder(w).Encode(ancestor)
					} else if strings.HasSuffix(r.URL.Path, string(th)) {
						_ = json.NewEncoder(w).Encode(tip)
					} else {
						t.Errorf("unexpected memory %s", r.URL.Path)
						w.WriteHeader(404)
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
			if capReads != 1 || planReads != 1 {
				t.Fatalf("capability/plan reads %d/%d", capReads, planReads)
			}
			if mode == "warm" && (metadata != 0 || bodies != 0 || memoryReads != 0) {
				t.Fatalf("warm I/O metadata/body/memory=%d/%d/%d", metadata, bodies, memoryReads)
			}
			if mode == "warm-missing-ancestor" && (metadata != 0 || bodies != 0 || memoryReads != 1) {
				t.Fatalf("warm repair I/O %d/%d/%d", metadata, bodies, memoryReads)
			}
			if mode == "denied" && (metadata != 0 || bodies != 0 || memoryReads != 0) {
				t.Fatal("denied plan transferred objects")
			}
		})
	}
}
