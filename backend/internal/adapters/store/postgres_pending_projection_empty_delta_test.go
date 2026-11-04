//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pendingProjectionEmptyDeltaSnapshot struct {
	work, ready, inputs, parts []byte
}

func pendingProjectionEmptyDeltaSnapshotPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) pendingProjectionEmptyDeltaSnapshot {
	t.Helper()
	var snapshot pendingProjectionEmptyDeltaSnapshot
	if err := storage.pool.QueryRow(ctx, `SELECT
		(SELECT to_jsonb(work) FROM pending_projection_work work WHERE repo_id=$1),
		(SELECT to_jsonb(ready) FROM pending_projection_ready ready WHERE repo_id=$1),
		(SELECT COALESCE(jsonb_agg(to_jsonb(input) || jsonb_build_object('xmin',input.xmin::text)
			ORDER BY input.dataset,input.source_seq,input.kind,input.entity_key),'[]'::jsonb)
			FROM pending_projection_inputs input WHERE repo_id=$1),
		(SELECT COALESCE(jsonb_agg(to_jsonb(part) || jsonb_build_object('xmin',part.xmin::text)
			ORDER BY part.dataset,part.source_seq,part.kind,part.entity_key),'[]'::jsonb)
			FROM pending_projection_parts part WHERE repo_id=$1)`, string(repo)).Scan(&snapshot.work, &snapshot.ready, &snapshot.inputs, &snapshot.parts); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func pendingProjectionEmptyDeltaReadyPG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash, domain.PendingProjectionRead) {
	t.Helper()
	storage, ctx, repo, _ := pendingProjectionPG(t)
	job := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	read := pendingProjectionReadPG(t, ctx, storage, job, view)
	if len(read.Parts) == 0 {
		t.Fatal("empty-delta fixture requires published parts")
	}
	return storage, ctx, repo, read
}

func pendingProjectionEmptyDeltaEvidencePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) {
	t.Helper()
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
		VALUES($1,'https://github.com/example/empty-delta','empty-delta-tree','{"stage":"evidence-only"}'::jsonb)`, string(repo))
}

type pendingProjectionEmptyDeltaTracer struct {
	queries   atomic.Int64
	forbidden atomic.Int64
}

func (trace *pendingProjectionEmptyDeltaTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	statement := strings.ToLower(query.SQL)
	if strings.Contains(statement, "pending_projection_inputs") || strings.Contains(statement, "pending_projection_parts") {
		trace.forbidden.Add(1)
	}
	return ctx
}

func (*pendingProjectionEmptyDeltaTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestPGPendingProjectionEmptyDeltaAdvancesReadyWithoutProjection(t *testing.T) {
	storage, ctx, repo, ready := pendingProjectionEmptyDeltaReadyPG(t)
	before := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	pendingProjectionEmptyDeltaEvidencePG(t, ctx, storage, repo)
	stale, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || stale.Ready || stale.Applied != ready.Applied || stale.Source.Sequence != ready.Source.Sequence+1 {
		t.Fatalf("evidence write did not invalidate only the shared source clock: before=%+v after=%+v err=%v", ready, stale, err)
	}
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	if job.Phase != "capture" || job.Cursor != "" || job.AppliedSequence != ready.Applied.Sequence || job.Dataset != ready.Dataset || job.InputSequence != -1 {
		t.Fatalf("evidence-only claim lost ready dataset: %+v", job)
	}
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if page.Mode != "delta" || len(page.Entries) != 0 || page.Checkpoint == nil || page.NextCursor != "" || *page.Checkpoint != stale.Source {
		t.Fatalf("evidence-only change was not a whole empty v1 delta: %+v", page)
	}
	trace := &pendingProjectionEmptyDeltaTracer{}
	writer := docRangeBoundaryPGTracedStore(t, storage, ctx, trace)
	if err := writer.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	if trace.queries.Load() == 0 || trace.forbidden.Load() != 0 {
		t.Fatalf("empty-delta capture read or rewrote input/part tables: queries=%d forbidden=%d", trace.queries.Load(), trace.forbidden.Load())
	}
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || !read.Enabled || !read.Ready || read.Dataset != ready.Dataset || read.Source != *page.Checkpoint || read.Applied != *page.Checkpoint || !reflect.DeepEqual(read.Parts, ready.Parts) {
		t.Fatalf("one capture step did not publish unchanged parts at the new generation: before=%+v after=%+v err=%v", ready, read, err)
	}
	after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(after.inputs, before.inputs) || !reflect.DeepEqual(after.parts, before.parts) {
		t.Fatal("empty delta rewrote input or part row versions")
	}
	wantReady := catalogJSONPG[map[string]any](t, before.ready)
	wantReady["source_seq"] = float64(page.Through)
	if got := catalogJSONPG[map[string]any](t, after.ready); !reflect.DeepEqual(got, wantReady) {
		t.Fatalf("empty delta changed ready metadata beyond its source sequence: got=%+v want=%+v", got, wantReady)
	}
	work, err := scanPendingProjectionWork(storage.pool.QueryRow(ctx, `SELECT `+pendingProjectionWorkColumns+` FROM pending_projection_work work WHERE repo_id=$1`, string(repo)))
	if err != nil || work.Phase != "idle" || work.Dataset != job.Dataset || work.Epoch != job.Epoch || work.Fence != job.Fence ||
		work.AppliedSequence != page.Through || work.InputSequence != page.Through || work.pin != page.Through || work.Cursor != "" ||
		!work.LeaseUntil.IsZero() || work.leased || !work.retained || work.Attempts != 0 {
		t.Fatalf("empty delta did not atomically settle work and retention: %+v err=%v", work, err)
	}
	if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("empty delta scheduled a project step instead of going idle: %v", err)
	}
}

func pendingProjectionEmptyDeltaStagePartialPG(t *testing.T, ctx context.Context, storage *PostgresStore, job domain.PendingProjectionLease) domain.PendingProjectionLease {
	t.Helper()
	page := repositoryViewSourcePagePG(t, ctx, storage, job.RepoID, job.SourceRequest())
	if len(page.Entries) == 0 || page.Checkpoint == nil || page.NextCursor != "" {
		t.Fatalf("empty-tail fixture requires a complete nonempty input page: %+v", page)
	}
	source, err := readPendingProjectionSource(ctx, storage.pool, job.RepoID, false)
	if err != nil {
		t.Fatal(err)
	}
	page.NextCursor = pendingProjectionValidationCursor(t, pendingProjectionWork{PendingProjectionLease: job, pin: source.floor}, source, page)
	page.Checkpoint = nil
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	resumed := pendingProjectionClaimPG(t, ctx, storage, job.RepoID)
	if resumed.Phase != "capture" || resumed.Cursor == "" || resumed.Cursor != page.NextCursor || resumed.Dataset != job.Dataset || resumed.AppliedSequence != job.AppliedSequence || resumed.Fence <= job.Fence {
		t.Fatalf("durable claim lost partial inputs or continuation: before=%+v after=%+v", job, resumed)
	}
	return resumed
}

func TestPGPendingProjectionEmptyDeltaPartialTailStillProjects(t *testing.T) {
	storage, ctx, repo, ready := pendingProjectionEmptyDeltaReadyPG(t)
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{author,name}','"Changed before empty tail"') WHERE repo_id=$1`, string(repo))
	pendingProjectionEmptyDeltaEvidencePG(t, ctx, storage, repo)
	job := pendingProjectionEmptyDeltaStagePartialPG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo))
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if page.Mode != "delta" || len(page.Entries) != 0 || page.Checkpoint == nil || page.NextCursor != "" || job.AppliedSequence != ready.Applied.Sequence {
		t.Fatalf("fixture did not reach an empty final delta tail: job=%+v page=%+v", job, page)
	}
	before := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(after.ready, before.ready) || !reflect.DeepEqual(after.parts, before.parts) || !reflect.DeepEqual(after.inputs, before.inputs) {
		t.Fatal("empty final tail promoted or rewrote a partially captured projection")
	}
	if read, err := storage.ReadPendingProjection(ctx, repo); err != nil || read.Ready || read.Applied != ready.Applied {
		t.Fatalf("empty tail exposed unprojected input changes: %+v err=%v", read, err)
	}
	project := pendingProjectionClaimPG(t, ctx, storage, repo)
	if project.Phase != "project" || project.InputSequence != page.Through || project.AppliedSequence != ready.Applied.Sequence || project.Dataset != job.Dataset || project.Cursor != "" {
		t.Fatalf("empty final tail skipped the pending project step: %+v", project)
	}
	view, parts := pendingProjectionPartsPG(t, ctx, storage, project)
	if len(view.Pending) != 1 || view.Pending[0].Author.Name != "Changed before empty tail" {
		t.Fatalf("partial capture lost its changed pending image: %+v", view)
	}
	if err := storage.PublishPendingProjection(ctx, project, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, project, view)
}

