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

// Index publication only: canonical verification and blob persistence happen
// outside timing. A 50,000-event prefix is already indexed before each append.
func BenchmarkPGReadBlockAppend(b *testing.B) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		b.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	s, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	if _, err = s.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		b.Fatal(err)
	}
	marker := time.Now().String()
	prepare := func(doc domain.VerifiedSessionDoc) {
		if _, err := s.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, doc.Hash(), docCompress(doc.Bytes())); err != nil {
			b.Fatal(err)
		}
	}
	write := func(doc domain.VerifiedSessionDoc) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			b.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = putReadIndexPG(ctx, tx, doc); err != nil {
			b.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			b.Fatal(err)
		}
	}
	seed := readBlockDoc(b, marker, "", 50000)
	prepare(seed)
	write(seed)
	b.ReportAllocs()
	b.SetBytes(int64(len(seed.Bytes())))
	var published []domain.ContentHash
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := readBlockDoc(b, marker, fmt.Sprintf("tail %d", i), 50000)
		prepare(doc)
		published = append(published, doc.Hash())
		b.StartTimer()
		write(doc)
	}
	b.StopTimer()
	var locations, references int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_block_locations_v3 WHERE doc_hash=ANY($1::text[])`, published).Scan(&references); err != nil {
		b.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_block_events_v3 e
      WHERE EXISTS(SELECT 1 FROM doc_read_block_locations_v3 l WHERE l.block_hash=e.block_hash AND l.doc_hash=ANY($1::text[]))
      AND NOT EXISTS(SELECT 1 FROM doc_read_block_locations_v3 l WHERE l.block_hash=e.block_hash AND l.doc_hash=$2)`, published, seed.Hash()).Scan(&locations); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(locations)/float64(b.N), "new-event-locations/op")
	b.ReportMetric(float64(references)/float64(b.N), "block-references/op")
}
