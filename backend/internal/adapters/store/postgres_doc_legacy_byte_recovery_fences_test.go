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
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func legacyByteRecoveryFencePGStage(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) legacyByteStage {
	test.Helper()
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	seed, err := domain.NewLegacyRangeMetadataSeed(ctx, session.proof, session.plan)
	if err != nil {
		test.Fatal(err)
	}
	stage, err := storage.stageLegacyByteSession(ctx, job, session, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || !stage.complete {
		test.Fatal("fence fixture did not complete its bounded stream", stage, err)
	}
	parts := (seed.EnvelopeBytes() + (64 << 10) - 1) / (64 << 10)
	for ordinal := 0; ordinal < parts; ordinal++ {
		complete, err := storage.stageLegacyByteHeader(ctx, job, stage, seed)
		if err != nil || complete != (ordinal == parts-1) {
			test.Fatal("fence fixture did not persist exactly one header part", ordinal, complete, err)
		}
	}
	return stage
}

func legacyByteRecoveryFencePGReady(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) legacyByteRecoveryState {
	test.Helper()
	state, ready, err := storage.readLegacyByteRecovery(ctx, job)
	if err != nil || !ready {
		test.Fatal("complete current checkpoint was not recovery-ready", ready, err)
	}
	return state
}

func legacyByteRecoveryFencePGWork(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) outbound.PreparedDocRangeWork {
	test.Helper()
	storage.ConfigureLegacyByteRangePublication(true)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	work, err := storage.PrepareDocRangeWork(ctx, job)
	if work != nil {
		test.Cleanup(func() {
			if err := work.Close(); err != nil {
				test.Error(err)
			}
		})
	}
	if err != nil || work == nil {
		test.Fatal("prepare recovery work", err)
	}
	if _, recovered := work.(*preparedLegacyRecoveryWork); !recovered {
		test.Fatalf("complete checkpoint fell back to original capture: %T", work)
	}
	return work
}

func legacyByteRecoveryFencePGImage(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'headers',(SELECT jsonb_agg(jsonb_build_array(ordinal,hash,md5(bytes),octet_length(bytes),xmin::text) ORDER BY ordinal)
 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1),
 'usage',(SELECT jsonb_build_array(to_jsonb(usage),usage.xmin::text) FROM doc_legacy_byte_usage_v1 usage WHERE doc_hash=$1)
 )::text`, hash).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return legacyByteStagePGImage(test, storage, ctx, hash, -1) + image
}

func legacyByteRecoveryFencePGPublic(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_array(to_jsonb(memory),memory.xmin::text)
 ORDER BY memory.repo_id,memory.snapshot_id),'[]'::jsonb)::text FROM memories memory WHERE repo_id=$1`, fixture.repo).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return legacyByteStagePGPublicImage(test, storage, ctx, fixture) + image
}

func TestLegacyByteRecoveryPGIncompleteStagesAreNotReady(test *testing.T) {
	for _, partial := range []string{"absent", "version zero", "partial header", "partial stream"} {
		test.Run(partial, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			fixture := legacyByteStagePGDocument(test, storage, ctx, repo, domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
					SessionOriginID: string(repo) + strings.Repeat("h", 128<<10), Fidelity: domain.FidelityFull},
				Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
					Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", 1<<20)}}}},
			})
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			if partial != "absent" {
				session, err := storage.prepareLegacyByteBackfill(ctx, job)
				if err != nil {
					test.Fatal(err)
				}
				limit := domain.DocLegacyByteBackfillBatchBlocks
				if partial == "partial stream" {
					limit = 1
				}
				stage, err := storage.stageLegacyByteSession(ctx, job, session, limit)
				if err != nil || stage.complete != (partial != "partial stream") {
					test.Fatal("incorrect partial stream fixture", stage, err)
				}
				if partial != "version zero" {
					seed, err := domain.NewLegacyRangeMetadataSeed(ctx, session.proof, session.plan)
					if err != nil {
						test.Fatal(err)
					}
					parts := (seed.EnvelopeBytes() + (64 << 10) - 1) / (64 << 10)
					if parts < 3 {
						test.Fatal("partial header fixture is not multi-part", parts)
					}
					writes := parts
					if partial == "partial header" {
						writes = 1
					}
					for ordinal := 0; ordinal < writes; ordinal++ {
						complete, err := storage.stageLegacyByteHeader(ctx, job, stage, seed)
						if err != nil || complete != (ordinal == parts-1) {
							test.Fatal("incorrect partial header fixture", ordinal, complete, err)
						}
					}
				}
			}
			private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
			public := legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture)
			state, ready, err := storage.readLegacyByteRecovery(ctx, job)
			if err != nil || ready {
				test.Fatal("missing, old or incomplete checkpoint became recovery-ready", partial, ready, err)
			}
			state.stage.complete, state.stage.next = true, state.stage.blocks
			for _, header := range []bool{true, false} {
				body, err := storage.readLegacyByteRecoveryPart(ctx, job, state, header, 0)
				if err == nil || len(body) != 0 {
					test.Fatal("local completion forgery exposed an incomplete checkpoint", partial, header, len(body), err)
				}
			}
			if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private ||
				legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public {
				test.Fatal("not-ready probing changed checkpoint or archival state")
			}
		})
	}
}

