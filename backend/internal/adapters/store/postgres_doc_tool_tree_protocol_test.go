//go:build postgres

package store

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocToolTreePGProtocolMigrationFencesOldGeneric(test *testing.T) {
	storage, ctx := docMessageDisplayProtocolPG(test)
	prior := seedDocMessageDisplayProtocolPG(test, storage, ctx, docMessageDisplayProtocolCase{name: "generic", profile: domain.DocDisplayProjectionProfile, state: "running", progress: "discovery"})
	pre := seedDocMessageDisplayProtocolPG(test, storage, ctx, docMessageDisplayProtocolCase{name: "pre", profile: domain.DocDisplayStringProfile, state: "running", progress: "decoding"})
	var expected []byte
	for _, migration := range []string{"0129_doc_message_display.sql", "0130_doc_tool_tree.sql"} {
		if migration == "0130_doc_tool_tree.sql" {
			if err := storage.pool.QueryRow(ctx, `SELECT preparation FROM doc_display_jobs_v1 WHERE repo_id=$1`, prior.RepoID).Scan(&expected); err != nil {
				test.Fatal(err)
			}
			transaction, err := storage.pool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := transaction.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='4'`); err != nil {
				test.Fatal(err)
			}
			prior, err = scanDocDisplayJob(transaction.QueryRow(ctx, `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,
 lease_until=clock_timestamp()+interval '1 hour' WHERE repo_id=$1 RETURNING `+docDisplayJobColumns, prior.RepoID))
			if err != nil {
				rollbackPG(transaction)
				test.Fatal(err)
			}
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal(err)
			}
		}
		raw, err := os.ReadFile("../../../../schemas/db/migrations/" + migration)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := storage.pool.Exec(ctx, string(raw)); err != nil {
			test.Fatal(migration, err)
		}
	}
	current, err := scanDocDisplayJob(storage.pool.QueryRow(ctx, `SELECT `+docDisplayJobColumns+` FROM doc_display_jobs_v1 WHERE repo_id=$1`, prior.RepoID))
	if err != nil || current.Version != prior.Version+1 || current.State != "waiting" || !current.LeaseUntil.IsZero() {
		test.Fatal("protocol handoff retained an old generic lease", current, err)
	}
	var stored []byte
	if err := storage.pool.QueryRow(ctx, `SELECT preparation FROM doc_display_jobs_v1 WHERE repo_id=$1`, prior.RepoID).Scan(&stored); err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(stored, expected) {
		test.Fatal("handoff changed existing preparation bytes")
	}
	if err := storage.RenewDocDisplay(ctx, prior, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old protocol 4 lease remained writable", err)
	}
	if tag, err := storage.pool.Exec(ctx, `UPDATE doc_display_jobs_v1 SET lease_until=GREATEST(lease_until,clock_timestamp()+interval '1 minute')
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4 AND version=$5 AND state='running' AND lease_until>clock_timestamp()`,
		pre.RepoID, pre.DocHash, pre.Target.Event.Index, pre.Target.DisplayProfile(), pre.Version); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("PRE protocol 2 historical lease was invalidated", tag, err)
	}
	for _, protocol := range []string{"2", "3", "4", "5"} {
		transaction, err := storage.pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `SELECT set_config('cxthub.doc_display_protocol',$1,true)`, protocol); err != nil {
			rollbackPG(transaction)
			test.Fatal(err)
		}
		_, err = transaction.Exec(ctx, `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1`, prior.RepoID)
		rollbackPG(transaction)
		if protocol == "5" {
			if err != nil {
				test.Fatal("compatible worker could not claim generic 5", err)
			}
		} else {
			var fence *pgconn.PgError
			if !errors.As(err, &fence) || fence.Code != "0A000" {
				test.Fatal("old claimant bypassed generic protocol fence", protocol, err)
			}
		}
	}
	claimed, err := storage.ClaimDocDisplay(ctx, time.Now(), time.Minute)
	if err != nil || claimed.RepoID != prior.RepoID || claimed.Version != current.Version+1 {
		test.Fatal("new worker did not reclaim migrated generic work", claimed, err)
	}
}
