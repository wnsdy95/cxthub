package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func rootWorkerFixture(t *testing.T) (domain.DocumentRepresentation, map[domain.ContentHash][]byte) {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: t.Name()}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "explicit root worker"}}}}}
	m, bodies, err := domain.ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := domain.ConversationManifestHash(m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	return domain.DocumentRepresentation{Hash: h, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}, bodies
}

func TestRootDocJobWorkerAndExternalFence(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	representation, bodies := rootWorkerFixture(t)
	// No P6 external capability or repository opt-in is enabled by this tranche.
	if _, err := svc.SubmitDocFinalization(ctx, repo, inbound.ChunkedDoc{Hash: representation.Hash, Identity: representation.Identity}); !errors.Is(err, domain.ErrRootPublicationDisabled) {
		t.Fatal("external root admission opened", err)
	}
	if have, err := st.HasDocs(ctx, repo, []domain.ContentHash{representation.Hash}); err != nil || len(have) != 0 {
		t.Fatal("external rejection published", err)
	}
	if _, _, err := st.PutChunks(ctx, repo, bodies); err != nil {
		t.Fatal(err)
	}
	j, err := domain.NewDocFinalizationJobForRepresentation(repo, representation, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	// This repository never opted in. Even a complete released worker must
	// refuse its root publication; admission-off accepted-job recovery is separate.
	if err := svc.ProcessDocFinalizations(ctx, 1); !errors.Is(err, domain.ErrRootPublicationDisabled) {
		t.Fatal(err)
	}
	if have, err := st.HasDocs(ctx, repo, []domain.ContentHash{representation.Hash}); err != nil || len(have) != 0 {
		t.Fatal(have, err)
	}

}

func TestRootDocJobWorkerInvalidRootIsTerminal(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	representation, bodies := rootWorkerFixture(t)
	manifest, err := representation.ConversationManifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.EventCount++
	representation.Hash, err = domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	representation.RootManifest, err = domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.PutChunks(ctx, repo, bodies); err != nil {
		t.Fatal(err)
	}
	j, err := domain.NewDocFinalizationJobForRepresentation(repo, representation, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocFinalizations(ctx, 1); err == nil {
		t.Fatal("semantic mismatch accepted")
	}
	status, err := svc.GetDocFinalization(ctx, repo, j.ID)
	if err != nil || status.State != "rejected" || status.Reason != "invalid_document" {
		t.Fatal(status, err)
	}
	if have, err := st.HasDocs(ctx, repo, []domain.ContentHash{representation.Hash}); err != nil || len(have) != 0 {
		t.Fatal("invalid root published", err)
	}
}

func TestRootDocJobWorkerFreshRepositoryBytes(t *testing.T) {
	representation, bodies := rootWorkerFixture(t)
	repo := hh(t.Name())
	j, err := domain.NewDocFinalizationJobForRepresentation(repo, representation, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(nil, rootPreparationChunks{preparationChunks{repo: repo, bodies: bodies}}, nil, nil, nil)
	if _, err := svc.verifyDocJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	svc.blobs = changedRootFinalizationChunks{svc.blobs}
	if proof, err := svc.verifyDocJob(context.Background(), j); err == nil || proof.Valid() {
		t.Fatal("warm proof hid changed bytes")
	}
}

// The fake models the bounded reader contract; real storage decoder regressions
// independently exercise compression limits and terminal errors.
type rootPreparationChunks struct{ preparationChunks }

func (s rootPreparationChunks) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	body, err := s.GetChunk(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != length {
		return nil, domain.ErrIntegrity
	}
	return body, ctx.Err()
}

type changedRootFinalizationChunks struct{ outbound.BlobStore }

func (s changedRootFinalizationChunks) ReadConversationChunk(ctx context.Context, repo, hash domain.ContentHash, length int64) ([]byte, error) {
	body, err := s.BlobStore.(outbound.ConversationChunkReader).ReadConversationChunk(ctx, repo, hash, length)
	if err != nil {
		return nil, err
	}
	body[0] ^= 1
	return body, nil
}
