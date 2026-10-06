package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

func TestScopedFetchDocumentPresenceAndIntegrity(t *testing.T) {
	for _, mode := range []string{"complete", "missing-descriptor", "missing-chunk"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			st := storage.NewFileStore(root)
			repo := string(domain.HashContent([]byte(t.Name())))
			doc := pullDoc(t, "existing immutable document with one removed chunk")
			if _, err := st.PutDoc(ctx, doc); err != nil {
				t.Fatal(err)
			}
			snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo}
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			raw, err := domain.CanonicalBytes(doc.CIR)
			if err != nil {
				t.Fatal(err)
			}
			chunks, ok := chunkcas.PlanDoc(raw)
			if !ok || len(chunks.Order) != 1 {
				t.Fatal("single chunk fixture")
			}
			missing := chunks.Order[0]
			path := filepath.Join(root, ".cxt", "objects", "chunks", strings.TrimPrefix(string(missing), "sha256:"))
			if mode != "complete" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-descriptor" {
				descriptor := filepath.Join(root, ".cxt", "objects", "docs", strings.TrimPrefix(string(doc.Hash), "sha256:"))
				if err := os.Remove(descriptor); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-chunk" {
				if exists, err := st.HasDoc(ctx, doc.Hash); err != nil || !exists {
					t.Fatal("fixture descriptor missing", err)
				}
				if err := st.VerifyStoredDoc(ctx, doc.Hash); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("fixture must have present descriptor with missing chunk", err)
				}
			}
			state, err := domain.SnapshotStateHash(snap)
			if err != nil {
				t.Fatal(err)
			}
			ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "feature", Target: doc.Hash}
			plan := domain.BranchPullPlan{Version: 1, RepoID: repo, Branch: "feature", SelectedRef: ref, Refs: []domain.Ref{ref}, SnapshotIndex: []domain.ContentHash{snap.ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{snap.ID: state}}
			plans := 0
			bodies := 0
			chunksDownloaded := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/"+repo):
					_ = json.NewEncoder(w).Encode(map[string]any{"id": repo, "default_branch": "main", "branch_pull_version": 1})
				case strings.HasSuffix(r.URL.Path, "/pull/branch-plan"):
					plans++
					_ = json.NewEncoder(w).Encode(plan)
				case strings.HasSuffix(r.URL.Path, "/pull/objects") || strings.HasSuffix(r.URL.Path, "/pull/chunks"):
					var req struct {
						Snapshots []domain.ContentHash `json:"snapshot_wants"`
						Docs      []domain.ContentHash `json:"doc_manifest_wants"`
						Chunks    []domain.ContentHash `json:"chunk_wants"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					switch {
					case len(req.Snapshots) > 0:
						_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": []domain.Snapshot{snap}})
					case len(req.Docs) > 0:
						bodies++
						_ = json.NewEncoder(w).Encode(map[string]any{"bounded_chunks_supported": true, "doc_manifests": []any{map[string]any{"hash": doc.Hash, "format": chunks.Manifest.Format, "envelope": chunks.Manifest.Envelope, "chunks": chunks.Manifest.Chunks}}})
					case len(req.Chunks) > 0:
						chunksDownloaded += len(req.Chunks)
						_ = json.NewEncoder(w).Encode(map[string]any{"chunk_objects": []any{map[string]any{"hash": missing, "data": chunks.Bodies[missing]}}})
					default:
						t.Errorf("unexpected request %+v", req)
						w.WriteHeader(500)
					}
				default:
					t.Error("unexpected route", r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			remote := backendclient.NewBackendClient(func() string { return srv.URL }, func() string { return "" }, domain.TeamIdentity{})
			remote.SetChunkLocal(st) // Matches the actual CLI composition root.
			before, err := st.ReadScopedRemoteObservation(ctx, repo, remote.SyncRemoteIdentity(), "feature")
			if err != nil {
				t.Fatal(err)
			}
			result, fetchErr := newTestSyncService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repo, Ref: "feature", FetchOnly: true})
			after, err := st.ReadScopedRemoteObservation(ctx, repo, remote.SyncRemoteIdentity(), "feature")
			if err != nil {
				t.Fatal(err)
			}
			_, chunkErr := os.Stat(path)
			t.Logf("fetch error=%v pulled=%d document-manifest-downloads=%d chunk-downloads=%d chunk-restored=%v observation-advanced=%v", fetchErr, result.Pulled, bodies, chunksDownloaded, chunkErr == nil, after.Revision != before.Revision)
			if plans != 1 {
				t.Fatal("fixture did not reach plan", fetchErr)
			}
			if mode == "missing-chunk" {
				if !errors.Is(fetchErr, domain.ErrHashMismatch) || bodies != 0 || chunksDownloaded != 0 || !os.IsNotExist(chunkErr) || after.Revision != before.Revision {
					t.Fatal("incomplete existing document was repaired/downloaded or observed instead of retaining the verification error")
				}
			} else {
				wantDownloads := 0
				if mode == "missing-descriptor" {
					wantDownloads = 1
				}
				if fetchErr != nil || bodies != wantDownloads || chunksDownloaded != wantDownloads || chunkErr != nil || after.Revision == before.Revision {
					t.Fatal("complete or absent document did not follow the normal transfer contract", fetchErr)
				}
				if err := st.VerifyStoredDoc(ctx, doc.Hash); err != nil {
					t.Fatal("successful transfer left an invalid document", err)
				}
			}
			if _, err := st.GetRef(ctx, repo, domain.RefBranch, "feature"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("fetch adopted the selected branch", err)
			}
		})
	}
}
