//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docCatalogAdmissionPGDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type docCatalogAdmissionPGFact struct {
	source     string
	generation string
	revision   int64
}

type docCatalogAdmissionPGSource struct {
	event   domain.ContentHash
	profile string
	hash    domain.ContentHash
	raw     []byte
	payload []byte
}

func docCatalogAdmissionFixturePG(test *testing.T) docDisplayPGFixture {
	test.Helper()
	isolated, _ := rootProjectionIsolatedPG(test)
	test.Setenv("CXT_TEST_DSN", isolated.pool.Config().ConnString())
	if _, err := isolated.pool.Exec(context.Background(), `ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write;
 ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_z_catalog_renew`); err != nil {
		test.Fatal(err)
	}
	unique := test.Name() + time.Now().String()
	return docDisplayPG(test,
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, CallID: "catalog-first", Output: unique},
		domain.CIREvent{Kind: domain.EventToolResult, Seq: 2, CallID: "catalog-second", Output: unique + " second"})
}

func docCatalogAdmissionTargetPG(fixture docDisplayPGFixture, index int, profile string) domain.DocDisplayTarget {
	target := fixture.targets[index]
	if profile == domain.DocDisplayProjectionProfile {
		target.Profile = profile
	}
	return target
}

func docCatalogAdmissionSourcePG(test *testing.T, fixture docDisplayPGFixture, index int, profile string) docCatalogAdmissionPGSource {
	test.Helper()
	result := docCatalogAdmissionPGSource{event: fixture.targets[index].Event.Hash, profile: profile}
	var err error
	if profile == domain.DocDisplayStringProfile {
		source := fixture.source(test, index)
		result.raw, err = source.Encode()
		if err != nil {
			test.Fatal(err)
		}
		state, err := domain.NewDocDisplayStringState(source)
		if err != nil {
			test.Fatal(err)
		}
		result.payload, err = state.Encode(source)
		if err != nil {
			test.Fatal(err)
		}
	} else {
		state, err := domain.NewDocEventDisplayPreparation(domain.DocRangeEntry{
			DocEventIndex: fixture.targets[index].Event, Kind: domain.EventToolResult})
		if err != nil {
			test.Fatal(err)
		}
		result.raw, err = state.Source.Encode()
		if err != nil {
			test.Fatal(err)
		}
		result.payload, err = state.Encode()
		if err != nil {
			test.Fatal(err)
		}
	}
	result.hash = domain.HashContent(result.raw)
	return result
}

func docCatalogAdmissionBeginPG(test *testing.T, fixture docDisplayPGFixture) pgx.Tx {
	test.Helper()
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { rollbackPG(transaction) })
	if _, err := transaction.Exec(fixture.ctx, `SET LOCAL lock_timeout='250ms'; SET LOCAL cxthub.doc_display_protocol='5'; SET LOCAL cxthub.doc_catalog_protocol='3'`); err != nil {
		test.Fatal(err)
	}
	return transaction
}

func docCatalogAdmissionExecPG(test *testing.T, ctx context.Context, database docCatalogAdmissionPGDB, statement string, arguments ...any) {
	test.Helper()
	tag, err := database.Exec(ctx, statement, arguments...)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("expected exactly one catalog fixture mutation", tag, err)
	}
}

func docCatalogAdmissionFactPG(test *testing.T, ctx context.Context, database docCatalogAdmissionPGDB, event domain.ContentHash, profile string) docCatalogAdmissionPGFact {
	test.Helper()
	var result docCatalogAdmissionPGFact
	if err := database.QueryRow(ctx, `SELECT coalesce(source_hash,''),generation::text,admission_revision
 FROM doc_display_catalog_admissions_v1 WHERE event_hash=$1 AND profile=$2`, event, profile).
		Scan(&result.source, &result.generation, &result.revision); err != nil {
		test.Fatal("missing current catalog admission", event, profile, err)
	}
	if result.generation == "" || result.revision < 0 {
		test.Fatal("invalid catalog generation or admission revision", result)
	}
	return result
}

func docCatalogAdmissionRequirePG(test *testing.T, ctx context.Context, database docCatalogAdmissionPGDB, event domain.ContentHash, profile string, expected docCatalogAdmissionPGFact) {
	test.Helper()
	if actual := docCatalogAdmissionFactPG(test, ctx, database, event, profile); actual != expected {
		test.Fatalf("catalog fact (%s,%s): got %+v, want %+v", event, profile, actual, expected)
	}
}

func docCatalogAdmissionImagePG(test *testing.T, ctx context.Context, database docCatalogAdmissionPGDB) string {
	test.Helper()
	var result string
	if err := database.QueryRow(ctx, `SELECT jsonb_build_object(
 'admissions',(SELECT jsonb_agg(to_jsonb(admission) ORDER BY event_hash,profile) FROM doc_display_catalog_admissions_v1 admission),
 'sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY event_hash,profile) FROM doc_display_sources_v1 source),
 'jobs',(SELECT jsonb_agg(to_jsonb(job) ORDER BY repo_id,doc_hash,event_index,profile) FROM doc_display_jobs_v1 job),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY source_hash,hash) FROM doc_display_objects_v1 object),
 'generations',(SELECT jsonb_agg(to_jsonb(generation) ORDER BY event_hash,profile) FROM doc_display_generations_v1 generation),
 'maintenance',(SELECT jsonb_agg(to_jsonb(work) ORDER BY source_hash) FROM doc_display_maintenance_v1 work))::text`).Scan(&result); err != nil {
		test.Fatal(err)
	}
	return result
}

