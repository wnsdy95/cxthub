package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	for _, name := range []string{"cold", "warm", "missing", "denied"} {
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
			if name == "warm" || name == "denied" {
				if _, err := st.PutDoc(ctx, doc); err != nil {
					t.Fatal(err)
				}
				if err := st.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
			}
			manifest := domain.Manifest{RepoID: repo, SnapshotIndex: []domain.ContentHash{snap.ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{snap.ID: state}, Refs: []domain.Ref{ref}}
			var catalogs, objects, histories atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/manifest") {
					catalogs.Add(1)
					if name == "denied" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					_ = json.NewEncoder(w).Encode(manifest)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/history") {
					histories.Add(1)
					_ = json.NewEncoder(w).Encode([]domain.HistoryEvent{})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pull/objects") {
					objects.Add(1)
					var req struct {
						Snapshots []domain.ContentHash `json:"snapshot_wants"`
						Docs      []domain.ContentHash `json:"doc_manifest_wants"`
					}
					if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
						t.Error("invalid object request")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					switch {
					case len(req.Snapshots) == 1 && req.Snapshots[0] == snap.ID:
						_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": []domain.Snapshot{snap}})
					case len(req.Docs) == 1 && req.Docs[0] == doc.Hash:
						_ = json.NewEncoder(w).Encode(map[string]any{"docs": []domain.SessionDoc{doc}})
					default:
						t.Errorf("unexpected object request: %+v", req)
						w.WriteHeader(http.StatusBadRequest)
					}
					return
				}
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/"+repo) {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(domain.Repo{ID: repo, DefaultBranch: "main"})
			}))
			defer server.Close()
			remote := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			branch := "feature"
			if name == "missing" {
				branch = "gone"
			}
			got, err := newTestSyncService(st, remote, nil).ResolveRemoteBranch(ctx, inbound.SyncInput{RepoID: repo}, branch)
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
				wantObjects := int32(2)
				if name == "warm" {
					wantObjects = 0
				}
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