func TestPGPendingProjectionEmptyDeltaRejectsInvalidReadyOrLease(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
		want  error
	}{
		{"rules", `UPDATE pending_projection_ready SET rules=rules+1 WHERE repo_id=$1`, domain.ErrIntegrity},
		{"dataset", `UPDATE pending_projection_ready SET dataset=gen_random_uuid() WHERE repo_id=$1`, domain.ErrIntegrity},
		{"epoch", `UPDATE pending_projection_ready SET source_epoch=gen_random_uuid() WHERE repo_id=$1`, domain.ErrIntegrity},
		{"applied", `UPDATE pending_projection_ready SET source_seq=source_seq-1 WHERE repo_id=$1`, domain.ErrIntegrity},
		{"missing_ready", `DELETE FROM pending_projection_ready WHERE repo_id=$1`, domain.ErrIntegrity},
		{"expired_lease", `UPDATE pending_projection_work SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, domain.ErrRefConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			storage, ctx, repo, _ := pendingProjectionEmptyDeltaReadyPG(t)
			pendingProjectionEmptyDeltaEvidencePG(t, ctx, storage, repo)
			job := pendingProjectionClaimPG(t, ctx, storage, repo)
			page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
			if page.Mode != "delta" || len(page.Entries) != 0 || page.Checkpoint == nil || job.Cursor != "" || job.AppliedSequence < 0 {
				t.Fatalf("guard fixture did not qualify for empty-delta advancement: job=%+v page=%+v", job, page)
			}
			catalogExecPG(t, ctx, storage.pool, test.query, string(repo))
			before := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
			if err := storage.SavePendingProjectionPage(ctx, job, page); !errors.Is(err, test.want) {
				t.Fatalf("invalid %s advanced ready generation: got=%v want=%v", test.name, err, test.want)
			}
			if after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected %s changed ready, work, or materialized rows: before=%+v after=%+v", test.name, before, after)
			}
		})
	}
}

func TestPGPendingProjectionEmptyDeltaBaselineTailStillProjects(t *testing.T) {
	storage, ctx, repo, pending := pendingProjectionPG(t)
	job := pendingProjectionEmptyDeltaStagePartialPG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo))
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if page.Mode != "baseline" || len(page.Entries) != 0 || page.Checkpoint == nil || job.AppliedSequence != -1 {
		t.Fatalf("fixture did not reach an empty final baseline tail: job=%+v page=%+v", job, page)
	}
	before := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	if len(after.ready) != 0 || !reflect.DeepEqual(after.parts, before.parts) || !reflect.DeepEqual(after.inputs, before.inputs) {
		t.Fatal("empty baseline tail fabricated a ready projection")
	}
	project := pendingProjectionClaimPG(t, ctx, storage, repo)
	if project.Phase != "project" || project.InputSequence != page.Through || project.AppliedSequence != -1 || project.Dataset != job.Dataset || project.Cursor != "" {
		t.Fatalf("baseline tail skipped initial projection: %+v", project)
	}
	view, parts := pendingProjectionPartsPG(t, ctx, storage, project)
	if len(view.Pending) != 1 || view.Pending[0].SessionID != pending.SessionID {
		t.Fatalf("baseline tail lost initial pending input: %+v", view)
	}
	if err := storage.PublishPendingProjection(ctx, project, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, project, view)
}

func TestPGPendingProjectionEmptyDeltaWorkFailureRollsBackReady(t *testing.T) {
	storage, ctx, repo, ready := pendingProjectionEmptyDeltaReadyPG(t)
	pendingProjectionEmptyDeltaEvidencePG(t, ctx, storage, repo)
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if page.Mode != "delta" || len(page.Entries) != 0 || page.Checkpoint == nil || job.Cursor != "" || job.AppliedSequence != ready.Applied.Sequence {
		t.Fatalf("rollback fixture did not qualify for empty-delta advancement: job=%+v page=%+v", job, page)
	}
	name := pgx.Identifier{domain.NewID("pending_empty_delta_")}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON pending_projection_work; DROP FUNCTION IF EXISTS %s()`, name, name)); err != nil {
			t.Error(err)
		}
	})
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS(SELECT 1 FROM pending_projection_ready
				WHERE repo_id=NEW.repo_id AND dataset=NEW.dataset AND source_epoch=NEW.source_epoch
				AND source_seq=NEW.applied_seq AND source_seq>OLD.applied_seq) THEN
				RAISE EXCEPTION 'forced empty delta work failure after ready update' USING ERRCODE='P0001';
			END IF;
			RAISE EXCEPTION 'ready did not advance before work transition' USING ERRCODE='P0002';
		END $$`, name))
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON pending_projection_work
		FOR EACH ROW WHEN (NEW.repo_id='%s' AND NEW.phase='idle') EXECUTE FUNCTION %s()`, name, string(repo), name))
	before := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	err := storage.SavePendingProjectionPage(ctx, job, page)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "P0001" || databaseError.Message != "forced empty delta work failure after ready update" {
		t.Fatalf("did not reach the injected failure after ready advanced: %v", err)
	}
	if after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed work transition left ready, work, or rows advanced: before=%+v after=%+v", before, after)
	}
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`DROP TRIGGER %s ON pending_projection_work; DROP FUNCTION %s()`, name, name))
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatalf("rolled-back fast path consumed the valid capture lease: %v", err)
	}
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || !read.Ready || read.Dataset != ready.Dataset || read.Applied != *page.Checkpoint || !reflect.DeepEqual(read.Parts, ready.Parts) {
		t.Fatalf("retry did not publish the unchanged ready projection: %+v err=%v", read, err)
	}
}

