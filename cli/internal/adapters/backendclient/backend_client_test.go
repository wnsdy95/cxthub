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

func TestCompareAndDeletePendingRemoteSendsExpectedTarget(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	expected := domain.HashContent([]byte("expected pending"))
	const sessionID = "session/with slash"
	var gotMethod, gotPath, gotExpected string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotExpected = r.URL.Query().Get("expect")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "kept"})
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	deleted, err := c.CompareAndDeletePendingRemote(context.Background(), repo, sessionID, expected)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("kept response reported deletion")
	}
	if gotMethod != http.MethodDelete || !strings.HasSuffix(gotPath, "/pending/session%2Fwith%20slash") || gotExpected != string(expected) {
		t.Fatalf("request method=%q path=%q expect=%q", gotMethod, gotPath, gotExpected)
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
	if !ok || len(plan.Order) < 2 {
		t.Fatalf("chunk plan ok=%v chunks=%d, want multiple", ok, len(plan.Order))
	}
	return doc, plan
}

func makePullClientDoc(t *testing.T, repoID string) (domain.Snapshot, domain.SessionDoc) {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, GitBranch: "main"},
		Events:   []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "pull delta"}}}},
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: cir}
	snap := domain.Snapshot{ID: doc.Hash, RepoID: repoID, Branch: "main", DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: "commit"}
	return snap, doc
}

