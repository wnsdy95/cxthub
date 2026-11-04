//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyByteHeaderPGQueryKey struct{}

type legacyByteHeaderPGTrace struct {
	queries []string
	inserts []int
	cancel  context.CancelFunc
}

func (trace *legacyByteHeaderPGTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries = append(trace.queries, query.SQL)
	if strings.HasPrefix(query.SQL, "INSERT INTO doc_legacy_byte_stage_headers_v1") {
		body, _ := query.Args[3].([]byte)
		trace.inserts = append(trace.inserts, len(body))
	}
	return context.WithValue(ctx, legacyByteHeaderPGQueryKey{}, query.SQL)
}

func (trace *legacyByteHeaderPGTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryEndData) {
	statement, _ := ctx.Value(legacyByteHeaderPGQueryKey{}).(string)
	if trace.cancel != nil && query.Err == nil && strings.HasPrefix(statement, "INSERT INTO doc_legacy_byte_stage_headers_v1") {
		trace.cancel()
	}
}

func legacyByteHeaderPGPeer(test *testing.T, storage *PostgresStore, ctx context.Context, trace *legacyByteHeaderPGTrace) *PostgresStore {
	test.Helper()
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}
}

func legacyByteHeaderPGSource(test *testing.T, storage *PostgresStore, ctx context.Context, headerBytes, streamBytes int) legacyByteStagePGFixture {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
		SessionOriginID: string(repo), Fidelity: domain.FidelityFull, GitBranch: "\ud55c🙂\"\\\n<>&"}, Events: []domain.CIREvent{}}
	if streamBytes > 0 {
		document.Events = append(document.Events, domain.CIREvent{Kind: domain.EventMessage, Seq: 7, Role: domain.RoleAssistant,
			Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", streamBytes)}}})
	}
	raw, err := domain.CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	var parts struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil || headerBytes < len(parts.Envelope) {
		test.Fatal("invalid requested envelope fixture extent", headerBytes, len(parts.Envelope), err)
	}
	document.Envelope.Cwd = strings.Repeat("h", headerBytes-len(parts.Envelope))
	fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
	if len(fixture.envelope) != headerBytes {
		test.Fatal("fixture envelope changed its exact extent", len(fixture.envelope), headerBytes)
	}
	return fixture
}

func legacyByteHeaderPGPrepare(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) (domain.DocRangeIndexJob, legacyByteBackfillSession, domain.LegacyRangeMetadataSeed) {
	test.Helper()
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	seed, err := domain.NewLegacyRangeMetadataSeed(ctx, session.proof, session.plan)
	if err != nil || seed.EnvelopeBytes() != len(fixture.envelope) {
		test.Fatal("sealed seed did not retain the canonical envelope", seed.EnvelopeBytes(), err)
	}
	return job, session, seed
}

func legacyByteHeaderPGAssert(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, next int) {
	test.Helper()
	var version, cursor int
	var declared *int
	var complete bool
	if err := storage.pool.QueryRow(ctx, `SELECT header_version,header_bytes,header_next,header_complete
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash).Scan(&version, &declared, &cursor, &complete); err != nil {
		test.Fatal(err)
	}
	parts := (len(fixture.envelope) + legacyByteHeaderPartBytes - 1) / legacyByteHeaderPartBytes
	if next == 0 {
		if version != 0 || declared != nil || cursor != 0 || complete {
			test.Fatal("old stage was not the uninitialized version-zero shape", version, declared, cursor, complete)
		}
	} else if version != 1 || declared == nil || *declared != len(fixture.envelope) || cursor != next || complete != (next == parts) {
		test.Fatal("header declaration, cursor, or EOF changed", version, declared, cursor, complete, next)
	}
	rows, err := storage.pool.Query(ctx, `SELECT ordinal,hash,bytes FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1 ORDER BY ordinal`, fixture.hash)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	count := 0
	var joined []byte
	for rows.Next() {
		var ordinal int
		var hash domain.ContentHash
		var body []byte
		if err := rows.Scan(&ordinal, &hash, &body); err != nil {
			test.Fatal(err)
		}
		if ordinal != count || ordinal >= next || ordinal >= parts {
			test.Fatal("header skipped or duplicated a cursor ordinal", ordinal, count, next)
		}
		start := ordinal * legacyByteHeaderPartBytes
		want := fixture.envelope[start:min(start+legacyByteHeaderPartBytes, len(fixture.envelope))]
		if len(body) == 0 || len(body) > legacyByteHeaderPartBytes || hash != domain.HashContent(body) || !bytes.Equal(body, want) {
			test.Fatal("header part changed its hash, bound, or exact canonical payload", ordinal, len(body))
		}
		joined = append(joined, body...)
		count++
	}
	if rows.Err() != nil || count != next || !bytes.Equal(joined, fixture.envelope[:min(next*legacyByteHeaderPartBytes, len(fixture.envelope))]) {
		test.Fatal("header cursor and persisted payload are not one atomic prefix", count, next, rows.Err())
	}
}

func legacyByteHeaderPGImage(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'parts',(SELECT jsonb_agg(jsonb_build_array(ordinal,hash,encode(bytes,'hex'),xmin::text) ORDER BY ordinal)
 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1),
 'usage',(SELECT jsonb_build_array(to_jsonb(usage),usage.xmin::text) FROM doc_legacy_byte_usage_v1 usage WHERE doc_hash=$1)
 )::text`, hash).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image + legacyByteStagePGImage(test, storage, ctx, hash, -1)
}

func legacyByteHeaderPGAssertLockOrder(test *testing.T, trace *legacyByteHeaderPGTrace) {
	test.Helper()
	var locks []string
	for _, statement := range trace.queries {
		if strings.Contains(statement, "source.bytes") {
			test.Fatal("header persistence reread the original blob body", statement)
		}
		if strings.Contains(statement, "FOR UPDATE OF source") {
			locks = append(locks, "source")
		} else if strings.Contains(statement, "FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1 FOR UPDATE") {
			locks = append(locks, "stage")
		} else if strings.HasPrefix(statement, "SELECT 1 FROM doc_range_jobs_v1") && strings.HasSuffix(statement, "FOR UPDATE") {
			locks = append(locks, "job")
		}
	}
	if strings.Join(locks, ",") != "source,stage,job" {
		test.Fatal("header writer changed the source-stage-job lock order", locks)
	}
}

func TestLegacyByteHeaderPGVersionZeroBoundedResumeAndLostACK(test *testing.T) {
	for _, candidate := range []struct {
		name        string
		headerBytes int
		streamBytes int
	}{
		{"small", 1024, 1024},
		{"exact-part", legacyByteHeaderPartBytes, 1024},
		{"three-parts", 2*legacyByteHeaderPartBytes + 17, 1024},
		{"empty-stream", legacyByteHeaderPartBytes + 1, 0},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteHeaderPGSource(test, storage, ctx, candidate.headerBytes, candidate.streamBytes)
			job, session, seed := legacyByteHeaderPGPrepare(test, storage, ctx, fixture)
			stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
			if err != nil || !stage.complete || stage.sourceVersion != 0 {
				test.Fatal("fixture did not initialize a current version-zero source stage", stage, err)
			}
			legacyByteHeaderPGAssert(test, storage, ctx, fixture, 0)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			stream := legacyByteStagePGImage(test, storage, ctx, fixture.hash, stage.next)
			parts := (candidate.headerBytes + legacyByteHeaderPartBytes - 1) / legacyByteHeaderPartBytes
			trace := &legacyByteHeaderPGTrace{}
			peer := legacyByteHeaderPGPeer(test, storage, ctx, trace)
			for next := 1; next <= parts; next++ {
				trace.queries = nil
				if next == 1 {
					_, _ = peer.stageLegacyByteHeader(ctx, job, stage, seed)
				} else {
					complete, err := peer.stageLegacyByteHeader(ctx, job, stage, seed)
					if err != nil || complete != (next == parts) {
						test.Fatal("header did not commit one bounded part", next, complete, err)
					}
				}
				legacyByteHeaderPGAssertLockOrder(test, trace)
				legacyByteHeaderPGAssert(test, storage, ctx, fixture, next)
				legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
				if next == 1 {
					peer = legacyByteHeaderPGPeer(test, storage, ctx, trace)
				}
			}
			if len(trace.inserts) != parts {
				test.Fatal("retry with the same pre-ACK stage duplicated or skipped a part", trace.inserts)
			}
			for ordinal, length := range trace.inserts {
				if length != min(legacyByteHeaderPartBytes, candidate.headerBytes-ordinal*legacyByteHeaderPartBytes) {
					test.Fatal("writer exceeded one 64-KiB envelope read per call", trace.inserts)
				}
			}
			terminal := legacyByteHeaderPGImage(test, storage, ctx, fixture.hash)
			for _, owner := range []*PostgresStore{storage, peer} {
				if complete, err := owner.stageLegacyByteHeader(ctx, job, stage, seed); err != nil || !complete {
					test.Fatal("complete header was not idempotent after an unobserved ACK", complete, err)
				}
			}
			if legacyByteHeaderPGImage(test, storage, ctx, fixture.hash) != terminal ||
				legacyByteStagePGImage(test, storage, ctx, fixture.hash, stage.next) != stream ||
				legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("header persistence rewrote completed parts, stream, source, snapshot, or job")
			}
		})
	}
}