func docCatalogAdmissionInsertSourcePG(ctx context.Context, database docCatalogAdmissionPGDB, source docCatalogAdmissionPGSource) (pgconn.CommandTag, error) {
	return database.Exec(ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4)`, source.event, source.profile, source.hash, source.raw)
}

func docCatalogAdmissionPayloadPG(source *docCatalogAdmissionPGSource) (any, any, any, any, any) {
	if source == nil {
		return nil, nil, nil, nil, nil
	}
	hash := domain.HashContent(source.payload)
	if source.profile == domain.DocDisplayStringProfile {
		return source.hash, hash, source.payload, nil, nil
	}
	return source.hash, nil, nil, hash, source.payload
}

func docCatalogAdmissionInsertJobPG(ctx context.Context, database docCatalogAdmissionPGDB, target domain.DocDisplayTarget, source *docCatalogAdmissionPGSource) (pgconn.CommandTag, error) {
	sourceHash, checkpointHash, checkpoint, preparationHash, preparation := docCatalogAdmissionPayloadPG(source)
	protocol := 2
	if target.DisplayProfile() == domain.DocDisplayProjectionProfile {
		protocol = 5
	}
	return database.Exec(ctx, `INSERT INTO doc_display_jobs_v1
 (repo_id,doc_hash,profile,event_index,event_hash,event_offset,event_length,event_seq,event_role,
 source_id,source_version,range_generation,state,created_at,next_attempt,worker_protocol,
 source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,catalog_epoch)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'waiting',$13,$13,$14,$15,$16,$17,$18,$19,
 CASE WHEN $3='public-event-display-v1' THEN (SELECT catalog_epoch FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$15) END)
 ON CONFLICT(repo_id,doc_hash,event_index,profile) DO NOTHING`,
		target.RepoID, target.DocHash, target.DisplayProfile(), target.Event.Index, target.Event.Hash,
		target.Event.Offset, target.Event.Length, target.Event.Seq, target.Event.Role,
		target.Binding.SourceID, target.Binding.SourceVersion, target.Binding.RangeGeneration, time.Now().UTC(), protocol,
		sourceHash, checkpointHash, checkpoint, preparationHash, preparation)
}

func docCatalogAdmissionRebindJobPG(ctx context.Context, database docCatalogAdmissionPGDB, target domain.DocDisplayTarget, source docCatalogAdmissionPGSource) (pgconn.CommandTag, error) {
	sourceHash, checkpointHash, checkpoint, preparationHash, preparation := docCatalogAdmissionPayloadPG(&source)
	protocol := 2
	if source.profile == domain.DocDisplayProjectionProfile {
		protocol = 5
	}
	return database.Exec(ctx, `UPDATE doc_display_jobs_v1 SET profile=$5,worker_protocol=$6,source_hash=$7,
 checkpoint_hash=$8,checkpoint=$9,preparation_hash=$10,preparation=$11,
 catalog_epoch=CASE WHEN $5='public-event-display-v1' THEN (SELECT catalog_epoch FROM doc_display_catalog_lifecycle_v1 WHERE source_hash=$7) END
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`, target.RepoID, target.DocHash,
		target.Event.Index, target.DisplayProfile(), source.profile, protocol, sourceHash, checkpointHash, checkpoint, preparationHash, preparation)
}

func docCatalogAdmissionSQLStatePG(test *testing.T, failure error, code string, fence bool) {
	test.Helper()
	var postgresError *pgconn.PgError
	if !errors.As(failure, &postgresError) || postgresError.Code != code {
		test.Fatal("unexpected catalog admission SQLSTATE", failure, "want", code)
	}
	if fence && (postgresError.Message != "document catalog admission is busy" ||
		!strings.Contains(postgresError.Where, "cxthub_lock_doc_catalog_affinities")) {
		test.Fatal("catalog fence waited for a timeout instead of failing immediately", postgresError)
	}
}

func docCatalogAdmissionTryFencePG(test *testing.T, ctx context.Context, transaction pgx.Tx, target domain.DocDisplayTarget, expected bool) {
	test.Helper()
	var locked bool
	if err := transaction.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(
 hashtextextended('cxthub-doc-catalog-affinity:'||$1::text||':'||$2::text,0))`, target.Event.Hash, target.DisplayProfile()).Scan(&locked); err != nil || locked != expected {
		test.Fatal("unexpected foreign-transaction catalog fence ownership", target.Event.Hash, target.DisplayProfile(), locked, expected, err)
	}
}

