//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type storedDocQueryCountKey struct{}
type storedDocQueryCount struct{ reads atomic.Int32 }
type storedDocQueryTracer struct{}

func (storedDocQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if c, ok := ctx.Value(storedDocQueryCountKey{}).(*storedDocQueryCount); ok && strings.Contains(data.SQL, "FROM repo_blobs rb JOIN blobs b") {
		c.reads.Add(1)
	}
	return ctx
}
func (storedDocQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPGStoredDocVerificationBatchesCurrentOwnedChunks(t *testing.T) {
	base, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := base.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo)}}
	for i := 0; i < 40; i++ {
		// Compressible but distinct chunks keep the regression bounded while crossing
		// more than two query windows. No search-index writes are part of this fixture.
		text := strings.Repeat(fmt.Sprintf("%s/%03d ", repo, i), 7200)
		cir.Events = append(cir.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: i, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok || len(plan.Bodies) < 33 {
		t.Fatal("fixture must cross three windows")
	}
	if _, _, err := base.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, hash, docCompress(manifest)); err != nil {
		t.Fatal(err)
	}
	if _, err := base.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, repo, hash); err != nil {
		t.Fatal(err)
	}
	cfg := base.pool.Config()
	cfg.ConnConfig.Tracer = storedDocQueryTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &PostgresStore{pool: pool}
	defer s.Close()
	counts := &storedDocQueryCount{}
	counted := context.WithValue(ctx, storedDocQueryCountKey{}, counts)
	want := int32(1 + (len(plan.Bodies)+15)/16)
	for _, phase := range []string{"cold", "warm"} {
		counts.reads.Store(0)
		err := s.WithinRepository(counted, repo, func(bound context.Context) error {
			proof, err := s.VerifyStoredDoc(bound, repo, hash)
			if err == nil && proof.Hash() != hash {
				t.Errorf("wrong proof: %v", proof)
			}
			return err
		})
		if err != nil {
			t.Fatal(phase, err)
		}
		if got := counts.reads.Load(); got != want {
			t.Fatalf("%s: %d current reads, want %d for %d distinct chunks", phase, got, want, len(plan.Bodies))
		}
		t.Logf("%s: chunks=%d raw_bytes=%d SQL_reads=%d", phase, len(plan.Bodies), len(raw), counts.reads.Load())
	}
	// Existing corruption and transaction tests cover both adapter paths; here
	// exercise an absent non-first chunk beyond the first two windows explicitly.
	last := plan.Order[len(plan.Order)-1]
	if _, err := base.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, last); err != nil {
		t.Fatal(err)
	}
	if p, err := s.VerifyStoredDoc(ctx, repo, hash); err == nil || p.Valid() {
		t.Fatal("warm proof ignored missing final-window ownership", p, err)
	}
	if _, err := base.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, repo, last); err != nil {
		t.Fatal(err)
	}
}
