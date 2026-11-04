//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docEventDisplayQueuePG(test *testing.T) docDisplayPGFixture {
	test.Helper()
	unique := test.Name() + time.Now().String()
	return docDisplayPG(test,
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, CallID: "queue", Output: unique},
		domain.CIREvent{Kind: domain.EventReasoning, Seq: 2, RedactedSummary: unique},
		domain.CIREvent{Kind: domain.EventMessage, Seq: 3, Role: "assistant", CompactSummary: true, Blocks: []domain.ContentBlock{{Type: "text", Text: unique}}},
		domain.CIREvent{Kind: domain.EventReasoning, Seq: 4, ID: unique},
	)
}

func docEventDisplayQueueTarget(fixture docDisplayPGFixture, index int) domain.DocDisplayTarget {
	target := fixture.targets[index]
	target.Profile = domain.DocDisplayProjectionProfile
	return target
}

func enqueueDocEventDisplayQueue(test *testing.T, fixture docDisplayPGFixture, index int) domain.DocDisplayJob {
	test.Helper()
	job, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, docEventDisplayQueueTarget(fixture, index), time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	return job
}

func TestDocEventDisplayPGQueueProfilesAndProtocols(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	pre := fixture.enqueue(test, 0)
	generic := enqueueDocEventDisplayQueue(test, fixture, 1)
	for _, receipt := range []domain.DocDisplayJob{pre, generic} {
		var protocol int
		if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT worker_protocol FROM doc_display_jobs_v1 WHERE `+docDisplayKey(receipt.Target),
			receipt.RepoID, receipt.DocHash, receipt.Target.Event.Index).Scan(&protocol); err != nil {
			test.Fatal(err)
		}
		want := 2
		if receipt.Target.DisplayProfile() == domain.DocDisplayProjectionProfile {
			want = 5
		}
		if protocol != want || receipt.Target.Profile != "" && receipt.Target.Profile != domain.DocDisplayProjectionProfile {
			test.Fatal("profile or protocol changed", receipt, protocol)
		}
	}
	before := fixture.image(test)
	for _, protocol := range []string{"2", "3", "4", "5"} {
		for _, receipt := range []domain.DocDisplayJob{pre, generic} {
			transaction, err := fixture.storage.pool.Begin(fixture.ctx)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := transaction.Exec(fixture.ctx, `SELECT set_config('cxthub.doc_display_protocol',$1,true),set_config('cxthub.doc_catalog_protocol','3',true)`, protocol); err != nil {
				rollbackPG(transaction)
				test.Fatal(err)
			}
			_, err = transaction.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,
 attempts=attempts+1,lease_until=clock_timestamp()+interval '1 minute' WHERE `+docDisplayKey(receipt.Target),
				receipt.RepoID, receipt.DocHash, receipt.Target.Event.Index)
			rollbackPG(transaction)
			if receipt.Target.DisplayProfile() == domain.DocDisplayStringProfile || protocol == "5" {
				if err != nil {
					test.Fatal("compatible protocol could not claim the profile", protocol, receipt.Target.DisplayProfile(), err)
				}
			} else {
				var fence *pgconn.PgError
				if !errors.As(err, &fence) || fence.Code != "0A000" {
					test.Fatal("old protocol claimed a valid generic protocol 5 row", protocol, err)
				}
			}
		}
	}
	if fixture.image(test) != before {
		test.Fatal("rolled-back protocol probes changed the queue")
	}
	first := fixture.claim(test)
	if first.Target.Profile != "" || first.Target.Event.Index != pre.Target.Event.Index {
		test.Fatal("new worker failed to claim PRE protocol 2", first)
	}
	if _, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("generic claim bypassed the shared running limit", err)
	}
	first.State = "rejected"
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, first, time.Now()); err != nil {
		test.Fatal(err)
	}
	claimed := fixture.claim(test)
	if claimed.Target.Profile != domain.DocDisplayProjectionProfile || claimed.Target.Event.Index != generic.Target.Event.Index {
		test.Fatal("new worker failed to claim generic protocol 5", claimed)
	}
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, claimed, time.Now(), time.Minute); err != nil {
		test.Fatal(err)
	}
	retry := claimed
	retry.State, retry.NextAttempt = "retrying", time.Now().Add(-time.Second)
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, retry, time.Now()); err != nil {
		test.Fatal(err)
	}
	reclaimed := fixture.claim(test)
	if reclaimed.Version != claimed.Version+1 || reclaimed.Attempts != claimed.Attempts+1 {
		test.Fatal("generic retry lost the version fence", claimed, reclaimed)
	}
	before = fixture.image(test)
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, claimed, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale generic worker renewed a new claim", err)
	}
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, retry, time.Now()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale generic worker finished a new claim", err)
	}
	if fixture.image(test) != before {
		test.Fatal("stale generic worker changed the queue")
	}
}