func docCatalogAdmissionCurrentPG(test *testing.T, fixture docDisplayPGFixture, target domain.DocDisplayTarget, source docCatalogAdmissionPGSource) {
	test.Helper()
	fact := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, target.DisplayProfile())
	var present bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM doc_display_sources_v1 source
 JOIN doc_display_catalog_admissions_v1 admission USING(event_hash,profile)
 JOIN doc_display_jobs_v1 job ON job.event_hash=source.event_hash AND job.profile=source.profile
 WHERE source.event_hash=$1 AND source.profile=$2 AND source.source_hash=$3 AND source.bytes=$4
 AND admission.source_hash=source.source_hash AND job.repo_id=$5 AND job.doc_hash=$6 AND job.event_index=$7
 AND job.source_hash IS NULL)`, target.Event.Hash, target.DisplayProfile(), source.hash, source.raw,
		target.RepoID, target.DocHash, target.Event.Index).Scan(&present); err != nil || !present || fact.source != string(source.hash) || fact.revision != 1 {
		test.Fatal("retry omitted source/current metadata or fabricated job preparation", present, fact, err)
	}
}

func TestDocDisplayPGCatalogAdmissionJobFacts(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	foreign, _ := rootPGRepo(test, fixture.ctx, fixture.storage)
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		test.Run(profile, func(test *testing.T) {
			transaction := docCatalogAdmissionBeginPG(test, fixture)
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			if tag, err := docCatalogAdmissionInsertJobPG(fixture.ctx, transaction, target, nil); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("insert real target before any source exists", tag, err)
			}
			fact := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, target.Event.Hash, profile)
			if fact.source != "" || fact.revision != 1 {
				test.Fatal("source-free job did not create native metadata", fact)
			}
			if tag, err := docCatalogAdmissionInsertJobPG(fixture.ctx, transaction, target, nil); err != nil || tag.RowsAffected() != 0 {
				test.Fatal("duplicate job was not a no-op", tag, err)
			}
			for _, mutation := range []string{
				`state='running',version=version+1,attempts=attempts+1,lease_until=clock_timestamp()+interval '1 minute'`,
				`reason='catalog-payload',event_offset=event_offset+1,event_length=event_length+1,event_seq=event_seq+1,
 event_role='assistant',source_id=gen_random_uuid(),source_version=source_version+1,range_generation=range_generation||'-payload',
 next_attempt=next_attempt+interval '1 second',lease_until=lease_until+interval '1 second'`,
				`state='retrying',lease_until=NULL`,
				`state='rejected'`,
				`event_hash=event_hash,profile=profile,source_hash=source_hash,state=state`,
			} {
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET `+mutation+` WHERE repo_id=$1`, fixture.repo)
				docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, target.Event.Hash, profile, fact)
			}
			for _, state := range []string{"completed", "completed", "waiting"} {
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET state=$2 WHERE repo_id=$1`, fixture.repo, state)
				if state != "completed" || fact.revision == 1 {
					fact.revision++
				}
				docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, target.Event.Hash, profile, fact)
			}
			for _, mutation := range []struct {
				statement string
				argument  any
			}{
				{`repo_id=$1`, foreign},
				{`doc_hash=$1`, domain.HashContent(fixture.doc.canonical)},
				{`event_index=$1`, fixture.targets[1].Event.Index},
			} {
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET `+mutation.statement, mutation.argument)
				fact.revision++
				docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, target.Event.Hash, profile, fact)
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET event_hash=$1`, fixture.targets[1].Event.Hash)
			fact.revision++
			docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, target.Event.Hash, profile, fact)
			next := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, fixture.targets[1].Event.Hash, profile)
			if next.source != "" || next.revision != 1 {
				test.Fatal("event relocation omitted new native affinity", next)
			}
			otherProfile, protocol := domain.DocDisplayProjectionProfile, 5
			if profile == otherProfile {
				otherProfile, protocol = domain.DocDisplayStringProfile, 2
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET profile=$1,worker_protocol=$2`, otherProfile, protocol)
			next.revision++
			docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, fixture.targets[1].Event.Hash, profile, next)
			moved := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, fixture.targets[1].Event.Hash, otherProfile)
			if moved.source != "" || moved.revision != 1 {
				test.Fatal("profile relocation omitted new native affinity", moved)
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `DELETE FROM doc_display_jobs_v1`)
			moved.revision++
			docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, fixture.targets[1].Event.Hash, otherProfile, moved)
		})
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("rolled-back job facts leaked durable state")
	}
}

func TestDocDisplayPGCatalogAdmissionQueueNoops(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		test.Run(profile, func(test *testing.T) {
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			fact := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
			for range 2 {
				if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
					test.Fatal(err)
				}
			}
			job := fixture.claim(test)
			if job.Target.DisplayProfile() != profile {
				test.Fatal("claimed another profile", job.Target.DisplayProfile())
			}
			if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now().UTC(), time.Minute); err != nil {
				test.Fatal(err)
			}
			docCatalogAdmissionRequirePG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile, fact)
			if profile == domain.DocDisplayStringProfile {
				prepared := fixture.initialize(test, job)
				fact = docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
				if fact.revision != 2 || fact.source != string(prepared.State.Source) {
					test.Fatal("first real binding did not revise exactly once", fact)
				}
				if _, err := fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, fixture.step(test, prepared)); err != nil {
					test.Fatal(err)
				}
			} else {
				prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
				if err != nil {
					test.Fatal(err)
				}
				source, err := prepared.Source.Hash()
				if err != nil {
					test.Fatal(err)
				}
				fact = docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
				if fact.revision != 2 || fact.source != string(source) {
					test.Fatal("first public binding did not revise exactly once", fact)
				}
				if _, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job); err != nil {
					test.Fatal(err)
				}
			}
			docCatalogAdmissionRequirePG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile, fact)
			job.State = "rejected"
			if err := fixture.storage.FinishDocDisplay(fixture.ctx, job, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			docCatalogAdmissionRequirePG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile, fact)
		})
	}
}

