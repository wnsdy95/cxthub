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

type changedFinalizationChunks struct{ outbound.BlobStore }

func (s changedFinalizationChunks) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	raw, err := s.BlobStore.GetChunk(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	copy := append([]byte{}, raw...)
	copy[0] ^= 1
	return copy, nil
}

func TestDocFinalizationWarmProofStillChecksCurrentOwnedChunks(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser}}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := domain.PlanDocChunks(raw)
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	j, err := domain.NewDocFinalizationJob(repo, domain.HashContent(raw), plan.Manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyDocJob(ctx, svc, j); err != nil {
		t.Fatal(err)
	}
	other := j
	other.RepoID = hh("unowned chunks")
	if proof, err := verifyDocJob(ctx, svc, other); err == nil || proof.Valid() {
		t.Fatal("proof granted foreign ownership")
	}
	svc.blobs = changedFinalizationChunks{st}
	if proof, err := verifyDocJob(ctx, svc, j); !errors.Is(err, domain.ErrIntegrity) || proof.Valid() {
		t.Fatal("warm proof hid chunk corruption", err)
	}
}
func TestDocFinalizationInterruptedWorkerIsReclaimable(t *testing.T) {
	svc, st := newFsckSvc(t)
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	ctx := systemTestContext()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "retry"}}}}}
	cb, _ := domain.CanonicalBytes(cir)
	plan, _ := domain.PlanDocChunks(cb)
	hash := domain.HashContent(cb)
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	wire := inbound.ChunkedDoc{Hash: hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
	accepted, err := svc.SubmitDocFinalization(ctx, repo, wire)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := st.ClaimDocJob(ctx, "", time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := runDocJob(canceled, svc, st, claim); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown %v", err)
	}
	job, err := st.GetDocJob(ctx, repo, accepted.ID)
	if err != nil || job.State != "running" {
		t.Fatalf("lost accepted work %+v %v", job, err)
	}
	recovered, err := st.ClaimDocJob(ctx, "", time.Now().Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := runDocJob(ctx, svc, st, recovered); err != nil {
		t.Fatal(err)
	}
}
