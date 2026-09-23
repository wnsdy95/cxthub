package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestDocFinalizationAcceptanceSurvivesCallerAndValidatesBeforePublish(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh(t.Name())
	bindCommitTestRepo(t, st, repo)
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "durable upload"}}}}}
	cb, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(cb)
	plan, ok := domain.PlanDocChunks(cb)
	if !ok {
		t.Fatal("no plan")
	}
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	wire := inbound.ChunkedDoc{Hash: hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
	// Accepted work is durable even after the accepting caller disappears.
	caller, cancel := context.WithCancel(ctx)
	job, err := svc.SubmitDocFinalization(caller, repo, wire)
	cancel()
	if err != nil || job.State != "waiting" {
		t.Fatalf("submit %+v %v", job, err)
	}
	if have, _ := st.HasDocs(ctx, repo, []domain.ContentHash{hash}); len(have) != 0 {
		t.Fatal("acceptance claimed verification")
	}
	if err := svc.ProcessDocFinalizations(ctx, 1); err != nil {
		t.Fatal(err)
	}
	job, err = svc.GetDocFinalization(ctx, repo, job.ID)
	if err != nil || job.State != "completed" {
		t.Fatalf("job %+v %v", job, err)
	}
	got, err := st.GetDoc(ctx, repo, hash)
	if err != nil || got.Hash != hash {
		t.Fatalf("body %v", err)
	}
	// A different manifest identity cannot borrow the successful receipt.
	bad := wire
	bad.Hash = hh("forged document hash")
	rejected, err := svc.SubmitDocFinalization(ctx, repo, bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocFinalizations(ctx, 1); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("forged body accepted: %v", err)
	}
	rejected, err = svc.GetDocFinalization(ctx, repo, rejected.ID)
	if err != nil || rejected.State != "rejected" {
		t.Fatalf("rejected %+v %v", rejected, err)
	}
	foreign := hh("other tenant")
	bindCommitTestRepo(t, st, foreign)
	if _, err := svc.SubmitDocFinalization(ctx, foreign, wire); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign chunks accepted %v", err)
	}
	if snaps, err := st.ListSnapshots(ctx, repo, ""); err != nil || len(snaps) != 0 {
		t.Fatalf("snapshots prematurely published %v %v", snaps, err)
	}
	if refs, err := st.ListRefs(ctx, repo); err != nil || len(refs) != 0 {
		t.Fatalf("refs prematurely published %v %v", refs, err)
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
	if err := svc.runDocJob(canceled, st, claim); !errors.Is(err, context.Canceled) {
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
	if err := svc.runDocJob(ctx, st, recovered); err != nil {
		t.Fatal(err)
	}
}
