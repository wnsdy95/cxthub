//go:build postgres

package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Models a growing capture whose uploaded chunks and shared search events are
// already present. Only the envelope changes between these synthetic documents.
// Verification before persistence and queue wait are deliberately outside timing.
func BenchmarkPGUploadedChunkFinalization(b *testing.B) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		b.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		b.Fatal(err)
	}
	repo := domain.HashContent([]byte(b.Name() + time.Now().String()))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		b.Fatal(err)
	}
	cir := chunkBigDoc(200)
	verify := func(i int) domain.VerifiedSessionDoc {
		cir.Envelope.SessionOriginID = fmt.Sprintf("%s-%d", repo, i)
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			b.Fatal(err)
		}
		doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
		if err != nil {
			b.Fatal(err)
		}
		return doc
	}
	base := verify(-1)
	if _, err = st.PutVerifiedDoc(ctx, repo, base); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(base.Bytes())))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := verify(i)
		b.StartTimer()
		if _, err = st.PutVerifiedDoc(ctx, repo, doc); err != nil {
			b.Fatal(err)
		}
	}
}
