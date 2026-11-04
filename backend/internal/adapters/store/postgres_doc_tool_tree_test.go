//go:build postgres

package store

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docToolTreePGFixture(test *testing.T, count int) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	values := make([]any, count)
	for index := range values {
		values[index] = map[string]any{"ordinal": index, "text": fmt.Sprintf("PUBLIC_BODY_%04d", index), "nested": []any{true, nil, []any{index, "text"}}}
	}
	fixture := docDisplayPG(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: "tree", Output: map[string]any{"rows": values, "tail": "after"}})
	target := fixture.targets[0]
	target.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now()); err != nil {
		test.Fatal(err)
	}
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	for prepared.Phase == "tool_discovery" {
		step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	if prepared.Phase != "tool_tree" || prepared.ToolTree == nil {
		test.Fatal("tool discovery did not select a source-bound tree", prepared.Phase)
	}
	return fixture, job, prepared
}

func docToolTreePGImage(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) string {
	test.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'jobs',(SELECT jsonb_agg(to_jsonb(job) ORDER BY repo_id,doc_hash,event_index,profile) FROM doc_display_jobs_v1 job WHERE repo_id=$1),
 'sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY source_hash) FROM doc_display_sources_v1 source WHERE event_hash=$2),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY object.source_hash,hash) FROM doc_display_objects_v1 object
   JOIN doc_display_sources_v1 source USING(source_hash) WHERE source.event_hash=$2),
 'trees',(SELECT jsonb_agg(to_jsonb(tree) ORDER BY scope) FROM doc_tool_tree_sources_v1 tree
   JOIN doc_display_sources_v1 source USING(source_hash) WHERE source.event_hash=$2),
 'nodes',(SELECT jsonb_agg(to_jsonb(node) ORDER BY scope,node_from) FROM doc_tool_tree_nodes_v1 node
   JOIN doc_tool_tree_sources_v1 tree USING(scope) JOIN doc_display_sources_v1 source USING(source_hash) WHERE source.event_hash=$2)
 )::text`, job.RepoID, job.Target.Event.Hash).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func TestDocToolTreePGActualQueueRestartAndNoPrematurePublication(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 60)
	seen := make(map[int]domain.DocToolTreeNode)
	steps := 0
	for prepared.Phase == "tool_tree" {
		step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
		source, scan, found := step.ToolTreeScan()
		if !found || scan.ReadBytes() > domain.DocFieldDiscoveryStepBytes || scan.Prior() != prepared.Active || source != *prepared.ToolTree {
			test.Fatal("tree scan lost its sealed source or read budget")
		}
		for _, node := range scan.Nodes() {
			if _, duplicate := seen[node.Value.From]; duplicate {
				test.Fatal("tree scan re-emitted a node across steps")
			}
			seen[node.Value.From] = node
		}
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal("actual tree save failed", steps, err)
		}
		steps++
		assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
		loaded, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
		if err != nil || !reflect.DeepEqual(loaded, prepared) {
			test.Fatal("stored preparation did not rehydrate", err)
		}
		prepared = loaded
	}
	if prepared.Phase != "tool_tree_done" || len(seen) < 100 || steps < 5 {
		test.Fatal("tree did not reach the private completed stage", prepared.Phase, len(seen), steps)
	}
	scope, _ := prepared.ToolTree.Hash()
	var count int
	var phase string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,node_count FROM doc_tool_tree_sources_v1 WHERE scope=$1`, scope).Scan(&phase, &count); err != nil || phase != "complete" || count != len(seen) {
		test.Fatal("private completion marker disagrees", phase, count, len(seen), err)
	}
	rows, err := fixture.storage.pool.Query(fixture.ctx, `SELECT node_hash,bytes FROM doc_tool_tree_nodes_v1 WHERE scope=$1 ORDER BY node_from`, scope)
	if err != nil {
		test.Fatal(err)
	}
	read := 0
	for rows.Next() {
		var hash domain.ContentHash
		var raw []byte
		if err := rows.Scan(&hash, &raw); err != nil {
			rows.Close()
			test.Fatal(err)
		}
		node, err := domain.DecodeDocToolTreeNode(*prepared.ToolTree, hash, raw)
		if err != nil || !reflect.DeepEqual(node, seen[node.Value.From]) || bytes.Contains(raw, []byte("PUBLIC_BODY")) {
			rows.Close()
			test.Fatal("stored node lost canonical provenance or retained body", err)
		}
		read++
	}
	if err := rows.Err(); err != nil || read != len(seen) {
		rows.Close()
		test.Fatal(read, err)
	}
	rows.Close()
	before := docToolTreePGImage(test, fixture, job)
	encoded, _ := prepared.Encode()
	if err := fixture.storage.CompleteDocEventDisplay(fixture.ctx, job, domain.HashContent(encoded), time.Now()); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("unformatted tree was accepted as a display generation", err)
	}
	if after := docToolTreePGImage(test, fixture, job); after != before {
		test.Fatal("rejected display publication changed the private stage")
	}
	test.Logf("private_nodes=%d scan_steps=%d raw_event_bytes=%d", len(seen), steps, job.Target.Event.Length)
}

