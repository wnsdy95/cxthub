//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocToolTreePGReviewPopulatedProtocolMigrationRollbackAndPREBytes(test *testing.T) {
	storage, ctx := docMessageDisplayProtocolPG(test)
	samples := []docMessageDisplayProtocolCase{
		{name: "pre_waiting", profile: domain.DocDisplayStringProfile, state: "waiting"},
		{name: "pre_running", profile: domain.DocDisplayStringProfile, state: "running", progress: "checkpoint"},
		{name: "pre_retrying", profile: domain.DocDisplayStringProfile, state: "retrying", progress: "discovery"},
		{name: "pre_completed", profile: domain.DocDisplayStringProfile, state: "completed", progress: "checkpoint"},
		{name: "pre_unsupported", profile: domain.DocDisplayStringProfile, state: "rejected", reason: "unsupported_display", progress: "checkpoint"},
		{name: "pre_rejected", profile: domain.DocDisplayStringProfile, state: "rejected", reason: "invalid_source", progress: "discovery"},
		{name: "generic_waiting_empty", profile: domain.DocDisplayProjectionProfile, state: "waiting"},
		{name: "generic_waiting", profile: domain.DocDisplayProjectionProfile, state: "waiting", progress: "discovery"},
		{name: "generic_running", profile: domain.DocDisplayProjectionProfile, state: "running", progress: "discovery"},
		{name: "generic_retrying", profile: domain.DocDisplayProjectionProfile, state: "retrying", progress: "discovery"},
		{name: "generic_completed", profile: domain.DocDisplayProjectionProfile, state: "completed", progress: "completed"},
		{name: "generic_unsupported_empty", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "unsupported_display"},
		{name: "generic_unsupported", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "unsupported_display", progress: "discovery"},
		{name: "generic_rejected", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "invalid_source", progress: "discovery"},
		{name: "generic_other_unsupported", profile: domain.DocDisplayProjectionProfile, state: "rejected", reason: "unsupported_display_other", progress: "discovery"},
	}
	captured := make(map[string]domain.DocDisplayJob)
	for _, sample := range samples {
		captured[sample.name] = seedDocMessageDisplayProtocolPG(test, storage, ctx, sample)
	}
	historical, err := os.ReadFile("../../../../schemas/db/migrations/0129_doc_message_display.sql")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, string(historical)); err != nil {
		test.Fatal("establish populated protocol 4 schema", err)
	}
	reset, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(reset)
	if _, err := reset.Exec(ctx, `SET LOCAL cxthub.doc_display_protocol='4'`); err != nil {
		test.Fatal(err)
	}
	for _, sample := range samples {
		if sample.profile != domain.DocDisplayProjectionProfile {
			continue
		}
		prior := captured[sample.name]
		var lease any
		if !prior.LeaseUntil.IsZero() {
			lease = prior.LeaseUntil
		}
		tag, err := reset.Exec(ctx, `UPDATE doc_display_jobs_v1 SET state=$2,reason=$3,version=$4,next_attempt=$5,lease_until=$6
 WHERE repo_id=$1 AND profile='public-event-display-v1' AND worker_protocol=4`,
			prior.RepoID, prior.State, prior.Reason, prior.Version, prior.NextAttempt, lease)
		if err != nil || tag.RowsAffected() != 1 {
			test.Fatal("restore protocol 4 status fixture", sample.name, err)
		}
	}
	if err := reset.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	images := make(map[string]string)
	for _, sample := range samples {
		var image string
		if err := storage.pool.QueryRow(ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job WHERE repo_id=$1`, captured[sample.name].RepoID).Scan(&image); err != nil {
			test.Fatal(err)
		}
		images[sample.name] = image
	}
	before := docMessageDisplayProtocolImagePG(test, storage, ctx, true)
	immutable := docMessageDisplayProtocolImagePG(test, storage, ctx, false)
	var constraint string
	if err := storage.pool.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
 WHERE conrelid='doc_display_jobs_v1'::regclass AND conname='doc_display_jobs_v1_worker_protocol_check'`).Scan(&constraint); err != nil {
		test.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0130_doc_tool_tree.sql")
	if err != nil {
		test.Fatal(err)
	}
	rolledBack, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(rolledBack)
	if _, err := rolledBack.Exec(ctx, string(migration)); err != nil {
		test.Fatal("migrate populated protocol 4 schema before rollback", err)
	}
	if err := rolledBack.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if docMessageDisplayProtocolImagePG(test, storage, ctx, true) != before {
		test.Fatal("0130 rollback changed receipt fields, binary progress, immutable objects or protocol guard")
	}
	var schemaRestored bool
	if err := storage.pool.QueryRow(ctx, `SELECT to_regclass('doc_tool_tree_sources_v1') IS NULL
 AND to_regclass('doc_tool_tree_nodes_v1') IS NULL
 AND (SELECT pg_get_constraintdef(oid)=$1 FROM pg_constraint
   WHERE conrelid='doc_display_jobs_v1'::regclass AND conname='doc_display_jobs_v1_worker_protocol_check')`, constraint).Scan(&schemaRestored); err != nil || !schemaRestored {
		test.Fatal("0130 rollback retained private tables or changed the protocol constraint", schemaRestored, err)
	}
	committed, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(committed)
	var started, finished time.Time
	if err := committed.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&started); err != nil {
		test.Fatal(err)
	}
	if _, err := committed.Exec(ctx, string(migration)); err != nil {
		test.Fatal("commit 0130 over populated protocol 4 schema", err)
	}
	if err := committed.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&finished); err != nil {
		test.Fatal(err)
	}
	if err := committed.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	for _, sample := range samples {
		prior := captured[sample.name]
		var unchanged bool
		if sample.profile == domain.DocDisplayStringProfile {
			if err := storage.pool.QueryRow(ctx, `SELECT to_jsonb(job)=$2::jsonb AND worker_protocol=2
 FROM doc_display_jobs_v1 job WHERE repo_id=$1`, prior.RepoID, images[sample.name]).Scan(&unchanged); err != nil || !unchanged {
				test.Fatal("0130 changed a PRE2 receipt or its exact binary progress", sample.name, err)
			}
			continue
		}
		if err := storage.pool.QueryRow(ctx, `SELECT worker_protocol=5
 AND (to_jsonb(job)-ARRAY['worker_protocol','version','state','reason','next_attempt','lease_until'])=
 ($2::jsonb-ARRAY['worker_protocol','version','state','reason','next_attempt','lease_until'])
 FROM doc_display_jobs_v1 job WHERE repo_id=$1`, prior.RepoID, images[sample.name]).Scan(&unchanged); err != nil || !unchanged {
			test.Fatal("0130 rewrote generic preparation bytes, hashes, bindings or unrelated receipt fields", sample.name, err)
		}
		current, err := scanDocDisplayJob(storage.pool.QueryRow(ctx, `SELECT `+docDisplayJobColumns+` FROM doc_display_jobs_v1 WHERE repo_id=$1`, prior.RepoID))
		if err != nil {
			test.Fatal(sample.name, err)
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
			test.Fatal("0130 changed the wrong state or failed to fence a generic receipt", sample.name, prior, current)
		}
		if reopened {
			if current.NextAttempt.Before(started) || current.NextAttempt.After(finished) {
				test.Fatal("compatible unsupported display did not become freshly due", sample.name, current.NextAttempt)
			}
		} else if !current.NextAttempt.Equal(prior.NextAttempt) {
			test.Fatal("0130 changed an unrelated due time", sample.name, prior.NextAttempt, current.NextAttempt)
		}
		if sample.progress != "" {
			prepared, err := readDocEventDisplayLegacyPreparationPG(ctx, storage, current)
			if err != nil {
				test.Fatal("protocol 4 preparation no longer decodes after 0130", sample.name, err)
			}
			encoded, err := prepared.Encode()
			if err != nil {
				test.Fatal(err)
			}
			var stored []byte
			if err := storage.pool.QueryRow(ctx, `SELECT preparation FROM doc_display_jobs_v1 WHERE repo_id=$1`, prior.RepoID).Scan(&stored); err != nil || !bytes.Equal(encoded, stored) {
				test.Fatal("protocol 4 preparation no longer round-trips byte exactly", sample.name, err)
			}
		}
	}
	if docMessageDisplayProtocolImagePG(test, storage, ctx, false) != immutable {
		test.Fatal("0130 rewrote immutable source, object or completed generation bytes")
	}
	stable := docMessageDisplayProtocolImagePG(test, storage, ctx, true)
	for _, sample := range samples {
		if sample.profile != domain.DocDisplayProjectionProfile {
			continue
		}
		stale := captured[sample.name]
		stale.State = "running"
		if err := storage.RenewDocDisplay(ctx, stale, time.Now(), time.Minute); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("captured protocol 4 token renewed after 0130", sample.name, err)
		}
		stale.State = "rejected"
		if err := storage.FinishDocDisplay(ctx, stale, time.Now()); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("captured protocol 4 token finished after 0130", sample.name, err)
		}
	}
	if docMessageDisplayProtocolImagePG(test, storage, ctx, true) != stable {
		test.Fatal("stale protocol 4 tokens mutated populated migrated receipts")
	}
}