func TestDocDisplayPGCatalogAdmissionSourceLifetime(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		test.Run(profile, func(test *testing.T) {
			transaction := docCatalogAdmissionBeginPG(test, fixture)
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			source := docCatalogAdmissionSourcePG(test, fixture, 0, profile)
			if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, transaction, target, nil); err != nil {
				test.Fatal(err)
			}
			cold := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, source.event, profile)
			if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, transaction, source); err != nil {
				test.Fatal(err)
			}
			bound := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, source.event, profile)
			if bound.generation == cold.generation || bound.source != string(source.hash) || bound.revision != cold.revision {
				test.Fatal("source birth did not rotate and bind the existing admission", cold, bound)
			}
			for _, mutation := range []string{
				`event_hash=event_hash,profile=profile,source_hash=source_hash,bytes=bytes`,
				`reference_revision=reference_revision+1`,
				`bytes=bytes,reference_revision=reference_revision+1`,
			} {
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_sources_v1 SET `+mutation+` WHERE source_hash=$1`, source.hash)
				docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, source.event, profile, bound)
			}
			if tag, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, source.event, profile, source.hash, source.raw); err != nil || tag.RowsAffected() != 0 {
				test.Fatal("duplicate source was not a no-op", tag, err)
			}
			docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, source.event, profile, bound)
			for _, mutation := range []struct {
				name      string
				statement string
				argument  any
			}{
				{"bytes", `bytes=$2`, []byte(`{}`)},
				{"hash", `source_hash=$2`, domain.HashContent(append(bytes.Clone(source.raw), '\n'))},
			} {
				if _, err := transaction.Exec(fixture.ctx, `SAVEPOINT catalog_source_mutation`); err != nil {
					test.Fatal(err)
				}
				if mutation.name == "hash" {
					_, err := transaction.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET source_hash=$2 WHERE source_hash=$1`, source.hash, mutation.argument)
					var foreignKey *pgconn.PgError
					if !errors.As(err, &foreignKey) || foreignKey.Code != "23503" || foreignKey.ConstraintName != "doc_display_catalog_lifecycle_v1_source_hash_fkey" {
						test.Fatal("live lifecycle identity must prevent in-place Source rekeying", err)
					}
					if _, err := transaction.Exec(fixture.ctx, `ROLLBACK TO SAVEPOINT catalog_source_mutation; RELEASE SAVEPOINT catalog_source_mutation`); err != nil {
						test.Fatal(err)
					}
					docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, source.event, profile, bound)
					continue
				}
				docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_sources_v1 SET `+mutation.statement+` WHERE source_hash=$1`, source.hash, mutation.argument)
				changed := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, source.event, profile)
				if changed.generation == bound.generation || changed.revision != bound.revision || mutation.name == "bytes" && changed.source != bound.source || mutation.name == "hash" && changed.source != string(mutation.argument.(domain.ContentHash)) {
					test.Fatal("actual source mutation did not rotate its generation/binding", mutation.name, bound, changed)
				}
				if _, err := transaction.Exec(fixture.ctx, `ROLLBACK TO SAVEPOINT catalog_source_mutation; RELEASE SAVEPOINT catalog_source_mutation`); err != nil {
					test.Fatal(err)
				}
				docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, source.event, profile, bound)
			}
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, source.hash)
			deleted := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, source.event, profile)
			if deleted.source != "" || deleted.generation == bound.generation || deleted.revision != bound.revision {
				test.Fatal("source deletion cascaded or retained a stale admission", deleted, bound)
			}
			if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, transaction, source); err != nil {
				test.Fatal(err)
			}
			reborn := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, source.event, profile)
			if reborn.source != bound.source || reborn.generation == bound.generation || reborn.generation == deleted.generation || reborn.revision != bound.revision {
				test.Fatal("same-hash delete/reinsert admitted generation ABA", bound, deleted, reborn)
			}
			next := docCatalogAdmissionSourcePG(test, fixture, 1, profile)
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, source.hash)
			if tag, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, transaction, next); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("replace Source identity through deletion and a fresh lifecycle birth", tag, err)
			}
			vacated := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, source.event, profile)
			moved := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, next.event, profile)
			if vacated.source != "" || vacated.generation == reborn.generation || vacated.revision != reborn.revision || moved.source != string(next.hash) || moved.revision != 0 {
				test.Fatal("source event relocation omitted old/new binding facts", vacated, moved)
			}
			otherProfile := domain.DocDisplayProjectionProfile
			if profile == otherProfile {
				otherProfile = domain.DocDisplayStringProfile
			}
			other := docCatalogAdmissionSourcePG(test, fixture, 1, otherProfile)
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, next.hash)
			if tag, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, transaction, other); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("replace Source profile through deletion and a fresh lifecycle birth", tag, err)
			}
			cleared := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, next.event, profile)
			separated := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, other.event, otherProfile)
			if cleared.source != "" || cleared.generation == moved.generation || cleared.revision != 0 || separated.source != string(other.hash) || separated.revision != 0 {
				test.Fatal("source profile relocation contaminated or omitted an affinity", cleared, separated)
			}
			docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, source.event, profile, vacated)
		})
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("rolled-back source lifecycle changed durable catalog state")
	}
}

func TestDocDisplayPGCatalogAdmissionFirstJobFencesSourceBirth(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		test.Run(profile, func(test *testing.T) {
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			source := docCatalogAdmissionSourcePG(test, fixture, 0, profile)
			before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
			jobTransaction := docCatalogAdmissionBeginPG(test, fixture)
			if tag, err := docCatalogAdmissionInsertJobPG(fixture.ctx, jobTransaction, target, nil); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("first source-free job INSERT", tag, err)
			}
			pending := docCatalogAdmissionFactPG(test, fixture.ctx, jobTransaction, target.Event.Hash, profile)
			if pending.source != "" || pending.revision != 1 {
				test.Fatal("uncommitted first job omitted metadata", pending)
			}
			probe := docCatalogAdmissionBeginPG(test, fixture)
			docCatalogAdmissionTryFencePG(test, fixture.ctx, probe, target, false)
			other := target
			if profile == domain.DocDisplayStringProfile {
				other.Profile = domain.DocDisplayProjectionProfile
			} else {
				other.Profile = ""
			}
			docCatalogAdmissionTryFencePG(test, fixture.ctx, probe, other, true)
			rollbackPG(probe)
			birth := docCatalogAdmissionBeginPG(test, fixture)
			_, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, birth, source)
			docCatalogAdmissionSQLStatePG(test, err, "55P03", true)
			rollbackPG(birth)
			if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
				test.Fatal("first-job/source-birth contention leaked an uncommitted source or admission")
			}
			if err := jobTransaction.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
				test.Fatal("first job rollback left phantom admission metadata")
			}
			if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source); err != nil {
				test.Fatal("source birth could not retry after job rollback", err)
			}
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
				test.Fatal("real enqueue could not retry after source birth", err)
			}
			docCatalogAdmissionCurrentPG(test, fixture, target, source)
		})
	}
}

func TestDocDisplayPGCatalogAdmissionSourceBirthFencesEnqueue(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		test.Run(profile, func(test *testing.T) {
			target := docCatalogAdmissionTargetPG(fixture, 0, profile)
			source := docCatalogAdmissionSourcePG(test, fixture, 0, profile)
			before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
			birth := docCatalogAdmissionBeginPG(test, fixture)
			if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, birth, source); err != nil {
				test.Fatal(err)
			}
			pending := docCatalogAdmissionFactPG(test, fixture.ctx, birth, source.event, profile)
			if pending.source != string(source.hash) || pending.revision != 0 {
				test.Fatal("uncommitted source birth omitted current metadata", pending)
			}
			probe := docCatalogAdmissionBeginPG(test, fixture)
			docCatalogAdmissionTryFencePG(test, fixture.ctx, probe, target, false)
			_, err := docCatalogAdmissionInsertJobPG(fixture.ctx, probe, target, nil)
			docCatalogAdmissionSQLStatePG(test, err, "55P03", true)
			rollbackPG(probe)
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); !errors.Is(err, domain.ErrResourceOverloaded) {
				test.Fatal("uncommitted source birth did not overload the real enqueue", err)
			}
			if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
				test.Fatal("source-first failure leaked a job or current metadata")
			}
			if err := birth.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
				test.Fatal("enqueue could not retry after source rollback", err)
			}
			cold := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
			if cold.source != "" || cold.revision != 1 {
				test.Fatal("retried enqueue lost source-free metadata", cold)
			}
			if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source); err != nil {
				test.Fatal("source birth could not retry after enqueue", err)
			}
			docCatalogAdmissionCurrentPG(test, fixture, target, source)
			bound := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
			if bound.generation == cold.generation || bound.generation == pending.generation {
				test.Fatal("source retry reused a rolled-back or source-free generation", pending, cold, bound)
			}
		})
	}
}

func TestDocDisplayPGCatalogAdmissionCommitOrder(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		for _, order := range []string{"job_first", "source_first"} {
			test.Run(profile+"/"+order, func(test *testing.T) {
				index := 0
				if order == "source_first" {
					index = 1
				}
				target := docCatalogAdmissionTargetPG(fixture, index, profile)
				source := docCatalogAdmissionSourcePG(test, fixture, index, profile)
				winner := docCatalogAdmissionBeginPG(test, fixture)
				var err error
				if order == "source_first" {
					_, err = docCatalogAdmissionInsertSourcePG(fixture.ctx, winner, source)
				} else {
					_, err = docCatalogAdmissionInsertJobPG(fixture.ctx, winner, target, nil)
				}
				if err != nil {
					test.Fatal("first actor did not hold its real admission fence", err)
				}
				pending := docCatalogAdmissionFactPG(test, fixture.ctx, winner, target.Event.Hash, profile)
				loser := docCatalogAdmissionBeginPG(test, fixture)
				if order == "source_first" {
					_, err = docCatalogAdmissionInsertJobPG(fixture.ctx, loser, target, nil)
				} else {
					_, err = docCatalogAdmissionInsertSourcePG(fixture.ctx, loser, source)
				}
				docCatalogAdmissionSQLStatePG(test, err, "55P03", true)
				rollbackPG(loser)
				if err := winner.Commit(fixture.ctx); err != nil {
					test.Fatal(err)
				}
				docCatalogAdmissionRequirePG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile, pending)
				if order == "source_first" {
					_, err = fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC())
				} else {
					_, err = docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source)
				}
				if err != nil {
					test.Fatal("second actor did not retry after the first committed", err)
				}
				docCatalogAdmissionCurrentPG(test, fixture, target, source)
				current := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, target.Event.Hash, profile)
				if (current.generation == pending.generation) != (order == "source_first") {
					test.Fatal("job retry reset source generation or source birth reused the source-free generation", order, pending, current)
				}
			})
		}
	}
}

func docCatalogAdmissionOwnersPG(test *testing.T, fixture docDisplayPGFixture) (domain.DocDisplayTarget, docCatalogAdmissionPGSource, docCatalogAdmissionPGSource, []domain.DocDisplayTarget) {
	test.Helper()
	owners := make([]domain.DocDisplayTarget, 0, 4)
	for _, index := range []int{0, 1} {
		for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
			source := docCatalogAdmissionSourcePG(test, fixture, index, profile)
			if profile == domain.DocDisplayStringProfile {
				fixture.cacheSource(test, index)
			} else if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, source); err != nil {
				test.Fatal(err)
			}
			owners = append(owners, docCatalogAdmissionTargetPG(fixture, index, profile))
		}
	}
	target := owners[0]
	previous := docCatalogAdmissionSourcePG(test, fixture, 1, domain.DocDisplayStringProfile)
	next := docCatalogAdmissionSourcePG(test, fixture, 1, domain.DocDisplayProjectionProfile)
	if _, err := docCatalogAdmissionInsertJobPG(fixture.ctx, fixture.storage.pool, target, &previous); err != nil {
		test.Fatal(err)
	}
	return target, previous, next, owners
}

func TestDocDisplayPGCatalogAdmissionExactOldNewOwners(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	target, _, next, owners := docCatalogAdmissionOwnersPG(test, fixture)
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	test.Run("no_membership_change_ignores_busy_fences", func(test *testing.T) {
		holder := docCatalogAdmissionBeginPG(test, fixture)
		for _, owner := range owners {
			docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, owner, true)
		}
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
			test.Fatal("duplicate enqueue acquired a membership fence", err)
		}
		writer := docCatalogAdmissionBeginPG(test, fixture)
		for _, mutation := range []string{
			`state='running',version=version+1,attempts=attempts+1,lease_until=clock_timestamp()+interval '1 minute'`,
			`checkpoint_hash=checkpoint_hash,checkpoint=checkpoint,source_hash=source_hash,reason='payload-only'`,
			`state='retrying',lease_until=NULL`,
		} {
			docCatalogAdmissionExecPG(test, fixture.ctx, writer, `UPDATE doc_display_jobs_v1 SET `+mutation+` WHERE repo_id=$1`, fixture.repo)
		}
		rollbackPG(writer)
		if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
			test.Fatal("no-op membership probes changed committed state")
		}
	})
	for _, owner := range owners {
		test.Run(string(owner.Event.Hash)+"/"+owner.DisplayProfile(), func(test *testing.T) {
			holder := docCatalogAdmissionBeginPG(test, fixture)
			docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, owner, true)
			writer := docCatalogAdmissionBeginPG(test, fixture)
			_, err := docCatalogAdmissionRebindJobPG(fixture.ctx, writer, target, next)
			docCatalogAdmissionSQLStatePG(test, err, "55P03", true)
			rollbackPG(writer)
			if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
				test.Fatal("busy OLD/NEW native/source affinity committed a partial revision or binding")
			}
		})
	}
	transaction := docCatalogAdmissionBeginPG(test, fixture)
	expected := make([]docCatalogAdmissionPGFact, len(owners))
	for index, owner := range owners {
		expected[index] = docCatalogAdmissionFactPG(test, fixture.ctx, transaction, owner.Event.Hash, owner.DisplayProfile())
	}
	if tag, err := docCatalogAdmissionRebindJobPG(fixture.ctx, transaction, target, next); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("actual OLD/NEW profile/source rebinding", tag, err)
	}
	probe := docCatalogAdmissionBeginPG(test, fixture)
	for index, owner := range owners {
		expected[index].revision++
		docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, owner.Event.Hash, owner.DisplayProfile(), expected[index])
		docCatalogAdmissionTryFencePG(test, fixture.ctx, probe, owner, false)
	}
	unrelated := target
	unrelated.Event.Hash = domain.HashContent([]byte(test.Name() + " unrelated exact mapping"))
	docCatalogAdmissionTryFencePG(test, fixture.ctx, probe, unrelated, true)
	rollbackPG(probe)
	for _, mutation := range []string{
		`source_hash=source_hash,profile=profile,event_hash=event_hash,state=state`,
		`preparation_hash=preparation_hash,preparation=preparation,reason='payload-only'`,
	} {
		docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET `+mutation+` WHERE repo_id=$1`, fixture.repo)
		for index, owner := range owners {
			docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, owner.Event.Hash, owner.DisplayProfile(), expected[index])
		}
	}
	for _, mutation := range []string{`state='completed'`, `state='completed'`, `state='waiting'`} {
		docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_jobs_v1 SET `+mutation+` WHERE repo_id=$1`, fixture.repo)
		for index, owner := range owners {
			if index%2 == 1 && (mutation != `state='completed'` || expected[index].revision == 1) {
				expected[index].revision++
			}
			docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, owner.Event.Hash, owner.DisplayProfile(), expected[index])
		}
	}
	docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo)
	for index, owner := range owners {
		if index%2 == 1 {
			expected[index].revision++
		}
		docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, owner.Event.Hash, owner.DisplayProfile(), expected[index])
	}
	if err := transaction.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("rolled-back OLD/NEW admission changes leaked")
	}
}

func TestDocDisplayPGCatalogAdmissionOverflowRollback(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	target, _, next, owners := docCatalogAdmissionOwnersPG(test, fixture)
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	for _, owner := range owners {
		test.Run(string(owner.Event.Hash)+"/"+owner.DisplayProfile(), func(test *testing.T) {
			transaction := docCatalogAdmissionBeginPG(test, fixture)
			docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_catalog_admissions_v1
 SET admission_revision=9223372036854775807 WHERE event_hash=$1 AND profile=$2`, owner.Event.Hash, owner.DisplayProfile())
			maximum := docCatalogAdmissionImagePG(test, fixture.ctx, transaction)
			if _, err := transaction.Exec(fixture.ctx, `SAVEPOINT catalog_overflow`); err != nil {
				test.Fatal(err)
			}
			_, err := docCatalogAdmissionRebindJobPG(fixture.ctx, transaction, target, next)
			docCatalogAdmissionSQLStatePG(test, err, "22003", false)
			if _, err := transaction.Exec(fixture.ctx, `ROLLBACK TO SAVEPOINT catalog_overflow; RELEASE SAVEPOINT catalog_overflow`); err != nil {
				test.Fatal(err)
			}
			if after := docCatalogAdmissionImagePG(test, fixture.ctx, transaction); after != maximum {
				test.Fatal("overflow retained an earlier affinity revision, queue side effect, or job mutation")
			}
			if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
				test.Fatal("overflow leaked durable catalog state")
			}
		})
	}
}