func TestLegacyByteHeaderPGDoesNotChangeStreamCursorOrEOF(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteHeaderPGSource(test, storage, ctx, 1024, 2*domain.ConversationManifestChunkBytes)
	job, session, seed := legacyByteHeaderPGPrepare(test, storage, ctx, fixture)
	partial, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || partial.complete || partial.next != 1 {
		test.Fatal("fixture did not retain an incomplete stream", partial, err)
	}
	if complete, err := storage.stageLegacyByteHeader(ctx, job, partial, seed); err != nil || !complete {
		test.Fatal("header could not be staged independently of the stream cursor", complete, err)
	}
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, partial)
	legacyByteHeaderPGAssert(test, storage, ctx, fixture, 1)
	completed, err := storage.stageLegacyByteSession(ctx, job, session, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || !completed.complete || completed.next != completed.blocks {
		test.Fatal("header protocol blocked the existing stream writer", completed, err)
	}
	legacyByteHeaderPGAssert(test, storage, ctx, fixture, 1)
	terminal := legacyByteHeaderPGImage(test, storage, ctx, fixture.hash)
	if complete, err := storage.stageLegacyByteHeader(ctx, job, partial, seed); err != nil || !complete {
		test.Fatal("stale local stream cursor hid the current persisted header cursor", complete, err)
	}
	if legacyByteHeaderPGImage(test, storage, ctx, fixture.hash) != terminal {
		test.Fatal("header retry changed the newer stream EOF")
	}
}

