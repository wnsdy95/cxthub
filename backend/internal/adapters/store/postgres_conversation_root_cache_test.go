//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestRootPGWarmCacheRejectsUnusedTailWithoutWrites(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing-grant"} {
		t.Run(mode, func(t *testing.T) {
			st, ctx := chunkReusePG(t)
			repo, ns := rootPGRepo(t, ctx, st)
			f := rootCacheTailFixture(t, string(repo))
			seedRootPG(t, ctx, st, repo, f)
			before := rootPGImage(t, ctx, st, repo, ns)
			for i := 0; i < 2; i++ {
				doc, err := st.ReadVerifiedDoc(ctx, repo, f.hash)
				if err != nil {
					t.Fatal(err)
				}
				assertRootCacheFirstEvent(t, doc, f)
			}
			if before != rootPGImage(t, ctx, st, repo, ns) {
				t.Fatal("warm read changed grants/bytes/usage/index/job state")
			}
			tail := f.manifest.Chunks[len(f.manifest.Chunks)-1].Hash
			want := domain.ErrNotFound
			if mode == "corrupt" {
				// Blobs are shared across repositories and repeated test runs.
				// Restore the exact original representation before the pool closes.
				var original []byte
				if err := st.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, tail).Scan(&original); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := st.pool.Exec(cleanup, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, original); err != nil {
						t.Error("restore synthetic tail", err)
					}
				})
				body := bytes.Clone(f.bodies[tail])
				body[len(body)-1] ^= 1
				if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, tail, docCompress(body)); err != nil {
					t.Fatal(err)
				}
				want = domain.ErrIntegrity
			} else if _, err := st.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, tail); err != nil {
				t.Fatal(err)
			}
			before = rootPGImage(t, ctx, st, repo, ns)
			peer, err := NewPostgresStore(ctx, st.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			for _, reader := range []*PostgresStore{st, peer} {
				if doc, err := reader.ReadVerifiedDoc(ctx, repo, f.hash); !errors.Is(err, want) || doc.Valid() {
					t.Fatal("invalid tail returned body proof", err)
				}
				if ref, err := reader.VerifyStoredDoc(ctx, repo, f.hash); !errors.Is(err, want) || ref.Valid() {
					t.Fatal("invalid tail returned reference proof", err)
				}
				if len(reader.docProofs.proofs) != 0 {
					t.Fatal("root entered legacy physical cache")
				}
			}
			if before != rootPGImage(t, ctx, st, repo, ns) {
				t.Fatal("failed read changed grants/bytes/usage/index/job state")
			}
		})
	}
}
