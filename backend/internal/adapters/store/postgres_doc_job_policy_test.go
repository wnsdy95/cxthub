//go:build postgres

package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"testing"
	"time"
)

func TestP9PGRootJobFinalPolicy(t *testing.T) {
	for _, mode := range []string{"no-opt-in", "missing-declaration", "old-binary", "old-peer", "ready"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := chunkReusePG(t)
			f := uniqueRootPublicationFixturePG(t, "final policy")
			repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
			if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.PutChunks(ctx, repo, f.bodies); err != nil {
				t.Fatal(err)
			}
			j := rootPublicationJob(t, repo, f)
			if _, err := s.EnqueueDocJob(ctx, j); err != nil {
				t.Fatal(err)
			}
			j, err := s.ClaimDocJob(ctx, repo, time.Now(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			proof := rootPublicationProof(t, f)
			prepared, err := s.PrepareDocJob(ctx, proof)
			if err != nil {
				t.Fatal(err)
			}
			// Requirement is re-read at final completion, after preparation.
			if mode != "no-opt-in" {
				if err = s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
			}
			peer, binary := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
			if mode == "old-peer" {
				peer = nil
			}
			if mode == "old-binary" {
				binary = nil
			}
			if mode != "missing-declaration" {
				ctx = outbound.WithDocumentIdentityCompatibility(ctx, peer, binary)
			}
			err = prepared.Complete(ctx, j, time.Now())
			if mode == "ready" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, domain.ErrRootPublicationDisabled) && !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal(err)
			}
			got, err := s.GetDocJob(ctx, repo, j.ID)
			if err != nil || got.State == "completed" {
				t.Fatal(got, err)
			}
			if have, err := s.HasDocs(ctx, repo, []domain.ContentHash{j.DocHash}); err != nil || len(have) != 0 {
				t.Fatal(have, err)
			}
		})
	}
}
func TestP9PGLegacyJobCannotFinishAfterOptIn(t *testing.T) {
	s, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo)}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser}}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("plan")
	}
	if _, _, err = s.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	j, err := domain.NewDocFinalizationJob(repo, hash, plan.Manifest, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	j, err = s.ClaimDocJob(ctx, repo, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: hash, CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareDocJob(ctx, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		t.Fatal(err)
	}
	if err = prepared.Complete(context.Background(), j, time.Now()); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal(err)
	}
	if have, err := s.HasDocs(ctx, repo, []domain.ContentHash{hash}); err != nil || len(have) != 0 {
		t.Fatal(have, err)
	}
}
