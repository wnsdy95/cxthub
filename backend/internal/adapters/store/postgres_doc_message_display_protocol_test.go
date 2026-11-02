//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docMessageDisplayProtocolCase struct {
	name     string
	profile  string
	state    string
	reason   string
	progress string
}

func readDocEventDisplayLegacyPreparationPG(ctx context.Context, storage *PostgresStore, job domain.DocDisplayJob) (domain.DocEventDisplayPreparation, error) {
	var hash, source domain.ContentHash
	var raw, sourceRaw []byte
	if err := storage.db(ctx).QueryRow(ctx, `SELECT substring(job.preparation_hash FROM 1 FOR 72),substring(job.preparation FROM 1 FOR $4),
 substring(job.source_hash FROM 1 FOR 72),substring(source.bytes FROM 1 FOR $5)
 FROM doc_display_jobs_v1 job JOIN doc_display_sources_v1 source ON source.source_hash=job.source_hash
 AND source.event_hash=job.event_hash AND source.profile=job.profile
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.event_index=$3 AND job.profile='public-event-display-v1'`,
		job.RepoID, job.DocHash, job.Target.Event.Index, domain.MaxDocEventDisplayPreparationBytes+1, domain.MaxDocDisplaySourceBytes+1).
		Scan(&hash, &raw, &source, &sourceRaw); err != nil {
		return domain.DocEventDisplayPreparation{}, mapNoRows(err)
	}
	state, err := domain.DecodeDocEventDisplayPreparation(hash, raw)
	if err != nil || state.Source.EventHash != job.Target.Event.Hash || state.Source.EventBytes != job.Target.Event.Length ||
		!equalDocEventDisplaySource(state.Source, sourceRaw, source) {
		return domain.DocEventDisplayPreparation{}, domain.ErrIntegrity
	}
	return state, ctx.Err()
}

func renewDocMessageDisplayLegacyPG(ctx context.Context, storage *PostgresStore, job domain.DocDisplayJob, now time.Time, lease time.Duration) error {
	transaction, err := storage.beginDocRangeJob(ctx)
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if err := lockDocDisplayJob(ctx, transaction, job); err != nil {
		return err
	}
	if _, err := transaction.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='4'`); err != nil {
		return err
	}
	var before, after string
	var previous, current time.Time
	renewed := now.Add(lease).Truncate(time.Microsecond)
	if err := transaction.QueryRow(ctx, `SELECT (to_jsonb(job)-'lease_until')::text,lease_until FROM doc_display_jobs_v1 job
 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index).Scan(&before, &previous); err != nil {
		return err
	}
	err = transaction.QueryRow(ctx, `UPDATE doc_display_jobs_v1 SET lease_until=GREATEST(lease_until,$6)
 WHERE `+docDisplayKey(job.Target)+` AND version=$4 AND state='running' AND lease_until>$5 AND lease_until>clock_timestamp()
 AND ((profile='public-event-display-v1' AND worker_protocol=4) OR (profile='tool-result-string-v1' AND worker_protocol=2))
 RETURNING (to_jsonb(doc_display_jobs_v1)-'lease_until')::text,lease_until`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Version, now, renewed).Scan(&after, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrConflict
	}
	if err != nil {
		return err
	}
	expected := previous
	if renewed.After(expected) {
		expected = renewed
	}
	if before != after || !current.Equal(expected) {
		return domain.ErrIntegrity
	}
	return transaction.Commit(ctx)
}