func TestDocEventDisplayPGQueueAuthoritativeKindAndProfile(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	target := docEventDisplayQueueTarget(fixture, 1)
	unknown := target
	unknown.Profile = "public-event-display-v1' OR TRUE --"
	if err := unknown.Validate(); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("unknown profile validated", err)
	}
	before := fixture.image(test)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, unknown, time.Now()); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("unknown profile enqueued", err)
	}
	for _, forged := range []domain.DocDisplayTarget{fixture.targets[1]} {
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, forged, time.Now()); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("profile bypassed authoritative event kind", forged, err)
		}
	}
	for _, field := range []string{"index", "hash", "offset", "length"} {
		forged := target
		switch field {
		case "index":
			forged.Event.Index = fixture.targets[0].Event.Index
		case "hash":
			forged.Event.Hash = fixture.targets[0].Event.Hash
		case "offset":
			forged.Event.Offset++
		case "length":
			forged.Event.Length--
		}
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, forged, time.Now()); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("forged event coordinates admitted", field, err)
		}
	}
	if fixture.image(test) != before {
		test.Fatal("invalid targets changed durable state")
	}
	target.Event.Seq, target.Event.Role = 999, "forged-role"
	queued, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now())
	if err != nil || queued.Target.Event != fixture.targets[1].Event {
		test.Fatal("range proof did not normalize event metadata", queued, err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		for _, index := range []int{1, 2, 3} {
			entry, err := fixture.storage.proveDocDisplayEntry(read, docEventDisplayQueueTarget(fixture, index), false)
			if err != nil {
				return err
			}
			wantKind := domain.EventReasoning
			if index == 2 {
				wantKind = domain.EventMessage
			}
			if entry.Kind != wantKind || entry.EmptyReasoning != (index == 3) || entry.CompactSummary != (index == 2) {
				test.Fatal("authoritative entry flags were lost", entry)
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	enqueueDocEventDisplayQueue(test, fixture, 2)
	claimed := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, claimed)
	if err != nil || prepared.Source.Kind != domain.EventReasoning || prepared.Source.EmptyReasoning {
		test.Fatal("generic initialization did not use the authoritative reasoning entry", prepared, err)
	}
	claimed.State = "rejected"
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, claimed, time.Now()); err != nil {
		test.Fatal(err)
	}
	message := fixture.claim(test)
	summary, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, message)
	if err != nil || summary.Version != 1 || summary.Source.Version != 1 || summary.Source.Kind != domain.EventMessage || !summary.Source.Summary || summary.Source.EmptyReasoning {
		test.Fatal("message initialization lost its authoritative summary mode", summary, err)
	}
	before = fixture.image(test)
	if duplicate, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, message); err != nil || !reflect.DeepEqual(duplicate, summary) || fixture.image(test) != before {
		test.Fatal("message initialization changed its idempotent preparation", duplicate, err)
	}
}