func TestLegacyByteRecoveryPGReadyPartAndRunningWorkRecheckFences(test *testing.T) {
	for _, fence := range []string{"ownership", "snapshot", "source version", "lease", "job version"} {
		test.Run(fence, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 2<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			legacyByteRecoveryFencePGStage(test, storage, ctx, job)
			peer, trace := docCaptureWorkPGStore(test, storage, ctx)
			state := legacyByteRecoveryFencePGReady(test, peer, ctx, job)
			work := legacyByteRecoveryFencePGWork(test, peer, ctx, job)
			if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
				test.Fatal("recovery did not leave a bounded in-progress handle", progress, err)
			}
			var err error
			switch fence {
			case "ownership":
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.hash)
			case "snapshot":
				_, err = storage.pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.hash)
			case "source version":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
			case "lease":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash)
			case "job version":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash)
				if err == nil {
					docRangeJobPGClaim(test, storage, ctx, fixture.repo, fixture.hash)
				}
			}
			if err != nil {
				test.Fatal(err)
			}
			private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
			public := legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture)
			if _, ready, err := peer.readLegacyByteRecovery(ctx, job); ready || fence != "source version" && err == nil || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("ready probe bypassed a current fence or mislabeled it as cache damage", fence, ready, err)
			}
			if fence == "source version" {
				fresh, err := peer.PrepareDocRangeWork(ctx, job)
				if err != nil || fresh == nil {
					test.Fatal("old checkpoint permanently blocked fresh current-source preparation", err)
				}
				if _, captured := fresh.(*preparedLegacyCaptureWork); !captured {
					defer fresh.Close()
					test.Fatalf("fresh preparation restored a stale checkpoint instead of capturing the current source: %T", fresh)
				}
				if err := fresh.Close(); err != nil {
					test.Fatal(err)
				}
			}
			for _, header := range []bool{true, false} {
				body, err := peer.readLegacyByteRecoveryPart(ctx, job, state, header, 0)
				if err == nil || len(body) != 0 || errors.Is(err, domain.ErrPreparedDocRangeObject) {
					test.Fatal("selected part bypassed a current fence or concealed it as cache damage", header, len(body), err)
				}
			}
			if progress, err := work.Step(ctx, job); err == nil || progress == outbound.DocRangeWorkPublishable || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("running recovery hid a fence failure behind automatic cache retry", progress, err)
			}
			if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private ||
				legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public || trace.bodyReads.Load() != 0 {
				test.Fatal("rejected recovery evicted bytes, touched public state or reread the source", trace.bodyReads.Load())
			}
		})
	}
}

