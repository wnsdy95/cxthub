//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestP5RootInitializationClosure(t *testing.T) {
	for _, mode := range []string{"valid", "mixed", "grant_revoke", "grant_readd", "chunk_recompress", "descriptor_recompress", "doc_revoke", "corrupt_before", "wrong_tag", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			st, repo := initializationPG(t)
			ctx := context.Background()
			receipt := initializationPGBegin(t, st, repo)
			f := rootFixture(t, "initialization "+string(repo.ID))
			seedRootPG(t, ctx, st, repo.ID, f)
			snap := domain.Snapshot{ID: f.hash, DocHash: f.hash, DocIdentity: domain.DocumentIdentityRootV1, RepoID: repo.ID, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
			var snaps []domain.Snapshot
			if mode == "mixed" {
				legacy := initializationPGSnapshot(t, st, repo.ID, "old")
				snap.Parents = []domain.ContentHash{legacy.ID}
				snaps = append(snaps, legacy)
			}
			if mode == "wrong_tag" {
				snap.DocIdentity = domain.DocumentIdentityLegacy
			}
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			snaps = append(snaps, snap)
			in := initializationPGAnchor(receipt, snaps...)
			chunk := f.manifest.Chunks[0].Hash
			if mode == "corrupt_before" {
				if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk, []byte("broken")); err != nil {
					t.Fatal(err)
				}
			}
			proof, err := initializationPGPrepare(st, repo.ID, in)
			if mode == "corrupt_before" || mode == "wrong_tag" {
				if err == nil {
					t.Fatal("invalid source accepted")
				}
				return
			}
			if err != nil {
				t.Fatal("root initialization capture", err)
			}
			captured := proof.(*initializationProofPG)
			for h := range f.bodies {
				if _, ok := captured.blobs[initializationBlobPG{"chunk", h}]; !ok {
					t.Fatal("root dependency not pinned")
				}
			}
			switch mode {
			case "grant_revoke", "grant_readd":
				if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo.ID, chunk); err != nil {
					t.Fatal(err)
				}
				if mode == "grant_readd" {
					if _, err := st.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, repo.ID, chunk); err != nil {
						t.Fatal(err)
					}
				}
			case "chunk_recompress":
				if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk, f.bodies[chunk]); err != nil {
					t.Fatal(err)
				}
			case "descriptor_recompress":
				if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, f.hash, f.manifestBytes); err != nil {
					t.Fatal(err)
				}
			case "doc_revoke":
				if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo.ID, f.hash); err != nil {
					t.Fatal(err)
				}
			}
			callctx := ctx
			if mode == "cancel" {
				var cancel context.CancelFunc
				callctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err = st.WithinRepository(callctx, repo.ID, func(tx context.Context) error {
				_, e := st.FinalizeRepositoryInitialization(tx, repo.ID, in, proof)
				return e
			})
			if mode == "valid" || mode == "mixed" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("changed/cancelled source published")
			}
			if mode != "cancel" && !errors.Is(err, domain.ErrRepositoryInitializationConflict) {
				t.Fatal("wrong stale proof error", err)
			}
			if _, e := st.GetRepositoryInitializationAnchor(ctx, repo.ID, "main"); !errors.Is(e, domain.ErrNotFound) {
				t.Fatal("failed initialization left anchor", e)
			}
		})
	}
}
