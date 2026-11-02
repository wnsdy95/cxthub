//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGHistoryPublicationMessagePromotionCAS(test *testing.T) {
	fixture := historyProofFixture(test)
	if _, err := fixture.s.pool.Exec(fixture.ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.snap.ID, domain.HookMessagePrefix+"checkpoint"); err != nil {
		test.Fatal(err)
	}
	const contenders = 8
	start := make(chan struct{})
	results := make(chan error, contenders)
	for index := 0; index < contenders; index++ {
		go func() {
			<-start
			results <- fixture.s.UpdateSnapshotMessage(fixture.ctx, fixture.repo, fixture.snap.ID, fmt.Sprintf("commit message %d", index))
		}()
	}
	close(start)
	winners, conflicts := 0, 0
	for index := 0; index < contenders; index++ {
		err := <-results
		switch {
		case err == nil:
			winners++
		case errors.Is(err, domain.ErrConflict):
			conflicts++
		default:
			test.Errorf("unexpected promotion result: %v", err)
		}
	}
	if winners != 1 || conflicts != contenders-1 {
		test.Fatalf("promotion CAS: winners=%d conflicts=%d", winners, conflicts)
	}
	var message, before string
	if err := fixture.s.pool.QueryRow(fixture.ctx, `SELECT message,xmin::text||':'||ctid::text FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.snap.ID).Scan(&message, &before); err != nil {
		test.Fatal(err)
	}
	if err := fixture.s.UpdateSnapshotMessage(fixture.ctx, fixture.repo, fixture.snap.ID, message); err != nil {
		test.Fatal("winning message replay", err)
	}
	var after string
	if err := fixture.s.pool.QueryRow(fixture.ctx, `SELECT xmin::text||':'||ctid::text FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.snap.ID).Scan(&after); err != nil || after != before {
		test.Fatalf("winning replay rewrote tuple: changed=%v err=%v", after != before, err)
	}
	missing := domain.HashContent([]byte("missing snapshot:" + string(fixture.repo)))
	if err := fixture.s.UpdateSnapshotMessage(fixture.ctx, fixture.repo, missing, message); !errors.Is(err, domain.ErrNotFound) {
		test.Fatalf("missing promotion returned %v", err)
	}
}

func TestPGHistoryPublicationMessagePromotionRollback(test *testing.T) {
	fixture := historyProofFixture(test)
	message := domain.HookMessagePrefix + "checkpoint"
	if _, err := fixture.s.pool.Exec(fixture.ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.snap.ID, message); err != nil {
		test.Fatal(err)
	}
	var before string
	if err := fixture.s.pool.QueryRow(fixture.ctx, `SELECT xmin::text||':'||ctid::text FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.snap.ID).Scan(&before); err != nil {
		test.Fatal(err)
	}
	stop := errors.New("rollback promotion")
	err := fixture.s.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
		for attempt := 0; attempt < 2; attempt++ {
			if err := fixture.s.UpdateSnapshotMessage(bound, fixture.repo, fixture.snap.ID, "first-source [git abc1234]"); err != nil {
				return err
			}
		}
		return stop
	})
	if !errors.Is(err, stop) {
		test.Fatal(err)
	}
	var current, after string
	if err := fixture.s.pool.QueryRow(fixture.ctx, `SELECT message,xmin::text||':'||ctid::text FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.snap.ID).Scan(&current, &after); err != nil || current != message || after != before {
		test.Fatalf("promotion escaped enclosing rollback: messageChanged=%v tupleChanged=%v err=%v", current != message, after != before, err)
	}
}

func TestPGHistoryPublicationMessageReplaySharesProofPin(test *testing.T) {
	for _, message := range []string{"first-source [git abc1234]", domain.HookMessagePrefix + "checkpoint"} {
		for _, finish := range []string{"commit", "rollback"} {
			test.Run(message+"/"+finish, func(test *testing.T) {
				fixture := historyProofFixture(test)
				if _, err := fixture.s.pool.Exec(fixture.ctx, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.snap.ID, message); err != nil {
					test.Fatal(err)
				}
				fixture.snap.Message = message
				replay, err := fixture.s.pool.Begin(fixture.ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(replay)
				replayContext := context.WithValue(fixture.ctx, repositoryTxKey{}, &repositoryTx{Tx: replay, owner: fixture.s, repo: fixture.repo})
				err = fixture.s.WithinHistoryDocumentProof(fixture.ctx, fixture.repo, func(ctx context.Context, proof outbound.HistoryDocumentProof) error {
					if err := fixture.prepare(ctx, proof, nil); err != nil {
						return err
					}
					if err := fixture.s.UpdateSnapshotMessage(replayContext, fixture.repo, fixture.snap.ID, message); err != nil {
						return err
					}
					return fixture.s.WithinRepository(ctx, fixture.repo, proof.Pin)
				})
				if err != nil {
					test.Fatalf("uncommitted exact replay blocked an unchanged proof: %v", err)
				}
				const writerLock = `SELECT 1 FROM snapshots WHERE repo_id=$1 AND id=$2 FOR NO KEY UPDATE NOWAIT`
				_, err = fixture.s.pool.Exec(fixture.ctx, writerLock, fixture.repo, fixture.snap.ID)
				var locked *pgconn.PgError
				if !errors.As(err, &locked) || locked.Code != "55P03" {
					test.Fatalf("replay did not exclude the message writer lock after proof commit: %v", err)
				}
				if finish == "commit" {
					err = replay.Commit(fixture.ctx)
				} else {
					err = replay.Rollback(fixture.ctx)
				}
				if err != nil {
					test.Fatal(err)
				}
				if _, err := fixture.s.pool.Exec(fixture.ctx, writerLock, fixture.repo, fixture.snap.ID); err != nil {
					test.Fatal("replay lock outlived its transaction", err)
				}
				err = fixture.s.UpdateSnapshotMessage(fixture.ctx, fixture.repo, fixture.snap.ID, "next commit")
				if message == "first-source [git abc1234]" {
					if !errors.Is(err, domain.ErrConflict) {
						test.Fatalf("different committed message was overwritten: %v", err)
					}
				} else if err != nil {
					test.Fatal("real promotion failed after replay released its lock", err)
				}
			})
		}
	}
}
