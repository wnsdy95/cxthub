package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type cancelChunkRead struct {
	outbound.BlobStore
	cancel context.CancelFunc
	reads  int
}

func (s *cancelChunkRead) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	s.reads++
	body, err := s.BlobStore.GetChunk(ctx, repo, hash)
	s.cancel()
	return body, err
}

func TestCommitCancellationStopsChunkValidationBeforePublication(t *testing.T) {
	svc, st := newFsckSvc(t)
	repo := hh("canceled-finalization")
	bindCommitTestRepo(t, st, repo)
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("a", 600<<10)}}},
		{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("b", 600<<10)}}},
	}}
	canonical, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(canonical)
	plan, ok := domain.PlanDocChunks(canonical)
	if !ok || len(plan.Manifest.Chunks) < 2 {
		t.Fatal("expected multiple chunks")
	}
	if _, _, err := st.PutChunks(systemTestContext(), repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(systemTestContext())
	defer cancel()
	spy := &cancelChunkRead{BlobStore: st, cancel: cancel}
	svc.blobs = spy
	out, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo,
		ChunkedDocs: []inbound.ChunkedDoc{{Hash: hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}},
	})
	if !errors.Is(err, context.Canceled) || spy.reads != 1 || out.StoredDocs != 0 {
		t.Fatalf("canceled finalization: reads=%d out=%+v err=%v", spy.reads, out, err)
	}
	if _, err := st.GetDoc(systemTestContext(), repo, hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("document published: %v", err)
	}
}

func TestCommitAlreadyCanceledHasNoSuccessfulReceipt(t *testing.T) {
	svc, st := newFsckSvc(t)
	repo := hh("canceled-empty-finalization")
	bindCommitTestRepo(t, st, repo)
	ctx, cancel := context.WithCancel(systemTestContext())
	cancel()
	if _, err := svc.Commit(ctx, inbound.CommitInput{RepoID: repo}); !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled returned %v", err)
	}
}

func wireChunk(body []byte) inbound.ChunkObject {
	return inbound.ChunkObject{Hash: domain.HashContent(body), Data: body}
}

func TestBoundedChunkStoreRejectsInvalidBatchBeforeWriting(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
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
