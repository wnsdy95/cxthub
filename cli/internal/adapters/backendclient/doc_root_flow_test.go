package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func p4DistinctRoot(t *testing.T) (domain.DocumentRepresentation, map[domain.ContentHash][]byte) {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}}
	for i := 0; i < 7; i++ {
		cir.Events = append(cir.Events, domain.Event{Kind: domain.EventMessage, Seq: i, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(string(rune('a'+i)), domain.ConversationManifestChunkBytes)}}})
	}
	m, b, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	h, err := domain.ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	return domain.DocumentRepresentation{Hash: h, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, b
}

func TestP4RootUpload(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(map[bool]string{true: "empty", false: "bounded"}[empty], func(t *testing.T) {
			rep, bodies := p4Root(t, true)
			if !empty {
				rep, bodies = p4DistinctRoot(t)
			}
			repo := domain.HashContent([]byte("upload repo"))
			jobID, err := domain.RootDocFinalizationID(repo, rep)
			if err != nil {
				t.Fatal(err)
			}
			m, _ := rep.ConversationManifest()
			var order []domain.ContentHash
			for _, c := range m.Chunks {
				order = append(order, c.Hash)
			}
			received := map[domain.ContentHash]int{}
			batches, jobs := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					json.NewEncoder(w).Encode(map[string]any{"id": repo, "required_doc_identity": rep.Identity, "doc_identities_supported": []domain.DocumentIdentity{rep.Identity}, "root_publication_enabled": true})
				case strings.HasSuffix(r.URL.Path, "/negotiate"):
					var req negotiateReq
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					var wants []domain.ContentHash
					if len(req.ChunkHaves) > 0 {
						wants = order
					}
					json.NewEncoder(w).Encode(negotiateResp{DocWants: []domain.ContentHash{rep.Hash}, ChunkWants: wants, ChunksSupported: true, BoundedChunksSupported: true, AsyncDocsSupported: true, RootPublicationEnabled: true, DocIdentitiesSupported: []domain.DocumentIdentity{rep.Identity}, ChunkFormatsSupported: []string{m.ChunkFormat}})
				case strings.HasSuffix(r.URL.Path, "/chunks"):
					var batch chunksReq
					if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
						t.Error(err)
					}
					total := 0
					for _, c := range batch.Chunks {
						total += len(c.Data)
						received[c.Hash]++
						if !bytes.Equal(c.Data, bodies[c.Hash]) {
							t.Error("wrong body")
						}
					}
					if len(batch.Chunks) > maxChunkWireObjects || total > maxChunkWireRawBytes {
						t.Error("unbounded batch")
					}
					batches++
				case strings.HasSuffix(r.URL.Path, "/doc-jobs"):
					var got domain.DocumentRepresentation
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
					}
					if got.DocumentRef() != rep.DocumentRef() || !bytes.Equal(got.RootManifest, rep.RootManifest) || len(received) != len(bodies) {
						t.Error("finalized before exact bodies")
					}
					jobs++
					json.NewEncoder(w).Encode(docJobStatus{ID: jobID, DocHash: rep.Hash, DocIdentity: rep.Identity, State: "completed"})
				default:
					t.Error("unexpected endpoint", r.URL.Path)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
			ok, err := c.PushDocChunks(context.Background(), repo, outbound.DocumentChunks{Representation: rep, ReadChunk: func(_ context.Context, h domain.ContentHash) ([]byte, error) { return bodies[h], nil }})
			if !ok || err != nil || jobs != 1 {
				t.Fatalf("upload %v %v jobs=%d", ok, err, jobs)
			}
			if !empty && batches < 2 {
				t.Fatal("fixture did not cross bounded upload batches")
			}
			if empty && batches != 0 {
				t.Fatal("empty root uploaded chunks")
			}
			for _, n := range received {
				if n != 1 {
					t.Fatal("duplicate chunk upload")
				}
			}
		})
	}
}

type p4RootReceiver struct {
	store *storage.FileStore
	calls int
}