func docMessageDisplayProtocolPG(test *testing.T) (*PostgresStore, context.Context) {
	test.Helper()
	parent, ctx := chunkReusePG(test)
	schema := pgx.Identifier{domain.NewID("cxt_message_protocol_")}.Sanitize()
	if _, err := parent.pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := parent.pool.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			test.Error(err)
		}
	})
	config := parent.pool.Config()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	storage := &PostgresStore{pool: pool}
	test.Cleanup(storage.Close)
	transaction, err := pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `CREATE TABLE repos(id text PRIMARY KEY)`); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"0126_doc_display.sql", "0127_doc_display_discovery.sql", "0128_doc_event_display.sql"} {
		raw, err := os.ReadFile("../../../../schemas/db/migrations/" + name)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, string(raw)); err != nil {
			test.Fatal(name, err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	return storage, ctx
}

func seedDocMessageDisplayProtocolPG(test *testing.T, storage *PostgresStore, ctx context.Context, sample docMessageDisplayProtocolCase) domain.DocDisplayJob {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name() + sample.name))
	document := domain.HashContent([]byte("document-" + string(repo)))
	event := domain.HashContent([]byte("event-" + string(repo)))
	rangeGeneration := domain.HashContent([]byte("range-" + string(repo)))
	now := time.Now().UTC().Truncate(time.Microsecond)
	nextAttempt := now.Add(-time.Hour)
	var leaseUntil *time.Time
	if sample.state == "running" {
		deadline := now.Add(time.Hour)
		leaseUntil = &deadline
		nextAttempt = now.Add(-2 * time.Hour)
	} else if sample.state == "rejected" || sample.state == "completed" {
		nextAttempt = now.Add(24 * time.Hour)
	}
	var sourceHash, checkpointHash, discoveryHash, preparationHash *domain.ContentHash
	var checkpoint, discovery, preparation []byte
	protocol := 2
	if sample.profile == domain.DocDisplayProjectionProfile {
		protocol = 3
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='3'; INSERT INTO repos(id) VALUES('`+string(repo)+`')`); err != nil {
		test.Fatal(err)
	}
	if sample.progress == "discovery" && protocol == 2 {
		discovery = append([]byte{0, 255, 128}, []byte(sample.name)...)
		hash := domain.HashContent(discovery)
		discoveryHash = &hash
	} else if sample.progress != "" {
		source := []byte("legacy-PRE-source-" + string(event))
		object := append([]byte{0, 255, 128}, []byte(sample.name)...)
		objectHash := domain.HashContent(object)
		if protocol == 3 {
			source = []byte(fmt.Sprintf(`{"empty_reasoning":false,"event_bytes":123,"event_hash":%q,"kind":"reasoning","profile":"public-event-display-v1","version":1}`, event))
			if sample.progress == "completed" {
				preparation = []byte(fmt.Sprintf(`{"decoded":%q,"generation":%q,"phase":"completed","plan":%q,"source":%s,"version":1}`, objectHash, objectHash, objectHash, source))
			} else {
				preparation = []byte(fmt.Sprintf(`{"active":%q,"phase":"discovery","source":%s,"version":1}`, objectHash, source))
			}
			hash := domain.HashContent(preparation)
			preparationHash = &hash
			if _, err := domain.DecodeDocEventDisplayPreparation(hash, preparation); err != nil {
				test.Fatal("legacy reasoning preparation no longer decodes canonically", err)
			}
		} else {
			checkpoint = append([]byte{0, 254, 127}, []byte(sample.name)...)
			hash := domain.HashContent(checkpoint)
			checkpointHash = &hash
		}
		hash := domain.HashContent(source)
		sourceHash = &hash
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes) VALUES($1,$2,$3,$4)`, event, sample.profile, hash, source); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) VALUES($1,$2,$3)`, hash, objectHash, object); err != nil {
			test.Fatal(err)
		}
		if sample.state == "completed" {
			if _, err := transaction.Exec(ctx, `INSERT INTO doc_display_generations_v1(event_hash,profile,source_hash,generation_hash,bytes) VALUES($1,$2,$3,$4,$5)`, event, sample.profile, hash, objectHash, object); err != nil {
				test.Fatal(err)
			}
		}
	}
	job, err := scanDocDisplayJob(transaction.QueryRow(ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,
 source_id,source_version,range_generation,state,reason,attempts,version,created_at,next_attempt,lease_until,
 source_hash,checkpoint_hash,checkpoint,discovery_hash,discovery,worker_protocol,preparation_hash,preparation)
 VALUES($1,$2,$3,0,$4,0,123,1,'assistant','11111111-1111-4111-8111-111111111111',7,$5,$6,$7,7,40,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
 RETURNING `+docDisplayJobColumns, repo, document, sample.profile, event, rangeGeneration, sample.state, sample.reason,
		now.Add(-24*time.Hour), nextAttempt, leaseUntil, sourceHash, checkpointHash, checkpoint, discoveryHash, discovery, protocol, preparationHash, preparation))
	if err != nil {
		test.Fatal(sample.name, err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	return job
}

func docMessageDisplayProtocolImagePG(test *testing.T, storage *PostgresStore, ctx context.Context, jobs bool) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'jobs',CASE WHEN $1::boolean THEN (SELECT jsonb_agg(to_jsonb(job) ORDER BY repo_id,event_index,profile) FROM doc_display_jobs_v1 job) END,
 'sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY event_hash,profile) FROM doc_display_sources_v1 source),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY source_hash,hash) FROM doc_display_objects_v1 object),
 'generations',(SELECT jsonb_agg(to_jsonb(generation) ORDER BY event_hash,profile) FROM doc_display_generations_v1 generation),
 'guard',CASE WHEN $1::boolean THEN pg_get_functiondef('cxthub_guard_doc_display_protocol()'::regprocedure) END)::text`, jobs).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func TestDocMessageDisplayPGProtocolMigrationHandoff(test *testing.T) {
	storage, ctx := docMessageDisplayProtocolPG(test)
	samples := []docMessageDisplayProtocolCase{
		{name: "pre_waiting", profile: domain.DocDisplayStringProfile, state: "waiting"},
		{name: "pre_running", profile: domain.DocDisplayStringProfile, state: "running", progress: "checkpoint"},
		{name: "pre_retrying", profile: domain.DocDisplayStringProfile, state: "retrying", progress: "discovery"},
		{name: "pre_completed", profile: domain.DocDisplayStringProfile, state: "completed", progress: "checkpoint"},
		{name: "pre_unsupported", profile: domain.DocDisplayStringProfile, state: "rejected", reason: "unsupported_display", progress: "checkpoint"},
		{name: "generic_waiting", profile: domain.DocDisplayProjectionProfile, state: "waiting"},
		{name: "generic_running", profile: domain.DocDisplayProjectionProfile, state: "running", progress: "discovery"},
		{name: "generic_retrying", profile: domain.DocDisplayProjectionProfile, state: "retrying", progress: "discovery"},
		{name: "generic_completed", profile: domain.DocDisplayProjectionProfile, state: "completed", progress: "completed"},
		{name: "generic_unsupported_message", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "unsupported_display"},
		{name: "generic_unsupported_reasoning", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "unsupported_display", progress: "discovery"},
		{name: "generic_rejected", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "invalid_source", progress: "discovery"},
		{name: "generic_other_unsupported", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "unsupported_display_other"},
	}
	captured := make(map[string]domain.DocDisplayJob)
	images := make(map[string]string)
	for _, sample := range samples {
		job := seedDocMessageDisplayProtocolPG(test, storage, ctx, sample)
		captured[sample.name] = job
		var image string
		if err := storage.pool.QueryRow(ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job WHERE repo_id=$1`, job.RepoID).Scan(&image); err != nil {
			test.Fatal(err)
		}
		images[sample.name] = image
	}
	before := docMessageDisplayProtocolImagePG(test, storage, ctx, true)
	immutable := docMessageDisplayProtocolImagePG(test, storage, ctx, false)
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0129_doc_message_display.sql")
	if err != nil {
		test.Fatal(err)
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, string(migration)); err != nil {
		test.Fatal("migration over populated protocol 3 schema", err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if docMessageDisplayProtocolImagePG(test, storage, ctx, true) != before {
		test.Fatal("rollback changed receipts, checkpoint bytes, objects or the protocol guard")
	}
	transaction, err = storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	var started, finished time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&started); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, string(migration)); err != nil {
		test.Fatal(err)
	}
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&finished); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	for _, sample := range samples {
		prior := captured[sample.name]
		current, err := scanDocDisplayJob(storage.pool.QueryRow(ctx, `SELECT `+docDisplayJobColumns+` FROM doc_display_jobs_v1 WHERE repo_id=$1`, prior.RepoID))
		if err != nil {
			test.Fatal(sample.name, err)
		}
		var unchanged bool
		if sample.profile == domain.DocDisplayStringProfile {
			if err := storage.pool.QueryRow(ctx, `SELECT to_jsonb(job)=$2::jsonb FROM doc_display_jobs_v1 job WHERE repo_id=$1`, prior.RepoID, images[sample.name]).Scan(&unchanged); err != nil || !unchanged {
				test.Fatal("PRE protocol 2 receipt or checkpoint changed", sample.name, err)
			}
			continue
		}
		if err := storage.pool.QueryRow(ctx, `SELECT
 (to_jsonb(job)-ARRAY['worker_protocol','version','state','reason','next_attempt','lease_until']) =
 ($2::jsonb-ARRAY['worker_protocol','version','state','reason','next_attempt','lease_until'])
 AND worker_protocol=4 FROM doc_display_jobs_v1 job WHERE repo_id=$1`, prior.RepoID, images[sample.name]).Scan(&unchanged); err != nil || !unchanged {
			test.Fatal("generic migration rewrote checkpoint bytes, hashes, binding or receipt metadata", sample.name, err)
		}
		wantState, wantReason := prior.State, prior.Reason
		reopened := prior.State == "rejected" && prior.Reason == "unsupported_display"
		if prior.State == "running" || reopened {
			wantState = "waiting"
		}
		if reopened {
			wantReason = ""
		}
		if current.Version != prior.Version+1 || current.State != wantState || current.Reason != wantReason || !current.LeaseUntil.IsZero() {
			test.Fatal("generic handoff lost its lease or version fence", sample.name, prior, current)
		}
		if reopened {
			if current.NextAttempt.Before(started) || current.NextAttempt.After(finished) {
				test.Fatal("unsupported display rejection did not become freshly due", sample.name, current)
			}
		} else if !current.NextAttempt.Equal(prior.NextAttempt) {
			test.Fatal("unrelated due time changed", sample.name, prior, current)
		}
		if sample.progress != "" {
			prepared, err := readDocEventDisplayLegacyPreparationPG(ctx, storage, current)
			if err != nil {
				test.Fatal("legacy reasoning source or preparation no longer reads", sample.name, err)
			}
			encoded, err := prepared.Encode()
			var stored []byte
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.pool.QueryRow(ctx, `SELECT preparation FROM doc_display_jobs_v1 WHERE repo_id=$1`, prior.RepoID).Scan(&stored); err != nil || !bytes.Equal(stored, encoded) {
				test.Fatal("legacy reasoning preparation was reformatted", sample.name, err)
			}
		}
	}
	if docMessageDisplayProtocolImagePG(test, storage, ctx, false) != immutable {
		test.Fatal("migration rewrote immutable sources, source hashes, objects or completed generations")
	}
	stable := docMessageDisplayProtocolImagePG(test, storage, ctx, true)
	for _, sample := range samples {
		if sample.profile != domain.DocDisplayProjectionProfile {
			continue
		}
		stale := captured[sample.name]
		stale.State = "running"
		if err := storage.RenewDocDisplay(ctx, stale, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("captured protocol 3 token renewed after migration", sample.name, err)
		}
		stale.State = "rejected"
		if err := storage.FinishDocDisplay(ctx, stale, time.Now()); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("captured protocol 3 token finished after migration", sample.name, err)
		}
	}
	if docMessageDisplayProtocolImagePG(test, storage, ctx, true) != stable {
		test.Fatal("stale protocol 3 tokens changed the upgraded queue")
	}
	for _, protocol := range []int{2, 3} {
		var generic, pre int
		if err := storage.pool.QueryRow(ctx, `SELECT
 count(*) FILTER (WHERE profile='public-event-display-v1'),count(*) FILTER (WHERE profile='tool-result-string-v1')
 FROM doc_display_jobs_v1 WHERE worker_protocol=2 OR ($1::integer=3 AND worker_protocol=3)`, protocol).Scan(&generic, &pre); err != nil || generic != 0 || pre == 0 {
			test.Fatal("legacy claimant protocol filter did not skip generic protocol 4", protocol, generic, pre, err)
		}
	}
	docMessageDisplayProtocolGuardsPG(test, storage, ctx, captured["pre_waiting"], captured["generic_waiting"])
	prior := captured["generic_running"]
	reclaim, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(reclaim)
	if _, err := reclaim.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='4'`); err != nil {
		test.Fatal(err)
	}
	reclaimed, err := scanDocDisplayJob(reclaim.QueryRow(ctx, `UPDATE doc_display_jobs_v1
 SET state='running',version=version+1,attempts=attempts+1,lease_until=clock_timestamp()+interval '1 minute'
 WHERE repo_id=$1 AND profile='public-event-display-v1' AND worker_protocol=4 AND state='waiting'
 RETURNING `+docDisplayJobColumns, prior.RepoID))
	if err == nil {
		err = reclaim.Commit(ctx)
	}
	if err != nil || reclaimed.RepoID != prior.RepoID || reclaimed.Version != prior.Version+2 || reclaimed.Attempts != prior.Attempts+1 || !reclaimed.LeaseUntil.After(finished) {
		test.Fatal("protocol 4 worker could not reclaim the migrated active job", reclaimed, err)
	}
	if err := storage.RenewDocDisplay(ctx, prior, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("protocol 3 token renewed the protocol 4 lease", err)
	}
	prior.State = "waiting"
	if err := storage.FinishDocDisplay(ctx, prior, time.Now()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("protocol 3 token finished the protocol 4 lease", err)
	}
	if err := renewDocMessageDisplayLegacyPG(ctx, storage, reclaimed, time.Now(), time.Minute); err != nil {
		test.Fatal("protocol 4 token lost its new lease", err)
	}
	if err := renewDocMessageDisplayLegacyPG(ctx, storage, captured["pre_running"], time.Now(), time.Minute); err != nil {
		test.Fatal("migration invalidated an active PRE protocol 2 token", err)
	}
}

func docMessageDisplayProtocolGuardsPG(test *testing.T, storage *PostgresStore, ctx context.Context, pre, generic domain.DocDisplayJob) {
	test.Helper()
	for _, profile := range []domain.DocDisplayJob{pre, generic} {
		for _, protocol := range []string{"", "2", "3", "4"} {
			for _, insert := range []bool{false, true} {
				transaction, err := storage.pool.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				if _, err := transaction.Exec(ctx, `SELECT set_config('cxthub.doc_display_protocol',$1,true)`, protocol); err != nil {
					rollbackPG(transaction)
					test.Fatal(err)
				}
				statement := `UPDATE doc_display_jobs_v1 SET state='running',version=version+1,lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1`
				if insert {
					statement = `INSERT INTO doc_display_jobs_v1
 SELECT (jsonb_populate_record(NULL::doc_display_jobs_v1,to_jsonb(job)||jsonb_build_object('event_index',100,
 'state','running','lease_until',clock_timestamp()+interval '1 minute'))).*
 FROM doc_display_jobs_v1 job WHERE repo_id=$1`
				}
				_, actionErr := transaction.Exec(ctx, statement, profile.RepoID)
				rollbackPG(transaction)
				allowed := protocol == "4" || profile.Target.DisplayProfile() == domain.DocDisplayStringProfile && (protocol == "2" || protocol == "3")
				var fence *pgconn.PgError
				if allowed && actionErr != nil || !allowed && (!errors.As(actionErr, &fence) || fence.Code != "0A000") {
					test.Fatal("unexpected protocol claim guard", profile.Target.DisplayProfile(), protocol, insert, actionErr)
				}
			}
		}
	}
	for _, statement := range []string{
		`UPDATE doc_display_jobs_v1 SET worker_protocol=1,state='running',lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1`,
		`UPDATE doc_display_jobs_v1 SET worker_protocol=4,state='running',lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1`,
		`UPDATE doc_display_jobs_v1 SET profile='unknown-profile',state='running',lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$1`,
		`UPDATE doc_display_jobs_v1 SET worker_protocol=2,state='running',lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$2`,
		`UPDATE doc_display_jobs_v1 SET worker_protocol=3,state='running',lease_until=clock_timestamp()+interval '1 minute' WHERE repo_id=$2`,
	} {
		_, err := storage.pool.Exec(ctx, statement+` AND $1::text<>$2::text`, pre.RepoID, generic.RepoID)
		var constraint *pgconn.PgError
		if !errors.As(err, &constraint) || constraint.Code != "23514" {
			test.Fatal("invalid profile/protocol combination did not retain check-violation semantics", statement, err)
		}
	}
}

func TestDocMessageDisplayPGProtocolNewClaimSkipsLegacyGeneric(test *testing.T) {
	storage, ctx := docMessageDisplayProtocolPG(test)
	seedDocMessageDisplayProtocolPG(test, storage, ctx, docMessageDisplayProtocolCase{
		name: "legacy_generic", profile: domain.DocDisplayProjectionProfile, state: "waiting", progress: "discovery",
	})
	before := docMessageDisplayProtocolImagePG(test, storage, ctx, true)
	if _, err := storage.ClaimDocDisplay(ctx, time.Now(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("protocol 4 claimant admitted generic protocol 3 work before migration", err)
	}
	if docMessageDisplayProtocolImagePG(test, storage, ctx, true) != before {
		test.Fatal("protocol 4 claimant changed skipped protocol 3 work")
	}
}
