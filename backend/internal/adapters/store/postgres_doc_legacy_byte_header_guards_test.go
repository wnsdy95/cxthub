//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyByteHeaderGuardPGFixture struct {
	source legacyByteStagePGFixture
	job    domain.DocRangeIndexJob
	stage  legacyByteStage
	seed   domain.LegacyRangeMetadataSeed
}

func legacyByteHeaderGuardPGSource(test *testing.T, storage *PostgresStore, ctx context.Context) legacyByteHeaderGuardPGFixture {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	source := legacyByteStagePGDocument(test, storage, ctx, repo, domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Cwd: strings.Repeat("header", (64<<10)/6), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: test.Name() + ":" + string(repo)}}}},
	})
	job := legacyByteStagePGClaim(test, storage, ctx, source)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	seed, err := domain.NewLegacyRangeMetadataSeed(ctx, session.proof, session.plan)
	if err != nil || seed.EnvelopeBytes() != len(source.envelope) || len(source.envelope) <= 64<<10 || len(source.envelope) >= 128<<10 {
		test.Fatal("guard fixture must have one full header part and a short tail", len(source.envelope), err)
	}
	stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || !stage.complete {
		test.Fatal("guard fixture must retain a complete original stream stage", stage, err)
	}
	var undeclared bool
	if err := storage.pool.QueryRow(ctx, `SELECT header_version=0 AND header_bytes IS NULL AND header_next=0 AND NOT header_complete
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, source.hash).Scan(&undeclared); err != nil || !undeclared {
		test.Fatal("stream writer declared a header without its sealed seed", undeclared, err)
	}
	return legacyByteHeaderGuardPGFixture{source: source, job: job, stage: stage, seed: seed}
}

func legacyByteHeaderGuardPGImage(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteHeaderGuardPGFixture) string {
	test.Helper()
	var header string
	if err := storage.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(
 ordinal,hash,encode(bytes,'hex'),xmin::text) ORDER BY ordinal),'[]'::jsonb)::text
 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1`, fixture.source.hash).Scan(&header); err != nil {
		test.Fatal(err)
	}
	return header + legacyByteStagePGImage(test, storage, ctx, fixture.source.hash, -1) +
		legacyByteStagePGPublicImage(test, storage, ctx, fixture.source)
}

func legacyByteHeaderGuardPGTransaction(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteHeaderGuardPGFixture) pgx.Tx {
	test.Helper()
	before := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture)
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		rollbackPG(transaction)
		if after := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture); after != before {
			test.Error("rejected SQL changed header parts, stream stage, original source, ownership, or job")
		}
	})
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'; SET LOCAL cxt.legacy_byte_header_protocol='1'`); err != nil {
		test.Fatal(err)
	}
	return transaction
}