func TestPullSkipsUnchangedSnapshotMetadata(t *testing.T) {
	repoID := string(domain.HashContent([]byte("delta-pull-repo")))
	const snapshotCount = 301
	index := make([]domain.ContentHash, 0, snapshotCount)
	states := make(map[domain.ContentHash]domain.ContentHash, snapshotCount)
	for i := 0; i < snapshotCount; i++ {
		id := domain.HashContent([]byte{byte(i >> 8), byte(i)})
		snap := domain.Snapshot{ID: id, RepoID: repoID, Branch: "main", DocHash: id, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, Message: "commit"}
		state, err := domain.SnapshotStateHash(snap)
		if err != nil {
			t.Fatal(err)
		}
		index = append(index, id)
		states[id] = state
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: index[len(index)-1]}
	objectCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(domain.Manifest{
				RepoID: repoID, SnapshotIndex: index, SnapshotStates: states, Refs: []domain.Ref{ref},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull/objects"):
			objectCalls++
			http.Error(w, "unchanged pull requested objects", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	snaps, docs, refs, err := c.Pull(context.Background(), repoID, states, index)
	if err != nil {
		t.Fatal(err)
	}
	if objectCalls != 0 || len(snaps) != 0 || len(docs) != 0 || len(refs) != 1 || refs[0].Target != ref.Target {
		t.Fatalf("calls=%d snapshots=%d docs=%d refs=%+v", objectCalls, len(snaps), len(docs), refs)
	}
}

func TestPullFetchesChangedStateAndVerifiesManifestToken(t *testing.T) {
	repoID := string(domain.HashContent([]byte("changed-pull-repo")))
	snap, _ := makePullClientDoc(t, repoID)
	snap.Grafted = true
	snap.GraftParents = []domain.ContentHash{domain.HashContent([]byte("graft-parent"))}
	snap.GraftSeq = 3
	state, err := domain.SnapshotStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	localState := domain.HashContent([]byte("old-state"))
	var wants []domain.ContentHash
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(domain.Manifest{RepoID: repoID, SnapshotIndex: []domain.ContentHash{snap.ID}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{snap.ID: state}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull/objects"):
			var req pullReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			wants = append(wants, req.SnapshotWants...)
			_ = json.NewEncoder(w).Encode(pullResp{Snapshots: []domain.Snapshot{snap}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	snaps, docs, _, err := c.Pull(context.Background(), repoID, map[domain.ContentHash]domain.ContentHash{snap.ID: localState}, []domain.ContentHash{snap.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(wants) != 1 || wants[0] != snap.ID || len(snaps) != 1 || len(docs) != 0 {
		t.Fatalf("wants=%v snapshots=%d docs=%d", wants, len(snaps), len(docs))
	}

	// A state catalog is an integrity commitment, not merely a cache hint.
	snap.Message = "tampered after manifest"
	if _, _, _, err := c.Pull(context.Background(), repoID, map[domain.ContentHash]domain.ContentHash{snap.ID: localState}, []domain.ContentHash{snap.ID}); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("mismatched state response error = %v", err)
	}
}

func TestPullLegacyManifestFallsBackAndMissingDocForcesSnapshot(t *testing.T) {
	repoID := string(domain.HashContent([]byte("legacy-pull-repo")))
	snap, doc := makePullClientDoc(t, repoID)
	state, err := domain.SnapshotStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "missing-doc"}[modern], func(t *testing.T) {
			snapshotCalls, docCalls := 0, 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest"):
					manifest := domain.Manifest{RepoID: repoID, SnapshotIndex: []domain.ContentHash{snap.ID}}
					if modern {
						manifest.SnapshotStates = map[domain.ContentHash]domain.ContentHash{snap.ID: state}
					}
					_ = json.NewEncoder(w).Encode(manifest)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pull/objects"):
					var req pullReq
					_ = json.NewDecoder(r.Body).Decode(&req)
					switch {
					case len(req.SnapshotWants) > 0:
						snapshotCalls++
						_ = json.NewEncoder(w).Encode(pullResp{Snapshots: []domain.Snapshot{snap}})
					case len(req.DocManifestWants) > 0:
						docCalls++
						_ = json.NewEncoder(w).Encode(pullResp{Docs: []domain.SessionDoc{doc}})
					default:
						http.Error(w, "unexpected pull request", http.StatusBadRequest)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()

			c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
			localStates := map[domain.ContentHash]domain.ContentHash{snap.ID: state}
			docHaves := []domain.ContentHash{snap.ID}
			if modern {
				docHaves = nil
			}
			snaps, docs, _, err := c.Pull(context.Background(), repoID, localStates, docHaves)
			if err != nil {
				t.Fatal(err)
			}
			if snapshotCalls != 1 || len(snaps) != 1 {
				t.Fatalf("snapshot calls=%d snapshots=%d", snapshotCalls, len(snaps))
			}
			if modern && (docCalls != 1 || len(docs) != 1) {
				t.Fatalf("missing-doc repair calls=%d docs=%d", docCalls, len(docs))
			}
			if !modern && (docCalls != 0 || len(docs) != 0) {
				t.Fatalf("legacy existing-doc calls=%d docs=%d", docCalls, len(docs))
			}
		})
	}
}

func TestRemoteManifestRejectsPartialSnapshotStates(t *testing.T) {
	repoID := string(domain.HashContent([]byte("partial-state-repo")))
	a := domain.HashContent([]byte("a"))
	b := domain.HashContent([]byte("b"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(domain.Manifest{
			RepoID: repoID, SnapshotIndex: []domain.ContentHash{a, b},
			SnapshotStates: map[domain.ContentHash]domain.ContentHash{a: domain.HashContent([]byte("state-a"))},
		})
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if _, err := c.RemoteManifest(context.Background(), repoID); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("partial state catalog error = %v", err)
	}
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
				ChunkFormatsSupported: []string{chunkcas.FormatV1, chunkcas.FormatV2},
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
	if committed.ChunkedDocs[0].Format != chunkcas.FormatV2 || len(plan.Order) < 2 {
		t.Fatalf("format=%q test chunks=%d", committed.ChunkedDocs[0].Format, len(plan.Order))
	}
}

func TestNegotiatePushObjectsUsesHashOnlyInventory(t *testing.T) {
	repoID := domain.HashContent([]byte("lazy-negotiate-repo"))
	snapshot := domain.HashContent([]byte("lazy-negotiate-snapshot"))
	doc := domain.HashContent([]byte("lazy-negotiate-doc"))
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/repos/"+string(repoID)+"/push/negotiate" {
			http.NotFound(w, r)
			return
		}
		var req negotiateReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode negotiate: %v", err)
		}
		if len(req.SnapshotHaves) != 1 || req.SnapshotHaves[0] != snapshot || len(req.DocHaves) != 1 || req.DocHaves[0] != doc || len(req.ChunkHaves) != 0 {
			t.Errorf("inventory snapshots=%v docs=%v chunks=%v", req.SnapshotHaves, req.DocHaves, req.ChunkHaves)
		}
		_ = json.NewEncoder(w).Encode(negotiateResp{SnapshotWants: []domain.ContentHash{snapshot}, DocWants: []domain.ContentHash{doc}})
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	wants, err := c.NegotiatePushObjects(context.Background(), string(repoID), []domain.ContentHash{snapshot}, []domain.ContentHash{doc})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || len(wants.Snapshots) != 1 || wants.Snapshots[0] != snapshot || len(wants.Docs) != 1 || wants.Docs[0] != doc {
		t.Fatalf("requests=%d wants=%+v", requests, wants)
	}
}

func TestNegotiatePushObjectsRejectsWantOutsideInventory(t *testing.T) {
	repoID := domain.HashContent([]byte("untrusted-negotiate-repo"))
	offered := domain.HashContent([]byte("offered-snapshot"))
	unoffered := domain.HashContent([]byte("unoffered-snapshot"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(negotiateResp{SnapshotWants: []domain.ContentHash{unoffered}})
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if _, err := c.NegotiatePushObjects(context.Background(), string(repoID), []domain.ContentHash{offered}, nil); !errors.Is(err, domain.ErrHashMismatch) {
		t.Fatalf("negotiate error=%v, want hash mismatch", err)
	}
}

func TestRefOnlyPushSkipsObjectNegotiation(t *testing.T) {
	repoID := domain.HashContent([]byte("ref-only-push-repo"))
	target := domain.HashContent([]byte("ref-only-push-target"))
	negotiateCalls, refCalls := 0, 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
			negotiateCalls++
			http.Error(w, "unexpected object negotiation", http.StatusInternalServerError)
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/refs/branch/main"):
			refCalls++
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: string(repoID), Target: target}
	if err := c.Push(context.Background(), string(repoID), nil, nil, []domain.Ref{ref}, false, false); err != nil {
		t.Fatal(err)
	}
	if negotiateCalls != 0 || refCalls != 1 {
		t.Fatalf("calls negotiate=%d refs=%d", negotiateCalls, refCalls)
	}
}

func TestPushRejectsCIRV2BeforeMutatingOldServer(t *testing.T) {
	repoID := domain.HashContent([]byte("cir-v2-old-server"))
	cir := domain.CIRDocument{
		Envelope: domain.Envelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex},
		Events:   []domain.Event{{Kind: domain.EventCompaction, Replacement: []domain.Event{}, ReplacementComplete: true}},
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: cir}
	snap := domain.Snapshot{ID: doc.Hash, RepoID: string(repoID), Branch: "main", DocHash: doc.Hash}
	objectCalls, refCalls := 0, 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/push/negotiate"):
			var req negotiateReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			// Pre-CIR-negotiation server: wants the objects but omits capability.
			_ = json.NewEncoder(w).Encode(negotiateResp{SnapshotWants: req.SnapshotHaves, DocWants: req.DocHaves})
		case strings.HasSuffix(r.URL.Path, "/push/objects"):
			objectCalls++
			_ = json.NewEncoder(w).Encode(map[string]int{})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/refs/"):
			refCalls++
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	ref := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: string(repoID), Target: doc.Hash}
	err = c.Push(context.Background(), string(repoID), []domain.Snapshot{snap}, []domain.SessionDoc{doc}, []domain.Ref{ref}, false, false)
	if !errors.Is(err, domain.ErrUnsupportedCIRVersion) {
		t.Fatalf("push error = %v, want unsupported CIR version", err)
	}
	if objectCalls != 0 || refCalls != 0 {
		t.Fatalf("old server was mutated before version rejection: objects=%d refs=%d", objectCalls, refCalls)
	}
}

func TestPushFallsBackToFullDocForChunkAwareV1Server(t *testing.T) {
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
	if pushChunksCalled || len(committed.Docs) != 1 || len(committed.ChunkObjects) != 0 || len(committed.ChunkedDocs) != 0 {
		t.Fatalf("v1 fallback pushChunks=%v docs=%d manifests=%d chunks=%d", pushChunksCalled, len(committed.Docs), len(committed.ChunkedDocs), len(committed.ChunkObjects))
	}
	_ = plan
}

func TestPushUsesV2ForOversizedSingleEvent(t *testing.T) {
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
			if len(req.ChunkHaves) < 2 {
				t.Errorf("oversized doc advertised only %d chunk haves", len(req.ChunkHaves))
			}
			_ = json.NewEncoder(w).Encode(negotiateResp{
				SnapshotWants: req.SnapshotHaves, DocWants: req.DocHaves, ChunkWants: req.ChunkHaves,
				ChunksSupported: true, BoundedChunksSupported: true,
				ChunkFormatsSupported: []string{chunkcas.FormatV1, chunkcas.FormatV2},
			})
		case "/repos/" + string(repoID) + "/push/chunks":
			pushChunksCalled = true
			_ = json.NewEncoder(w).Encode(map[string]int{})
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
	format := ""
	if len(committed.ChunkedDocs) == 1 {
		format = committed.ChunkedDocs[0].Format
	}
	if !pushChunksCalled || len(committed.Docs) != 0 || len(committed.ChunkedDocs) != 1 || format != chunkcas.FormatV2 || len(committed.ChunkObjects) != 0 {
		t.Fatalf("oversized v2 pushChunks=%v docs=%d manifests=%d chunks=%d format=%q", pushChunksCalled, len(committed.Docs), len(committed.ChunkedDocs), len(committed.ChunkObjects), format)
	}
}

func TestPushFallsBackToFullDocForOversizedEventOnV1Server(t *testing.T) {
	repoID := domain.HashContent([]byte("oversized-event-old-server"))
	cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1"}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", chunkcas.MaxPortableChunkBytes+1)}}}}}
	canonical, _ := domain.CanonicalBytes(cir)
	doc := domain.SessionDoc{Hash: domain.HashContent(canonical), CIR: cir}
	snap := domain.Snapshot{ID: doc.Hash, RepoID: string(repoID), Branch: "main", DocHash: doc.Hash}
	var committed objectsReq
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + string(repoID) + "/push/negotiate":
			var req negotiateReq
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(negotiateResp{SnapshotWants: req.SnapshotHaves, DocWants: req.DocHaves, ChunkWants: req.ChunkHaves, ChunksSupported: true, BoundedChunksSupported: true})
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
	if len(committed.Docs) != 1 || len(committed.ChunkedDocs) != 0 {
		t.Fatalf("old-server fallback docs=%d manifests=%d", len(committed.Docs), len(committed.ChunkedDocs))
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
			if !containsString(req.ChunkFormatsSupported, chunkcas.FormatV2) {
				t.Errorf("client formats=%v", req.ChunkFormatsSupported)
			}
			if !domain.SupportsCIRVersion(req.CIRVersionsSupported, domain.CIRVersionV2) {
				t.Errorf("client CIR versions=%v", req.CIRVersionsSupported)
			}
			_ = json.NewEncoder(w).Encode(pullResp{
				DocManifests:           []chunkedDocWire{{Hash: doc.Hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}},
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
	wantCalls := (len(plan.Order) + 1) / 2
	if pullCalls != wantCalls {
		t.Fatalf("pull/chunks calls=%d, want %d", pullCalls, wantCalls)
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

func TestMemoryClientUsesCausalAttachmentAndObjectEndpoints(t *testing.T) {
	repoID := domain.HashContent([]byte("memory-client-repo"))
	snapshotID := domain.HashContent([]byte("memory-client-snapshot"))
	previous := domain.HashContent([]byte("memory-client-previous"))
	digest := domain.MemoryDigest{
		SnapshotID: snapshotID, PreviousMemoryHash: previous, Summary: "causal memory", Provider: domain.ProviderCodex,
	}
	digestHash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		t.Fatal(err)
	}
	var putPath, getPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			putPath = r.URL.Path
			var got domain.MemoryDigest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.PreviousMemoryHash != previous {
				t.Errorf("put digest=%+v err=%v", got, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]domain.ContentHash{"memory_hash": digestHash})
		case http.MethodGet:
			getPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(digest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	c := NewBackendClient(func() string { return ts.URL }, func() string { return "" }, domain.TeamIdentity{})
	if err := c.PushMemory(context.Background(), string(repoID), digest); err != nil {
		t.Fatal(err)
	}
	got, err := c.PullMemoryObject(context.Background(), string(repoID), digestHash)
	if err != nil || got.PreviousMemoryHash != previous {
		t.Fatalf("pull object=%+v err=%v", got, err)
	}
	wantPut := "/repos/" + string(repoID) + "/memory-attachments/" + string(snapshotID)
	wantGet := "/repos/" + string(repoID) + "/memory-objects/" + string(digestHash)
	if putPath != wantPut || getPath != wantGet {
		t.Fatalf("paths put=%q get=%q, want %q / %q", putPath, getPath, wantPut, wantGet)
	}
}
