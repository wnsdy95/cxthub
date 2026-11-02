//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocMessageDisplayPGPublicationLateLeaseExpiry(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	enqueueDocEventDisplayQueue(test, fixture, 2)
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	for steps := 0; prepared.Phase != "completed" && steps < 256; steps++ {
		step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	if prepared.Phase != "completed" || prepared.Source.Kind != domain.EventMessage || !prepared.Source.Summary || job.Target.Event.Index != 2 {
		test.Fatal("expected a completed generic compact-summary message preparation", prepared)
	}
	encoded, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	checkpoint := domain.HashContent(encoded)
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now().UTC(), time.Minute); err != nil {
		test.Fatal(err)
	}
	var originalReceipt string
	var initialLeaseLive bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT to_jsonb(receipt)::text,
 lease_until>clock_timestamp()+interval '50 seconds' FROM doc_display_jobs_v1 receipt WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&originalReceipt, &initialLeaseLive); err != nil || !initialLeaseLive {
		test.Fatal("publication must begin with a fresh minute-long lease", err)
	}
	image := func() string {
		var snapshot string
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'job',(SELECT to_jsonb(receipt) FROM doc_display_jobs_v1 receipt
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND event_hash=$4 AND profile=$6),
 'source',(SELECT to_jsonb(stored) FROM doc_display_sources_v1 stored
 WHERE event_hash=$4 AND source_hash=$5 AND profile=$6),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY hash) FROM doc_display_objects_v1 object WHERE source_hash=$5),
 'generation',(SELECT to_jsonb(generation) FROM doc_display_generations_v1 generation
 WHERE event_hash=$4 AND source_hash=$5 AND profile=$6))::text`,
			job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.Event.Hash, source, job.Target.DisplayProfile()).Scan(&snapshot); err != nil {
			test.Fatal(err)
		}
		return snapshot
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
	before := image()
	var currentSchema string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT current_schema()`).Scan(&currentSchema); err != nil {
		test.Fatal(err)
	}
	ownedSchemaName := domain.NewID("cxt_message_publication_")
	ownedSchema := pgx.Identifier{ownedSchemaName}.Sanitize()
	marker := pgx.Identifier{ownedSchemaName, "entered"}.Sanitize()
	function := pgx.Identifier{ownedSchemaName, "expire_lease"}.Sanitize()
	trigger := pgx.Identifier{ownedSchemaName}.Sanitize()
	jobs := pgx.Identifier{currentSchema, "doc_display_jobs_v1"}.Sanitize()
	generations := pgx.Identifier{currentSchema, "doc_display_generations_v1"}.Sanitize()
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fixture.storage.pool.Exec(cleanup, `DROP TRIGGER IF EXISTS `+trigger+` ON `+generations+`; DROP SCHEMA IF EXISTS `+ownedSchema+` CASCADE`); err != nil {
			test.Error(err)
		}
	})
	hook := fmt.Sprintf(`CREATE SCHEMA %[1]s;
 CREATE SEQUENCE %[2]s;
 CREATE FUNCTION %[3]s() RETURNS trigger LANGUAGE plpgsql AS $publication$
 BEGIN
   IF NOT EXISTS(SELECT 1 FROM %[4]s
     WHERE repo_id='%[5]s' AND doc_hash='%[6]s' AND event_hash='%[7]s' AND event_index=%[8]d
       AND profile='public-event-display-v1' AND version=%[9]d AND preparation_hash='%[10]s'
       AND source_hash='%[13]s' AND state='running' AND lease_until>clock_timestamp()) THEN
     RAISE EXCEPTION 'publication hook requires a live matching lease at insert entry';
   END IF;
   PERFORM nextval('%[2]s'::regclass);
   UPDATE %[4]s SET lease_until=clock_timestamp()+interval '100 milliseconds'
     WHERE repo_id='%[5]s' AND doc_hash='%[6]s' AND event_hash='%[7]s' AND event_index=%[8]d
       AND profile='public-event-display-v1' AND version=%[9]d AND preparation_hash='%[10]s'
       AND source_hash='%[13]s' AND state='running' AND lease_until>clock_timestamp();
   IF NOT FOUND THEN
     RAISE EXCEPTION 'publication hook did not shorten the matching lease';
   END IF;
   PERFORM pg_sleep(0.2);
   RETURN NEW;
 END;
 $publication$;
 CREATE TRIGGER %[11]s BEFORE INSERT ON %[12]s
 FOR EACH ROW WHEN (NEW.event_hash='%[7]s' AND NEW.profile='public-event-display-v1' AND NEW.source_hash='%[13]s')
 EXECUTE FUNCTION %[3]s()`, ownedSchema, marker, function, jobs, job.RepoID, job.DocHash,
		job.Target.Event.Hash, job.Target.Event.Index, job.Version, checkpoint, trigger, generations, source)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, hook); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, checkpoint, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("publication did not reject the lease expired inside its generation insert", err)
	}
	var entered int64
	var called bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT last_value,is_called FROM `+marker).Scan(&entered, &called); err != nil || !called || entered != 1 {
		test.Fatal("completion did not reach the generation insert with a live entry lease", entered, called, err)
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
	if after := image(); after != before {
		test.Fatal("late expiry failed to restore the original running receipt, lease, version, preparation bytes or immutable objects")
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		if _, err := fixture.storage.ReadDocEventDisplayGeneration(read, job.Target); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("rolled-back generation became readable", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TRIGGER `+trigger+` ON `+generations); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now().UTC(), time.Minute); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, checkpoint, time.Now().UTC()); err != nil {
		test.Fatal("retry after removing the synthetic expiry hook did not publish", err)
	}
	var completedReceipt bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state='completed' AND reason='' AND lease_until IS NULL
 AND (to_jsonb(receipt)-ARRAY['state','reason','lease_until'])=($4::jsonb-ARRAY['state','reason','lease_until'])
 FROM doc_display_jobs_v1 receipt WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index, originalReceipt).Scan(&completedReceipt); err != nil || !completedReceipt {
		test.Fatal("publication retry changed the version or saved preparation instead of completing its receipt", err)
	}
	var count int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_generations_v1
 WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, job.Target.DisplayProfile()).Scan(&count); err != nil || count != 1 {
		test.Fatal("publication retry did not leave exactly one serving generation", count, err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		generation, err := fixture.storage.ReadDocEventDisplayGeneration(read, job.Target)
		if err != nil {
			return err
		}
		raw, err := generation.Encode()
		if err != nil || domain.HashContent(raw) != prepared.Generation {
			test.Fatal("publication retry served a different generation", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	published := image()
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, checkpoint, time.Now().UTC()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("completed publication accepted the old running token again", err)
	}
	if image() != published {
		test.Fatal("duplicate completion changed the single published result")
	}
	test.Log("Synthetic same-transaction lease shortening models clock expiry after a live generation-insert entry; it does not model an external mutation. The common post-callback fence rolls back this publication, and a hook-free retry publishes one generation.")
}
