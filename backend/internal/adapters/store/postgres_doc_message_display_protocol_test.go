//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