func TestLegacyByteHeaderPGCurrentSourceJobAndSeedFences(test *testing.T) {
	for _, change := range []string{"source version", "source incarnation", "ownership", "snapshot", "lease", "job version", "completed job", "seed", "seed reference", "plan", "blocks", "local EOF", "read only", "canceled"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteHeaderPGSource(test, storage, ctx, 2*legacyByteHeaderPartBytes+1, 1024)
			job, session, seed := legacyByteHeaderPGPrepare(test, storage, ctx, fixture)
			stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
			if err != nil {
				test.Fatal(err)
			}
			if complete, err := storage.stageLegacyByteHeader(ctx, job, stage, seed); err != nil || complete {
				test.Fatal("fixture did not retain a partial header", complete, err)
			}
			work := ctx
			expected := domain.ErrConflict
			switch change {
			case "source version":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
			case "source incarnation":
				for _, statement := range []string{`DELETE FROM snapshots WHERE doc_hash=$1`, `DELETE FROM repo_blobs WHERE hash=$1`, `DELETE FROM blobs WHERE hash=$1`} {
					if _, err := storage.pool.Exec(ctx, statement, fixture.hash); err != nil {
						test.Fatal(err)
					}
				}
				if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, fixture.hash, fixture.raw); err != nil {
					test.Fatal(err)
				}
				if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, fixture.repo, fixture.hash); err != nil {
					test.Fatal(err)
				}
				err = storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: fixture.repo, DocHash: fixture.hash,
					DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull})
			case "ownership":
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.hash)
				expected = domain.ErrNotFound
			case "snapshot":
				_, err = storage.pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.hash)
				expected = domain.ErrNotFound
			case "lease":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, fixture.hash)
			case "job version":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, fixture.hash)
				if err == nil {
					reclaimed := docRangeJobPGClaim(test, storage, ctx, fixture.repo, fixture.hash)
					if reclaimed.Version != job.Version+1 {
						test.Fatal("replacement claim did not advance the real job fence", reclaimed.Version, job.Version)
					}
				}
			case "completed job":
				legacyByteHeaderPGComplete(test, storage, ctx, job, session)
			case "seed":
				seed = domain.LegacyRangeMetadataSeed{}
				expected = domain.ErrIntegrity
			case "seed reference":
				var document domain.CIRDocument
				if err := json.Unmarshal(fixture.raw, &document); err != nil {
					test.Fatal(err)
				}
				document.Envelope.Cwd += "foreign envelope"
				raw, err := domain.CanonicalBytes(document)
				if err != nil {
					test.Fatal(err)
				}
				proof, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: document})
				if err != nil {
					test.Fatal(err)
				}
				plan, err := domain.NewLegacyByteBackfill(ctx, proof)
				if err != nil {
					test.Fatal(err)
				}
				seed, err = domain.NewLegacyRangeMetadataSeed(ctx, proof, plan)
				if err != nil {
					test.Fatal(err)
				}
				stage.plan = seed.PlanKey()
				expected = domain.ErrIntegrity
			case "plan":
				stage.plan = domain.HashContent([]byte("unrelated header plan"))
				expected = domain.ErrIntegrity
			case "blocks":
				stage.blocks++
				stage.complete = false
				expected = domain.ErrIntegrity
			case "local EOF":
				stage.complete = false
				expected = domain.ErrValidation
			case "read only":
				work = outbound.WithDocReadOnly(ctx)
			case "canceled":
				var cancel context.CancelFunc
				work, cancel = context.WithCancel(ctx)
				cancel()
				expected = context.Canceled
			}
			if err != nil {
				test.Fatal(err)
			}
			private := legacyByteHeaderPGImage(test, storage, ctx, fixture.hash)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			if complete, err := storage.stageLegacyByteHeader(work, job, stage, seed); complete || !errors.Is(err, expected) {
				test.Fatal("stale or unbound header write passed its current fence", complete, err, expected)
			}
			if legacyByteHeaderPGImage(test, storage, ctx, fixture.hash) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("rejected header write changed private, archival, or job state")
			}
		})
	}
}