func legacyByteHeaderGuardPGDeclare(test *testing.T, ctx context.Context, transaction pgx.Tx, fixture legacyByteHeaderGuardPGFixture) {
	test.Helper()
	tag, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_version=1,header_bytes=$2 WHERE doc_hash=$1`,
		fixture.source.hash, len(fixture.source.envelope))
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("could not declare the current header extent", tag, err)
	}
}

func legacyByteHeaderGuardPGError(test *testing.T, err error, code, message string) {
	test.Helper()
	var rejected *pgconn.PgError
	if !errors.As(err, &rejected) || rejected.Code != code || !strings.Contains(rejected.Message, message) {
		test.Fatalf("expected SQL guard %s containing %q, got %v", code, message, err)
	}
}

func legacyByteHeaderGuardPGAdvance(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteHeaderGuardPGFixture, expectedNext int) {
	test.Helper()
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture.source)
	done, err := storage.stageLegacyByteHeader(ctx, fixture.job, fixture.stage, fixture.seed)
	if err != nil || done != (expectedNext == 2) {
		test.Fatal("sealed header writer did not advance exactly one part", expectedNext, done, err)
	}
	var version, extent, next int
	var complete bool
	if err := storage.pool.QueryRow(ctx, `SELECT header_version,header_bytes,header_next,header_complete
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.source.hash).Scan(&version, &extent, &next, &complete); err != nil ||
		version != 1 || extent != len(fixture.source.envelope) || next != expectedNext || complete != done {
		test.Fatal("header declaration and durable checkpoint disagree", version, extent, next, complete, err)
	}
	rows, err := storage.pool.Query(ctx, `SELECT ordinal,hash,bytes FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1 ORDER BY ordinal`, fixture.source.hash)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var ordinal int
		var hash domain.ContentHash
		var body []byte
		if err := rows.Scan(&ordinal, &hash, &body); err != nil {
			test.Fatal(err)
		}
		if ordinal != count || ordinal >= expectedNext {
			test.Fatal("header parts skipped or exceeded their durable cursor", ordinal, count, expectedNext)
		}
		start := ordinal * (64 << 10)
		want := fixture.source.envelope[start:min(start+(64<<10), len(fixture.source.envelope))]
		if len(body) > 64<<10 || hash != domain.HashContent(body) || !bytes.Equal(body, want) {
			test.Fatal("header part is not the exact canonical envelope slice", ordinal)
		}
		count++
	}
	if err := rows.Err(); err != nil || count != expectedNext {
		test.Fatal("header membership and checkpoint disagree", count, expectedNext, err)
	}
	rows.Close()
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture.source); after != public {
		test.Fatal("private header persistence changed public source or job state")
	}
}

func TestLegacyByteHeaderPGRequiresBothSQLProtocols(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
	for _, protocol := range []struct {
		name, stage, header string
	}{
		{"neither", "", ""}, {"stage-only", "1", ""}, {"header-only", "", "1"},
		{"wrong-stage", "2", "1"}, {"wrong-header", "1", "2"},
	} {
		test.Run(protocol.name, func(test *testing.T) {
			for _, operation := range []string{"declaration", "part"} {
				test.Run(operation, func(test *testing.T) {
					transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
					if operation == "part" {
						legacyByteHeaderGuardPGDeclare(test, ctx, transaction, fixture)
					}
					if _, err := transaction.Exec(ctx, `SELECT set_config('cxt.legacy_byte_stage_protocol',$1,true),
 set_config('cxt.legacy_byte_header_protocol',$2,true)`, protocol.stage, protocol.header); err != nil {
						test.Fatal(err)
					}
					var err error
					if operation == "part" {
						body := fixture.source.envelope[:64<<10]
						_, err = transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes) VALUES($1,0,$2,$3)`,
							fixture.source.hash, domain.HashContent(body), body)
					} else {
						_, err = transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_version=1,header_bytes=$2 WHERE doc_hash=$1`,
							fixture.source.hash, len(fixture.source.envelope))
					}
					legacyByteHeaderGuardPGError(test, err, "0A000", "protocol 1")
				})
			}
		})
	}
}

