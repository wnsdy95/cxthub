//go:build postgres

package store

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docToolValuesPGStep(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation, requested *int) domain.DocEventDisplayStep {
	test.Helper()
	raw, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	var result domain.DocEventDisplayStep
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent(raw))
		if err != nil {
			return err
		}
		reader := work.(*postgresDocEventDisplayWork)
		tree, keys, orders := *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders
		result, err = reader.PrepareStep(read, prepared, func(read context.Context, from, length int) ([]byte, error) {
			if err := read.Err(); err != nil {
				return nil, err
			}
			if from < 0 || length <= 0 || length > domain.DocToolValueStepSourceBytes || from > job.Target.Event.Length-length {
				return nil, domain.ErrIntegrity
			}
			*requested += length
			start := job.Target.Event.Offset + from
			return bytes.Clone(fixture.stream[start : start+length]), nil
		}, domain.DocToolPreparationReader{
			Nodes: func(ctx context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
				return reader.ReadToolTreeNodes(ctx, tree, after, limit)
			},
			OrderPlan: func(ctx context.Context, parent int) (domain.DocToolOrderPlan, error) {
				return reader.ReadToolOrderPlan(ctx, tree, keys, orders, parent)
			},
		})
		return err
	}); err != nil {
		test.Fatal(err)
	}
	return result
}

func TestDocToolValuesPGColdPreparationCounts(test *testing.T) {
	for _, count := range []int{100, 1000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture, job, prepared := docToolTreePGFixture(test, count)
			ctx, cancel := context.WithTimeout(context.WithoutCancel(fixture.ctx), 2*time.Minute)
			defer cancel()
			fixture.ctx = ctx
			prepared = completeDocToolTreePG(test, fixture, job, prepared)
			prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
			prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
			if err := fixture.storage.RenewDocDisplay(ctx, job, time.Now(), time.Minute); err != nil {
				test.Fatal(err)
			}
			owner, _ := prepared.Source.Hash()
			var beforeObjects, beforeBytes int64
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(bytes)),0) FROM doc_display_objects_v1 WHERE source_hash=$1`, owner).Scan(&beforeObjects, &beforeBytes); err != nil {
				test.Fatal(err)
			}
			started := time.Now()
			steps, requested := 0, 0
			for prepared.Phase != "tool_values_done" {
				step := docToolValuesPGStep(test, fixture, job, prepared, &requested)
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
			var objects, objectBytes int64
			var raw []byte
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(bytes)),0) FROM doc_display_objects_v1 WHERE source_hash=$1`, owner).Scan(&objects, &objectBytes); err != nil {
				test.Fatal(err)
			}
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, owner, prepared.ToolPlan).Scan(&raw); err != nil {
				test.Fatal(err)
			}
			plan, err := domain.DecodeDocToolValuePlan(prepared.ToolPlan, raw)
			if err != nil || plan.TextBytes <= 0 || steps < 2 {
				test.Fatal("value plan did not complete", err)
			}
			assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
			test.Logf("rows=%d event_bytes=%d value_steps=%d requested_raw_bytes=%d added_objects=%d added_object_bytes=%d value_pages=%d value_text_bytes=%d value_phase_elapsed_ms=%d excludes_indexes_prior_preparation_final_event_policy_and_projection=true", count, job.Target.Event.Length, steps, requested, objects-beforeObjects, objectBytes-beforeBytes, plan.Pages.Count, plan.TextBytes, elapsed.Milliseconds())
		})
	}
}