func TestDocDisplayPGCatalogAdmissionSourceMappingNOWAIT(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	target, previous, _, _ := docCatalogAdmissionOwnersPG(test, fixture)
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	holder := docCatalogAdmissionBeginPG(test, fixture)
	var locked domain.ContentHash
	if err := holder.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR UPDATE`, previous.hash).Scan(&locked); err != nil || locked != previous.hash {
		test.Fatal(err)
	}
	docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, target, true)
	writer := docCatalogAdmissionBeginPG(test, fixture)
	_, err := writer.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET state='completed' WHERE repo_id=$1`, fixture.repo)
	docCatalogAdmissionSQLStatePG(test, err, "55P03", false)
	var postgresError *pgconn.PgError
	if !errors.As(err, &postgresError) || !strings.Contains(postgresError.Message, "could not obtain lock on row") ||
		!strings.Contains(postgresError.Where, "cxthub_lock_doc_catalog_affinities") {
		test.Fatal("explicit-source mapping was not locked NOWAIT before the busy native fence", err)
	}
	rollbackPG(writer)
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("source-row NOWAIT failure changed membership or metadata")
	}
	if err := holder.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	retry := docCatalogAdmissionBeginPG(test, fixture)
	docCatalogAdmissionExecPG(test, fixture.ctx, retry, `UPDATE doc_display_jobs_v1 SET state='completed' WHERE repo_id=$1`, fixture.repo)
	for _, owner := range []domain.DocDisplayTarget{target, docCatalogAdmissionTargetPG(fixture, 1, previous.profile)} {
		fact := docCatalogAdmissionFactPG(test, fixture.ctx, retry, owner.Event.Hash, owner.DisplayProfile())
		original := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, owner.Event.Hash, owner.DisplayProfile())
		original.revision++
		if fact != original {
			test.Fatal("source-row retry lost an exact owner", fact, original)
		}
	}
}