func TestLegacyByteHeaderPGCompletedHeaderStillRequiresCurrentFences(test *testing.T) {
	for _, change := range []string{"source", "lease", "job"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteHeaderPGSource(test, storage, ctx, 1024, 1024)
			job, session, seed := legacyByteHeaderPGPrepare(test, storage, ctx, fixture)
			stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
			if err != nil {
				test.Fatal(err)
			}
			if complete, err := storage.stageLegacyByteHeader(ctx, job, stage, seed); err != nil || !complete {
				test.Fatal("fixture header did not reach EOF", complete, err)
			}
			switch change {
			case "source":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
			case "lease":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, fixture.hash)
			case "job":
				legacyByteHeaderPGComplete(test, storage, ctx, job, session)
			}
			if err != nil {
				test.Fatal(err)
			}
			private := legacyByteHeaderPGImage(test, storage, ctx, fixture.hash)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			if complete, err := storage.stageLegacyByteHeader(ctx, job, stage, seed); complete || !errors.Is(err, domain.ErrConflict) {
				test.Fatal("completed header bypassed current source or job authorization", complete, err)
			}
			if legacyByteHeaderPGImage(test, storage, ctx, fixture.hash) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("completed header retry changed a stale source or a terminal job")
			}
		})
	}
}

func legacyByteHeaderPGComplete(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob, session legacyByteBackfillSession) {
	test.Helper()
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	work, err := storage.prepareLegacyByteWork(ctx, job, session)
	if err != nil {
		test.Fatal("prepare a real completed-job fence", err)
	}
	defer work.Close()
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("publish a real completed-job fence", err)
	}
}