func TestLegacyByteHeaderPGDeclarationVersionAndExtentGuards(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
	for _, candidate := range []struct{ name, assignment string }{
		{"version-zero-bytes", "header_bytes=2"},
		{"version-zero-cursor", "header_next=1"},
		{"version-zero-complete", "header_complete=true"},
		{"negative-version", "header_version=-1"},
		{"unknown-version", "header_version=2,header_bytes=2"},
		{"missing-extent", "header_version=1"},
		{"empty-extent", "header_version=1,header_bytes=0"},
		{"one-byte-extent", "header_version=1,header_bytes=1"},
		{"oversized-extent", "header_version=1,header_bytes=536870913"},
		{"initialized-cursor", "header_version=1,header_bytes=2,header_next=1"},
		{"initialized-complete", "header_version=1,header_bytes=2,header_complete=true"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
			_, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET `+candidate.assignment+` WHERE doc_hash=$1`, fixture.source.hash)
			legacyByteHeaderGuardPGError(test, err, "23514", "")
		})
	}
	test.Run("predeclared-stage-insert", func(test *testing.T) {
		transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
		_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stages_v1
 (doc_hash,source_id,source_version,plan_hash,spec,blocks,next_block,complete,header_version,header_bytes)
 SELECT doc_hash,source_id,source_version,plan_hash,spec,blocks,0,blocks=0,1,2
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.source.hash)
		legacyByteHeaderGuardPGError(test, err, "23514", "header must start undeclared")
	})
}

func TestLegacyByteHeaderPGOrdinalHashAndExactPartSizeGuards(test *testing.T) {
	for _, position := range []string{"first", "tail"} {
		test.Run(position, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
			ordinal := 0
			if position == "tail" {
				legacyByteHeaderGuardPGAdvance(test, storage, ctx, fixture, 1)
				ordinal = 1
			}
			damages := []string{"negative-ordinal", "past-extent", "ordinal-8192", "empty", "short", "long", "fake-hash", "changed-bytes"}
			if position == "first" {
				damages = append(damages, "skipped-ordinal")
			} else {
				damages = append(damages, "previous-ordinal")
			}
			for _, damage := range damages {
				test.Run(damage, func(test *testing.T) {
					transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
					if position == "first" {
						legacyByteHeaderGuardPGDeclare(test, ctx, transaction, fixture)
					}
					start := ordinal * (64 << 10)
					body := bytes.Clone(fixture.source.envelope[start:min(start+(64<<10), len(fixture.source.envelope))])
					candidate := ordinal
					switch damage {
					case "negative-ordinal":
						candidate = -1
					case "skipped-ordinal":
						candidate = 1
						body = bytes.Clone(fixture.source.envelope[64<<10:])
					case "previous-ordinal":
						candidate = 0
						body = bytes.Clone(fixture.source.envelope[:64<<10])
					case "past-extent":
						candidate = 2
					case "ordinal-8192":
						candidate = 8192
					case "empty":
						body = body[:0]
					case "short":
						body = body[:len(body)-1]
					case "long":
						body = append(body, 'x')
					}
					hash := domain.HashContent(body)
					if damage == "fake-hash" {
						hash = domain.HashContent([]byte("not this header part"))
					}
					if damage == "changed-bytes" {
						body[0] ^= 1
					}
					_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes) VALUES($1,$2,$3,$4)`,
						fixture.source.hash, candidate, hash, body)
					legacyByteHeaderGuardPGError(test, err, "23514", "outside its declared extent")
				})
			}
		})
	}
}

func TestLegacyByteHeaderPGRequiresAtomicCurrentCheckpoint(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
	for _, damage := range []string{"undeclared", "cursor-without-part", "skipped-cursor", "part-without-cursor", "early-complete", "source-changed-before-commit"} {
		test.Run(damage, func(test *testing.T) {
			transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
			body := fixture.source.envelope[:64<<10]
			if damage == "undeclared" {
				_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes) VALUES($1,0,$2,$3)`,
					fixture.source.hash, domain.HashContent(body), body)
				legacyByteHeaderGuardPGError(test, err, "23514", "outside its declared extent")
				return
			}
			legacyByteHeaderGuardPGDeclare(test, ctx, transaction, fixture)
			if damage == "cursor-without-part" || damage == "skipped-cursor" {
				next := 1
				message := "checkpoint skipped its part"
				if damage == "skipped-cursor" {
					next = 2
					message = "declaration or checkpoint changed"
				}
				_, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_next=$2,header_complete=($2=2) WHERE doc_hash=$1`, fixture.source.hash, next)
				legacyByteHeaderGuardPGError(test, err, "23514", message)
				return
			}
			if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes) VALUES($1,0,$2,$3)`,
				fixture.source.hash, domain.HashContent(body), body); err != nil {
				test.Fatal("valid part was rejected before its deferred checkpoint", err)
			}
			if damage == "part-without-cursor" {
				legacyByteHeaderGuardPGError(test, transaction.Commit(ctx), "23514", "atomic current checkpoint")
				return
			}
			_, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_next=1,header_complete=$2 WHERE doc_hash=$1`,
				fixture.source.hash, damage == "early-complete")
			if damage == "early-complete" {
				legacyByteHeaderGuardPGError(test, err, "23514", "legacy_byte_stage_header_extent")
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			if _, err := transaction.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.source.hash); err != nil {
				test.Fatal(err)
			}
			legacyByteHeaderGuardPGError(test, transaction.Commit(ctx), "23514", "atomic current checkpoint")
		})
	}
}