func TestDocToolTreePGConcurrentSaveAndLostAcknowledgement(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 12)
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	type outcome struct {
		state domain.DocEventDisplayPreparation
		err   error
	}
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			state, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
			results <- outcome{state, err}
		})
	}
	workers.Wait()
	close(results)
	committed := 0
	for result := range results {
		if result.err == nil {
			committed++
		} else if !errors.Is(result.err, domain.ErrConflict) {
			test.Fatal(result.err)
		}
	}
	if committed != 1 {
		test.Fatal("concurrent sealed step did not CAS exactly once", committed)
	}
	before := docToolTreePGImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("lost-ack retry reused its stale predecessor", err)
	}
	if docToolTreePGImage(test, fixture, job) != before {
		test.Fatal("stale retry changed private nodes or frame objects")
	}
	loaded, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil || loaded.Active != step.Preparation().Active {
		test.Fatal("lost-ack recovery did not read committed progress", err)
	}
}

func TestDocToolTreePGSaveRollbackAndScopeFence(test *testing.T) {
	fixture, job, prepared := docToolTreePGFixture(test, 20)
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	if _, scan, found := step.ToolTreeScan(); !found || len(scan.Nodes()) == 0 {
		test.Fatal("fixture requires nodes in its first scan batch")
	}
	var schema string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		test.Fatal(err)
	}
	function := pgx.Identifier{schema, domain.NewID("fail_tree_")}.Sanitize()
	trigger := pgx.Identifier{domain.NewID("tree_failure_")}.Sanitize()
	table := pgx.Identifier{schema, "doc_tool_tree_nodes_v1"}.Sanitize()
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'injected node storage failure'; END $$;
 CREATE TRIGGER `+trigger+` BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+function+`() `); err != nil {
		test.Fatal(err)
	}
	cleanup := func() {
		if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TRIGGER IF EXISTS `+trigger+` ON `+table+`; DROP FUNCTION IF EXISTS `+function+`() `); err != nil {
			test.Error(err)
		}
	}
	test.Cleanup(cleanup)
	before := docToolTreePGImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); err == nil {
		test.Fatal("injected storage failure did not abort the save")
	}
	if docToolTreePGImage(test, fixture, job) != before {
		test.Fatal("failure advanced progress or left partial node/frame/source rows")
	}
	cleanup()
	for _, changed := range []func(*domain.DocDisplayJob){
		func(job *domain.DocDisplayJob) { job.Version++ },
		func(job *domain.DocDisplayJob) { job.Target.Event.Hash = domain.HashContent(nil) },
	} {
		forged := job
		changed(&forged)
		if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, forged, step); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("wrong source or lease mutated a stage", err)
		}
	}
	if docToolTreePGImage(test, fixture, job) != before {
		test.Fatal("scope failure changed the stage")
	}
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); err != nil {
		test.Fatal("valid retry after rollback did not commit", err)
	}
}

func TestDocToolTreePGColdPreparationCostCounts(test *testing.T) {
	for _, count := range []int{100, 1000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture, job, prepared := docToolTreePGFixture(test, count)
			steps, rawReads, writtenObjects, writtenBytes := 0, 0, 0, 0
			for prepared.Phase == "tool_tree" {
				step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
				_, scan, found := step.ToolTreeScan()
				if !found {
					test.Fatal("missing sealed scanner metadata")
				}
				rawReads += scan.ReadBytes()
				for _, object := range step.Objects() {
					writtenObjects++
					writtenBytes += len(object.Bytes)
				}
				var err error
				prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
				if err != nil {
					test.Fatal(err)
				}
				steps++
				if steps%100 == 0 {
					if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), time.Minute); err != nil {
						test.Fatal(err)
					}
				}
			}
			scope, _ := prepared.ToolTree.Hash()
			owner, _ := prepared.Source.Hash()
			var nodes, nodeBytes, objectCount, objectBytes int64
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 (SELECT count(*) FROM doc_tool_tree_nodes_v1 WHERE scope=$1),
 (SELECT COALESCE(sum(octet_length(bytes)),0) FROM doc_tool_tree_nodes_v1 WHERE scope=$1),
 (SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$2),
 (SELECT COALESCE(sum(octet_length(bytes)),0) FROM doc_display_objects_v1 WHERE source_hash=$2)`, scope, owner).Scan(&nodes, &nodeBytes, &objectCount, &objectBytes); err != nil {
				test.Fatal(err)
			}
			test.Logf("input_rows=%d event_bytes=%d root_bytes=%d nodes=%d node_metadata_bytes=%d retained_objects=%d object_bytes=%d steps=%d raw_requested=%d write_attempts=%d written_object_bytes=%d excludes_pg_row_index_overhead_and_final_render=true", count, job.Target.Event.Length, prepared.ToolTree.Root.To-prepared.ToolTree.Root.From, nodes, nodeBytes, objectCount, objectBytes, steps, rawReads, writtenObjects, writtenBytes)
		})
	}
}