func TestDocToolTreeReviewNoDatabaseSealedPreparation(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":[` + strings.Repeat(`{"nested":[0,true,null]},`, 40) + `{"nested":[0,true,null]}]}`)
	prepared, err := domain.NewDocEventDisplayPreparation(domain.DocRangeEntry{
		DocEventIndex: domain.DocEventIndex{Hash: domain.HashContent(raw), Length: len(raw)}, Kind: domain.EventToolResult,
	})
	if err != nil {
		test.Fatal(err)
	}
	objects := make(map[domain.ContentHash][]byte)
	nodes := make(map[int]domain.DocToolTreeNode)
	treeReadBytes := 0
	for attempts := 0; attempts < 1000; attempts++ {
		if prepared.Phase == "tool_tree_done" {
			if prepared.ToolTree == nil || prepared.Generation != "" || len(nodes) != 206 {
				test.Fatal("private preparation lost nodes or became public", prepared.Phase, len(nodes))
			}
			root, present := nodes[prepared.ToolTree.Root.From]
			if !present || root.Parent != -1 || root.Children != 41 {
				test.Fatal("root node does not cover every nested candidate", root)
			}
			for _, node := range nodes {
				if node.Parent == -1 {
					continue
				}
				parent, present := nodes[node.Parent]
				if !present || parent.Depth+1 != node.Depth || node.Ordinal >= parent.Children {
					test.Fatal("sealed nested nodes omitted their parent or ordinal", node)
				}
			}
			test.Logf("selected tree bytes=%d; requested tree source bytes=%d", prepared.ToolTree.Root.To-prepared.ToolTree.Root.From, treeReadBytes)
			return
		}
		step, err := domain.StepDocEventDisplay(context.Background(), prepared, func(_ context.Context, offset, length int) ([]byte, error) {
			if offset < 0 || length <= 0 || length > domain.DocFieldDiscoveryStepBytes || offset > len(raw)-length {
				return nil, domain.ErrIntegrity
			}
			if prepared.Phase == "tool_tree" {
				treeReadBytes += length
			}
			return bytes.Clone(raw[offset : offset+length]), nil
		}, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
			encoded, present := objects[hash]
			if !present {
				return nil, domain.ErrIntegrity
			}
			return bytes.Clone(encoded), nil
		})
		if err != nil {
			test.Fatal("in-memory private preparation failed", prepared.Phase, err)
		}
		if _, scan, found := step.ToolTreeScan(); found {
			if len(scan.Nodes()) > domain.DocToolTreeStepNodes || len(scan.Objects()) > domain.DocToolTreeStepObjects {
				test.Fatal("sealed scanner exceeded a batch bound")
			}
			for _, node := range scan.Nodes() {
				if _, present := nodes[node.Value.From]; present {
					test.Fatal("single-pass scanner emitted the same node twice", node.Value.From)
				}
				nodes[node.Value.From] = node
			}
		}
		for _, object := range step.Objects() {
			if prior, present := objects[object.Hash]; present && !bytes.Equal(prior, object.Bytes) {
				test.Fatal("immutable object changed during preparation")
			}
			objects[object.Hash] = object.Bytes
		}
		prepared = step.Preparation()
	}
	test.Fatal("in-memory private preparation did not converge")
}