func TestDocEventDisplayPGQueueSameEventProfileIsolation(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	queued := enqueueDocEventDisplayQueue(test, fixture, 1)
	pre := queued.Target
	pre.Profile = ""
	if sameDocDisplayTarget(pre, queued.Target) || docDisplayKey(pre) == docDisplayKey(queued.Target) {
		test.Fatal("same event aliases different display profiles")
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 state,created_at,next_attempt,worker_protocol)
 SELECT repo_id,doc_hash,'tool-result-string-v1',event_index,event_hash,event_offset,event_length,event_seq,event_role,
 source_id,source_version,range_generation,'waiting',created_at,clock_timestamp()+interval '1 hour',2
 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(queued.Target), queued.RepoID, queued.DocHash, queued.Target.Event.Index); err != nil {
		test.Fatal(err)
	}
	var before string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job WHERE `+docDisplayKey(pre),
		pre.RepoID, pre.DocHash, pre.Event.Index).Scan(&before); err != nil {
		test.Fatal(err)
	}
	if duplicate := enqueueDocEventDisplayQueue(test, fixture, 1); !reflect.DeepEqual(duplicate, queued) {
		test.Fatal("sibling profile changed the idempotent receipt", duplicate, queued)
	}
	claimed := fixture.claim(test)
	if claimed.Target.Profile != domain.DocDisplayProjectionProfile {
		test.Fatal("claimed the wrong profile", claimed)
	}
	forged := claimed
	forged.Target.Profile = ""
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, forged, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("generic lease crossed into PRE profile", err)
	}
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, claimed, time.Now(), time.Minute); err != nil {
		test.Fatal(err)
	}
	claimed.State = "rejected"
	if err := fixture.storage.FinishDocDisplay(fixture.ctx, claimed, time.Now()); err != nil {
		test.Fatal(err)
	}
	var after string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job WHERE `+docDisplayKey(pre),
		pre.RepoID, pre.DocHash, pre.Event.Index).Scan(&after); err != nil || before != after {
		test.Fatal("generic operations changed same-event PRE receipt", err)
	}
}

