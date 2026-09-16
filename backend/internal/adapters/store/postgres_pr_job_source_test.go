//go:build postgres

package store

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pausePRClaimKey struct{}
type pausePRClaimTracer struct {
	id                string
	paused            atomic.Bool
	selected, release chan struct{}
}

func (p *pausePRClaimTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if data.SQL == claimPRJobSQL && len(data.Args) > 1 && data.Args[1] == p.id && p.paused.CompareAndSwap(false, true) {
		return context.WithValue(ctx, pausePRClaimKey{}, true)
	}
	return ctx
}
func (p *pausePRClaimTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(pausePRClaimKey{}) == true {
		close(p.selected)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}
}

func checkPGPRClaimWakeRace(t *testing.T, st *PostgresStore) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repo, proof, now := sourceJobFixture(t, st)
	old := sourceJob(repo, 1, now)
	young := sourceJob(repo, 2, now.Add(time.Second))
	young.State, young.Reason, young.Attempts = "waiting", "", 0
	for _, j := range []domain.PRPromotionJob{old, young} {
		if _, err := st.EnqueuePRJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	storeSourcePublication(t, st, proof)
	trace := &pausePRClaimTracer{id: young.ID, selected: make(chan struct{}), release: make(chan struct{})}
	config := st.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	claimant := &PostgresStore{pool: pool}
	done := make(chan error, 1)
	now = now.Add(time.Minute)
	go func() {
		_, err := claimant.ClaimPRJob(ctx, repo, young.ID, now, time.Minute)
		done <- err
	}()
	select {
	case <-trace.selected:
	case <-ctx.Done():
		t.Fatal("younger claim did not reach selection barrier")
	}
	// Younger is selected and row-locked, but its transaction is uncommitted.
	// Wake and claim the older row using an independent connection.
	if err := st.WakePRSourceJobs(ctx, repo, now); err != nil {
		close(trace.release)
		t.Fatal(err)
	}
	claimed, err := st.ClaimPRJob(ctx, repo, old.ID, now, time.Minute)
	close(trace.release)
	if err != nil || claimed.ID != old.ID {
		t.Fatalf("older wake/claim: %+v %v", claimed, err)
	}
	if err := <-done; !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale younger selection became a concurrent running job: %v", err)
	}
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM pr_promotion_jobs WHERE repo_id=$1 AND state='running'`, repo).Scan(&count); err != nil || count != 1 {
		t.Fatalf("running rows=%d: %v", count, err)
	}
}
