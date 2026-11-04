//go:build postgres

package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNormalizationCheckpointPGObjectPointPlansStayBounded(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	connection, err := storage.pool.Acquire(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Release()
	var original string
	if err := connection.QueryRow(ctx, `SHOW enable_seqscan`).Scan(&original); err != nil {
		test.Fatal(err)
	}
	inserted := 0
	for _, count := range []int{0, 1, 128} {
		for inserted < count {
			objects := make([]legacyByteCheckpointObject, min(domain.DocLegacyByteBackfillBatchBlocks, count-inserted))
			for ordinal := range objects {
				objects[ordinal] = legacyCheckpointPGObject(fmt.Sprintf("point-plan-object-%d", inserted+ordinal))
			}
			if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, state, objects); err != nil {
				test.Fatal(err)
			}
			inserted += len(objects)
		}
		transaction, err := connection.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(transaction)
		if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
			test.Fatal(err)
		}
		object := legacyCheckpointPGObject("point-plan-object-0")
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			encoded := explainDocToolOrdersPGTraceQuery(test, ctx, transaction, legacyNormalizationBatchObjectRead,
				[]any{job.DocHash, state.stage.incarnation, object.hash, len(object.data), state.sequence}, mode)
			var plans []struct {
				Plan map[string]any `json:"Plan"`
			}
			if err := json.Unmarshal(encoded, &plans); err != nil || len(plans) != 1 {
				test.Fatal("invalid checkpoint point-query plan", err)
			}
			probes := 0
			var inspect func(map[string]any)
			inspect = func(plan map[string]any) {
				if relation, _ := plan["Relation Name"].(string); relation != "" && plan["Node Type"] != "ModifyTable" {
					condition, _ := plan["Index Cond"].(string)
					for _, field := range []string{"(doc_hash =", "(stage_id =", "(hash ="} {
						if !strings.Contains(condition, field) {
							test.Fatal("checkpoint access lost its complete point identity", count, mode, string(encoded))
						}
					}
					if plan["Node Type"] != "Index Scan" && plan["Node Type"] != "Index Only Scan" {
						test.Fatal("checkpoint access is not an indexed point probe", count, mode, string(encoded))
					}
					probes++
				}
				if children, ok := plan["Plans"].([]any); ok {
					for _, child := range children {
						inspect(child.(map[string]any))
					}
				}
			}
			inspect(plans[0].Plan)
			if probes < 2 {
				test.Fatal("checkpoint plan omitted object or staging point access", count, mode, string(encoded))
			}
		}
		if err := transaction.Rollback(ctx); err != nil {
			test.Fatal(err)
		}
		var restored string
		if err := connection.QueryRow(ctx, `SHOW enable_seqscan`).Scan(&restored); err != nil || restored != original {
			test.Fatal("checkpoint planner setting leaked beyond its transaction", original, restored, err)
		}
	}
}
