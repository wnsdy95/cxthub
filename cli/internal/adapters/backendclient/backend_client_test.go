package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/chunkcas"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestOptionalAssetNoContentMapsToNotFound(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if _, err := c.PullSettings(context.Background(), repo, "claude"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PullSettings 204 error = %v, want ErrNotFound", err)
	}
	if _, err := c.PullSecrets(context.Background(), repo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PullSecrets 204 error = %v, want ErrNotFound", err)
	}
}

func TestSnapshotForCreateStripsServerOwnedGraftMetadata(t *testing.T) {
	id := domain.HashContent([]byte("snapshot"))
	parent := domain.HashContent([]byte("parent"))
	in := domain.Snapshot{ID: id, DocHash: id, Grafted: true, GraftParents: []domain.ContentHash{parent}, GraftSeq: 7}

	out := snapshotForCreate(in)
	if out.Grafted || len(out.GraftParents) != 0 || out.GraftSeq != 0 {
		t.Fatalf("server-owned graft metadata leaked into create payload: %+v", out)
	}
	if !in.Grafted || len(in.GraftParents) != 1 || in.GraftSeq != 7 {
		t.Fatalf("input snapshot mutated: %+v", in)
	}
}

func TestGraftSnapshotParentsSendsExpectedSequence(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	id := domain.HashContent([]byte("snapshot"))
	parent := domain.HashContent([]byte("parent"))
	var got struct {
		Parents     []domain.ContentHash `json:"parents"`
		ExpectedSeq *uint64              `json:"expected_seq"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := c.GraftSnapshotParents(context.Background(), repo, id, []domain.ContentHash{parent}, 7); err != nil {
		t.Fatal(err)
	}
	if got.ExpectedSeq == nil || *got.ExpectedSeq != 7 || len(got.Parents) != 1 || got.Parents[0] != parent {
		t.Fatalf("graft wire body=%+v", got)
	}
}

func TestGetSnapshotRemoteValidatesIdentity(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	id := domain.HashContent([]byte("snapshot"))
	snap := domain.Snapshot{ID: id, RepoID: repo, Branch: "main", DocHash: id, GraftSeq: 3}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(snap)
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	got, err := c.GetSnapshotRemote(context.Background(), repo, id)
	if err != nil || got.GraftSeq != 3 {
		t.Fatalf("snapshot=%+v err=%v", got, err)
	}
}

func makeChunkedClientDoc(t *testing.T, events int) (domain.SessionDoc, chunkcas.Plan) {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"},
	}
	for i := 0; i < events; i++ {
		cir.Events = append(cir.Events, domain.Event{
			Kind: domain.EventMessage, Seq: i, Role: "user",
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(string(rune('a'+i)), 600<<10)}},
		})
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: cir}
	plan, ok := chunkcas.PlanDoc(canonical)
	if !ok || len(plan.Order) != events {
		t.Fatalf("chunk plan ok=%v chunks=%d, want %d", ok, len(plan.Order), events)
	}
	return doc, plan
}

func TestChunkUploadBatchesEnforceRawAndCountBounds(t *testing.T) {
	countChunks := make([]chunkObjWire, maxChunkWireObjects+1)
	for i := range countChunks {
		body := []byte{byte(i + 1)}
		countChunks[i] = chunkObjWire{Hash: domain.HashContent(body), Data: body}
	}
	batches, err := chunkUploadBatches(countChunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != maxChunkWireObjects || len(batches[1]) != 1 {
		t.Fatalf("count batches = %v", []int{len(batches[0]), len(batches[1])})
	}

	rawChunks := make([]chunkObjWire, 4)
	for i := range rawChunks {
		body := bytes.Repeat([]byte{byte(i + 1)}, 1<<20)
		rawChunks[i] = chunkObjWire{Hash: domain.HashContent(body), Data: body}
	}
	batches, err = chunkUploadBatches(rawChunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 2 {
		t.Fatalf("raw batches sizes = %d/%d", len(batches[0]), len(batches[1]))
	}

	oversized := bytes.Repeat([]byte{'x'}, maxChunkWireRawBytes+1)
	if _, err := chunkUploadBatches([]chunkObjWire{{Hash: domain.HashContent(oversized), Data: oversized}}); err == nil {
		t.Fatal("oversized single chunk was accepted")
	}
}

func TestPushUploadsBoundedChunksBeforeManifestCommit(t *testing.T) {
	repoID := domain.HashContent([]byte("bounded-push-repo"))
	doc, plan := makeChunkedClientDoc(t, 7)
	snap := domain.Snapshot{ID: doc.Hash, RepoID: string(repoID), Branch: "main", DocHash: doc.Hash, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}
	var batchSizes []int
	var committed objectsReq

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + string(repoID) + "/push/negotiate":
			var req negotiateReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode negotiate: %v", err)
			}
			_ = json.NewEncoder(w).Encode(negotiateResp{
				SnapshotWants: req.SnapshotHaves, DocWants: req.DocHaves, ChunkWants: req.ChunkHaves,
				ChunksSupported: true, BoundedChunksSupported: true,
			})
		case "/repos/" + string(repoID) + "/push/chunks":
			var req chunksReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode chunks: %v", err)
			}
			total := 0
			for _, chunk := range req.Chunks {
				total += len(chunk.Data)
				if domain.HashContent(chunk.Data) != chunk.Hash {
					t.Errorf("bad chunk hash %s", chunk.Hash)
				}
			}
			if len(req.Chunks) > maxChunkWireObjects || total > maxChunkWireRawBytes {
				t.Errorf("unbounded batch count=%d raw=%d", len(req.Chunks), total)
			}
			batchSizes = append(batchSizes, len(req.Chunks))
			_ = json.NewEncoder(w).Encode(map[string]int{"stored": len(req.Chunks), "deduped": 0})
		case "/repos/" + string(repoID) + "/push/objects":
			if len(batchSizes) == 0 {
				t.Error("manifest committed before chunk upload")
			}
			if err := json.NewDecoder(r.Body).Decode(&committed); err != nil {
				t.Errorf("decode objects: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]int{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := c.Push(context.Background(), string(repoID), []domain.Snapshot{snap}, []domain.SessionDoc{doc}, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if len(batchSizes) < 2 {
		t.Fatalf("chunk batches=%v, want multiple bounded requests", batchSizes)
	}
	if len(committed.ChunkedDocs) != 1 || committed.ChunkedDocs[0].Hash != doc.Hash {
		t.Fatalf("manifest commit = %+v", committed.ChunkedDocs)
	}
	if len(committed.ChunkObjects) != 0 || len(committed.Docs) != 0 {
		t.Fatalf("bounded final commit leaked inline bodies: docs=%d chunks=%d", len(committed.Docs), len(committed.ChunkObjects))
	}
	if len(plan.Order) != 7 {
		t.Fatalf("test precondition chunks=%d", len(plan.Order))
	}
}

func TestPushKeepsInlineChunksForChunkAwareLegacyServer(t *testing.T) {
	repoID := domain.HashContent([]byte("legacy-chunk-push-repo"))
	doc, plan := makeChunkedClientDoc(t, 2)
	snap := domain.Snapshot{ID: doc.Hash, RepoID: string(repoID), Branch: "main", DocHash: doc.Hash}
	pushChunksCalled := false
	var committed objectsReq
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + string(repoID) + "/push/negotiate":
			var req negotiateReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(negotiateResp{
				SnapshotWants: req.SnapshotHaves, DocWants: req.DocHaves, ChunkWants: req.ChunkHaves,
				ChunksSupported: true,
			})
		case "/repos/" + string(repoID) + "/push/chunks":
			pushChunksCalled = true
			http.Error(w, "unexpected", http.StatusInternalServerError)
		case "/repos/" + string(repoID) + "/push/objects":
			_ = json.NewDecoder(r.Body).Decode(&committed)
			_ = json.NewEncoder(w).Encode(map[string]int{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := c.Push(context.Background(), string(repoID), []domain.Snapshot{snap}, []domain.SessionDoc{doc}, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if pushChunksCalled || len(committed.ChunkObjects) != len(plan.Order) || len(committed.ChunkedDocs) != 1 {
		t.Fatalf("legacy fallback pushChunks=%v manifests=%d chunks=%d", pushChunksCalled, len(committed.ChunkedDocs), len(committed.ChunkObjects))
	}
}

func TestPushFallsBackToFullDocForOversizedSingleEvent(t *testing.T) {
	repoID := domain.HashContent([]byte("oversized-event-push-repo"))
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"},
		Events: []domain.Event{{
			Kind: domain.EventMessage, Seq: 0, Role: "user",
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", chunkcas.MaxPortableChunkBytes+1)}},
		}},
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: cir}
	snap := domain.Snapshot{ID: doc.Hash, RepoID: string(repoID), Branch: "main", DocHash: doc.Hash}
	pushChunksCalled := false
	var committed objectsReq
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + string(repoID) + "/push/negotiate":
			var req negotiateReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.ChunkHaves) != 0 {
				t.Errorf("oversized doc advertised chunk haves: %d", len(req.ChunkHaves))
			}
			_ = json.NewEncoder(w).Encode(negotiateResp{
				SnapshotWants: req.SnapshotHaves, DocWants: req.DocHaves,
				ChunksSupported: true, BoundedChunksSupported: true,
			})
		case "/repos/" + string(repoID) + "/push/chunks":
			pushChunksCalled = true
			http.Error(w, "unexpected", http.StatusInternalServerError)
		case "/repos/" + string(repoID) + "/push/objects":
			_ = json.NewDecoder(r.Body).Decode(&committed)
			_ = json.NewEncoder(w).Encode(map[string]int{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := c.Push(context.Background(), string(repoID), []domain.Snapshot{snap}, []domain.SessionDoc{doc}, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if pushChunksCalled || len(committed.Docs) != 1 || len(committed.ChunkedDocs) != 0 || len(committed.ChunkObjects) != 0 {
		t.Fatalf("oversized fallback pushChunks=%v docs=%d manifests=%d chunks=%d", pushChunksCalled, len(committed.Docs), len(committed.ChunkedDocs), len(committed.ChunkObjects))
	}
}

func TestPullFetchesBoundedChunkPrefixesUntilComplete(t *testing.T) {
	repoID := domain.HashContent([]byte("bounded-pull-repo"))
	doc, plan := makeChunkedClientDoc(t, 7)
	pullCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + string(repoID) + "/pull/objects":
			var req pullReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.DocManifestWants) != 1 || req.DocManifestWants[0] != doc.Hash {
				t.Errorf("manifest wants=%v", req.DocManifestWants)
			}
			_ = json.NewEncoder(w).Encode(pullResp{
				DocManifests:           []chunkedDocWire{{Hash: doc.Hash, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}},
				BoundedChunksSupported: true,
			})
		case "/repos/" + string(repoID) + "/pull/chunks":
			pullCalls++
			var req pullReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			count := len(req.ChunkWants)
			if count > 2 {
				count = 2 // exercise prefix advancement even when the client asks for more.
			}
			resp := pullResp{}
			for _, hash := range req.ChunkWants[:count] {
				resp.ChunkObjects = append(resp.ChunkObjects, chunkObjWire{Hash: hash, Data: plan.Bodies[hash]})
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	docs, err := c.pullDocs(context.Background(), string(repoID), []domain.ContentHash{doc.Hash}, map[domain.ContentHash]bool{doc.Hash: true})
	if err != nil {
		t.Fatal(err)
	}
	if pullCalls != 4 {
		t.Fatalf("pull/chunks calls=%d, want 4", pullCalls)
	}
	if len(docs) != 1 || docs[0].Hash != doc.Hash || len(docs[0].CIR.Events) != 7 {
		t.Fatalf("pulled docs=%+v", docs)
	}
}

func TestBoundedChunkResponseBodyLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte{' '}, maxChunkWireJSONBody+1))
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	var out pullResp
	err := c.doLimited(context.Background(), http.MethodPost, "/pull/chunks", pullReq{}, &out, maxChunkWireJSONBody)
	if err == nil || !strings.Contains(err.Error(), "exceeds bounded transport limit") {
		t.Fatalf("oversized bounded response err=%v", err)
	}
}
