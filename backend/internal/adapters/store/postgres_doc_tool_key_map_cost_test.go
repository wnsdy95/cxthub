//go:build postgres

package store

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docToolKeyMapCostStep(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) domain.DocEventDisplayStep {
	test.Helper()
	encoded, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	var result domain.DocEventDisplayStep
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		reader, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent(encoded))
		if err != nil {
			return err
		}
		nodes, supported := reader.(outbound.DocToolTreeWorkReader)
		if !supported {
			return domain.ErrDocFindDisplayUnavailable
		}
		result, err = reader.PrepareStep(read, prepared, func(ctx context.Context, from, length int) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if from < 0 || length <= 0 || length > domain.DocFieldDiscoveryStepBytes || from > job.Target.Event.Length-length {
				return nil, domain.ErrIntegrity
			}
			start := job.Target.Event.Offset + from
			return bytes.Clone(fixture.stream[start : start+length]), nil
		}, domain.DocToolPreparationReader{Nodes: func(ctx context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
			return nodes.ReadToolTreeNodes(ctx, *prepared.ToolTree, after, limit)
		}})
		return err
	}); err != nil {
		test.Fatal(err)
	}
	return result
}

func TestDocToolKeyMapPGColdPreparationCounts(test *testing.T) {
	for _, count := range []int{100, 1000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture, job, prepared := docToolTreePGFixture(test, count)
			ctx, cancel := context.WithTimeout(context.WithoutCancel(fixture.ctx), 2*time.Minute)
			defer cancel()
			fixture.ctx = ctx
			prepared = completeDocToolTreePG(test, fixture, job, prepared)
			owner, _ := prepared.Source.Hash()
			var objectsBefore, bytesBefore int64
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(bytes)),0)
 FROM doc_display_objects_v1 WHERE source_hash=$1`, owner).Scan(&objectsBefore, &bytesBefore); err != nil {
				test.Fatal(err)
			}
			steps, rawReads, items := 0, 0, 0
			started := time.Now()
			for prepared.Phase != "tool_keys_done" {
				step := docToolKeyMapCostStep(test, fixture, job, prepared)
				if _, mapped, found := step.ToolKeyMap(); found {
					rawReads += mapped.ReadBytes()
					items += len(mapped.Items())
					if len(mapped.Objects()) != 0 {
						test.Fatal("short-key preparation retained string plans")
					}
				}
				var err error
				prepared, err = fixture.storage.SaveDocEventDisplay(ctx, job, step)
				if err != nil {
					test.Fatal(err)
				}
				steps++
				if steps%100 == 0 {
					if err := fixture.storage.RenewDocDisplay(ctx, job, time.Now(), time.Minute); err != nil {
						test.Fatal(err)
					}
				}
			}
			elapsed := time.Since(started)
			scope, _ := prepared.ToolKeys.Hash(*prepared.ToolTree)
			var observations, members, observationBytes, memberBytes, objectCount, objectBytes int64
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_tool_key_observations_v1 WHERE scope=$1),
 (SELECT member_count FROM doc_tool_key_sources_v1 WHERE scope=$1 AND state='complete'),
 (SELECT COALESCE(sum(pg_column_size(observation)),0) FROM doc_tool_key_observations_v1 observation WHERE scope=$1),
 (SELECT COALESCE(sum(pg_column_size(member)),0) FROM doc_tool_key_members_v1 member WHERE scope=$1),
 (SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$2),
 (SELECT COALESCE(sum(octet_length(bytes)),0) FROM doc_display_objects_v1 WHERE source_hash=$2)`, scope, owner).
				Scan(&observations, &members, &observationBytes, &memberBytes, &objectCount, &objectBytes); err != nil {
				test.Fatal(err)
			}
			if observations != int64(items) || members != observations || observations != int64(count*3+2) {
				test.Fatal("cold preparation lost scoped keys", observations, members, items)
			}
			assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
			test.Logf("rows=%d event_bytes=%d tree_nodes=%d key_occurrences=%d members=%d key_steps=%d key_raw_requested=%d added_objects=%d added_object_bytes=%d observation_composite_bytes=%d member_composite_bytes=%d key_phase_elapsed_ms=%d excludes_indexes_pages_tree_storage_and_final_render=true", count, job.Target.Event.Length, prepared.ToolKeys.Nodes, observations, members, steps, rawReads, objectCount-objectsBefore, objectBytes-bytesBefore, observationBytes, memberBytes, elapsed.Milliseconds())
		})
	}
}