func docToolTreeReviewEvent(test *testing.T, size int) domain.CIREvent {
	test.Helper()
	values := make([]any, size)
	for index := range values {
		values[index] = map[string]any{"nested": []any{index, true, nil}}
	}
	return domain.CIREvent{Kind: domain.EventToolResult, Seq: 1, CallID: test.Name(), Output: values}
}

func docToolTreeReviewStart(test *testing.T, event domain.CIREvent) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	fixture := docDisplayPG(test, event, domain.CIREvent{Kind: domain.EventMessage, Seq: 2, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: test.Name()}}})
	enqueueDocEventDisplayQueue(test, fixture, 0)
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	for attempts := 0; attempts < 100; attempts++ {
		if prepared.Phase == "tool_tree" {
			return fixture, job, prepared
		}
		step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	test.Fatal("tool discovery did not reach the private tree phase")
	return fixture, job, prepared
}

func docToolTreeReviewFinalStep(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) domain.DocEventDisplayStep {
	test.Helper()
	for attempts := 0; attempts < 1000; attempts++ {
		step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
		if step.Preparation().Phase == "tool_tree_done" {
			return step
		}
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	test.Fatal("private tree preparation did not converge")
	return domain.DocEventDisplayStep{}
}

func docToolTreeReviewImage(test *testing.T, fixture docDisplayPGFixture) string {
	test.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'outer',$1::text,
 'tree_sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY scope) FROM doc_tool_tree_sources_v1 source
   WHERE source_hash IN (SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=$2)),
 'tree_nodes',(SELECT jsonb_agg(to_jsonb(node) ORDER BY node.scope,node.node_from) FROM doc_tool_tree_nodes_v1 node
   JOIN doc_tool_tree_sources_v1 source ON source.scope=node.scope
   WHERE source.source_hash IN (SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash=$2)))::text`,
		fixture.image(test), fixture.targets[0].Event.Hash).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func docToolTreeReviewSharedCleanup(test *testing.T, first, second docDisplayPGFixture) {
	test.Helper()
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := first.storage.pool.Exec(cleanup, `DELETE FROM doc_display_jobs_v1 WHERE repo_id IN ($1,$2)`, first.repo, second.repo); err != nil {
			test.Error(err)
		}
	})
}

func TestDocToolTreePGReviewPrivateCompletionDoesNotPublish(test *testing.T) {
	fixture, job, prepared := docToolTreeReviewStart(test, docToolTreeReviewEvent(test, 24))
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	source, scan, found := step.ToolTreeScan()
	if !found || scan.State().Done() || len(scan.Nodes()) == 0 || len(scan.Nodes()) > domain.DocToolTreeStepNodes {
		test.Fatal("fixture did not yield a bounded partial node batch")
	}
	prepared, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil {
		test.Fatal(err)
	}
	scope, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	var private bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state='scanning' AND node_count IS NULL
 FROM doc_tool_tree_sources_v1 WHERE scope=$1`, scope).Scan(&private); err != nil || !private {
		test.Fatal("partial node rows were marked complete", private, err)
	}
	final := docToolTreeReviewFinalStep(test, fixture, job, prepared)
	prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, final)
	if err != nil {
		test.Fatal(err)
	}
	_, finalScan, found := final.ToolTreeScan()
	if !found {
		test.Fatal("final preparation omitted its sealed scanner step")
	}
	var count, rows int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT node_count,
 (SELECT count(*) FROM doc_tool_tree_nodes_v1 WHERE scope=$1)
 FROM doc_tool_tree_sources_v1 WHERE scope=$1 AND state='complete'`, scope).Scan(&count, &rows); err != nil || count != finalScan.State().NodeCount || rows != count {
		test.Fatal("complete source does not contain exactly the scanned node count", count, rows, err)
	}
	before := docToolTreeReviewImage(test, fixture)
	encoded, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, domain.HashContent(encoded), time.Now()); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("private tree completion published a public generation", err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		_, err := fixture.storage.ReadDocEventDisplayGeneration(read, job.Target)
		return err
	}); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("public read exposed private tree preparation", err)
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
	if docToolTreeReviewImage(test, fixture) != before {
		test.Fatal("failed publication or public read changed private preparation")
	}
}

func TestDocToolTreePGReviewFinalRowsFailClosed(test *testing.T) {
	for _, mutation := range []string{"missing_node", "missing_parent", "ordinal_gap"} {
		test.Run(mutation, func(test *testing.T) {
			fixture, job, prepared := docToolTreeReviewStart(test, docToolTreeReviewEvent(test, 12))
			final := docToolTreeReviewFinalStep(test, fixture, job, prepared)
			source, _, found := final.ToolTreeScan()
			if !found {
				test.Fatal("missing sealed final scanner step")
			}
			scope, err := source.Hash()
			if err != nil {
				test.Fatal(err)
			}
			statement := `DELETE FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND node_from=(
 SELECT min(node_from) FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND depth>1)`
			if mutation == "missing_parent" {
				statement = `UPDATE doc_tool_tree_nodes_v1 SET parent_from=$2::bigint+2 WHERE scope=$1 AND node_from=(
 SELECT min(node_from) FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND depth>1)`
			} else if mutation == "ordinal_gap" {
				statement = `UPDATE doc_tool_tree_nodes_v1 SET ordinal=100000 WHERE scope=$1 AND node_from=(
 SELECT min(node_from) FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND depth>1)`
			}
			arguments := []any{scope}
			if mutation == "missing_parent" {
				arguments = append(arguments, source.Root.From)
			}
			var tag pgconn.CommandTag
			if mutation == "missing_node" {
				tag, err = execDocToolCatalogPGWithCorruptDeletes(test, fixture, job, []string{"doc_tool_tree_nodes_v1"}, statement, arguments...)
			} else {
				tag, err = execDocToolCatalogPG(fixture, job, statement, arguments...)
			}
			if err != nil || tag.RowsAffected() != 1 {
				test.Fatal("failed to corrupt exactly one previously saved node", tag.RowsAffected(), err)
			}
			before := docToolTreeReviewImage(test, fixture)
			result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, final)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) {
				test.Fatal("incomplete node graph became complete", result, err)
			}
			if docToolTreeReviewImage(test, fixture) != before {
				test.Fatal("failed completion leaked final nodes, objects or checkpoint")
			}
		})
	}
}

func TestDocToolTreePGReviewSameSourceConcurrentReplay(test *testing.T) {
	event := docToolTreeReviewEvent(test, 12)
	first, firstJob, firstPrepared := docToolTreeReviewStart(test, event)
	second, secondJob, secondPrepared := docToolTreeReviewStart(test, event)
	docToolTreeReviewSharedCleanup(test, first, second)
	firstStep := docEventDisplayBoundaryStep(test, first, firstJob, firstPrepared)
	secondStep := docEventDisplayBoundaryStep(test, second, secondJob, secondPrepared)
	source, scan, found := firstStep.ToolTreeScan()
	if !found {
		test.Fatal("missing scanner step")
	}
	secondSource, _, found := secondStep.ToolTreeScan()
	if !found || secondSource != source {
		test.Fatal("same event did not reuse one source-global tree scope")
	}
	results := make(chan error, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, operation := range []struct {
		fixture docDisplayPGFixture
		job     domain.DocDisplayJob
		step    domain.DocEventDisplayStep
	}{{first, firstJob, firstStep}, {second, secondJob, secondStep}} {
		workers.Go(func() {
			<-start
			err := retryDocDisplayCatalogBusyPG(operation.fixture.ctx, func(retry context.Context) error {
				_, err := operation.fixture.storage.SaveDocEventDisplay(retry, operation.job, operation.step)
				return err
			})
			results <- err
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			test.Fatal("same-source jobs conflicted on identical private nodes", err)
		}
	}
	scope, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	var count int
	if err := first.storage.pool.QueryRow(first.ctx, `SELECT count(*) FROM doc_tool_tree_nodes_v1 WHERE scope=$1`, scope).Scan(&count); err != nil || count != len(scan.Nodes()) {
		test.Fatal("same-source replay duplicated node facts", count, err)
	}
	for _, operation := range []struct {
		fixture  docDisplayPGFixture
		job      domain.DocDisplayJob
		prepared domain.DocEventDisplayPreparation
	}{{first, firstJob, firstStep.Preparation()}, {second, secondJob, secondStep.Preparation()}} {
		final := docToolTreeReviewFinalStep(test, operation.fixture, operation.job, operation.prepared)
		if _, err := operation.fixture.storage.SaveDocEventDisplay(operation.fixture.ctx, operation.job, final); err != nil {
			test.Fatal("completed source refused an exact duplicate replay", err)
		}
		assertDocEventDisplayBoundaryUnpublished(test, operation.fixture, operation.job)
	}
}

func TestDocToolTreePGReviewCompletedSourceRejectsNewNodes(test *testing.T) {
	event := docToolTreeReviewEvent(test, 12)
	first, firstJob, firstPrepared := docToolTreeReviewStart(test, event)
	second, secondJob, secondPrepared := docToolTreeReviewStart(test, event)
	docToolTreeReviewSharedCleanup(test, first, second)
	final := docToolTreeReviewFinalStep(test, first, firstJob, firstPrepared)
	if _, err := first.storage.SaveDocEventDisplay(first.ctx, firstJob, final); err != nil {
		test.Fatal(err)
	}
	step := docEventDisplayBoundaryStep(test, second, secondJob, secondPrepared)
	source, scan, found := step.ToolTreeScan()
	if !found || len(scan.Nodes()) == 0 {
		test.Fatal("missing replay node batch")
	}
	scope, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	tag, err := execDocToolCatalogPGWithCorruptDeletes(test, second, secondJob, []string{"doc_tool_tree_nodes_v1"}, `DELETE FROM doc_tool_tree_nodes_v1 WHERE scope=$1 AND node_from=$2`, scope, scan.Nodes()[0].Value.From)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("failed to remove a node from the completed source", err)
	}
	before := docToolTreeReviewImage(test, second)
	if _, err := second.storage.SaveDocEventDisplay(second.ctx, secondJob, step); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("complete source accepted a previously absent node", err)
	}
	if docToolTreeReviewImage(test, second) != before {
		test.Fatal("complete-source replay inserted nodes or advanced the losing job")
	}
}

func TestDocToolTreePGReviewImmutableCollisionRollsBack(test *testing.T) {
	event := docToolTreeReviewEvent(test, 12)
	first, firstJob, firstPrepared := docToolTreeReviewStart(test, event)
	second, secondJob, secondPrepared := docToolTreeReviewStart(test, event)
	docToolTreeReviewSharedCleanup(test, first, second)
	firstStep := docEventDisplayBoundaryStep(test, first, firstJob, firstPrepared)
	if _, err := first.storage.SaveDocEventDisplay(first.ctx, firstJob, firstStep); err != nil {
		test.Fatal(err)
	}
	step := docEventDisplayBoundaryStep(test, second, secondJob, secondPrepared)
	source, scan, found := step.ToolTreeScan()
	if !found || len(scan.Nodes()) == 0 {
		test.Fatal("missing replay node batch")
	}
	scope, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	tag, err := execDocToolCatalogPG(second, secondJob, `UPDATE doc_tool_tree_nodes_v1 SET bytes=$3,node_hash=$4 WHERE scope=$1 AND node_from=$2`,
		scope, scan.Nodes()[0].Value.From, []byte("{}"), domain.HashContent([]byte("{}")))
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("failed to poison a duplicate node", err)
	}
	before := docToolTreeReviewImage(test, second)
	if _, err := second.storage.SaveDocEventDisplay(second.ctx, secondJob, step); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("immutable node collision did not fail closed", err)
	}
	if docToolTreeReviewImage(test, second) != before {
		test.Fatal("immutable collision leaked another node or preparation")
	}
}

func TestDocToolTreePGReviewLateLeaseRollback(test *testing.T) {
	fixture, job, prepared := docToolTreeReviewStart(test, docToolTreeReviewEvent(test, 1))
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	source, scan, found := step.ToolTreeScan()
	if !found || len(scan.Nodes()) == 0 {
		test.Fatal("missing node batch")
	}
	scope, err := source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	docDisplayPGDelay(test, fixture, "doc_tool_tree_nodes_v1", "scope", scope)
	if _, err := docDisplayLeaseMutationPG(fixture.ctx, fixture.storage, job.RepoID, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()+interval '80 milliseconds' WHERE `+docDisplayKey(job.Target),
		job.RepoID, job.DocHash, job.Target.Event.Index); err != nil {
		test.Fatal(err)
	}
	before := docToolTreeReviewImage(test, fixture)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("late lease expiry committed private node facts", err)
	}
	if docToolTreeReviewImage(test, fixture) != before {
		test.Fatal("late lease expiry leaked source, nodes, objects or checkpoint")
	}
}