func TestDocEventDisplayPGQueuePREPortsRejectGeneric(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	enqueueDocEventDisplayQueue(test, fixture, 1)
	job := fixture.claim(test)
	before := fixture.image(test)
	if _, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("generic job entered PRE preparation", err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		if _, err := fixture.storage.ReadDocDisplayGeneration(read, job.Target); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("generic target entered PRE generation read", err)
		}
		if _, err := fixture.storage.ReadDocDisplayPage(read, job.Target, 0); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("generic target entered PRE page read", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if fixture.image(test) != before {
		test.Fatal("PRE profile rejection changed generic state")
	}
}

func TestDocEventDisplayPGQueueMixedCapacityAndBindingVersion(test *testing.T) {
	unique := test.Name() + time.Now().String()
	events := make([]domain.CIREvent, domain.MaxPendingDocRangeJobs+2)
	for index := range events {
		text := fmt.Sprintf("%s-%d", unique, index)
		events[index] = domain.CIREvent{Kind: domain.EventToolResult, Seq: index + 1, CallID: text, Output: text}
		if index%2 == 1 {
			events[index] = domain.CIREvent{Kind: domain.EventReasoning, Seq: index + 1, RedactedSummary: text}
		}
	}
	fixture := docDisplayPG(test, events...)
	receipts := make([]domain.DocDisplayJob, domain.MaxPendingDocRangeJobs)
	for index := range receipts {
		if index%2 == 0 {
			receipts[index] = fixture.enqueue(test, index)
		} else {
			receipts[index] = enqueueDocEventDisplayQueue(test, fixture, index)
		}
	}
	before := fixture.image(test)
	for _, target := range []domain.DocDisplayTarget{fixture.targets[domain.MaxPendingDocRangeJobs], docEventDisplayQueueTarget(fixture, domain.MaxPendingDocRangeJobs+1)} {
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
			test.Fatal("mixed profile queue exceeded shared capacity", err)
		}
	}
	for _, receipt := range receipts[:2] {
		duplicate, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, receipt.Target, time.Now())
		if err != nil || !reflect.DeepEqual(duplicate, receipt) {
			test.Fatal("full mixed queue lost idempotent receipt", duplicate, err)
		}
	}
	if fixture.image(test) != before {
		test.Fatal("full queue demand changed existing jobs")
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.doc.hash); err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
		return fixture.storage.ensureRangeGeneration(bound, fixture.repo, fixture.doc.hash)
	}); err != nil {
		test.Fatal(err)
	}
	fixture.refreshTargets(test)
	for index, previous := range receipts[:2] {
		target := fixture.targets[index]
		if index == 1 {
			target.Profile = domain.DocDisplayProjectionProfile
		}
		replaced, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now())
		if err != nil || replaced.Version != previous.Version+1 || replaced.State != "waiting" || replaced.Attempts != 0 || replaced.Target.Binding == previous.Target.Binding {
			test.Fatal("full mixed queue failed to replace an active source binding", replaced, err)
		}
	}
	var active int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_jobs_v1 WHERE repo_id=$1
 AND state IN ('waiting','running','retrying')`, fixture.repo).Scan(&active); err != nil || active != domain.MaxPendingDocRangeJobs {
		test.Fatal("binding replacement changed shared capacity", active, err)
	}
}

func TestDocEventDisplayPGQueueCompletedPreparationReopen(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	enqueueDocEventDisplayQueue(test, fixture, 1)
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	foreignRaw := []byte("queue-profile-presence-probe")
	foreignHash := domain.HashContent(foreignRaw)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,'tool-result-string-v1',$2,$3)`, job.Target.Event.Hash, foreignHash, foreignRaw); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes)
 VALUES($1,'tool-result-string-v1',$2,$2,$3)`, job.Target.Event.Hash, foreignHash, foreignRaw); err != nil {
		test.Fatal(err)
	}
	if _, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET state='completed',lease_until=NULL,
 attempts=7,reason='descriptor_evicted',next_attempt=clock_timestamp()+interval '1 hour' WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 state,created_at,next_attempt,worker_protocol)
 SELECT repo_id,doc_hash,CASE WHEN slot%2=0 THEN 'tool-result-string-v1' ELSE 'public-event-display-v1' END,
 100+slot,event_hash,event_offset,event_length,event_seq,event_role,source_id,source_version,range_generation,
 'waiting',created_at,clock_timestamp()+interval '1 hour',CASE WHEN slot%2=0 THEN 2 ELSE 5 END
 FROM doc_display_jobs_v1 CROSS JOIN generate_series(0,$4::integer-1) slot WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index, domain.MaxPendingDocRangeJobs); err != nil {
		test.Fatal(err)
	}
	full := fixture.image(test)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, job.Target, time.Now()); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("generic reopen exceeded mixed active capacity", err)
	}
	if fixture.image(test) != full {
		test.Fatal("overloaded generic reopen changed its completed receipt")
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1 AND event_index=100`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	reopened, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, job.Target, now)
	if err != nil || reopened.State != "waiting" || reopened.Version != job.Version+1 || reopened.Attempts != 0 || reopened.Reason != "" || !reopened.CreatedAt.Equal(job.CreatedAt) || !reopened.NextAttempt.Equal(now) {
		test.Fatal("generic receipt did not reopen without its own generation", reopened, err)
	}
	var restoredHash, restoredSource domain.ContentHash
	var restored []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT preparation_hash,preparation,source_hash FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&restoredHash, &restored, &restoredSource); err != nil || restoredHash != domain.HashContent(raw) || restoredSource != source || !bytes.Equal(restored, raw) {
		test.Fatal("generic reopen changed its preparation or immutable scope", err)
	}
	before := fixture.image(test)
	if duplicate := enqueueDocEventDisplayQueue(test, fixture, 1); !reflect.DeepEqual(duplicate, reopened) || fixture.image(test) != before {
		test.Fatal("duplicate reopen demand changed the receipt")
	}
	reclaimed := fixture.claim(test)
	if reclaimed.Version != reopened.Version+1 || reclaimed.Attempts != 1 {
		test.Fatal("generic reopen claim did not advance the version", reclaimed)
	}
	if _, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old generic worker loaded reopened progress", err)
	}
	loaded, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, reclaimed)
	if err != nil || !reflect.DeepEqual(loaded, prepared) {
		test.Fatal("reopened generic job lost progress", loaded, err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes)
 VALUES($1,$2,$3,$4,$5)`, job.Target.Event.Hash, domain.DocDisplayProjectionProfile, source, domain.HashContent(raw), raw); err != nil {
		test.Fatal(err)
	}
	if _, err := docDisplayJobCorruptionPG(fixture.ctx, fixture.storage, `UPDATE doc_display_jobs_v1 SET state='completed',lease_until=NULL WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index); err != nil {
		test.Fatal(err)
	}
	before = fixture.image(test)
	stable := enqueueDocEventDisplayQueue(test, fixture, 1)
	if stable.State != "completed" || stable.Version != reclaimed.Version || fixture.image(test) != before {
		test.Fatal("present generic generation reopened a completed receipt", stable)
	}
}

