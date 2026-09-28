//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGSharedSearchReuseSurvivesPublicationAndDeletion(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	makeDoc := func(extra string) domain.VerifiedSessionDoc {
		cir := domain.CIRDocument{Events: []domain.CIREvent{{Seq: 1, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "Shared \ud55c\uad6d\uc5b4 50%_\\ " + string(repo)}}}}}
		if extra != "" {
			cir.Events = append(cir.Events, domain.CIREvent{Seq: 2, Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: extra}}})
		}
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
	parent, left, right := makeDoc(""), makeDoc("left needle"), makeDoc("right needle")
	if _, err := s.PutVerifiedDoc(ctx, repo, parent); err != nil {
		t.Fatal(err)
	}
	plan, err := left.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	known, err := retainSearchEventsPG(ctx, tx, plan.EventHashes())
	if err != nil || len(known) != 1 {
		t.Fatal("must reuse exact shared event only", known, err)
	}
	idx, err := plan.Build(known)
	if err != nil || idx.Events[0].Text != "" || idx.Events[1].Text != "left needle" {
		t.Fatal("delta projection", idx, err)
	}
	// An overlapping last-owner delete cannot invalidate the reuse decision.
	deleting, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleting.Rollback(context.Background())
	if _, err := deleting.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = deleting.Exec(ctx, `DELETE FROM doc_read_indexes_v2 WHERE hash=$1`, parent.Hash())
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
		t.Fatal("reused text was not retained", err)
	}
	if err := deleting.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Two new documents can reuse a prefix while adding different search rows.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, doc := range []domain.VerifiedSessionDoc{left, right} {
		wg.Go(func() { _, err := s.PutVerifiedDoc(ctx, repo, doc); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteDoc(ctx, repo, parent.Hash()); err != nil {
		t.Fatal(err)
	}
	for _, doc := range []domain.VerifiedSessionDoc{left, right} {
		for _, query := range []string{"\ud55c\uad6d\uc5b4", "50%_\\", "needle"} {
			hits, err := s.SearchDocEvents(ctx, repo, doc.Hash(), query, -1, 10)
			if err != nil || len(hits) != 1 {
				t.Fatalf("search after parent deletion %q: %+v %v", query, hits, err)
			}
		}
	}
	foreign := domain.HashContent([]byte("unowned reused search"))
	if _, err := s.SearchDocEvents(ctx, foreign, left.Hash(), "needle", -1, 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("reuse granted foreign ownership", err)
	}
	for _, doc := range []domain.VerifiedSessionDoc{left, right} {
		if err := s.DeleteDoc(ctx, repo, doc.Hash()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=$1`, idx.Events[0].Hash).Scan(&count); err != nil || count != 0 {
		t.Fatal("last owner did not remove search text", count, err)
	}
}