func TestLegacyByteRecoveryPGReadersRejectBorrowedAndReadOnlyContexts(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	legacyByteRecoveryFencePGStage(test, storage, ctx, job)
	state := legacyByteRecoveryFencePGReady(test, storage, ctx, job)
	private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
	var expired context.Context
	peer := &PostgresStore{pool: storage.pool}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		expired = read
		for _, reader := range []*PostgresStore{storage, peer} {
			if _, ready, err := reader.readLegacyByteRecovery(read, job); !errors.Is(err, domain.ErrConflict) || ready {
				test.Fatal("ready probe borrowed a repository transaction", ready, err)
			}
			for _, header := range []bool{true, false} {
				body, err := reader.readLegacyByteRecoveryPart(read, job, state, header, 0)
				if !errors.Is(err, domain.ErrConflict) || len(body) != 0 {
					test.Fatal("part reader borrowed a repository transaction", header, len(body), err)
				}
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, current := range []context.Context{outbound.WithDocReadOnly(ctx), expired} {
		if _, ready, err := storage.readLegacyByteRecovery(current, job); !errors.Is(err, domain.ErrConflict) || ready {
			test.Fatal("read-only or expired context admitted a recovery ready probe", ready, err)
		}
		for _, header := range []bool{true, false} {
			body, err := storage.readLegacyByteRecoveryPart(current, job, state, header, 0)
			if !errors.Is(err, domain.ErrConflict) || len(body) != 0 {
				test.Fatal("read-only or expired context admitted a recovery part", header, len(body), err)
			}
		}
	}
	if err := storage.WithinRepository(ctx, fixture.repo, func(write context.Context) error {
		if _, ready, err := storage.readLegacyByteRecovery(write, job); !errors.Is(err, domain.ErrConflict) || ready {
			test.Fatal("ready probe borrowed a write transaction", ready, err)
		}
		body, err := storage.readLegacyByteRecoveryPart(write, job, state, true, 0)
		if !errors.Is(err, domain.ErrConflict) || len(body) != 0 {
			test.Fatal("part reader borrowed a write transaction", len(body), err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, header := range []bool{true, false} {
		body, err := storage.readLegacyByteRecoveryPart(ctx, job, state, header, 0)
		want := []byte(fixture.envelope)
		if !header {
			want, _ = legacyByteStagePGBlock(fixture, 0)
		}
		if err != nil || !bytes.Equal(body, want) {
			test.Fatal("fresh reader failed to establish its own current snapshot", header, len(body), err)
		}
	}
	if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private {
		test.Fatal("invalid snapshot probes changed the checkpoint")
	}
}

func TestLegacyByteRecoveryPGSerializedStateIsNotPartAuthority(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	legacyByteRecoveryFencePGStage(test, storage, ctx, job)
	state := legacyByteRecoveryFencePGReady(test, storage, ctx, job)
	private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
	for _, fault := range []string{"plan", "spec", "incarnation", "version", "extent", "header extent", "partial", "negative ordinal", "past header", "past stream"} {
		test.Run(fault, func(test *testing.T) {
			candidate := state
			candidate.encoded = bytes.Clone(state.encoded)
			header, ordinal := true, 0
			switch fault {
			case "plan":
				candidate.stage.plan = domain.HashContent([]byte("unrelated recovery plan"))
			case "spec":
				candidate.encoded[len(candidate.encoded)-1] ^= 1
			case "incarnation":
				candidate.stage.sourceID = "00000000-0000-0000-0000-000000000001"
			case "version":
				candidate.stage.sourceVersion++
			case "extent":
				candidate.stage.blocks++
				candidate.stage.next++
			case "header extent":
				candidate.headerBytes++
			case "partial":
				candidate.stage.complete = false
			case "negative ordinal":
				ordinal = -1
			case "past header":
				ordinal = (state.headerBytes + (64 << 10) - 1) / (64 << 10)
			case "past stream":
				header, ordinal = false, state.stage.blocks
			}
			body, err := storage.readLegacyByteRecoveryPart(ctx, job, candidate, header, ordinal)
			if err == nil || len(body) != 0 {
				test.Fatal("unbound recovery metadata authorized a selected part", fault, len(body), err)
			}
		})
	}
	if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private {
		test.Fatal("forged recovery metadata evicted or rewrote the real checkpoint")
	}
}

func TestLegacyByteRecoveryPGCancellationDoesNotEvictCheckpoint(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 2<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	legacyByteRecoveryFencePGStage(test, storage, ctx, job)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	state := legacyByteRecoveryFencePGReady(test, peer, ctx, job)
	work := legacyByteRecoveryFencePGWork(test, peer, ctx, job)
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
		test.Fatal("cancellation fixture did not enter recovery", progress, err)
	}
	private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
	public := legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture)
	for _, deadline := range []bool{false, true} {
		if err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
			var canceled context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if deadline {
				canceled, cancel = context.WithDeadline(read, time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			} else {
				canceled, cancel = context.WithCancel(read)
				cancel()
			}
			defer cancel()
			if _, ready, err := peer.readLegacyByteRecovery(canceled, job); ready || !errors.Is(err, want) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("ready probe concealed current context failure", ready, err)
			}
			for _, header := range []bool{true, false} {
				body, err := peer.readLegacyByteRecoveryPart(canceled, job, state, header, 0)
				if len(body) != 0 || !errors.Is(err, want) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
					test.Fatal("part read concealed current context failure", header, len(body), err)
				}
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if progress, err := work.Step(canceled, job); progress == outbound.DocRangeWorkPublishable || !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("canceled recovery was classified as damaged checkpoint", progress, err)
	}
	if err := work.Close(); err != nil {
		test.Fatal(err)
	}
	if progress, err := work.Step(ctx, job); err == nil || progress == outbound.DocRangeWorkPublishable {
		test.Fatal("closed recovery handle remained usable", progress, err)
	}
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("canceling and closing recovery retained a local spool", files, err)
	}
	if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private ||
		legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public || trace.bodyReads.Load() != 0 {
		test.Fatal("cancellation changed persisted state or reread original bytes", trace.bodyReads.Load())
	}
}

func TestLegacyByteRecoveryPGCancellationAfterReadCommitReturnsNoBytes(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	legacyByteRecoveryFencePGStage(test, storage, ctx, job)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	state := legacyByteRecoveryFencePGReady(test, peer, ctx, job)
	private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
	public := legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture)
	for _, operation := range []string{"ready", "header", "stream"} {
		current, cancel := context.WithCancel(ctx)
		committed := false
		trace.commit.Store(&docCaptureCommitHook{run: func() {
			committed = true
			trace.commit.Store(nil)
			cancel()
		}})
		if operation == "ready" {
			if _, ready, err := peer.readLegacyByteRecovery(current, job); ready || !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("ready probe ignored cancellation at its authenticated read boundary", ready, err)
			}
		} else {
			body, err := peer.readLegacyByteRecoveryPart(current, job, state, operation == "header", 0)
			if len(body) != 0 || !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("part escaped after its read context was canceled at commit", operation, len(body), err)
			}
		}
		trace.commit.Store(nil)
		cancel()
		if !committed {
			test.Fatal("cancellation did not occur after an authenticated read commit", operation)
		}
	}
	if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private ||
		legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public || trace.bodyReads.Load() != 0 {
		test.Fatal("read-boundary cancellation evicted a cache or changed archival data", trace.bodyReads.Load())
	}
}