func TestDocEventDisplayPGQueueMigrationPreservesPREAndConstraints(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	fixture.initialize(test, job)
	before := fixture.image(test)
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	var originalSchema string
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_schema()`).Scan(&originalSchema); err != nil {
		test.Fatal(err)
	}
	schema := pgx.Identifier{domain.NewID("cxt_event_queue_migration_")}.Sanitize()
	if _, err := transaction.Exec(fixture.ctx, `CREATE SCHEMA `+schema+`; SET LOCAL search_path TO `+schema+`; CREATE TABLE repos(id text PRIMARY KEY)`); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"0126_doc_display.sql", "0127_doc_display_discovery.sql"} {
		raw, err := os.ReadFile("../../../../schemas/db/migrations/" + name)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(fixture.ctx, string(raw)); err != nil {
			test.Fatal(err)
		}
	}
	var oldConstraint bool
	if err := transaction.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint
 WHERE conrelid='doc_display_jobs_v1'::regclass AND conname='doc_display_jobs_v1_check1')`).Scan(&oldConstraint); err != nil || !oldConstraint {
		test.Fatal("migration 0126 generated an unexpected checkpoint constraint name", oldConstraint, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `INSERT INTO repos(id) VALUES($1)`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 SELECT event_hash,profile,source_hash,bytes FROM `+pgx.Identifier{originalSchema, "doc_display_sources_v1"}.Sanitize()+` WHERE event_hash=$1`, job.Target.Event.Hash); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `SET LOCAL cxthub.doc_display_protocol='2'`); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_display_jobs_v1
 SELECT (jsonb_populate_record(NULL::doc_display_jobs_v1,to_jsonb(stored))).*
 FROM `+pgx.Identifier{originalSchema, "doc_display_jobs_v1"}.Sanitize()+` stored WHERE repo_id=$1`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	var oldImage string
	if err := transaction.QueryRow(fixture.ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job`).Scan(&oldImage); err != nil {
		test.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0128_doc_event_display.sql")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, string(migration)); err != nil {
		test.Fatal("apply migration 0128 over populated PRE protocol 2 schema", err)
	}
	var newImage string
	if err := transaction.QueryRow(fixture.ctx, `SELECT (to_jsonb(job)-'preparation_hash'-'preparation')::text FROM doc_display_jobs_v1 job`).Scan(&newImage); err != nil || newImage != oldImage {
		test.Fatal("migration changed an active PRE receipt or checkpoint", err)
	}
	if _, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_display_jobs_v1
 SELECT (jsonb_populate_record(NULL::doc_display_jobs_v1,to_jsonb(stored)||
 jsonb_build_object('profile','public-event-display-v1','worker_protocol',3,'state','waiting','lease_until',NULL,
 'source_hash',NULL,'checkpoint_hash',NULL,'checkpoint',NULL))).*
 FROM doc_display_jobs_v1 stored WHERE profile='tool-result-string-v1'`); err != nil {
		test.Fatal("migration rejected an uninitialized generic queue row", err)
	}
	checks := []struct {
		name      string
		statement string
		arguments []any
		code      string
	}{
		{name: "invalid_protocol_check", statement: `UPDATE doc_display_jobs_v1 SET worker_protocol=1 WHERE profile='tool-result-string-v1'`, code: "23514"},
		{name: "invalid_profile_check", statement: `UPDATE doc_display_jobs_v1 SET profile='unknown-profile' WHERE profile='tool-result-string-v1'`, code: "23514"},
		{name: "generic_wrong_protocol", statement: `UPDATE doc_display_jobs_v1 SET worker_protocol=2,state='running',lease_until=clock_timestamp()+interval '1 minute' WHERE profile='public-event-display-v1'`, code: "23514"},
		{name: "old_worker_generic_claim", statement: `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,lease_until=clock_timestamp()+interval '1 minute' WHERE profile='public-event-display-v1'`, code: "0A000"},
		{name: "pre_preparation", statement: `UPDATE doc_display_jobs_v1 SET preparation_hash=$1,preparation=$2 WHERE profile='tool-result-string-v1'`, arguments: []any{domain.HashContent([]byte("prep")), []byte("prep")}, code: "23514"},
		{name: "generic_pre_checkpoint", statement: `UPDATE doc_display_jobs_v1 SET checkpoint_hash=$1,checkpoint=$2 WHERE profile='public-event-display-v1'`, arguments: []any{domain.HashContent([]byte("checkpoint")), []byte("checkpoint")}, code: "23514"},
		{name: "generic_pre_discovery", statement: `UPDATE doc_display_jobs_v1 SET discovery_hash=$1,discovery=$2 WHERE profile='public-event-display-v1'`, arguments: []any{domain.HashContent([]byte("discovery")), []byte("discovery")}, code: "23514"},
		{name: "preparation_pair", statement: `UPDATE doc_display_jobs_v1 SET preparation_hash=$1 WHERE profile='public-event-display-v1'`, arguments: []any{domain.HashContent([]byte("unpaired"))}, code: "23514"},
		{name: "preparation_source_pair", statement: `UPDATE doc_display_jobs_v1 SET preparation_hash=$1,preparation=$2 WHERE profile='public-event-display-v1'`, arguments: []any{domain.HashContent([]byte("unscoped")), []byte("unscoped")}, code: "23514"},
		{name: "preparation_bounds", statement: `UPDATE doc_display_jobs_v1 SET source_hash=(SELECT source_hash FROM doc_display_sources_v1 LIMIT 1),preparation_hash=$1,preparation=$2 WHERE profile='public-event-display-v1'`, arguments: []any{domain.HashContent(bytes.Repeat([]byte("x"), 2049)), bytes.Repeat([]byte("x"), 2049)}, code: "23514"},
	}
	for _, check := range checks {
		test.Run(check.name, func(test *testing.T) {
			if _, err := transaction.Exec(fixture.ctx, `SAVEPOINT queue_constraint`); err != nil {
				test.Fatal(err)
			}
			_, actionErr := transaction.Exec(fixture.ctx, check.statement, check.arguments...)
			if _, err := transaction.Exec(fixture.ctx, `ROLLBACK TO SAVEPOINT queue_constraint; RELEASE SAVEPOINT queue_constraint`); err != nil {
				test.Fatal(err)
			}
			var constraint *pgconn.PgError
			if !errors.As(actionErr, &constraint) || constraint.Code != check.code {
				test.Fatal("unexpected queue constraint/protocol error", actionErr, check.code)
			}
		})
	}
	if err := transaction.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if fixture.image(test) != before {
		test.Fatal("private migration probe changed the fixture")
	}
}
