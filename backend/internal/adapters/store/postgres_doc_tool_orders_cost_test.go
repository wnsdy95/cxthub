//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDocToolOrdersPGColdPreparationCost(test *testing.T) {
	for _, count := range []int{100, 1000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			fixture, job, prepared := docToolTreePGFixture(test, count)
			ctx, cancel := context.WithTimeout(context.WithoutCancel(fixture.ctx), 2*time.Minute)
			defer cancel()
			fixture.ctx = ctx
			prepared = completeDocToolTreePG(test, fixture, job, prepared)
			prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
			owner, _ := prepared.Source.Hash()
			var beforeObjects, beforeBytes int64
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(bytes)),0)
 FROM doc_display_objects_v1 WHERE source_hash=$1`, owner).Scan(&beforeObjects, &beforeBytes); err != nil {
				test.Fatal(err)
			}
			started := time.Now()
			steps, plans := 0, 0
			for prepared.Phase != "tool_orders_done" {
				step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
				if _, order, found := step.ToolOrders(); found {
					if _, complete := order.Plan(); complete {
						plans++
					}
				}
				var err error
				prepared, err = fixture.storage.SaveDocEventDisplay(ctx, job, step)
				if err != nil {
					var failure *pgconn.PgError
					if errors.As(err, &failure) {
						test.Log("failed step", steps, "database detail", failure.Detail, "database location", failure.Where)
					}
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
			scope, _ := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
			var storedPlans, planBytes, objects, objectBytes int64
			if err := fixture.storage.pool.QueryRow(ctx, `SELECT
 (SELECT plan_count FROM doc_tool_order_sources_v1 WHERE scope=$1 AND state='complete'),
 (SELECT COALESCE(sum(pg_column_size(plan)),0) FROM doc_tool_order_plans_v1 plan WHERE scope=$1),
 (SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$2),
 (SELECT COALESCE(sum(octet_length(bytes)),0) FROM doc_display_objects_v1 WHERE source_hash=$2)`, scope, owner).
				Scan(&storedPlans, &planBytes, &objects, &objectBytes); err != nil {
				test.Fatal(err)
			}
			if storedPlans != int64(plans) || plans != count*3+2 {
				test.Fatal("order preparation omitted source containers", storedPlans, plans)
			}
			assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
			test.Logf("rows=%d event_bytes=%d containers=%d order_steps=%d added_objects=%d added_object_bytes=%d plan_composite_bytes=%d order_phase_elapsed_ms=%d original_raw_reads=0 excludes_indexes_pages_previous_tree_key_storage_and_final_render=true", count, job.Target.Event.Length, plans, steps, objects-beforeObjects, objectBytes-beforeBytes, planBytes, elapsed.Milliseconds())
		})
	}
}
