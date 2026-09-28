//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func readBlockStore(t *testing.T) (*PostgresStore, domain.ContentHash) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	s, err := NewPostgresStore(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err = s.ApplyMigrations(context.Background(), "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err = s.PutRepo(context.Background(), domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	return s, repo
}

func readBlockDoc(t testing.TB, marker, tail string, n int) domain.VerifiedSessionDoc {
	t.Helper()
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: marker}}
	for i := 0; i < n; i++ {
		cir.Events = append(cir.Events, domain.CIREvent{Seq: i, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("%s shared literal 50%%_\\ event %d", marker, i)}}})
	}
	if tail != "" {
		cir.Events = append(cir.Events, domain.CIREvent{Seq: n, Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: tail}}})
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

func assertBlockRead(t *testing.T, s *PostgresStore, repo domain.ContentHash, doc domain.VerifiedSessionDoc) {
	t.Helper()
	ctx := context.Background()
	want, err := doc.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	for i := range want.Events {
		want.Events[i].Text = ""
	}
	got, err := s.DocReadIndex(ctx, repo, doc.Hash())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("read coordinates changed: events=%d want=%d err=%v", len(got.Events), len(want.Events), err)
	}
	hits, err := s.SearchDocEvents(ctx, repo, doc.Hash(), "50%_\\", 127, 3)
	if err != nil || len(hits) != 3 || hits[0].Index != 128 || hits[2].Index != 130 {
		t.Fatalf("block boundary search: %+v %v", hits, err)
	}
	candidates, err := s.MatchingDocHashes(ctx, repo, "50%_\\")
	if err != nil || !candidates[doc.Hash()] {
		t.Fatal("candidate lost", err)
	}
}