type legacyByteRecoveryFencePGConstraint struct {
	table      string
	name       string
	definition string
}

func legacyByteRecoveryFencePGDamage(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, header bool, ordinal int, damage string) {
	test.Helper()
	tables := []string{"doc_legacy_byte_stage_headers_v1"}
	if !header {
		tables = []string{"doc_legacy_byte_stage_objects_v1", "doc_legacy_byte_stage_parts_v1"}
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	var constraints []legacyByteRecoveryFencePGConstraint
	for _, table := range tables {
		rows, err := transaction.Query(ctx, `SELECT conname,pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conrelid=$1::regclass AND contype='c' ORDER BY conname`, table)
		if err != nil {
			test.Fatal(err)
		}
		for rows.Next() {
			constraint := legacyByteRecoveryFencePGConstraint{table: table}
			if err := rows.Scan(&constraint.name, &constraint.definition); err != nil {
				rows.Close()
				test.Fatal(err)
			}
			constraints = append(constraints, constraint)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` DISABLE TRIGGER USER`); err != nil {
			test.Fatal(err)
		}
	}
	for _, constraint := range constraints {
		if _, err := transaction.Exec(ctx, `ALTER TABLE `+pgx.Identifier{constraint.table}.Sanitize()+` DROP CONSTRAINT `+pgx.Identifier{constraint.name}.Sanitize()); err != nil {
			test.Fatal(err)
		}
	}
	var body []byte
	var hash domain.ContentHash
	query := `SELECT hash,bytes FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1 AND ordinal=$2`
	if !header {
		query = `SELECT object.hash,object.bytes FROM doc_legacy_byte_stage_parts_v1 part
 JOIN doc_legacy_byte_stage_objects_v1 object ON object.doc_hash=part.doc_hash AND object.hash=part.hash
 WHERE part.doc_hash=$1 AND part.ordinal=$2`
	}
	if err := transaction.QueryRow(ctx, query, fixture.hash, ordinal).Scan(&hash, &body); err != nil {
		test.Fatal(err)
	}
	if len(body) < 2 {
		test.Fatal("damage fixture has no payload", len(body))
	}
	switch damage {
	case "hash":
		body[0] ^= 1
	case "short":
		body = body[:len(body)-1]
	case "oversized":
		body = append(body, bytes.Repeat([]byte{'x'}, domain.ConversationManifestChunkBytes)...)
	case "self-consistent":
		needle := []byte("x")
		if header {
			needle = []byte("codex")
		}
		position := bytes.Index(body, needle)
		if position < 0 {
			test.Fatal("self-consistent damage cannot preserve JSON shape")
		}
		body[position] = 'z'
	case "missing":
	default:
		test.Fatal("unknown fixture damage", damage)
	}
	if damage == "missing" {
		table := "doc_legacy_byte_stage_headers_v1"
		if !header {
			table = "doc_legacy_byte_stage_parts_v1"
		}
		_, err = transaction.Exec(ctx, `DELETE FROM `+pgx.Identifier{table}.Sanitize()+` WHERE doc_hash=$1 AND ordinal=$2`, fixture.hash, ordinal)
	} else if header {
		changedHash := hash
		if damage == "self-consistent" {
			changedHash = domain.HashContent(body)
		}
		_, err = transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_headers_v1 SET bytes=$3,hash=$4 WHERE doc_hash=$1 AND ordinal=$2`, fixture.hash, ordinal, body, changedHash)
	} else if damage == "self-consistent" {
		changedHash := domain.HashContent(body)
		if _, err = transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_objects_v1(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, changedHash, body); err == nil {
			_, err = transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_parts_v1 SET hash=$3 WHERE doc_hash=$1 AND ordinal=$2`, fixture.hash, ordinal, changedHash)
		}
	} else {
		_, err = transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_objects_v1 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, hash, body)
	}
	if err != nil {
		test.Fatal(err)
	}
	for _, table := range tables {
		if _, err := transaction.Exec(ctx, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` ENABLE TRIGGER USER`); err != nil {
			test.Fatal(err)
		}
	}
	for _, constraint := range constraints {
		if _, err := transaction.Exec(ctx, `ALTER TABLE `+pgx.Identifier{constraint.table}.Sanitize()+` ADD CONSTRAINT `+
			pgx.Identifier{constraint.name}.Sanitize()+` `+constraint.definition+` NOT VALID`); err != nil {
			test.Fatal(err)
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash); err != nil {
			test.Error(err)
		}
		for _, constraint := range constraints {
			if _, err := storage.pool.Exec(cleanup, `ALTER TABLE `+pgx.Identifier{constraint.table}.Sanitize()+` VALIDATE CONSTRAINT `+pgx.Identifier{constraint.name}.Sanitize()); err != nil {
				test.Error(err)
			}
		}
	})
}

