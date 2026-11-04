//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNormalizationCheckpointPGAdmissionRechecksAfterSourceWait(test *testing.T) {
	for _, change := range []string{"lease expiry", "job fence", "source version", "healthy renewal"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
			peer, trace := normalizationBatchPGPeer(test, storage, ctx)
			connection, err := peer.pool.Acquire(ctx)
			if err != nil {
				test.Fatal(err)
			}
			waiter := connection.Conn().PgConn().PID()
			connection.Release()
			observer, err := storage.pool.Acquire(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer observer.Release()
			holder, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(holder)
			var held domain.ContentHash
			if err := holder.QueryRow(ctx, `SELECT hash FROM blobs WHERE hash=$1 FOR UPDATE`, job.DocHash).Scan(&held); err != nil {
				test.Fatal(err)
			}
			selected, cancel := context.WithCancel(ctx)
			completed, finished := make(chan error, 1), make(chan struct{})
			go func() {
				defer close(finished)
				transaction, actual, err := peer.openLegacyByteCheckpoint(selected, job, state.stage, &state, false)
				if transaction != nil {
					rollbackPG(transaction)
				}
				if err == nil && actual != state {
					err = domain.ErrIntegrity
				}
				completed <- err
			}()
			defer func() { cancel(); rollbackPG(holder); <-finished }()
			authorityScopeWaitPG(test, ctx, observer.Conn(), holder.Conn().PgConn().PID(), waiter, "retained_source", completed)
			update := `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()+interval '1 hour' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`
			switch change {
			case "lease expiry":
				update = `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`
			case "job fence":
				update = `UPDATE doc_range_jobs_v1 SET version=version+1,state='waiting',lease_until=NULL WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`
			case "source version":
				update = `UPDATE blobs SET bytes=bytes WHERE hash=$2 AND EXISTS(SELECT 1 FROM repos WHERE id=$1)`
			}
			if _, err := holder.Exec(ctx, update, job.RepoID, job.DocHash); err != nil {
				test.Fatal(err)
			}
			if err := holder.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			select {
			case err := <-completed:
				if change == "healthy renewal" && err != nil || change != "healthy renewal" && !errors.Is(err, domain.ErrConflict) {
					test.Fatal("batched admission reused a pre-wait fence or rejected renewal", change, err)
				}
			case <-ctx.Done():
				test.Fatal("admission did not finish after releasing the source lock", ctx.Err())
			}
			trace.mutex.Lock()
			maximum := trace.maximum
			trace.mutex.Unlock()
			if maximum != 2 {
				test.Fatal("source admission and post-wait liveness were not separate statements in one batch", maximum)
			}
		})
	}
}
