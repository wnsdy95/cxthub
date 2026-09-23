package backendclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type stagedPullDocs map[domain.ContentHash]domain.SessionDoc

func (s stagedPullDocs) HasVerifiedDoc(_ context.Context, id domain.ContentHash) (bool, error) {
	_, ok := s[id]
	return ok, nil
}
func (s stagedPullDocs) ReceiveDoc(_ context.Context, doc domain.SessionDoc) error {
	s[doc.Hash] = doc
	return nil
}

func TestPullToResumesBodiesWithoutPublishingPartialMetadata(t *testing.T) {
	repo := string(domain.HashContent([]byte("streamed pull")))
	one, doc1 := makePullClientDoc(t, repo)
	doc2 := doc1
	doc2.CIR.Envelope.GitBranch = "second"
	raw, err := domain.CanonicalBytes(doc2.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc2.Hash = domain.HashContent(raw)
	two := one
	two.ID, two.DocHash, two.Parents = doc2.Hash, doc2.Hash, []domain.ContentHash{one.ID}
	docs := map[domain.ContentHash]domain.SessionDoc{doc1.Hash: doc1, doc2.Hash: doc2}
	states := map[domain.ContentHash]domain.ContentHash{}
	for _, s := range []domain.Snapshot{one, two} {
		states[s.ID], err = domain.SnapshotStateHash(s)
		if err != nil {
			t.Fatal(err)
		}
	}
	manifest := domain.Manifest{RepoID: repo, SnapshotIndex: []domain.ContentHash{one.ID, two.ID}, SnapshotStates: states, Refs: []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: two.ID}}}
	receiver := stagedPullDocs{}
	firstReads := 0
	failSecond := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		var req pullReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(req.SnapshotWants) > 0 {
			_ = json.NewEncoder(w).Encode(pullResp{Snapshots: []domain.Snapshot{one, two}})
			return
		}
		if len(req.DocManifestWants) != 1 {
			t.Errorf("unbounded document batch: %+v", req)
			w.WriteHeader(400)
			return
		}
		id := req.DocManifestWants[0]
		if id == one.ID {
			firstReads++
		}
		if id == two.ID {
			if _, ok := receiver[one.ID]; !ok {
				t.Error("previous body still buffered")
			}
			if failSecond {
				w.WriteHeader(503)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(pullResp{Docs: []domain.SessionDoc{docs[id]}})
	}))
	defer server.Close()
	c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
	snaps, refs, err := c.PullTo(context.Background(), repo, nil, nil, receiver)
	if err == nil || len(snaps) != 0 || len(refs) != 0 || len(receiver) != 1 {
		t.Fatalf("partial pull: snapshots=%d refs=%d staged=%d err=%v", len(snaps), len(refs), len(receiver), err)
	}
	failSecond = false
	snaps, refs, err = c.PullTo(context.Background(), repo, nil, nil, receiver)
	if err != nil || len(snaps) != 2 || len(refs) != 1 || len(receiver) != 2 || firstReads != 1 {
		t.Fatalf("resume: snapshots=%d refs=%d staged=%d first reads=%d err=%v", len(snaps), len(refs), len(receiver), firstReads, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.PullTo(ctx, repo, nil, nil, receiver); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pull: %v", err)
	}
}

// Interrupted documents reuse immutable chunks across a client/store restart.
func TestInterruptedPullRetainsVerifiedChunks(t *testing.T) {
	repo := string(domain.HashContent([]byte("resumable chunks")))
	doc, plan := makeChunkedClientDoc(t, 7)
	if len(plan.Order) < 2 {
		t.Fatal("fixture needs multiple chunks")
	}
	calls := map[domain.ContentHash]int{}
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req pullReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.DocManifestWants) > 0 {
			_ = json.NewEncoder(w).Encode(pullResp{BoundedChunksSupported: true, DocManifests: []chunkedDocWire{{Hash: doc.Hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}}})
			return
		}
		if len(req.ChunkWants) == 0 {
			w.WriteHeader(400)
			return
		}
		h := req.ChunkWants[0]
		calls[h]++
		if fail && h != plan.Order[0] {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(pullResp{ChunkObjects: []chunkObjWire{{Hash: h, Data: plan.Bodies[h]}}})
	}))
	defer server.Close()
	root := t.TempDir()
	makeClient := func() *BackendClient {
		c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
		c.SetChunkLocal(storage.NewFileStore(root))
		return c
	}
	if _, err := makeClient().pullDocs(context.Background(), repo, []domain.ContentHash{doc.Hash}, map[domain.ContentHash]bool{doc.Hash: true}); err == nil {
		t.Fatal("interruption ignored")
	}
	if !storage.NewFileStore(root).HasChunk(plan.Order[0]) {
		t.Fatal("verified progress lost")
	}
	fail = false
	docs, err := makeClient().pullDocs(context.Background(), repo, []domain.ContentHash{doc.Hash}, map[domain.ContentHash]bool{doc.Hash: true})
	if err != nil || len(docs) != 1 || docs[0].Hash != doc.Hash {
		t.Fatalf("retry: %v", err)
	}
	if calls[plan.Order[0]] != 1 {
		t.Fatal("downloaded the first chunk again")
	}
	if err = storage.NewFileStore(root).PutChunk(context.Background(), plan.Order[0], []byte("tampered")); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("accepted bad chunk: %v", err)
	}
}