func TestDocDisplayPGCatalogAdmissionSourceWriterFenceBusy(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	source := fixture.cacheSource(test, 0)
	hash, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	target := fixture.targets[0]
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	holder := docCatalogAdmissionBeginPG(test, fixture)
	docCatalogAdmissionTryFencePG(test, fixture.ctx, holder, target, true)
	writer := docCatalogAdmissionBeginPG(test, fixture)
	var writerPID, holderPID int32
	if err := writer.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		test.Fatal(err)
	}
	if err := holder.QueryRow(fixture.ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil || holderPID == writerPID {
		test.Fatal("source writer and affinity holder must use different PostgreSQL backends", holderPID, writerPID, err)
	}
	var owned domain.ContentHash
	if err := writer.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE`, hash).Scan(&owned); err != nil || owned != hash {
		test.Fatal("writer did not first own its source row", owned, err)
	}
	witness, err := fixture.storage.pool.Acquire(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer witness.Release()
	witnessPID := int32(witness.Conn().PgConn().PID())
	wait, cancel := context.WithTimeout(fixture.ctx, docRangeEnqueueTimeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var observed domain.ContentHash
		result <- witness.QueryRow(wait, `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR UPDATE`, hash).Scan(&observed)
	}()
	defer func() {
		cancel()
		<-result
	}()
	blocked := false
	for !blocked && wait.Err() == nil {
		if err := fixture.storage.pool.QueryRow(wait, `SELECT $1::integer=ANY(pg_blocking_pids($2))
 AND EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$2 AND wait_event_type='Lock')`, writerPID, witnessPID).Scan(&blocked); err != nil {
			test.Fatal("observe source-row owner using backend PID evidence", err)
		}
		if !blocked {
			time.Sleep(time.Millisecond)
		}
	}
	if !blocked {
		test.Fatal("source witness never observed the writer as its row-lock blocker", wait.Err())
	}
	test.Logf("source writer PID=%d blocks row witness PID=%d; affinity fence belongs to PID=%d", writerPID, witnessPID, holderPID)
	_, err = writer.Exec(fixture.ctx, `UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, hash, []byte(`{}`))
	docCatalogAdmissionSQLStatePG(test, err, "55P03", true)
	rollbackPG(writer)
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before {
		test.Fatal("source-row-owned writer waiting on a busy fence leaked corrupt bytes or generation")
	}
	probe := docCatalogAdmissionBeginPG(test, fixture)
	docCatalogAdmissionTryFencePG(test, fixture.ctx, probe, target, false)
	rollbackPG(probe)
}

