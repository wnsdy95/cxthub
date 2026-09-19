//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func checkDocJobRollback(t *testing.T, s *PostgresStore) {
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	j, doc, _ := docJobFixture(t, repo, "atomic rollback "+string(repo))
	if _, err := s.EnqueueDocJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimDocJob(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil || claim.ID != j.ID {
		t.Fatalf("claim %+v %v", claim, err)
	}
	stop := errors.New("abort after body and completion")
	err = s.WithinRepository(ctx, repo, func(tx context.Context) error {
		if err := s.CompleteDocJob(tx, claim, doc, time.Now().UTC()); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	got, err := s.GetDocJob(ctx, repo, j.ID)
	if err != nil || got.State != "running" {
		t.Fatalf("completion escaped rollback %+v %v", got, err)
	}
	if have, err := s.HasDocs(ctx, repo, []domain.ContentHash{doc.Hash()}); err != nil || len(have) > 0 {
		t.Fatalf("body escaped rollback %v %v", have, err)
	}
	var indexes int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_indexes_v2 WHERE hash=$1`, doc.Hash()).Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("index escaped rollback %d %v", indexes, err)
	}
	if err := s.CompleteDocJob(ctx, claim, doc, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}
