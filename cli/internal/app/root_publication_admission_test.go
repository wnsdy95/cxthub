package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Real app, local FileStore and HTTP client. Only the remote server is synthetic.
// Count every effect class; successful controls traverse the same route switch.
func TestRootPublicationAdmissionBeforeAppEffects(t *testing.T) {
	for _, route := range []string{"pending", "normal", "strict", "connect", "shadow"} {
		for _, state := range []string{"wanted-off", "existing-off", "new-enabled"} {
			t.Run(route+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				rep, bodies, _ := p4AppRoot(t)
				root := t.TempDir()
				repo := domain.Repo{ID: domain.HashContent([]byte(t.Name())), LocalPath: root, DefaultBranch: "main", ContextProtocol: 1}
				st := storage.NewFileStore(root)
				for h, b := range bodies {
					if err := st.PutChunk(ctx, h, b); err != nil {
						t.Fatal(err)
					}
				}
				if err := st.PutConversationManifest(ctx, rep); err != nil {
					t.Fatal(err)
				}
				setting, err := st.PutSettingsObject(ctx, domain.SettingsBundle{Kind: "claude"})
				if err != nil {
					t.Fatal(err)
				}
				snap := domain.Snapshot{ID: rep.Hash, DocHash: rep.Hash, DocIdentity: rep.Identity, RepoID: repo.ID, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, SessionID: "synthetic", ClaudeSettings: setting}
				if route == "strict" || route == "shadow" {
					snap.Branch = "main"
				}
				if err := st.PutSnapshot(ctx, snap); err != nil {
					t.Fatal(err)
				}
				pending := domain.Pending{RepoID: repo.ID, Provider: domain.ProviderCodex, SessionID: "synthetic", Target: snap.ID}
				if err := st.PutPending(ctx, pending); err != nil {
					t.Fatal(err)
				}
				ref := domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: "B2", Target: snap.ID}
				birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: repo.ID, Kind: "birth", BranchID: "B2", Branch: "main", Target: snap.ID, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(1, 0).UTC()}
				if route == "strict" || route == "shadow" {
					if err := st.PutHistoryEvent(ctx, birth); err != nil {
						t.Fatal(err)
					}
					if err := st.PutRef(ctx, ref); err != nil {
						t.Fatal(err)
					}
				}
				before, err := st.Manifest(ctx, repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				beforePending, err := st.ListPendings(ctx, repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				counts := map[string]int{}
				enabled := state == "new-enabled"
				owned := state == "existing-off"
				var accepted []domain.HistoryEvent
				received := map[domain.ContentHash][]byte{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					var out any = struct{}{}
					path := r.URL.Path
					switch {
					case strings.HasSuffix(path, "/push/negotiate"):
						counts["negotiate"]++
						var req struct {
							Snapshots []domain.ContentHash `json:"snapshot_haves"`
							Docs      []domain.ContentHash `json:"doc_haves"`
							Chunks    []domain.ContentHash `json:"chunk_haves"`
						}
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						var wants []domain.ContentHash
						for _, h := range req.Docs {
							if h != rep.Hash {
								t.Error("unexpected offered doc", h)
							}
							if !owned {
								wants = append(wants, h)
							}
						}
						out = map[string]any{"snapshot_wants": req.Snapshots, "doc_wants": wants, "chunk_wants": req.Chunks, "doc_identities_supported": []domain.DocumentIdentity{rep.Identity}, "root_publication_enabled": enabled, "chunks_supported": true, "bounded_chunks_supported": true, "async_docs_supported": true, "chunk_formats_supported": []string{domain.ConversationManifestChunkFormat}}
					case r.Method == http.MethodGet && strings.HasSuffix(path, "/manifest"):
						out = domain.Manifest{RepoID: repo.ID}
					case r.Method == http.MethodGet && strings.HasSuffix(path, "/history"):
						out = accepted
					case r.Method == http.MethodGet && strings.HasSuffix(path, "/refs"):
						out = []domain.Ref{}
					case r.Method == http.MethodGet:
						out = map[string]any{"id": repo.ID, "default_branch": "main", "context_protocol": 1, "required_doc_identity": rep.Identity, "doc_identities_supported": []domain.DocumentIdentity{rep.Identity}, "root_publication_enabled": enabled}
					case strings.Contains(path, "/settings-objects/"):
						counts["settings"]++
					case strings.HasSuffix(path, "/push/chunks"):
						counts["chunks"]++
						var req struct {
							Chunks []struct {
								Hash domain.ContentHash `json:"hash"`
								Data []byte             `json:"data"`
							} `json:"chunks"`
						}
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						for _, c := range req.Chunks {
							if !bytes.Equal(c.Data, bodies[c.Hash]) {
								t.Error("wrong source chunk")
							}
							received[c.Hash] = c.Data
						}
					case strings.HasSuffix(path, "/push/doc-jobs"):
						counts["jobs"]++
						var got domain.DocumentRepresentation
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Error(err)
						}
						if got.DocumentRef() != rep.DocumentRef() || len(received) != len(bodies) {
							t.Error("job before exact chunks")
						}
						id, err := domain.RootDocFinalizationID(repo.ID, got)
						if err != nil {
							t.Error(err)
						}
						owned = true
						out = domain.DocFinalizationStatus{ID: id, DocHash: rep.Hash, DocIdentity: rep.Identity, State: "completed"}
					case strings.HasSuffix(path, "/push/objects"):
						counts["metadata"]++
						var req struct {
							Snapshots []domain.Snapshot               `json:"snapshots"`
							Docs      []domain.SessionDoc             `json:"docs"`
							Chunked   []domain.DocumentRepresentation `json:"chunked_docs"`
						}
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Error(err)
						}
						if !owned || len(req.Snapshots) == 0 || len(req.Docs)+len(req.Chunked) != 0 {
							t.Error("metadata before owned root or body fallback")
						}
						for _, s := range req.Snapshots {
							if s.DocumentRef() != rep.DocumentRef() {
								t.Error("identity lost")
							}
						}
					case path == "/repos":
						counts["register"]++
						out = repo
					case strings.Contains(path, "/initialization"):
						counts["initialization"]++
						http.Error(w, "unexpected initialization", 500)
						return
					case strings.HasSuffix(path, "/history"):
						counts["history"]++
						var e domain.HistoryEvent
						if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
							t.Error(err)
						}
						accepted = append(accepted, e)
					case strings.Contains(path, "/pending/"):
						if r.Method == http.MethodDelete {
							counts["resolution"]++
							out = map[string]string{"status": "kept"}
						} else {
							counts["pending"]++
						}
					case strings.Contains(path, "/unsync/"):
						counts["unsync"]++
					case strings.HasSuffix(path, "/refs/batch"):
						counts["refs"]++
					default:
						counts["unexpected"]++
						t.Error("unexpected route", r.Method, path)
						http.Error(w, "unexpected", 500)
						return
					}
					if err := json.NewEncoder(w).Encode(out); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				repo.RemoteURL = server.URL + "/team/repo"
				client := backendclient.NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
				client.SetChunkLocal(st)
				svc := NewSyncRepoService(st, client, pushOrderGit{repo: repo}, storage.NewSyncOutbox())
				switch route {
				case "pending":
					_, err = svc.SyncPendings(ctx, inbound.SyncInput{RepoID: repo.ID, PendingSessionID: pending.SessionID}, []inbound.PendingResolution{{SessionID: "old", ExpectedTarget: snap.ID}})
				case "shadow":
					// No target pending: exercise shadow-unsync settings and object selection.
					if err = st.DeletePending(ctx, repo.ID, pending.SessionID); err != nil {
						t.Fatal(err)
					}
					beforePending, err = st.ListPendings(ctx, repo.ID)
					if err != nil {
						t.Fatal(err)
					}
					_, err = svc.SyncPendings(ctx, inbound.SyncInput{RepoID: repo.ID}, nil)
				case "normal":
					_, err = svc.Push(ctx, inbound.SyncInput{RepoID: repo.ID, Cwd: root})
				case "strict":
					_, err = svc.Push(ctx, inbound.SyncInput{RepoID: repo.ID, Cwd: root, Ref: "main"})
				case "connect":
					_, err = svc.ConnectRepository(ctx, repo)
				}
				mu.Lock()
				defer mu.Unlock()
				if state == "wanted-off" {
					if err == nil {
						t.Fatal("wanted root admitted", counts)
					}
					for k, n := range counts {
						if k != "negotiate" && n != 0 {
							t.Fatal("effects before admission", counts)
						}
					}
					after, _ := st.Manifest(ctx, repo.ID)
					before.UpdatedAt, after.UpdatedAt = time.Time{}, time.Time{}
					afterPending, _ := st.ListPendings(ctx, repo.ID)
					if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforePending, afterPending) {
						t.Fatal("rejection changed local metadata or pending")
					}
				} else {
					if err != nil {
						t.Fatal("allowed publication", err, counts)
					}
					if route != "connect" && (counts["settings"] == 0 || counts["metadata"] == 0) {
						t.Fatal("control did not exercise settings and root metadata", counts)
					}
					if route == "connect" && counts["register"] != 1 {
						t.Fatal("connection control did not register", counts)
					}
					if state == "existing-off" && counts["chunks"]+counts["jobs"] != 0 {
						t.Fatal("existing root was prepared again", counts)
					}
					if state == "new-enabled" && route != "connect" && (counts["chunks"] == 0 || counts["jobs"] != 1) {
						t.Fatal("enabled root did not prepare", counts)
					}
				}
				if _, err := st.GetDocReference(ctx, rep.DocumentRef()); err != nil {
					t.Fatal("local root changed", err)
				}
				got, err := st.GetSnapshot(ctx, snap.ID)
				if err != nil || got.DocumentRef() != rep.DocumentRef() {
					t.Fatal("local identity changed", err)
				}
			})
		}
	}
}