func TestDocDisplayPGCatalogAdmissionMigrationSeedAndGuards(test *testing.T) {
	fixture := docCatalogAdmissionFixturePG(test)
	fixture.enqueue(test, 0)
	job := fixture.claim(test)
	prepared := fixture.initialize(test, job)
	public := docCatalogAdmissionSourcePG(test, fixture, 0, domain.DocDisplayProjectionProfile)
	if _, err := docCatalogAdmissionInsertSourcePG(fixture.ctx, fixture.storage.pool, public); err != nil {
		test.Fatal(err)
	}
	for _, profile := range []string{domain.DocDisplayStringProfile, domain.DocDisplayProjectionProfile} {
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, docCatalogAdmissionTargetPG(fixture, 1, profile), time.Now().UTC()); err != nil {
			test.Fatal(err)
		}
	}
	before := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)
	legacy := fixture.image(test)
	transaction := docCatalogAdmissionBeginPG(test, fixture)
	if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER doc_display_jobs_v1_z_catalog_admission ON doc_display_jobs_v1;
 DROP TRIGGER doc_display_sources_v1_catalog_admission ON doc_display_sources_v1;
 DROP FUNCTION cxthub_record_doc_catalog_job_admission();
 DROP FUNCTION cxthub_record_doc_catalog_source();
 DROP FUNCTION cxthub_lock_doc_catalog_affinities(text[],text[],text[]);
 DROP TABLE doc_display_catalog_admissions_v1`); err != nil {
		test.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0136_doc_display_catalog_admission.sql")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, string(migration)); err != nil {
		test.Fatal("replay authoritative 0136 over populated source and source-free job tuples", err)
	}
	var complete, shape bool
	if err := transaction.QueryRow(fixture.ctx, `WITH expected AS (
 SELECT event_hash,profile FROM doc_display_sources_v1 UNION SELECT event_hash,profile FROM doc_display_jobs_v1)
 SELECT (SELECT count(*) FROM expected)=4 AND NOT EXISTS(
 SELECT 1 FROM expected FULL JOIN doc_display_catalog_admissions_v1 admission USING(event_hash,profile)
 LEFT JOIN doc_display_sources_v1 source USING(event_hash,profile)
 WHERE expected.event_hash IS NULL OR admission.event_hash IS NULL OR admission.admission_revision<>0
 OR admission.source_hash IS DISTINCT FROM source.source_hash OR admission.generation IS NULL)`).Scan(&complete); err != nil || !complete {
		test.Fatal("migration seed omitted a native tuple, current source binding, or generation", complete, err)
	}
	if err := transaction.QueryRow(fixture.ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='doc_display_catalog_admissions_v1'::regclass AND contype='f')
 AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema()
 AND table_name='doc_display_catalog_admissions_v1' AND column_name='source_hash' AND is_nullable='YES')
 AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema()
 AND table_name='doc_display_catalog_admissions_v1' AND column_name='generation' AND udt_name='uuid' AND is_nullable='NO')
 AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='doc_display_catalog_admissions_v1'::regclass
 AND contype='p' AND pg_get_constraintdef(oid)='PRIMARY KEY (event_hash, profile)')
 AND (SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgtype=29
 AND ((tgrelid='doc_display_jobs_v1'::regclass AND tgname='doc_display_jobs_v1_z_catalog_admission'
 AND tgfoid='cxthub_record_doc_catalog_job_admission()'::regprocedure)
 OR (tgrelid='doc_display_sources_v1'::regclass AND tgname='doc_display_sources_v1_catalog_admission'
 AND tgfoid='cxthub_record_doc_catalog_source()'::regprocedure)))=2
 AND EXISTS(SELECT 1 FROM pg_trigger roots JOIN pg_trigger admission ON roots.tgrelid=admission.tgrelid
 WHERE roots.tgrelid='doc_display_jobs_v1'::regclass AND roots.tgname='doc_display_jobs_v1_queue_roots'
 AND admission.tgname='doc_display_jobs_v1_z_catalog_admission' AND roots.tgname<admission.tgname)`).Scan(&shape); err != nil || !shape {
		test.Fatal("admission metadata gained a source FK, lost its tuple PK/UUID, or stopped using AFTER DML triggers", shape, err)
	}
	var definition string
	if err := transaction.QueryRow(fixture.ctx, `SELECT pg_get_functiondef('cxthub_lock_doc_catalog_affinities(text[],text[],text[])'::regprocedure)`).Scan(&definition); err != nil {
		test.Fatal(err)
	}
	definition = strings.Join(strings.Fields(strings.ToLower(definition)), " ")
	for _, required := range []string{"for key share nowait", "order by identity", "pg_try_advisory_xact_lock(fence.identity)", "hashtextextended('cxthub-doc-catalog-affinity:'", "input.profile,0)"} {
		if !strings.Contains(definition, required) {
			test.Fatal("catalog helper lost exact mapping, sorted try-locks, or seed-zero namespace", required, definition)
		}
	}
	if strings.Contains(definition, "pg_advisory_xact_lock(") {
		test.Fatal("catalog helper introduced a blocking advisory fence", definition)
	}
	bound := context.WithValue(fixture.ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: fixture.storage, repo: fixture.repo})
	initial := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, job.Target.Event.Hash, domain.DocDisplayStringProfile)
	if _, err := transaction.Exec(fixture.ctx, `SAVEPOINT catalog_corruption`); err != nil {
		test.Fatal(err)
	}
	docCatalogAdmissionExecPG(test, fixture.ctx, transaction, `UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, prepared.State.Source, []byte(`{}`))
	corrupt := docCatalogAdmissionFactPG(test, fixture.ctx, transaction, job.Target.Event.Hash, domain.DocDisplayStringProfile)
	if corrupt.generation == initial.generation || corrupt.source != initial.source || corrupt.revision != initial.revision {
		test.Fatal("source bytes corruption did not revise source generation only", initial, corrupt)
	}
	if _, err := fixture.storage.readDocDisplayPreparation(bound, job); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("new catalog metadata bypassed the existing source byte-integrity guard", err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ROLLBACK TO SAVEPOINT catalog_corruption; RELEASE SAVEPOINT catalog_corruption`); err != nil {
		test.Fatal(err)
	}
	docCatalogAdmissionRequirePG(test, fixture.ctx, transaction, job.Target.Event.Hash, domain.DocDisplayStringProfile, initial)
	if _, err := fixture.storage.readDocDisplayPreparation(bound, job); err != nil {
		test.Fatal("source bytes/generation rollback did not restore the PRE checkpoint", err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	for _, check := range []struct {
		statement string
		arguments []any
		code      string
	}{
		{`UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, []any{prepared.State.Source, bytes.Repeat([]byte("x"), domain.MaxDocDisplaySourceBytes+1)}, "23514"},
		{`UPDATE doc_display_jobs_v1 SET checkpoint=NULL WHERE repo_id=$1 AND event_index=0`, []any{fixture.repo}, "23514"},
		{`UPDATE doc_display_jobs_v1 SET source_hash=$2 WHERE repo_id=$1 AND event_index=0`, []any{fixture.repo, domain.HashContent([]byte("missing catalog source"))}, "23503"},
		{`UPDATE doc_display_jobs_v1 SET worker_protocol=1 WHERE repo_id=$1 AND event_index=0`, []any{fixture.repo}, "23514"},
		{`UPDATE doc_display_catalog_admissions_v1 SET admission_revision=-1`, nil, "23514"},
	} {
		image := docCatalogAdmissionImagePG(test, fixture.ctx, transaction)
		if _, err := transaction.Exec(fixture.ctx, `SAVEPOINT catalog_old_guard`); err != nil {
			test.Fatal(err)
		}
		_, err := transaction.Exec(fixture.ctx, check.statement, check.arguments...)
		docCatalogAdmissionSQLStatePG(test, err, check.code, false)
		if _, err := transaction.Exec(fixture.ctx, `ROLLBACK TO SAVEPOINT catalog_old_guard; RELEASE SAVEPOINT catalog_old_guard`); err != nil {
			test.Fatal(err)
		}
		if after := docCatalogAdmissionImagePG(test, fixture.ctx, transaction); after != image {
			test.Fatal("existing guard failure left partial metadata or source changes", check.statement)
		}
	}
	if err := transaction.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if after := docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool); after != before || fixture.image(test) != legacy {
		test.Fatal("migration replay/guard probes modified committed PRE/source bytes or catalog facts")
	}
}