func TestLegacyByteHeaderPGPartAndCursorRollbackTogether(test *testing.T) {
	for _, failure := range []string{"initial lease", "append lease", "checkpoint", "cancel after insert"} {
		test.Run(failure, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteHeaderPGSource(test, storage, ctx, 2*legacyByteHeaderPartBytes+1, 1024)
			job, session, seed := legacyByteHeaderPGPrepare(test, storage, ctx, fixture)
			stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
			if err != nil {
				test.Fatal(err)
			}
			if failure == "append lease" {
				if complete, err := storage.stageLegacyByteHeader(ctx, job, stage, seed); err != nil || complete {
					test.Fatal(complete, err)
				}
			}
			private := legacyByteHeaderPGImage(test, storage, ctx, fixture.hash)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			owner, work := storage, ctx
			if failure == "cancel after insert" {
				var cancel context.CancelFunc
				work, cancel = context.WithCancel(ctx)
				defer cancel()
				owner = legacyByteHeaderPGPeer(test, storage, ctx, &legacyByteHeaderPGTrace{cancel: cancel})
			} else {
				name := pgx.Identifier{domain.NewID("legacy_header_fence_")}.Sanitize()
				target := "doc_legacy_byte_stage_headers_v1"
				action := fmt.Sprintf(`IF NEW.doc_hash='%s' THEN UPDATE doc_range_jobs_v1 SET version=version+1,
 state='waiting',lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash; END IF;`, fixture.hash, fixture.repo)
				event := "AFTER INSERT"
				if failure == "checkpoint" {
					target, event = "doc_legacy_byte_stages_v1", "BEFORE UPDATE"
					action = fmt.Sprintf(`IF NEW.doc_hash='%s' AND NEW.header_next>OLD.header_next THEN
 RAISE EXCEPTION 'injected header checkpoint failure' USING ERRCODE='23514'; END IF;`, fixture.hash)
				}
				statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN %s RETURN NEW; END; $$;
 CREATE TRIGGER %s %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, action, name, event, target, name)
				if _, err := storage.pool.Exec(ctx, statement); err != nil {
					test.Fatal(err)
				}
				defer func() {
					if _, err := storage.pool.Exec(context.WithoutCancel(ctx), `DROP TRIGGER IF EXISTS `+name+` ON `+target+`; DROP FUNCTION IF EXISTS `+name+`() `); err != nil {
						test.Error(err)
					}
				}()
			}
			complete, err := owner.stageLegacyByteHeader(work, job, stage, seed)
			if complete || err == nil || strings.Contains(failure, "lease") && !errors.Is(err, domain.ErrConflict) ||
				failure == "cancel after insert" && !errors.Is(err, context.Canceled) {
				test.Fatal("failed header transaction advanced or hid its fence error", complete, err)
			}
			if legacyByteHeaderPGImage(test, storage, ctx, fixture.hash) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("failed header transaction retained initialization, part, cursor, accounting, or job mutation")
			}
		})
	}
}
