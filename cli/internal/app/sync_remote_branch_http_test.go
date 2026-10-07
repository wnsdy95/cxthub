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

// Exercise the application with the real streaming adapter. A fake RemoteSync
// alone cannot show a second manifest read inside transport negotiation, or an
// absent branch causing unrelated document downloads.
func TestResolveRemoteBranchHTTPNegotiatesOnce(t *testing.T) {
	for _, name := range []string{"cold", "local-only", "warm", "missing", "denied"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo := string(domain.HashContent([]byte("branch HTTP contract")))
			doc := pullDoc(t, "remote branch")
			snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo}
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature", Target: snap.ID, BranchID: "remote-feature"}
			state, err := domain.SnapshotStateHash(snap)
			if err != nil {
				t.Fatal(err)
			}
			st := storage.NewFileStore(t.TempDir())
			if name == "warm" || name == "local-only" || name == "denied" {
				if _, err := st.PutDoc(ctx, doc); err != nil {
					t.Fatal(err)
				}
				if err := st.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
			}
			manifest := domain.Manifest{RepoID: repo, SnapshotIndex: []domain.ContentHash{snap.ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{snap.ID: state}, Refs: []domain.Ref{ref}}
			var catalogs, objects, histories, metadata, bodies, authorizations atomic.Int32
			base := "/repos/" + repo
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == base+"/manifest" {
					catalogs.Add(1)
					if name == "denied" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					_ = json.NewEncoder(w).Encode(manifest)
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == base+"/history" {
					histories.Add(1)
					_ = json.NewEncoder(w).Encode([]domain.HistoryEvent{})
					return
				}
				if r.URL.Path == base+"/pull/objects" {
					objects.Add(1)
					var req struct {
						Snapshots    []domain.ContentHash `json:"snapshot_wants"`
						Docs         []domain.ContentHash `json:"doc_manifest_wants"`
						DocWants     []domain.ContentHash `json:"doc_wants"`
						Chunks       []domain.ContentHash `json:"chunk_wants"`
						ChunkFormats []string             `json:"chunk_formats_supported"`
						CIRVersions  []string             `json:"cir_versions_supported"`
					}
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					if r.Method != http.MethodPost || decoder.Decode(&req) != nil || !reflect.DeepEqual(req.CIRVersions, domain.SupportedCIRVersions()) {
						t.Error("invalid object request")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					switch {
					case len(req.Snapshots)+len(req.Docs)+len(req.DocWants)+len(req.Chunks)+len(req.ChunkFormats) == 0:
						// The warm request authorizes reuse without transferring objects.
						authorizations.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{})
					case reflect.DeepEqual(req.Snapshots, []domain.ContentHash{snap.ID}) && len(req.Docs)+len(req.DocWants)+len(req.Chunks)+len(req.ChunkFormats) == 0:
						metadata.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": []domain.Snapshot{snap}})
					case reflect.DeepEqual(req.Docs, []domain.ContentHash{doc.Hash}) && len(req.Snapshots)+len(req.DocWants)+len(req.Chunks) == 0:
						bodies.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"docs": []domain.SessionDoc{doc}})
					default:
						t.Errorf("unexpected object request: %+v", req)
						w.WriteHeader(http.StatusBadRequest)
					}
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != base {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(domain.Repo{ID: repo, DefaultBranch: "main"})
			}))
			defer server.Close()
			remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			if name == "warm" {
				observation, err := st.ReadRemoteObservation(ctx, repo, remote.SyncRemoteIdentity())
				if err != nil {
					t.Fatal(err)
				}
				observation.Snapshots, observation.Refs = []domain.Snapshot{snap}, []domain.Ref{ref}
				if err := st.CompareAndSwapRemoteObservation(ctx, observation.Revision, observation); err != nil {
					t.Fatal(err)
				}
			}
			branch := "feature"
			if name == "missing" {
				branch = "gone"
			}
			observation, err := newTestSyncService(st, remote, nil).ResolveRemoteBranchObservation(ctx, inbound.SyncInput{RepoID: repo}, branch)
			got := observation.Ref
			if name == "missing" || name == "denied" {
				if err == nil || got.Target != "" || (name == "missing" && !errors.Is(err, domain.ErrNotFound)) {
					t.Fatalf("unavailable branch: %+v %v", got, err)
				}
				if name == "denied" {
					var denied *backendclient.HTTPError
					if !errors.As(err, &denied) || denied.StatusCode() != http.StatusForbidden {
						t.Fatalf("server denial was replaced: %v", err)
					}
				}
				if objects.Load() != 0 || histories.Load() != 0 {
					t.Fatalf("unavailable branch downloaded dependencies: objects=%d history=%d", objects.Load(), histories.Load())
				}
			} else {
				if err != nil || got != ref {
					t.Fatalf("resolved %+v %v, want %+v", got, err, ref)
				}
				if len(observation.Snapshots) != 1 || observation.Snapshots[0].ID != snap.ID {
					t.Fatalf("verified graph missing locally known snapshot: %+v", observation.Snapshots)
				}
				want := map[string][3]int32{
					"cold":       {0, 1, 1},
					"local-only": {0, 1, 0},
					"warm":       {1, 0, 0},
				}[name]
				gotRequests := [3]int32{authorizations.Load(), metadata.Load(), bodies.Load()}
				if gotRequests != want {
					t.Fatalf("authorization/metadata/body requests=%v, want %v", gotRequests, want)
				}
				wantObjects := want[0] + want[1] + want[2]
				if objects.Load() != wantObjects || histories.Load() != 1 {
					t.Fatalf("object/history requests: %d/%d, want %d/1", objects.Load(), histories.Load(), wantObjects)
				}
				if _, err := st.GetDoc(ctx, doc.Hash); err != nil {
					t.Fatal(err)
				}
			}
			if catalogs.Load() != 1 {
				t.Fatalf("manifest reads=%d, want 1", catalogs.Load())
			}
			if _, err := st.GetRef(ctx, repo, domain.RefBranch, "feature"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("lookup adopted local ref: %v", err)
			}
		})
	}
}