func TestPGPendingProjectionEmptyDeltaRevisionImageStillProjects(t *testing.T) {
	storage, ctx, repo, ready := pendingProjectionEmptyDeltaReadyPG(t)
	pendingProjectionEmptyDeltaEvidencePG(t, ctx, storage, repo)
	if err := storage.AdvanceEvidenceRevision(ctx, repo); err != nil {
		t.Fatal(err)
	}
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	page := repositoryViewSourcePagePG(t, ctx, storage, repo, job.SourceRequest())
	if page.Mode != "delta" || len(page.Entries) != 1 || page.Entries[0].Kind != "revision" || page.Checkpoint == nil || job.Cursor != "" {
		t.Fatalf("evidence revision did not produce a real nonempty v1 delta: job=%+v page=%+v", job, page)
	}
	before := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	if err := storage.SavePendingProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(after.ready, before.ready) || !reflect.DeepEqual(after.parts, before.parts) || reflect.DeepEqual(after.inputs, before.inputs) {
		t.Fatal("revision-image capture fast-forwarded ready or failed to persist input")
	}
	project := pendingProjectionClaimPG(t, ctx, storage, repo)
	if project.Phase != "project" || project.InputSequence != page.Through || project.AppliedSequence != ready.Applied.Sequence || project.Dataset != job.Dataset {
		t.Fatalf("nonempty revision delta skipped projection: %+v", project)
	}
	view, parts := pendingProjectionPartsPG(t, ctx, storage, project)
	if err := storage.PublishPendingProjection(ctx, project, parts); err != nil {
		t.Fatal(err)
	}
	pendingProjectionReadPG(t, ctx, storage, project, view)
}