func TestLegacyByteHeaderPGSourceAndStageSQLFences(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
	for _, damage := range []string{"stage-incarnation", "stage-version", "stale-declaration", "stale-part", "missing-stage", "missing-source"} {
		test.Run(damage, func(test *testing.T) {
			transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
			if damage == "stage-incarnation" || damage == "stage-version" {
				_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stages_v1
 (doc_hash,source_id,source_version,plan_hash,spec,blocks,next_block,complete)
 SELECT doc_hash,CASE WHEN $2 THEN '00000000-0000-0000-0000-000000000001'::uuid ELSE source_id END,
 source_version+CASE WHEN $2 THEN 0 ELSE 1 END,plan_hash,spec,blocks,0,blocks=0
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.source.hash, damage == "stage-incarnation")
				legacyByteHeaderGuardPGError(test, err, "23514", "current original source")
				return
			}
			if damage != "stale-declaration" {
				legacyByteHeaderGuardPGDeclare(test, ctx, transaction, fixture)
			}
			if damage == "stale-declaration" || damage == "stale-part" {
				if _, err := transaction.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.source.hash); err != nil {
					test.Fatal(err)
				}
			}
			if damage == "stale-declaration" {
				_, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_version=1,header_bytes=$2 WHERE doc_hash=$1`,
					fixture.source.hash, len(fixture.source.envelope))
				legacyByteHeaderGuardPGError(test, err, "23514", "current original source")
				return
			}
			if damage == "missing-stage" {
				if _, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.source.hash); err != nil {
					test.Fatal(err)
				}
			}
			hash := fixture.source.hash
			message := "current original source"
			if damage == "missing-source" {
				hash = domain.HashContent([]byte(test.Name() + ":absent:" + string(fixture.source.repo)))
			}
			if damage == "missing-stage" {
				message = "requires its stage"
			}
			body := fixture.source.envelope[:64<<10]
			_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes) VALUES($1,0,$2,$3)`,
				hash, domain.HashContent(body), body)
			legacyByteHeaderGuardPGError(test, err, "23514", message)
		})
	}
}

