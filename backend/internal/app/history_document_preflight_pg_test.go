//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyDocumentLockObservation struct {
	readOnly, graphAvailable bool
}

// The probe delegates every byte check and mutation to the real PG adapter.
// A separate connection tries the actual repository advisory lock without
// waiting, so these assertions do not depend on elapsed-time thresholds.
type historyDocumentLockProbe struct {
	*store.PostgresStore
	peer         *pgx.Conn
	observations []historyDocumentLockObservation
	applies      int
	afterVerify  func(context.Context) error
	afterApply   func(context.Context) error
	afterAbort   func(error) error
}

func (p *historyDocumentLockProbe) WithinRepository(ctx context.Context, repo domain.ContentHash, fn func(context.Context) error) error {
	err := p.PostgresStore.WithinRepository(ctx, repo, fn)
	if err != nil && p.afterAbort != nil {
		if hookErr := p.afterAbort(err); hookErr != nil {
			return hookErr
		}
	}
	return err
}

func (p *historyDocumentLockProbe) graphAvailable(ctx context.Context, repo domain.ContentHash) (bool, error) {
	tx, err := p.peer.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var available bool
	err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1),1735289201)`, string(repo)).Scan(&available)
	return available, err
}

func (p *historyDocumentLockProbe) VerifyStoredDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	available, err := p.graphAvailable(ctx, repo)
	if err != nil {
		return domain.VerifiedDocReference{}, err
	}
	p.observations = append(p.observations, historyDocumentLockObservation{p.InReadOnlyTransaction(ctx), available})
	proof, err := p.PostgresStore.VerifyStoredDoc(ctx, repo, hash)
	if err == nil && p.afterVerify != nil {
		err = p.afterVerify(ctx)
	}
	return proof, err
}

func (p *historyDocumentLockProbe) ApplyHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	available, err := p.graphAvailable(ctx, domain.ContentHash(event.RepoID))
	if err != nil {
		return err
	}
	if available || p.InReadOnlyTransaction(ctx) {
		return fmt.Errorf("history apply lost its repository write lock")
	}
	p.applies++
	if err := p.PostgresStore.ApplyHistoryEvent(ctx, event); err != nil {
		return err
	}
	if p.afterApply != nil {
		return p.afterApply(ctx)
	}
	return nil
}

func historyDocumentLockFixture(t *testing.T) (*Service, *historyDocumentLockProbe, domain.HistoryEvent) {
	t.Helper()
	svc, st, repo := collaborationPG(t)
	ctx := systemTestContext()
	id := collaborationSnapshot(t, st, repo, "request-local proof lock fixture")
	// PutDoc can seed a physical proof. Reopen the store so the first request
	// starts cold and the second request exercises its warmed proof cache.
	reader, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	peer, err := pgx.Connect(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close(context.Background()) })
	probe := &historyDocumentLockProbe{PostgresStore: reader, peer: peer}
	svc.meta, svc.blobs, svc.repositories = probe, probe, probe
	event := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), BranchID: "main", Branch: "main", Kind: "position", Source: id, Target: id, SharedTarget: id, CreatedAt: time.Now().UTC()}
	return svc, probe, event
}

func TestPGHistoryDocumentPreparationReleasesGraphLock(t *testing.T) {
	svc, probe, event := historyDocumentLockFixture(t)
	ctx := systemTestContext()
	for i := 1; i <= 2; i++ {
		event.ID = fmt.Sprintf("%032x", i)
		if err := svc.RecordHistory(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if len(probe.observations) != 2 || probe.applies != 2 {
		t.Fatalf("each request must verify once despite repeated root fields: verifies=%d applies=%d", len(probe.observations), probe.applies)
	}
	for i, got := range probe.observations {
		if !got.readOnly || !got.graphAvailable {
			t.Errorf("request %d: body verification readOnly=%v graphAvailable=%v; want owned RR verification outside graph lock", i+1, got.readOnly, got.graphAvailable)
		}
	}
}

func TestPGHistoryDocumentPreparationNestedKeepsWriteTransaction(t *testing.T) {
	svc, probe, event := historyDocumentLockFixture(t)
	ctx := systemTestContext()
	stop := errors.New("rollback enclosing operation")
	err := probe.WithinRepository(ctx, domain.ContentHash(event.RepoID), func(bound context.Context) error {
		if err := svc.RecordHistory(bound, event); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if len(probe.observations) != 1 || probe.observations[0].readOnly || probe.observations[0].graphAvailable || probe.applies != 1 {
		t.Fatalf("nested body verification escaped its write transaction: %+v applies=%d", probe.observations, probe.applies)
	}
	events, err := probe.ListHistoryEvents(ctx, domain.ContentHash(event.RepoID))
	if err != nil || len(events) != 0 {
		t.Fatalf("history escaped outer rollback: count=%d err=%v", len(events), err)
	}
	revision, err := probe.RepositoryRevision(ctx, domain.ContentHash(event.RepoID))
	if err != nil || revision.Graph != 0 {
		t.Fatalf("revision escaped outer rollback: %+v err=%v", revision, err)
	}
}

func TestPGHistoryDocumentPreparationAcceptedReplaySkipsMissingBody(t *testing.T) {
	svc, probe, event := historyDocumentLockFixture(t)
	ctx := systemTestContext()
	if err := svc.RecordHistory(ctx, event); err != nil {
		t.Fatal(err)
	}
	if _, err := probe.peer.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, event.RepoID, event.Target); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordHistory(ctx, event); err != nil {
		t.Fatal("exact accepted replay", err)
	}
	if len(probe.observations) != 1 || probe.applies != 1 {
		t.Fatalf("accepted replay reverified or reapplied: verifies=%d applies=%d", len(probe.observations), probe.applies)
	}
	changed := event
	changed.CreatedAt = changed.CreatedAt.Add(time.Second)
	if err := svc.RecordHistory(ctx, changed); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("different payload reused accepted ID", err)
	}
	if len(probe.observations) != 1 || probe.applies != 1 {
		t.Fatal("conflicting replay touched document bodies or reapplied")
	}
}

func TestPGHistoryDocumentPreparationConcurrentAcceptanceWinsFailure(t *testing.T) {
	svc, probe, event := historyDocumentLockFixture(t)
	peer, err := store.NewPostgresStore(systemTestContext(), collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewService(peer, peer, svc.auth, svc.engine, peer)
	probe.afterVerify = func(ctx context.Context) error {
		if !probe.InReadOnlyTransaction(ctx) {
			return errors.New("preparation still holds the write transaction")
		}
		if err := other.RecordHistory(systemTestContext(), event); err != nil {
			return err
		}
		return domain.ErrIntegrity // Redundant preparation failed after acceptance.
	}
	if err := svc.RecordHistory(systemTestContext(), event); err != nil {
		t.Fatal("concurrent exact accepted replay lost", err)
	}
	if probe.applies != 0 || len(probe.observations) != 1 {
		t.Fatal("concurrent accepted event was reapplied or reverified")
	}
}

func TestPGHistoryDocumentPreparationReauthorizesAndHasNoPreflightWrites(t *testing.T) {
	for _, mode := range []string{"verification_failure", "revoked_member"} {
		t.Run(mode, func(t *testing.T) {
			svc, probe, event := historyDocumentLockFixture(t)
			f := newHistoryMemorySelectionFixture(t, svc, probe, domain.ContentHash(event.RepoID))
			before, err := probe.RepositoryRevision(f.ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			probe.afterVerify = func(ctx context.Context) error {
				current, err := probe.PostgresStore.RepositoryRevision(ctx, f.repo)
				if err != nil || current != before {
					return fmt.Errorf("preflight wrote revision: %+v %v", current, err)
				}
				if mode == "verification_failure" {
					return domain.ErrIntegrity
				}
				return probe.WithinIdentity(f.ctx, func(bound context.Context) error {
					return probe.AddMember(bound, domain.Membership{RepositoryID: f.repository, UserID: f.member, Role: domain.RoleViewer})
				})
			}
			want := domain.ErrIntegrity
			if mode == "revoked_member" {
				want = domain.ErrForbidden
			}
			if err := svc.RecordHistory(f.ctx, f.before); !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			current, err := probe.RepositoryRevision(f.ctx, f.repo)
			if err != nil || current != before || probe.applies != 0 {
				t.Fatalf("failed preparation/admission wrote state: %+v applies=%d err=%v", current, probe.applies, err)
			}
		})
	}
}

func TestPGHistoryDocumentPreparationRetryRepins(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(fmt.Sprintf("change_after_abort_%v", mutate), func(t *testing.T) {
			svc, probe, event := historyDocumentLockFixture(t)
			ctx := systemTestContext()
			probe.afterApply = func(context.Context) error {
				if probe.applies == 1 {
					return &pgconn.PgError{Code: "40001", Message: "injected serialization abort"}
				}
				return nil
			}
			probe.afterAbort = func(err error) error {
				var pg *pgconn.PgError
				if mutate && errors.As(err, &pg) && pg.Code == "40001" {
					_, err = probe.peer.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, event.Target)
					return err
				}
				return nil
			}
			err := svc.RecordHistory(ctx, event)
			wantEvents, wantApplies := 1, 2
			if mutate {
				if !errors.Is(err, domain.ErrConflict) {
					t.Fatal("retry trusted stale tuple proof", err)
				}
				wantEvents, wantApplies = 0, 1
			} else if err != nil {
				t.Fatal("retry of unchanged evidence", err)
			}
			events, err := probe.ListHistoryEvents(ctx, domain.ContentHash(event.RepoID))
			if err != nil || len(events) != wantEvents || probe.applies != wantApplies || len(probe.observations) != 1 {
				t.Fatalf("retry escaped rollback/request scope: events=%d applies=%d byte_verifications=%d err=%v", len(events), probe.applies, len(probe.observations), err)
			}
		})
	}
}

func TestPGHistoryDocumentPreparationCurrentCASStillWins(t *testing.T) {
	svc, probe, event := historyDocumentLockFixture(t)
	f := newHistoryMemorySelectionFixture(t, svc, probe, domain.ContentHash(event.RepoID))
	advance := f.before
	advance.Kind, advance.Target = "advance", f.other
	changed := false
	probe.afterVerify = func(context.Context) error {
		if changed {
			return nil
		}
		changed = true
		// A separate current writer wins the ref CAS while bodies are read.
		return probe.PostgresStore.WithinRepository(f.ctx, f.repo, func(write context.Context) error {
			return probe.PostgresStore.CompareAndSwapRef(write, f.repo, domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", BranchID: f.before.BranchID, Target: f.other}, f.before.Target)
		})
	}
	if err := svc.RecordHistory(f.ctx, advance); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("prepared document proof bypassed current ref CAS", err)
	}
	events, err := probe.ListHistoryEvents(f.ctx, f.repo)
	if err != nil || len(events) != 0 {
		t.Fatal("failed CAS retained a history event", events, err)
	}
	ref, err := probe.GetRef(f.ctx, f.repo, domain.RefBranch, "main")
	if err != nil || ref.Target != f.other {
		t.Fatal("failed CAS overwrote the concurrent winner", ref, err)
	}
}

func TestPGHistoryDocumentPreparationConcurrentWriterProgress(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		t.Run(fmt.Sprintf("prepared_%v", prepared), func(t *testing.T) {
			svc, probe, event := historyDocumentLockFixture(t)
			ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			probe.afterVerify = func(ctx context.Context) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			done := make(chan error, 1)
			go func() {
				if prepared {
					done <- svc.RecordHistory(ctx, event)
				} else {
					// The unchanged nested admission path is the old top-level
					// behavior, matched to the same current fixture and verifier.
					done <- repositoryWriteError(ctx, svc, domain.ContentHash(event.RepoID), func(write context.Context) error { return svc.recordHistory(write, event, false, nil) })
				}
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("did not enter verifier", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			writer, err := pgx.Connect(ctx, os.Getenv("CXT_TEST_DSN"))
			if err != nil {
				close(release)
				t.Fatal(err)
			}
			defer writer.Close(context.Background())
			writerDone := make(chan error, 1)
			started := time.Now()
			go func() {
				tx, err := writer.Begin(ctx)
				if err == nil {
					defer tx.Rollback(ctx)
					_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1),1735289201)`, event.RepoID)
					if err == nil {
						err = tx.Commit(ctx)
					}
				}
				writerDone <- err
			}()
			var writerLatency time.Duration
			if prepared {
				select {
				case err = <-writerDone:
					writerLatency = time.Since(started)
				case <-ctx.Done():
					err = ctx.Err()
				}
			} else {
				// Confirm an actual PG wait edge before a fixed 250 ms hold.
				for {
					var blocked bool
					if err = probe.peer.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, writer.PgConn().PID()).Scan(&blocked); err != nil || blocked {
						break
					}
					select {
					case err = <-writerDone:
						t.Fatal("baseline writer escaped lock", err)
					case <-time.After(time.Millisecond):
					}
				}
			}
			if err == nil {
				time.Sleep(250 * time.Millisecond)
			}
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			if !prepared {
				if err = <-writerDone; err != nil {
					t.Fatal(err)
				}
				writerLatency = time.Since(started)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			t.Logf("prepared=%v controlled_pause_ms=250 concurrent_writer_ms=%.3f", prepared, float64(writerLatency.Microseconds())/1000)
		})
	}
}
