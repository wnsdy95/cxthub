//go:build postgres

package store

import (
	"context"
	"fmt"
	"testing"
)

func TestPGMemoryReadWakeLateSequenceAllocationDoesNotOverwriteNewerNotice(test *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, physical := range []bool{false, true} {
			test.Run(fmt.Sprintf("existing=%t/physical=%t", existing, physical), func(test *testing.T) {
				storage, ctx, target := memoryReadWorkPG(test)
				if !existing {
					catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_wakes WHERE repo_id=$1`, string(target.RepoID))
				}
				name := test.Name()
				catalogExecPG(test, ctx, storage.pool, `CREATE FUNCTION memory_read_wake_sequence_gate() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
 IF current_setting('cxt.test_wake_sequence_gate',true)='on' THEN
   PERFORM pg_advisory_xact_lock(hashtextextended(current_setting('cxt.test_wake_sequence_name'),0));
 END IF;
 RETURN NEW;
 END $$;
 CREATE TRIGGER memory_read_wake_sequence_gate BEFORE INSERT ON memory_read_wakes
 FOR EACH ROW EXECUTE FUNCTION memory_read_wake_sequence_gate()`)
				test.Cleanup(func() {
					_, _ = storage.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS memory_read_wake_sequence_gate ON memory_read_wakes;
 DROP FUNCTION IF EXISTS memory_read_wake_sequence_gate()`)
				})
				blocker := catalogTxPG(test, ctx, storage)
				defer rollbackPG(blocker)
				catalogExecPG(test, ctx, blocker, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, name)
				writer := catalogTxPG(test, ctx, storage)
				defer rollbackPG(writer)
				catalogExecPG(test, ctx, writer, `SELECT set_config('cxt.test_wake_sequence_gate','on',true),set_config('cxt.test_wake_sequence_name',$1,true)`, name)
				done := make(chan error, 1)
				go func() {
					_, err := writer.Exec(ctx, `SELECT cxt_notify_memory_read_wake($1,$2)`, string(target.RepoID), physical)
					done <- err
				}()
				memoryReadPGWaitGate(test, storage, ctx, name, done)
				catalogExecPG(test, ctx, storage.pool, `SELECT cxt_notify_memory_read_wake($1,$2)`, string(target.RepoID), !physical)
				newer := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
				if err := blocker.Commit(ctx); err != nil {
					test.Fatal(err)
				}
				if err := <-done; err != nil {
					test.Fatal("late sequence allocation violated wake constraints", err)
				}
				var first int64
				if err := writer.QueryRow(ctx, `SELECT revision FROM memory_read_wakes WHERE repo_id=$1`, string(target.RepoID)).Scan(&first); err != nil {
					test.Fatal(err)
				}
				catalogExecPG(test, ctx, writer, `SELECT cxt_notify_memory_read_wake($1,$2)`, string(target.RepoID), physical)
				if err := writer.Commit(ctx); err != nil {
					test.Fatal(err)
				}
				actual := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
				if actual.revision <= newer.revision || actual.revision != first {
					test.Fatal("late writer lost a wake or same-transaction coalescing", newer, actual, first)
				}
				if (physical && actual.objectRevision != actual.revision) || (!physical && actual.objectRevision != newer.objectRevision) {
					test.Fatal("physical revision did not follow its locked notice", newer, actual)
				}
			})
		}
	}
}