func TestLegacyByteHeaderPGImmutableLivePartsAndTerminalCursor(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
	legacyByteHeaderGuardPGAdvance(test, storage, ctx, fixture, 1)
	for _, candidate := range []struct{ name, statement, message string }{
		{"reset-version", `UPDATE doc_legacy_byte_stages_v1 SET header_version=0,header_bytes=NULL,header_next=0,header_complete=false WHERE doc_hash=$1`, "declaration or checkpoint changed"},
		{"replace-stage-incarnation", `UPDATE doc_legacy_byte_stages_v1 SET stage_id=gen_random_uuid() WHERE doc_hash=$1`, "stage incarnation is immutable"},
		{"change-version", `UPDATE doc_legacy_byte_stages_v1 SET header_version=2 WHERE doc_hash=$1`, "declaration or checkpoint changed"},
		{"change-extent", `UPDATE doc_legacy_byte_stages_v1 SET header_bytes=header_bytes+1 WHERE doc_hash=$1`, "declaration or checkpoint changed"},
		{"rewind", `UPDATE doc_legacy_byte_stages_v1 SET header_next=0 WHERE doc_hash=$1`, "declaration or checkpoint changed"},
		{"fake-completion", `UPDATE doc_legacy_byte_stages_v1 SET header_complete=true WHERE doc_hash=$1`, "declaration or checkpoint changed"},
		{"rewrite-identical-part", `UPDATE doc_legacy_byte_stage_headers_v1 SET bytes=bytes WHERE doc_hash=$1`, "header parts are immutable"},
		{"rewrite-part-hash", `UPDATE doc_legacy_byte_stage_headers_v1 SET hash='sha256:'||repeat('0',64) WHERE doc_hash=$1`, "header parts are immutable"},
		{"delete-live-part", `DELETE FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1`, "header parts are retained"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
			_, err := transaction.Exec(ctx, candidate.statement, fixture.source.hash)
			legacyByteHeaderGuardPGError(test, err, "23514", candidate.message)
		})
	}
	legacyByteHeaderGuardPGAdvance(test, storage, ctx, fixture, 2)
	for _, candidate := range []struct{ name, statement, message string }{
		{"terminal-update-without-protocol", `UPDATE doc_legacy_byte_stage_headers_v1 SET bytes=bytes WHERE doc_hash=$1`, "header parts are immutable"},
		{"terminal-delete-without-protocol", `DELETE FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1`, "header parts are retained"},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
			if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol=''; SET LOCAL cxt.legacy_byte_header_protocol=''`); err != nil {
				test.Fatal(err)
			}
			_, err := transaction.Exec(ctx, candidate.statement, fixture.source.hash)
			legacyByteHeaderGuardPGError(test, err, "23514", candidate.message)
		})
	}
	for _, ordinal := range []int{0, 1, 2} {
		name := []string{"duplicate-first", "duplicate-tail", "append-after-complete"}[ordinal]
		test.Run(name, func(test *testing.T) {
			transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
			body := fixture.source.envelope[64<<10:]
			_, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes) VALUES($1,$2,$3,$4)`,
				fixture.source.hash, ordinal, domain.HashContent(body), body)
			legacyByteHeaderGuardPGError(test, err, "23514", "outside its declared extent")
		})
	}
	test.Run("advance-after-complete", func(test *testing.T) {
		transaction := legacyByteHeaderGuardPGTransaction(test, storage, ctx, fixture)
		_, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET header_next=header_next+1 WHERE doc_hash=$1`, fixture.source.hash)
		legacyByteHeaderGuardPGError(test, err, "23514", "declaration or checkpoint changed")
	})
}

func TestLegacyByteHeaderPGCompletedHeaderDoesNotBlockStreamWriter(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	source := legacyByteStagePGSource(test, storage, ctx, 512<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, source)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	seed, err := domain.NewLegacyRangeMetadataSeed(ctx, session.proof, session.plan)
	if err != nil {
		test.Fatal(err)
	}
	partial, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || partial.complete || partial.next != 1 || partial.blocks != 2 {
		test.Fatal("fixture must leave one stream block after header completion", partial, err)
	}
	if done, err := storage.stageLegacyByteHeader(ctx, job, partial, seed); err != nil || !done {
		test.Fatal("single-part header did not complete independently of stream", done, err)
	}
	var before string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_array(stage.header_version,stage.header_bytes,stage.header_next,stage.header_complete,
 (SELECT jsonb_agg(jsonb_build_array(ordinal,hash,encode(bytes,'hex'),xmin::text) ORDER BY ordinal)
 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1))::text FROM doc_legacy_byte_stages_v1 stage WHERE doc_hash=$1`, source.hash).Scan(&before); err != nil {
		test.Fatal(err)
	}
	complete, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || !complete.complete || complete.next != 2 || complete.plan != partial.plan {
		test.Fatal("unchanged header fields blocked the existing stream writer", complete, err)
	}
	var after string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_array(stage.header_version,stage.header_bytes,stage.header_next,stage.header_complete,
 (SELECT jsonb_agg(jsonb_build_array(ordinal,hash,encode(bytes,'hex'),xmin::text) ORDER BY ordinal)
 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1))::text FROM doc_legacy_byte_stages_v1 stage WHERE doc_hash=$1`, source.hash).Scan(&after); err != nil || after != before {
		test.Fatal("stream progress rewrote completed header payload or declaration", err)
	}
	legacyByteStagePGAssertStored(test, storage, ctx, source, complete)
}

func TestLegacyByteHeaderPGWriterRequiresCurrentOwnershipAndSealedSeed(test *testing.T) {
	for _, copied := range []int{0, 1, 2} {
		name := []string{"before-declaration", "partial", "complete"}[copied]
		test.Run(name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
			for next := 1; next <= copied; next++ {
				legacyByteHeaderGuardPGAdvance(test, storage, ctx, fixture, next)
			}
			if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.source.repo, fixture.source.hash); err != nil {
				test.Fatal(err)
			}
			before := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture)
			if done, err := storage.stageLegacyByteHeader(ctx, fixture.job, fixture.stage, fixture.seed); done || !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("header writer reused revoked ownership or terminal completion", done, err)
			}
			if after := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture); after != before {
				test.Fatal("revoked header write changed persisted state")
			}
		})
	}
	test.Run("unsealed-and-wrong-plan", func(test *testing.T) {
		storage, ctx := chunkReusePG(test)
		fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
		var decoded domain.LegacyRangeMetadataSeed
		if err := json.Unmarshal(fixture.source.envelope, &decoded); err != nil {
			test.Fatal(err)
		}
		wrongPlan := fixture.stage
		wrongPlan.plan = domain.HashContent([]byte("not the sealed header plan"))
		for _, candidate := range []struct {
			name  string
			stage legacyByteStage
			seed  domain.LegacyRangeMetadataSeed
		}{
			{"zero-seed", fixture.stage, domain.LegacyRangeMetadataSeed{}},
			{"serialized-envelope", fixture.stage, decoded},
			{"wrong-plan", wrongPlan, fixture.seed},
		} {
			test.Run(candidate.name, func(test *testing.T) {
				before := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture)
				if done, err := storage.stageLegacyByteHeader(ctx, fixture.job, candidate.stage, candidate.seed); done || !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("unsealed bytes or unrelated plan acquired a header declaration", done, err)
				}
				if after := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture); after != before {
					test.Fatal("fake header changed durable stage or public state")
				}
			})
		}
	})
}

func TestLegacyByteHeaderPGParentAndSourceDeletionCascade(test *testing.T) {
	for _, parent := range []string{"stage", "source"} {
		test.Run(parent, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteHeaderGuardPGSource(test, storage, ctx)
			legacyByteHeaderGuardPGAdvance(test, storage, ctx, fixture, 1)
			legacyByteHeaderGuardPGAdvance(test, storage, ctx, fixture, 2)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture.source)
			statements := []string{`DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`}
			if parent == "source" {
				statements = []string{`DELETE FROM snapshots WHERE doc_hash=$1`, `DELETE FROM repo_blobs WHERE hash=$1`, `DELETE FROM blobs WHERE hash=$1`}
			}
			for _, statement := range statements {
				if _, err := storage.pool.Exec(ctx, statement, fixture.source.hash); err != nil {
					test.Fatal("legitimate parent deletion was blocked by retained header parts", err)
				}
			}
			var remaining int
			if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1)`, fixture.source.hash).Scan(&remaining); err != nil || remaining != 0 {
				test.Fatal("parent deletion retained private header, stream, or accounting rows", remaining, err)
			}
			if parent == "stage" {
				if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture.source); after != public {
					test.Fatal("derived stage deletion changed source, ownership, snapshot, or job")
				}
				return
			}
			if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, fixture.source.hash, fixture.source.raw); err != nil {
				test.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, fixture.source.repo, fixture.source.hash); err != nil {
				test.Fatal(err)
			}
			if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.source.hash, RepoID: fixture.source.repo, DocHash: fixture.source.hash,
				DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
				test.Fatal(err)
			}
			fresh, err := storage.stageLegacyByteBackfill(ctx, fixture.job, 1)
			if err != nil || fresh.sourceID == fixture.stage.sourceID || fresh.sourceVersion != fixture.stage.sourceVersion || fresh.plan != fixture.stage.plan {
				test.Fatal("same-hash reinsert did not acquire a new source incarnation", fresh, fixture.stage, err)
			}
			before := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture)
			if done, err := storage.stageLegacyByteHeader(ctx, fixture.job, fixture.stage, fixture.seed); done || !errors.Is(err, domain.ErrConflict) {
				test.Fatal("old header stage crossed a same-hash source incarnation", done, err)
			}
			if after := legacyByteHeaderGuardPGImage(test, storage, ctx, fixture); after != before {
				test.Fatal("old header stage changed the new incarnation")
			}
		})
	}
}
