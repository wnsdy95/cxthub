//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyByteStageSourceTrace struct{ reads atomic.Int64 }

func (trace *legacyByteStageSourceTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "source.bytes") {
		trace.reads.Add(1)
	}
	return ctx
}

func (*legacyByteStageSourceTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestLegacyByteStagePGSessionReadsOriginalOnceAndRejectsChangedSource(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 2<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	trace := &legacyByteStageSourceTrace{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	peer := &PostgresStore{pool: pool}
	session, err := peer.prepareLegacyByteBackfill(ctx, job)
	if err != nil || trace.reads.Load() != 1 {
		test.Fatal("session did not verify one bounded original", trace.reads.Load(), err)
	}
	var complete legacyByteStage
	for position := 0; position < session.stage.blocks; position++ {
		complete, err = peer.stageLegacyByteSession(ctx, job, session, 1)
		if err != nil || complete.next != position+1 || complete.complete != (complete.next == complete.blocks) {
			test.Fatal("session did not advance exactly one acknowledged block", complete, err)
		}
	}
	if trace.reads.Load() != 1 {
		test.Fatal("each copy batch reread the full original", trace.reads.Load())
	}
	before := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	if _, err := peer.stageLegacyByteSession(ctx, job, session, 1); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old proof session copied after a physical source change", err)
	}
	_, descriptor := legacyByteStagePGBlock(fixture, 0)
	if err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
		body, err := peer.readLegacyByteStageBlock(read, fixture.repo, fixture.hash, complete, 0, descriptor)
		if len(body) != 0 || err == nil {
			test.Fatal("old private complete bytes survived the source fence", len(body), err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != before {
		test.Fatal("rejected stale session changed the private checkpoint")
	}
	fresh, err := peer.stageLegacyByteBackfill(ctx, job, 1)
	if err != nil || fresh.next != 1 || fresh.complete || fresh.sourceVersion <= complete.sourceVersion || fresh.sourceID != complete.sourceID {
		test.Fatal("fresh proof did not reset the obsolete physical-source checkpoint", fresh, complete, err)
	}
	if trace.reads.Load() != 2 {
		test.Fatal("new physical source did not obtain exactly one fresh proof", trace.reads.Load())
	}
}

func TestLegacyByteStagePGRevocationStaleLeaseAndReadOnlyDoNotWrite(test *testing.T) {
	for _, change := range []string{"ownership", "lease", "job fence", "read only"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 512<<10)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			session, err := storage.prepareLegacyByteBackfill(ctx, job)
			if err != nil {
				test.Fatal(err)
			}
			switch change {
			case "ownership":
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.hash)
			case "lease":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, fixture.hash)
			case "job fence":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET version=version+1,state='waiting',lease_until=NULL WHERE repo_id=$1 AND doc_hash=$2`, fixture.repo, fixture.hash)
			}
			if err != nil {
				test.Fatal(err)
			}
			before := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			work := ctx
			if change == "read only" {
				work = outbound.WithDocReadOnly(work)
			}
			stage, err := storage.stageLegacyByteSession(work, job, session, 1)
			if err == nil || stage != (legacyByteStage{}) {
				test.Fatal("invalid work staged private bytes", stage, err)
			}
			if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != before {
				test.Fatal("rejected work changed the private image")
			}
			if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
				test.Fatal("rejected work changed archival or job state")
			}
		})
	}
}

func TestLegacyByteStagePGFinalLeaseFenceRollsBackCopiedBatch(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.stageLegacyByteSession(ctx, job, session, 1); err != nil {
		test.Fatal(err)
	}
	before := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	name := pgx.Identifier{domain.NewID("legacy_stage_fence_")}.Sanitize()
	statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER INSERT ON doc_legacy_byte_stage_parts_v1 FOR EACH ROW EXECUTE FUNCTION %s()`, name, fixture.hash, fixture.repo, name, name)
	if _, err := storage.pool.Exec(ctx, statement); err != nil {
		test.Fatal(err)
	}
	defer func() {
		if _, err := storage.pool.Exec(context.WithoutCancel(ctx), `DROP TRIGGER IF EXISTS `+name+` ON doc_legacy_byte_stage_parts_v1; DROP FUNCTION IF EXISTS `+name+`() `); err != nil {
			test.Error(err)
		}
	}()
	stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if !errors.Is(err, domain.ErrConflict) || stage != (legacyByteStage{}) {
		test.Fatal("changed lease committed copied bytes", stage, err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != before {
		test.Fatal("failed lease fence left a new CAS object, part or checkpoint")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("failed lease fence committed the injected job change")
	}
}

func TestLegacyByteStagePGObjectsRetainedUntilSourceDeletion(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	if _, err := storage.stageLegacyByteBackfill(ctx, job, 1); err != nil {
		test.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE doc_legacy_byte_stage_objects_v1 SET bytes=bytes WHERE doc_hash=$1`,
		`DELETE FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1`,
		`DELETE FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1`,
	} {
		_, err := storage.pool.Exec(ctx, statement, fixture.hash)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "23514" {
			test.Fatal("live staged content was changed or deleted", statement, err)
		}
	}
	for _, statement := range []string{`DELETE FROM snapshots WHERE doc_hash=$1`, `DELETE FROM repo_blobs WHERE hash=$1`, `DELETE FROM blobs WHERE hash=$1`} {
		if _, err := storage.pool.Exec(ctx, statement, fixture.hash); err != nil {
			test.Fatal(err)
		}
	}
	var remaining int
	if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)`, fixture.hash).Scan(&remaining); err != nil || remaining != 0 {
		test.Fatal("source deletion retained orphan private copies", remaining, err)
	}
}

func TestLegacyByteStagePGReinsertedSameHashRejectsOldSourceProof(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	old, err := storage.stageLegacyByteSession(ctx, job, session, 8)
	if err != nil || !old.complete || old.sourceVersion != 0 {
		test.Fatal("initial source does not exercise a resettable version", old, err)
	}
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
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: fixture.repo, DocHash: fixture.hash, Provider: domain.ProviderCodex,
		DocIdentity: domain.DocumentIdentityLegacy, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	if stage, err := storage.stageLegacyByteSession(ctx, job, session, 1); !errors.Is(err, domain.ErrConflict) || stage != (legacyByteStage{}) {
		test.Fatal("same hash and reset version reused the old original's proof", stage, err)
	}
	_, descriptor := legacyByteStagePGBlock(fixture, 0)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		body, err := storage.readLegacyByteStageBlock(read, fixture.repo, fixture.hash, old, 0, descriptor)
		if err == nil || len(body) != 0 {
			test.Fatal("reinserted source admitted an old selected read", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	fresh, err := storage.stageLegacyByteBackfill(ctx, job, 1)
	if err != nil || !fresh.complete || fresh.sourceVersion != old.sourceVersion || fresh.sourceID == old.sourceID || fresh.plan != old.plan {
		test.Fatal("fresh original incarnation did not obtain an independent proof", fresh, old, err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		body, err := storage.readLegacyByteStageBlock(read, fixture.repo, fixture.hash, old, 0, descriptor)
		if err == nil || len(body) != 0 {
			test.Fatal("new complete cache admitted the old source incarnation", err)
		}
		body, err = storage.readLegacyByteStageBlock(read, fixture.repo, fixture.hash, fresh, 0, descriptor)
		if err != nil || domain.HashContent(body) != descriptor.Hash {
			test.Fatal("new source incarnation lost its own verified bytes", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}
