package backendclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestPushFinalizesParentFirstAndResumesAfterLostAcknowledgement(t *testing.T) {
	repo := string(domain.HashContent([]byte(t.Name())))
	var docs []domain.SessionDoc
	var snaps []domain.Snapshot
	for _, text := range []string{"first", "second", "third"} {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}, Events: []domain.Event{{Seq: 0, Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(raw)
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main"}
		if len(snaps) > 0 {
			snap.Parents = []domain.ContentHash{snaps[len(snaps)-1].ID}
		}
		snaps = append(snaps, snap)
		docs = append(docs, domain.SessionDoc{Hash: id, CIR: cir})
	}
	stored := map[domain.ContentHash]bool{}
	calls, refs := 0, 0
	failed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
			var in negotiateReq
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			out := negotiateResp{}
			for _, id := range in.SnapshotHaves {
				if !stored[id] {
					out.SnapshotWants = append(out.SnapshotWants, id)
				}
			}
			for _, id := range in.DocHaves {
				if !stored[id] {
					out.DocWants = append(out.DocWants, id)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(r.URL.Path, "/push/objects"):
			calls++
			var batch objectsReq
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				t.Error(err)
			}
			if len(batch.Snapshots) != 1 || len(batch.Docs) != 1 || len(batch.ChunkedDocs) != 0 {
				t.Errorf("unbounded/misaligned finalization: snapshots=%d docs=%d manifests=%d", len(batch.Snapshots), len(batch.Docs), len(batch.ChunkedDocs))
				http.Error(w, "bad batch", 422)
				return
			}
			snap := batch.Snapshots[0]
			if snap.DocHash != batch.Docs[0].Hash {
				t.Error("wrong paired document")
			}
			for _, p := range snap.Parents {
				if !stored[p] {
					t.Error("child finalized before parent")
					http.Error(w, "missing parent", 422)
					return
				}
			}
			stored[snap.ID] = true
			if calls == 2 && !failed {
				failed = true
				http.Error(w, "lost acknowledgement after storage", 500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]int{})
		case strings.HasSuffix(r.URL.Path, "/refs"):
			_ = json.NewEncoder(w).Encode([]domain.Ref{})
		case strings.HasSuffix(r.URL.Path, "/refs/batch"):
			refs++
			if len(stored) != 3 {
				t.Error("ref advanced with incomplete object set")
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"applied": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := NewBackendClient(func() string { return server.URL }, func() string { return "" }, domain.TeamIdentity{})
	reversed := []domain.Snapshot{snaps[2], snaps[1], snaps[0]}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: snaps[2].ID}
	if err := c.Push(context.Background(), repo, reversed, docs, []domain.Ref{ref}, false, false); err == nil {
		t.Fatal("failure hidden")
	}
	if calls != 2 || refs != 0 {
		t.Fatalf("failure progressed: objects=%d refs=%d", calls, refs)
	}
	if err := c.Push(context.Background(), repo, reversed, docs, []domain.Ref{ref}, false, false); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || refs != 1 {
		t.Fatalf("resume repeated accepted work or lost ref: objects=%d refs=%d", calls, refs)
	}
}

func TestObjectCommitBatchesRejectsCyclesAndSharesLegacyChunks(t *testing.T) {
	a, b := domain.HashContent([]byte("a")), domain.HashContent([]byte("b"))
	if _, err := objectCommitBatches([]domain.Snapshot{{ID: a, Parents: []domain.ContentHash{b}}, {ID: b, Parents: []domain.ContentHash{a}}}, nil, nil, nil); err == nil {
		t.Fatal("cycle accepted")
	}
	chunk := chunkObjWire{Hash: domain.HashContent([]byte("shared")), Data: []byte("shared")}
	batches, err := objectCommitBatches(nil, nil, []chunkedDocWire{{Hash: a, Chunks: []domain.ContentHash{chunk.Hash}}, {Hash: b, Chunks: []domain.ContentHash{chunk.Hash}}}, []chunkObjWire{chunk})
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[0].ChunkObjects) != 1 || len(batches[1].ChunkObjects) != 0 {
		t.Fatalf("legacy chunk dependencies not ordered: %+v", batches)
	}
}
