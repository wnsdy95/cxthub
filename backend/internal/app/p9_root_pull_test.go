package app

import (
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestP9RootAdmissionRequiresFlagOptInAndCompatibility(t *testing.T) {
	for _, mode := range []string{"flag-off", "no-opt-in", "old-peer", "old-binary", "ready"} {
		t.Run(mode, func(t *testing.T) {
			svc, st := newFsckSvc(t)
			ctx := systemTestContext()
			repo := hh(t.Name())
			bindCommitTestRepo(t, st, repo)
			rep, bodies := rootWorkerFixture(t)
			if _, _, err := st.PutChunks(ctx, repo, bodies); err != nil {
				t.Fatal(err)
			}
			if mode != "no-opt-in" {
				if err := st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
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
			ctx = outbound.WithDocumentIdentityCompatibility(ctx, peer, binary)
			got, err := svc.submitDocFinalizationWithPolicy(ctx, repo, rep, mode != "flag-off")
			if mode == "ready" {
				if err != nil || got.State != "waiting" || got.DocIdentity != rep.Identity {
					t.Fatal(got, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrRootPublicationDisabled) && !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal(err)
			}
			job, err := domain.NewDocFinalizationJobForRepresentation(repo, rep, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = st.GetDocJob(ctx, repo, job.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("denial queued job", err)
			}
		})
	}
}