func (r *p4RootReceiver) HasVerifiedDoc(ctx context.Context, ref domain.DocumentRef) (bool, error) {
	err := r.store.VerifyStoredDocReference(ctx, ref)
	if errors.Is(err, domain.ErrNotFound) {
		exists, herr := r.store.HasDoc(ctx, ref.Hash)
		if herr != nil {
			return false, herr
		}
		if !exists {
			return false, nil
		}
	}
	return err == nil, err
}
func (r *p4RootReceiver) ReceiveDoc(context.Context, domain.SessionDoc) error {
	return domain.ErrUnsupportedDocumentIdentity
}
func (r *p4RootReceiver) ReceiveRoot(ctx context.Context, rep domain.DocumentRepresentation) error {
	r.calls++
	return r.store.PutConversationManifest(ctx, rep)
}

func TestP4RootBoundedPull(t *testing.T) {
	for _, mode := range []string{"good", "empty", "missing-sink", "corrupt-wire", "cancel", "endpoint-change", "corrupt-existing"} {
		t.Run(mode, func(t *testing.T) {
			rep, bodies := p4Root(t, true)
			if mode != "empty" {
				rep, bodies = p4DistinctRoot(t)
			}
			dir := t.TempDir()
			store := storage.NewFileStore(dir)
			receiver := &p4RootReceiver{store: store}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests, batches := 0, 0
			var endpointChanged atomic.Bool
			if mode == "corrupt-existing" {
				for h := range bodies {
					path := filepath.Join(dir, ".cxt", "objects", "chunks", strings.TrimPrefix(h, "sha256:"))
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if strings.HasSuffix(r.URL.Path, "/objects") {
					json.NewEncoder(w).Encode(pullResp{BoundedChunksSupported: true, DocManifests: []chunkedDocWire{rep}})
					return
				}
				var req pullReq
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if len(req.ChunkWants) > maxChunkWireObjects {
					t.Error("unbounded chunk count")
				}
				var out pullResp
				total := 0
				for _, h := range req.ChunkWants {
					b := bodies[h]
					if total+len(b) > maxChunkWireRawBytes {
						break
					}
					total += len(b)
					out.ChunkObjects = append(out.ChunkObjects, chunkObjWire{Hash: h, Data: b})
				}
				if mode == "corrupt-wire" && len(out.ChunkObjects) > 0 {
					out.ChunkObjects[0].Data = []byte("bad")
				}
				if mode == "cancel" {
					cancel()
				}
				if mode == "endpoint-change" {
					endpointChanged.Store(true)
				}
				batches++
				json.NewEncoder(w).Encode(out)
			}))
			defer server.Close()
			c := NewBackendClient(func() string {
				if endpointChanged.Load() {
					return server.URL + "/changed"
				}
				return server.URL
			}, func() string { return "" }, domain.TeamIdentity{})
			if mode != "missing-sink" {
				c.SetChunkLocal(store)
			}
			err := c.pullRootDocument(ctx, string(domain.HashContent([]byte("pull repo"))), rep.DocumentRef(), receiver)
			good := mode == "good" || mode == "empty"
			if (err == nil) != good {
				t.Fatalf("pull: %v", err)
			}
			exists, herr := store.HasDoc(context.Background(), rep.Hash)
			if herr != nil || exists != good {
				t.Fatalf("descriptor visible=%v err=%v", exists, herr)
			}
			if good {
				if err := store.VerifyStoredDocReference(context.Background(), rep.DocumentRef()); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "good" && batches < 2 {
				t.Fatal("fixture did not exercise multiple batches")
			}
			if mode == "missing-sink" && (requests != 0 || receiver.calls != 0) {
				t.Fatal("download before safe sink")
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestP4WireLegacyUnknownPreserved(t *testing.T) {
	var rep chunkedDocWire
	raw := `{"hash":"first","HASH":"second","future":{"ignored":true},"chunks":[]}`
	if err := json.Unmarshal([]byte(raw), &rep); err != nil || rep.Hash != "second" {
		t.Fatalf("legacy behavior changed: %#v %v", rep, err)
	}
	root, _ := p4Root(t, true)
	b, _ := json.Marshal(root)
	for _, bad := range [][]byte{append([]byte(`{"identity":"",`), b[1:]...), bytes.Replace(b, []byte(`"root_manifest"`), []byte(`"ROOT_MANIFEST"`), 1)} {
		if err := json.Unmarshal(bad, &rep); err == nil {
			t.Fatal("ambiguous root wire accepted")
		}
	}
}

func TestP4RootPullToResumeAndWarmVerify(t *testing.T) {
	rep, bodies := p4Root(t, false)
	m, _ := rep.ConversationManifest()
	repo := domain.HashContent([]byte(t.Name()))
	snap := domain.Snapshot{ID: rep.Hash, DocHash: rep.Hash, DocIdentity: rep.Identity, RepoID: repo, Branch: "main"}
	state, err := domain.SnapshotStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	man := domain.Manifest{RepoID: repo, SnapshotIndex: []domain.ContentHash{snap.ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{snap.ID: state}, Refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snap.ID}}}
	var fail atomic.Bool
	fail.Store(true)
	first := m.Chunks[0].Hash
	counts := map[domain.ContentHash]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			json.NewEncoder(w).Encode(man)
			return
		}
		var req pullReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.SnapshotWants) > 0 {
			json.NewEncoder(w).Encode(pullResp{Snapshots: []domain.Snapshot{snap}})
			return
		}
		if len(req.DocManifestWants) > 0 {
			json.NewEncoder(w).Encode(pullResp{BoundedChunksSupported: true, DocManifests: []chunkedDocWire{rep}})
			return
		}
		if len(req.ChunkWants) == 0 {
			t.Error("unexpected pull")
			w.WriteHeader(400)
			return
		}
		h := req.ChunkWants[0]
		counts[h]++
		if fail.Load() && h != first {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(pullResp{ChunkObjects: []chunkObjWire{{Hash: h, Data: bodies[h]}}})
	}))
	defer server.Close()
	dir := t.TempDir()
	client := func() (*BackendClient, *p4RootReceiver) {
		store := storage.NewFileStore(dir)
		c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
		c.SetChunkLocal(store)
		return c, &p4RootReceiver{store: store}
	}
	c, r := client()
	snaps, refs, err := c.PullTo(context.Background(), repo, nil, nil, r)
	if err == nil || len(snaps) != 0 || len(refs) != 0 || !r.store.HasChunk(first) {
		t.Fatal("interruption exposed metadata or lost staged body", err)
	}
	if has, _ := r.store.HasDoc(context.Background(), rep.Hash); has {
		t.Fatal("partial root installed")
	}
	fail.Store(false)
	c, r = client()
	snaps, refs, err = c.PullTo(context.Background(), repo, nil, nil, r)
	if err != nil || len(snaps) != 1 || len(refs) != 1 || snaps[0].DocumentRef() != rep.DocumentRef() || counts[first] != 1 {
		t.Fatal("resume identity/progress", err)
	}
	previous := 0
	for _, n := range counts {
		previous += n
	}
	c, r = client()
	if _, _, err := c.PullTo(context.Background(), repo, man.SnapshotStates, nil, r); err != nil || r.calls != 0 {
		t.Fatal("warm installed again", err)
	}
	path := filepath.Join(dir, ".cxt", "objects", "chunks", strings.TrimPrefix(first, "sha256:"))
	if err := os.WriteFile(path, []byte("corrupt-current"), 0600); err != nil {
		t.Fatal(err)
	}
	c, r = client()
	if _, _, err := c.PullTo(context.Background(), repo, man.SnapshotStates, nil, r); err == nil {
		t.Fatal("warm root trusted old proof")
	}
	now := 0
	for _, n := range counts {
		now += n
	}
	if now != previous {
		t.Fatal("warm corruption implicitly repaired")
	}
}
