package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func wireChunk(body []byte) inbound.ChunkObject {
	return inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
}

func TestBoundedChunkStorePullAndRepoScope(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("bounded-chunks-repo")
	otherRepo := hh("bounded-chunks-other-repo")
	bindCommitTestRepo(t, st, repo)
	bindCommitTestRepo(t, st, otherRepo)

	// Five 700 KiB chunks force the pull response to stop at the 2 MiB raw limit.
	chunks := make([]inbound.ChunkObject, 0, 5)
	for i := byte(1); i <= 5; i++ {
		chunks = append(chunks, wireChunk(bytes.Repeat([]byte{i}, 700<<10)))
	}
	first, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: chunks[:2]})
	if err != nil || first.Stored != 2 || first.Deduped != 0 {
		t.Fatalf("first StoreChunks = %+v, err=%v", first, err)
	}
	second, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: chunks[2:4]})
	if err != nil || second.Stored != 2 || second.Deduped != 0 {
		t.Fatalf("second StoreChunks = %+v, err=%v", second, err)
	}
	third, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: chunks[4:]})
	if err != nil || third.Stored != 1 || third.Deduped != 0 {
		t.Fatalf("third StoreChunks = %+v, err=%v", third, err)
	}
	dedup, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: chunks[:2]})
	if err != nil || dedup.Stored != 0 || dedup.Deduped != 2 {
		t.Fatalf("dedup StoreChunks = %+v, err=%v", dedup, err)
	}

	wants := make([]domain.ContentHash, 0, len(chunks))
	for _, chunk := range chunks {
		wants = append(wants, chunk.Hash)
	}
	pulled, err := svc.PullChunks(ctx, inbound.PullChunksInput{RepoID: repo, Wants: wants})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulled.ChunkObjects) != 2 {
		t.Fatalf("bounded prefix count = %d, want 2", len(pulled.ChunkObjects))
	}
	for i, got := range pulled.ChunkObjects {
		if got.Hash != wants[i] || !bytes.Equal(got.Data, chunks[i].Data) {
			t.Fatalf("chunk[%d] did not preserve requested prefix/order", i)
		}
	}
	if _, err := svc.PullChunks(ctx, inbound.PullChunksInput{RepoID: otherRepo, Wants: wants[:1]}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-repo PullChunks err=%v, want ErrNotFound", err)
	}

	neg, err := svc.Negotiate(ctx, inbound.PushNegotiateInput{RepoID: repo, ChunkHaves: wants})
	if err != nil {
		t.Fatal(err)
	}
	if !neg.ChunksSupported || !neg.BoundedChunksSupported || len(neg.ChunkWants) != 0 {
		t.Fatalf("negotiate after staging = %+v", neg)
	}
}

func TestBoundedChunkStoreRejectsInvalidBatchBeforeWriting(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("bounded-chunks-invalid-repo")
	bindCommitTestRepo(t, st, repo)

	valid := wireChunk([]byte("valid chunk that must not be partially stored"))
	bad := inbound.ChunkObject{Hash: hh("false chunk claim"), Data: []byte("different body")}
	if _, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{valid, bad}}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("hash mismatch err=%v, want ErrIntegrity", err)
	}
	if _, err := st.GetChunk(ctx, repo, valid.Hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("valid prefix was partially stored: %v", err)
	}

	tooMany := make([]inbound.ChunkObject, inbound.MaxChunkWireObjects+1)
	if _, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: tooMany}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("too many chunks err=%v, want ErrValidation", err)
	}
	oversizedBody := bytes.Repeat([]byte{'x'}, inbound.MaxChunkWireRawBytes+1)
	if _, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: []inbound.ChunkObject{wireChunk(oversizedBody)}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized chunk err=%v, want ErrValidation", err)
	}
}

func TestCommitUsesPreviouslyStagedChunks(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := context.Background()
	repo := hh("bounded-chunks-commit-repo")
	bindCommitTestRepo(t, st, repo)

	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"},
	}
	for i := 0; i < 3; i++ {
		cir.Events = append(cir.Events, domain.CIREvent{
			Kind: domain.EventMessage, Seq: i, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat(string(rune('a'+i)), 600<<10)}},
		})
	}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	docHash := domain.HashContent(canonical)
	plan, ok := domain.PlanDocChunks(canonical)
	if !ok || len(plan.Order) < 2 {
		t.Fatalf("expected multi-chunk plan, ok=%v chunks=%d", ok, len(plan.Order))
	}
	objects := make([]inbound.ChunkObject, 0, len(plan.Order))
	for _, hash := range plan.Order {
		objects = append(objects, inbound.ChunkObject{Hash: hash, Data: plan.Bodies[hash]})
	}
	if _, err := svc.StoreChunks(ctx, inbound.StoreChunksInput{RepoID: repo, Chunks: objects}); err != nil {
		t.Fatalf("stage chunks: %v", err)
	}

	out, err := svc.Commit(ctx, inbound.CommitInput{
		RepoID: repo,
		Snapshots: []domain.Snapshot{{
			ID: docHash, RepoID: repo, Branch: "main", DocHash: docHash,
			Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull,
		}},
		ChunkedDocs: []inbound.ChunkedDoc{{Hash: docHash, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}},
	})
	if err != nil {
		t.Fatalf("commit staged manifest: %v", err)
	}
	if out.StoredDocs != 1 || out.StoredSnapshots != 1 {
		t.Fatalf("commit result = %+v", out)
	}
	got, err := st.GetDoc(ctx, repo, docHash)
	if err != nil || len(got.CIR.Events) != len(cir.Events) {
		t.Fatalf("staged doc roundtrip events=%d err=%v", len(got.CIR.Events), err)
	}
}