func TestLegacyByteRecoveryPGDamagedHeaderAndStreamDiscardOnlyCurrentCache(test *testing.T) {
	for _, header := range []bool{true, false} {
		kind := "stream"
		if header {
			kind = "header"
		}
		for _, damage := range []string{"hash", "short", "oversized", "missing", "self-consistent"} {
			test.Run(kind+"/"+damage, func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				directory := rangeSpoolTempDir(test)
				fixture := legacyByteStagePGSource(test, storage, ctx, 2<<20)
				job := legacyByteStagePGClaim(test, storage, ctx, fixture)
				stage := legacyByteRecoveryFencePGStage(test, storage, ctx, job)
				if _, err := storage.pool.Exec(ctx, `INSERT INTO memories(repo_id,snapshot_id,summary,key_facts,open_tasks,provider)
 VALUES($1,$2,'recovery must preserve memory',ARRAY['archival fact'],ARRAY['pending task'],'codex')`, fixture.repo, fixture.hash); err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() {
					if _, err := storage.pool.Exec(context.WithoutCancel(ctx), `DELETE FROM memories WHERE repo_id=$1 AND snapshot_id=$2`, fixture.repo, fixture.hash); err != nil {
						test.Error(err)
					}
				})
				other := legacyByteStagePGSource(test, storage, ctx, 1<<10)
				otherJob := legacyByteStagePGClaim(test, storage, ctx, other)
				legacyByteRecoveryFencePGStage(test, storage, ctx, otherJob)
				otherPrivate := legacyByteRecoveryFencePGImage(test, storage, ctx, other.hash)
				peer, trace := docCaptureWorkPGStore(test, storage, ctx)
				state := legacyByteRecoveryFencePGReady(test, peer, ctx, job)
				var work outbound.PreparedDocRangeWork
				if damage != "missing" {
					work = legacyByteRecoveryFencePGWork(test, peer, ctx, job)
				}
				ordinal := 0
				if !header {
					if work != nil {
						if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
							test.Fatal("stream corruption fixture did not enter bounded recovery", progress, err)
						}
					}
					ordinal = stage.blocks - 1
				}
				legacyByteRecoveryFencePGDamage(test, storage, ctx, fixture, header, ordinal, damage)
				public := legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture)
				private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
				_, ready, err := peer.readLegacyByteRecovery(ctx, job)
				wantReady := damage == "hash" || damage == "self-consistent"
				if ready != wantReady || wantReady && err != nil || !wantReady && !errors.Is(err, domain.ErrPreparedDocRangeObject) {
					test.Fatal("ready metadata confused exact extent with verified canonical content", damage, ready, err)
				}
				if damage != "self-consistent" {
					body, err := peer.readLegacyByteRecoveryPart(ctx, job, state, header, ordinal)
					if err == nil || len(body) != 0 {
						test.Fatal("damaged complete part escaped size, extent or SHA validation", damage, len(body), err)
					}
					if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private {
						test.Fatal("read-only corruption detection deleted its checkpoint")
					}
				}
				var failure error
				if damage == "missing" {
					work, failure = peer.PrepareDocRangeWork(ctx, job)
					if work != nil {
						defer work.Close()
						test.Fatal("missing completed part created recovery or original-capture work before retry")
					}
				} else {
					for step := 0; step < docCaptureWorkPGPreparationStepLimit; step++ {
						progress, err := work.Step(ctx, job)
						if err != nil {
							failure = err
							break
						}
						if progress != outbound.DocRangeWorkPending {
							test.Fatal("damaged checkpoint became publishable or fell back without retry", damage, progress)
						}
					}
				}
				if !errors.Is(failure, domain.ErrPreparedDocRangeObject) {
					test.Fatal("cache damage did not request the existing prepared-object retry", damage, failure)
				}
				var remaining int
				if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1)`, fixture.hash).Scan(&remaining); err != nil || remaining != 0 {
					test.Fatal("retry retained corrupt current derived storage", remaining, err)
				}
				if legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public ||
					legacyByteRecoveryFencePGImage(test, storage, ctx, other.hash) != otherPrivate || trace.bodyReads.Load() != 0 {
					test.Fatal("cache repair touched the original, memory, job, another cache or source bytes", trace.bodyReads.Load())
				}
				if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
					test.Fatal("damaged recovery advertised published byte readiness", ready, err)
				}
				if work != nil {
					if err := work.Close(); err != nil {
						test.Fatal(err)
					}
				}
				if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
					test.Fatal("failed recovery leaked a private spool", files, err)
				}
				retry, err := peer.PrepareDocRangeWork(ctx, job)
				if retry != nil {
					defer retry.Close()
				}
				if err != nil || retry == nil {
					test.Fatal("discarded checkpoint did not allow normal original-source retry", err)
				}
				if _, captured := retry.(*preparedLegacyCaptureWork); !captured {
					test.Fatalf("missing checkpoint reused a recovery handle instead of original capture: %T", retry)
				}
				if err := retry.Close(); err != nil {
					test.Fatal(err)
				}
				rebuilt := legacyByteRecoveryFencePGStage(test, storage, ctx, job)
				if rebuilt.sourceID != stage.sourceID || rebuilt.sourceVersion != stage.sourceVersion || rebuilt.plan != stage.plan {
					test.Fatal("derived repair changed original authority or canonical plan", rebuilt, stage)
				}
				legacyByteRecoveryFencePGReady(test, storage, ctx, job)
				if legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public {
					test.Fatal("rebuilding a derived checkpoint modified archival data or rejected the valid job")
				}
			})
		}
	}
}

func TestLegacyByteRecoveryPGDiscardRequiresCurrentSourcePlanAndLiveClaim(test *testing.T) {
	for _, fence := range []string{"ownership", "snapshot", "source version", "incarnation", "plan", "lease", "job version", "canceled"} {
		test.Run(fence, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			stage := legacyByteRecoveryFencePGStage(test, storage, ctx, job)
			selected, currentJob, current := stage, job, ctx
			var err error
			switch fence {
			case "ownership":
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.hash)
			case "snapshot":
				_, err = storage.pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.hash)
			case "source version":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
			case "incarnation":
				selected.sourceID = "00000000-0000-0000-0000-000000000001"
			case "plan":
				selected.plan = domain.HashContent([]byte("another source-current recovery plan"))
			case "lease":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash)
			case "job version":
				currentJob.Version++
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				current = canceled
			}
			if err != nil {
				test.Fatal(err)
			}
			private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
			public := legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture)
			if err := storage.discardLegacyByteRecovery(current, currentJob, selected); err == nil ||
				errors.Is(err, domain.ErrPreparedDocRangeObject) || fence == "canceled" && !errors.Is(err, context.Canceled) {
				test.Fatal("discard bypassed a current fence or concealed it as cache damage", fence, err)
			}
			if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private ||
				legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public {
				test.Fatal("rejected discard changed a source, claim, plan or its private storage", fence)
			}
		})
	}
}

func TestLegacyByteRecoveryPGStaleSourceAndClaimCannotEvictReplacement(test *testing.T) {
	for _, replacement := range []string{"source version", "source incarnation", "job claim"} {
		test.Run(replacement, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 2<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			stage := legacyByteRecoveryFencePGStage(test, storage, ctx, job)
			peer, trace := docCaptureWorkPGStore(test, storage, ctx)
			state := legacyByteRecoveryFencePGReady(test, peer, ctx, job)
			work := legacyByteRecoveryFencePGWork(test, peer, ctx, job)
			if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
				test.Fatal("stale handle fixture did not start recovery", progress, err)
			}
			currentJob := job
			switch replacement {
			case "source version":
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
					test.Fatal(err)
				}
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
				if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: fixture.repo, DocHash: fixture.hash,
					DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
					test.Fatal(err)
				}
			case "job claim":
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash); err != nil {
					test.Fatal(err)
				}
				currentJob = docRangeJobPGClaim(test, storage, ctx, fixture.repo, fixture.hash)
				if currentJob.Version <= job.Version {
					test.Fatal("reclaim did not advance its live job fence", job.Version, currentJob.Version)
				}
			}
			fresh := legacyByteRecoveryFencePGStage(test, storage, ctx, currentJob)
			if fresh.plan != stage.plan || fresh.blocks != stage.blocks ||
				replacement == "source version" && (fresh.sourceVersion <= stage.sourceVersion || fresh.sourceID != stage.sourceID) ||
				replacement == "source incarnation" && (fresh.sourceID == stage.sourceID || fresh.sourceVersion != stage.sourceVersion) {
				test.Fatal("replacement did not exercise same-hash and same-plan ABA", fresh, stage)
			}
			currentState := legacyByteRecoveryFencePGReady(test, peer, ctx, currentJob)
			for _, header := range []bool{true, false} {
				body, err := peer.readLegacyByteRecoveryPart(ctx, currentJob, currentState, header, 0)
				want := []byte(fixture.envelope)
				if !header {
					want, _ = legacyByteStagePGBlock(fixture, 0)
				}
				if err != nil || !bytes.Equal(body, want) {
					test.Fatal("current replacement could not authenticate its own bytes", header, err)
				}
			}
			legacyByteRecoveryFencePGDamage(test, storage, ctx, fixture, false, fresh.blocks-1, "hash")
			private := legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash)
			public := legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture)
			for _, header := range []bool{true, false} {
				body, err := peer.readLegacyByteRecoveryPart(ctx, job, state, header, 0)
				if err == nil || len(body) != 0 || errors.Is(err, domain.ErrPreparedDocRangeObject) {
					test.Fatal("stale source or job handle gained replacement authority", header, len(body), err)
				}
			}
			if progress, err := work.Step(ctx, job); err == nil || progress == outbound.DocRangeWorkPublishable || errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("stale recovery mislabeled a fence failure as its own corrupt cache", progress, err)
			}
			if legacyByteRecoveryFencePGImage(test, storage, ctx, fixture.hash) != private ||
				legacyByteRecoveryFencePGPublic(test, storage, ctx, fixture) != public || trace.bodyReads.Load() != 0 {
				test.Fatal("obsolete recovery deleted another source or claim's cache", replacement, trace.bodyReads.Load())
			}
		})
	}
}
