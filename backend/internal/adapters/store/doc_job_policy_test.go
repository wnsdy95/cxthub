package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestP9FSRootJobFinalPolicy(t *testing.T) {
	root := domain.DocumentIdentityRootV1
	for _, mode := range []string{"no-opt-in", "missing-declaration", "old-binary", "old-peer", "ready"} {
		t.Run(mode, func(t *testing.T) {
			st := NewFSStore(t.TempDir())
			f := rootFixture(t, "root final policy")
			j, proof := rootPublicationClaimFS(t, st, f, false)
			ctx := context.Background()
			peer, binary := []domain.DocumentIdentity{root}, []domain.DocumentIdentity{root}
			if mode != "no-opt-in" {
				if err := st.RequireDocumentIdentity(ctx, j.RepoID, root); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "old-binary":
				binary = nil
			case "old-peer":
				peer = nil
			}
			if mode != "missing-declaration" {
				ctx = outbound.WithDocumentIdentityCompatibility(ctx, peer, binary)
			}
			err := st.CompleteDocJob(ctx, j, proof, time.Now())
			if mode == "ready" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, domain.ErrRootPublicationDisabled) && !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatalf("policy admitted: %v", err)
			}
			job, err := st.GetDocJob(context.Background(), j.RepoID, j.ID)
			if err != nil || job.State == "completed" {
				t.Fatal(job, err)
			}
			if have, err := st.HasDocs(context.Background(), j.RepoID, []domain.ContentHash{j.DocHash}); err != nil || len(have) != 0 {
				t.Fatal("policy failure published", have, err)
			}
		})
	}
}
