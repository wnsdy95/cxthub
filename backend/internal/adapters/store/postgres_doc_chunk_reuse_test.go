//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func chunkReusePG(t *testing.T) (*PostgresStore, context.Context) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	st, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err = st.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	return st, ctx
}

func TestPGFinalizationReusesChunksAndRejectsCorruptPresence(t *testing.T) {
	st, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	cir := chunkBigDoc(40)
	for i := range cir.Events {
		cir.Events[i].Blocks[0].Text = string(repo) + cir.Events[i].Blocks[0].Text
	}
	verify := func() domain.VerifiedSessionDoc {
		raw, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	doc := verify()
	plan, chunked := domain.PlanDocChunks(doc.Bytes())
	if !chunked {
		t.Fatal("fixture must span chunks")
	}
	if _, _, err := st.PutChunks(ctx, repo, plan.Bodies); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutVerifiedDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDoc(ctx, repo, doc.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateSessionDocHash(got); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDoc(ctx, domain.HashContent([]byte("unowned")), doc.Hash()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("chunk reuse granted foreign document access", err)
	}
	// A new envelope must still validate the already stored event chunks. The
	// good document's immutable identity cannot authorize corrupted shared bytes.
	if _, err := st.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(plan.Order[0]), []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	cir.Envelope.SessionOriginID = "next-capture"
	next := verify()
	if _, err := st.PutVerifiedDoc(ctx, repo, next); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("corrupt chunk reused", err)
	}
	if _, err := st.GetDoc(ctx, repo, next.Hash()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("failed publication leaked a document", err)
	}
}

func TestPGReusedChunkRetainedUntilOwnershipCommit(t *testing.T) {
	st, ctx := chunkReusePG(t)
	body := []byte(t.Name() + time.Now().String())
	hash := domain.HashContent(body)
	if _, err := st.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), docCompress(body)); err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := retainDocChunkPG(ctx, tx, hash, body, true); err != nil {
		t.Fatal(err)
	}
	deleting, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleting.Rollback(context.Background())
	if _, err := deleting.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = deleting.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, string(hash))
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
		t.Fatal("reused chunk was removable before ownership", err)
	}
}

func TestPGConcurrentChunkInsertionValidatesTheWinner(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "corrupt"}[corrupt], func(t *testing.T) {
			st, ctx := chunkReusePG(t)
			body := []byte(t.Name() + time.Now().String())
			hash := domain.HashContent(body)
			first, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Rollback(context.Background())
			stored := body
			if corrupt {
				stored = []byte("wrong bytes")
			}
			if _, err = first.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), docCompress(stored)); err != nil {
				t.Fatal(err)
			}
			second, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Rollback(context.Background())
			pid := second.Conn().PgConn().PID()
			done := make(chan error, 1)
			go func() { done <- retainDocChunkPG(ctx, second, hash, body, true) }()
			// Establish the real insertion conflict before publishing the winner.
			deadline := time.Now().Add(5 * time.Second)
			for {
				var blockers int
				if err := st.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))`, pid).Scan(&blockers); err != nil {
					t.Fatal(err)
				}
				if blockers > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("competing insert did not wait")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err = first.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = <-done
			if corrupt && !errors.Is(err, domain.ErrIntegrity) || !corrupt && err != nil {
				t.Fatal("incorrect competing-writer result", err)
			}
		})
	}
}