func TestPGReadBlocksShareLocationsAndRetainConcurrentOwners(t *testing.T) {
	s, repo := readBlockStore(t)
	ctx := context.Background()
	base := readBlockDoc(t, string(repo), "", 257)
	left := readBlockDoc(t, string(repo), "left", 257)
	right := readBlockDoc(t, string(repo), "right", 257)
	if _, err := s.PutVerifiedDoc(ctx, repo, base); err != nil {
		t.Fatal(err)
	}
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
	for _, doc := range []domain.VerifiedSessionDoc{base, left, right} {
		assertBlockRead(t, s, repo, doc)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_block_events_v3 WHERE block_hash IN (SELECT block_hash FROM doc_read_block_locations_v3 WHERE doc_hash=ANY($1::text[]))`, []domain.ContentHash{base.Hash(), left.Hash(), right.Hash()}).Scan(&count); err != nil || count != 261 {
		t.Fatalf("locations=%d want 261, not 773; err=%v", count, err)
	}
	foreign := domain.HashContent([]byte("foreign " + string(repo)))
	if _, err := s.DocReadIndex(ctx, foreign, left.Hash()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("foreign shared block read", err)
	}
	stop := errors.New("rollback shared index")
	rollback := readBlockDoc(t, string(repo), "rolled-back", 257)
	err := s.WithinRepository(ctx, repo, func(tx context.Context) error {
		if _, e := s.PutVerifiedDoc(tx, repo, rollback); e != nil {
			return e
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if _, err := s.DocReadIndex(ctx, repo, rollback.Hash()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("index escaped rollback", err)
	}
	// A pinned shared block cannot be removed by its final owner while another
	// transaction is preparing to publish references to it.
	plan, _ := base.PlanReadIndex()
	block := plan.Blocks()[0]
	for _, doc := range []domain.VerifiedSessionDoc{base, left} {
		if err = s.DeleteDoc(ctx, repo, doc.Hash()); err != nil {
			t.Fatal(err)
		}
	}
	assertBlockRead(t, s, repo, right)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT hash FROM doc_read_blocks_v3 WHERE hash=$1 FOR KEY SHARE`, block.Hash); err != nil {
		t.Fatal(err)
	}
	deleting, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleting.Rollback(ctx)
	if _, err = deleting.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = deleting.Exec(ctx, `DELETE FROM doc_read_indexes_v3 WHERE hash=$1`, right.Hash())
	var lockError *pgconn.PgError
	if !errors.As(err, &lockError) || lockError.Code != "55P03" {
		t.Fatal("unretained shared block", err)
	}
	if err = deleting.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDoc(ctx, repo, right.Hash()); err != nil {
		t.Fatal(err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_blocks_v3 WHERE hash=$1`, block.Hash).Scan(&count); err != nil || count != 0 {
		t.Fatal("last block owner cleanup", count, err)
	}
	if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=$1`, block.EventHashes()[0]).Scan(&count); err != nil || count != 0 {
		t.Fatal("last search owner cleanup", count, err)
	}
}

func TestPGReadBlocksRollingV2Compatibility(t *testing.T) {
	for _, legacyFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(legacyFirst), func(t *testing.T) {
			s, repo := readBlockStore(t)
			ctx := context.Background()
			old := readBlockDoc(t, string(repo), "", 257)
			next := readBlockDoc(t, string(repo), "next", 257)
			for _, doc := range []domain.VerifiedSessionDoc{old, next} {
				if _, err := s.PutVerifiedDoc(ctx, repo, doc); err != nil {
					t.Fatal(err)
				}
			}
			// Model a previously persisted/rolling older replica's v2 projection.
			if _, err := s.pool.Exec(ctx, `INSERT INTO doc_read_indexes_v2 SELECT * FROM doc_read_indexes_v3 WHERE hash=$1`, old.Hash()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, `INSERT INTO doc_read_events_v2 SELECT doc_hash,ordinal,byte_offset,byte_length,event_hash,seq,role FROM doc_read_event_locations_current WHERE doc_hash=$1`, old.Hash()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(ctx, `DELETE FROM doc_read_indexes_v3 WHERE hash=$1`, old.Hash()); err != nil {
				t.Fatal(err)
			}
			assertBlockRead(t, s, repo, old)
			first, last := old, next
			if !legacyFirst {
				first, last = next, old
			}
			if err := s.DeleteDoc(ctx, repo, first.Hash()); err != nil {
				t.Fatal(err)
			}
			assertBlockRead(t, s, repo, last)
			if err := s.DeleteDoc(ctx, repo, last.Hash()); err != nil {
				t.Fatal(err)
			}
			plan, _ := old.PlanReadIndex()
			var count int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=$1`, plan.EventHashes()[0]).Scan(&count); err != nil || count != 0 {
				t.Fatal("cross-version text cleanup", count, err)
			}
		})
	}
}

func TestPGReadBlocksConcurrentFinalOwnerDeletion(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			s, repo := readBlockStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			left, right := readBlockDoc(t, string(repo), "left", 257), readBlockDoc(t, string(repo), "right", 257)
			for _, doc := range []domain.VerifiedSessionDoc{left, right} {
				if _, err := s.PutVerifiedDoc(ctx, repo, doc); err != nil {
					t.Fatal(err)
				}
			}
			if legacy {
				if _, err := s.pool.Exec(ctx, `INSERT INTO doc_read_indexes_v2 SELECT * FROM doc_read_indexes_v3 WHERE hash=$1`, left.Hash()); err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(ctx, `INSERT INTO doc_read_events_v2 SELECT doc_hash,ordinal,byte_offset,byte_length,event_hash,seq,role FROM doc_read_event_locations_current WHERE doc_hash=$1`, left.Hash()); err != nil {
					t.Fatal(err)
				}
				if _, err := s.pool.Exec(ctx, `DELETE FROM doc_read_indexes_v3 WHERE hash=$1`, left.Hash()); err != nil {
					t.Fatal(err)
				}
			}
			// Hold a common row until both cleanup transactions are waiting. This
			// forces the last-owner decisions to overlap instead of relying on
			// scheduler luck. Neither cleanup may use its pre-wait snapshot.
			p, _ := left.PlanReadIndex()
			gate, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Rollback(context.Background())
			gateSQL := `SELECT hash FROM doc_read_blocks_v3 WHERE hash=$1 FOR UPDATE`
			gateID := p.Blocks()[0].Hash
			if legacy {
				// Legacy locations do not own a v3 block. Their common resource is
				// the search row, after both event-location deletions.
				gateSQL = `SELECT hash FROM doc_search_events_v2 WHERE hash=$1 FOR UPDATE`
				gateID = p.EventHashes()[0]
			}
			if _, err = gate.Exec(ctx, gateSQL, gateID); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			errs := make(chan error, 2)
			for _, doc := range []domain.VerifiedSessionDoc{left, right} {
				go func() { <-start; errs <- s.DeleteDoc(ctx, repo, doc.Hash()) }()
			}
			close(start)
			for {
				select {
				case err := <-errs:
					t.Fatal("cleanup skipped ownership fence", err)
				default:
				}
				var waiting int
				if err = s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting >= 2 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(5 * time.Millisecond):
				}
			}
			if err = gate.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_read_blocks_v3 WHERE hash=ANY($1::text[])`, []domain.ContentHash{p.Blocks()[0].Hash, p.Blocks()[1].Hash}).Scan(&count); err != nil || count != 0 {
				t.Fatal("concurrent deletion leaked shared blocks", count, err)
			}
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM doc_search_events_v2 WHERE hash=ANY($1::text[])`, p.EventHashes()).Scan(&count); err != nil || count != 0 {
				t.Fatal("concurrent deletion leaked shared text", count, err)
			}
		})
	}
}
