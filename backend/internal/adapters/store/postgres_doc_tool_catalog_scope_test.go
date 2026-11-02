//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docToolCatalogScopeTracePG(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork) (*docToolOrdersPGTrace, func()) {
	test.Helper()
	if reader.owner != read.Value(repositoryTxKey{}) || !reader.storage.InReadOnlyTransaction(read) {
		test.Fatal("trace requires the actual work reader's original snapshot")
	}
	original := reader.owner.Tx
	trace := &docToolOrdersPGTrace{Tx: original}
	reader.owner.Tx = trace
	return trace, func() { reader.owner.Tx = original }
}

func assertDocToolCatalogScopeTracePG(test *testing.T, trace *docToolOrdersPGTrace, expected [5]int) {
	test.Helper()
	relations := [5]string{"doc_tool_tree_sources_v1", "doc_tool_key_sources_v1", "doc_tool_order_sources_v1", "doc_tool_tree_nodes_v1", "doc_tool_order_plans_v1"}
	var actual [5]int
	for _, query := range trace.queries {
		for index, relation := range relations {
			if strings.Contains(query, relation) {
				actual[index]++
			}
		}
	}
	if actual != expected || len(trace.queries) != expected[0]+expected[2]+expected[3]+expected[4] || len(trace.queries) != len(trace.arguments) {
		test.Fatal("actual tree/key/order header, parent and plan SQL counts changed", actual, expected, trace.queries)
	}
}

func docToolCatalogScopePlanPG(read context.Context, reader *postgresDocEventDisplayWork, prepared domain.DocEventDisplayPreparation) (domain.DocToolOrderPlan, error) {
	return reader.ReadToolOrderPlan(read, *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders, prepared.ToolTree.Root.From)
}

func assertDocToolCatalogScopeFailurePG(test *testing.T, result domain.DocToolOrderPlan, failure, expected error) {
	test.Helper()
	if !errors.Is(failure, expected) || !reflect.DeepEqual(result, domain.DocToolOrderPlan{}) {
		test.Fatal("failed scoped plan read returned data or changed its error", result, failure, expected)
	}
}

func docToolCatalogScopeRawPG(test *testing.T, fixture docDisplayPGFixture, prepared domain.DocEventDisplayPreparation) []byte {
	test.Helper()
	scope, err := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
	if err != nil {
		test.Fatal(err)
	}
	var hash domain.ContentHash
	var raw []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT plan_hash,bytes FROM doc_tool_order_plans_v1
 WHERE scope=$1 AND parent_from=$2`, scope, prepared.ToolTree.Root.From).Scan(&hash, &raw); err != nil || domain.HashContent(raw) != hash {
		test.Fatal("fixture lacks the actual hash-checked selected plan", hash, err)
	}
	return raw
}

func assertDocToolCatalogScopePlanPG(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork, prepared domain.DocEventDisplayPreparation, expected []byte) {
	test.Helper()
	plan, err := docToolCatalogScopePlanPG(read, reader, prepared)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := plan.Encode(*prepared.ToolTree, *prepared.ToolKeys)
	if err != nil || !bytes.Equal(raw, expected) {
		test.Fatal("scoped plan changed its canonical selected bytes", err)
	}
}

func docToolCatalogScopeMutatePG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, table, statement string, arguments ...any) {
	test.Helper()
	if table == "doc_tool_tree_sources_v1" || table == "doc_tool_tree_nodes_v1" {
		tag, err := execDocToolCatalogPG(fixture, job, statement, arguments...)
		if err != nil || tag.RowsAffected() != 1 {
			test.Fatal("actual calculation could not seed the selected corruption", tag, err)
		}
		return
	}
	docToolKeyMapStoreBypassGuards(test, fixture, &job, []string{table}, statement, arguments...)
}

func TestDocToolCatalogScopePGPlanHeadersPinnedOnlyWithinWork(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	expected := docToolCatalogScopeRawPG(test, fixture, prepared)
	before := docCatalogUseImagePG(test, fixture)
	for scope := 0; scope < 2; scope++ {
		readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
			trace, restore := docToolCatalogScopeTracePG(test, read, reader)
			defer restore()
			assertDocToolCatalogScopePlanPG(test, read, reader, prepared, expected)
			assertDocToolCatalogScopeTracePG(test, trace, [5]int{1, 1, 1, 1, 1})
			assertDocToolCatalogScopePlanPG(test, read, reader, prepared, expected)
			assertDocToolCatalogScopeTracePG(test, trace, [5]int{1, 1, 1, 2, 2})
		})
	}
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("pinned header reads changed durable state")
	}
}

func TestDocToolCatalogScopePGFailedHeadersAreNotCached(test *testing.T) {
	for _, mutation := range []struct {
		name, table, set string
		want             error
	}{
		{"tree_bytes", "doc_tool_tree_sources_v1", "bytes=bytes||decode('20','hex')", domain.ErrIntegrity},
		{"tree_incomplete", "doc_tool_tree_sources_v1", "state='scanning',node_count=NULL", domain.ErrDocRangeIndexUnavailable},
		{"key_count", "doc_tool_key_sources_v1", "member_count=$2", domain.ErrIntegrity},
		{"key_incomplete", "doc_tool_key_sources_v1", "state='scanning',member_count=NULL", domain.ErrDocRangeIndexUnavailable},
		{"order_bytes", "doc_tool_order_sources_v1", "bytes=bytes||decode('20','hex')", domain.ErrIntegrity},
		{"order_incomplete", "doc_tool_order_sources_v1", "state='building',plan_count=NULL", domain.ErrDocRangeIndexUnavailable},
	} {
		test.Run(mutation.name, func(test *testing.T) {
			fixture, job, prepared := docCatalogProtocolFixturePG(test)
			scope := prepared.ToolOrders.Tree
			if mutation.table == "doc_tool_key_sources_v1" {
				scope = prepared.ToolOrders.Keys
			} else if mutation.table == "doc_tool_order_sources_v1" {
				var err error
				scope, err = prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
				if err != nil {
					test.Fatal(err)
				}
			}
			arguments := []any{scope}
			if strings.Contains(mutation.set, "$2") {
				arguments = append(arguments, prepared.ToolKeys.Nodes+1)
			}
			docToolCatalogScopeMutatePG(test, fixture, job, mutation.table,
				`UPDATE `+pgx.Identifier{mutation.table}.Sanitize()+` SET `+mutation.set+` WHERE scope=$1`, arguments...)
			before := docCatalogUseImagePG(test, fixture)
			readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
				trace, restore := docToolCatalogScopeTracePG(test, read, reader)
				defer restore()
				for attempt := 1; attempt <= 2; attempt++ {
					plan, err := docToolCatalogScopePlanPG(read, reader, prepared)
					assertDocToolCatalogScopeFailurePG(test, plan, err, mutation.want)
					expected := [5]int{attempt, attempt, 0, 0, 0}
					if mutation.table == "doc_tool_order_sources_v1" {
						expected[2] = attempt
					}
					assertDocToolCatalogScopeTracePG(test, trace, expected)
				}
			})
			if docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("failed header validation changed durable state")
			}
		})
	}
}

func TestDocToolCatalogScopePGFreshSnapshotSeesChangedHeader(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	expected := docToolCatalogScopeRawPG(test, fixture, prepared)
	var afterMutation string
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		trace, restore := docToolCatalogScopeTracePG(test, read, reader)
		defer restore()
		assertDocToolCatalogScopePlanPG(test, read, reader, prepared, expected)
		docToolCatalogScopeMutatePG(test, fixture, job, "doc_tool_tree_sources_v1",
			`UPDATE doc_tool_tree_sources_v1 SET bytes=bytes||decode('20','hex') WHERE scope=$1`, prepared.ToolOrders.Tree)
		afterMutation = docCatalogUseImagePG(test, fixture)
		assertDocToolCatalogScopePlanPG(test, read, reader, prepared, expected)
		assertDocToolCatalogScopeTracePG(test, trace, [5]int{1, 1, 1, 2, 2})
		readDocToolKeyMapStore(test, fixture, job, prepared, func(fresh context.Context, current *postgresDocEventDisplayWork) {
			currentTrace, restoreCurrent := docToolCatalogScopeTracePG(test, fresh, current)
			defer restoreCurrent()
			plan, err := docToolCatalogScopePlanPG(fresh, current, prepared)
			assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrIntegrity)
			assertDocToolCatalogScopeTracePG(test, currentTrace, [5]int{1, 1, 0, 0, 0})
		})
	})
	if docCatalogUseImagePG(test, fixture) != afterMutation {
		test.Fatal("snapshot reads changed state beyond the committed fixture corruption")
	}
}

func TestDocToolCatalogScopePGWarmProofRequiresOriginalLiveScope(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	expected := docToolCatalogScopeRawPG(test, fixture, prepared)
	before := docCatalogUseImagePG(test, fixture)
	foreign, err := NewPostgresStore(fixture.ctx, fixture.storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(foreign.Close)
	var escaped *postgresDocEventDisplayWork
	var original context.Context
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		escaped, original = reader, read
		trace, restore := docToolCatalogScopeTracePG(test, read, reader)
		defer restore()
		assertDocToolCatalogScopePlanPG(test, read, reader, prepared, expected)
		trace.queries, trace.arguments = nil, nil
		canceled, cancel := context.WithCancel(read)
		cancel()
		expired, expire := context.WithDeadline(read, time.Now().Add(-time.Second))
		defer expire()
		for _, stopped := range []struct {
			ctx  context.Context
			want error
		}{{canceled, context.Canceled}, {expired, context.DeadlineExceeded}, {fixture.ctx, domain.ErrConflict}} {
			plan, err := docToolCatalogScopePlanPG(stopped.ctx, reader, prepared)
			assertDocToolCatalogScopeFailurePG(test, plan, err, stopped.want)
		}
		if err := foreign.WithinReadSnapshot(fixture.ctx, func(other context.Context) error {
			work, err := foreign.OpenDocEventDisplayWork(other, job, docCatalogUseCheckpointPG(test, prepared))
			if err != nil {
				return err
			}
			current := work.(*postgresDocEventDisplayWork)
			assertDocToolCatalogScopePlanPG(test, other, current, prepared, expected)
			plan, err := docToolCatalogScopePlanPG(other, reader, prepared)
			assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
			plan, err = docToolCatalogScopePlanPG(read, current, prepared)
			assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
			return nil
		}); err != nil {
			test.Fatal(err)
		}
		wrongTree, wrongKeys, wrongOrders := *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders
		wrongTree.Root.To++
		wrongKeys.Nodes++
		wrongOrders.Nodes++
		plan, err := reader.ReadToolOrderPlan(read, wrongTree, *prepared.ToolKeys, *prepared.ToolOrders, prepared.ToolTree.Root.From)
		assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
		plan, err = reader.ReadToolOrderPlan(read, *prepared.ToolTree, wrongKeys, *prepared.ToolOrders, prepared.ToolTree.Root.From)
		assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
		plan, err = reader.ReadToolOrderPlan(read, *prepared.ToolTree, *prepared.ToolKeys, wrongOrders, prepared.ToolTree.Root.From)
		assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
		wrongSource := *reader
		wrongSource.source = domain.HashContent([]byte("foreign header owner"))
		plan, err = docToolCatalogScopePlanPG(read, &wrongSource, prepared)
		assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
		wrongStorage := *reader
		wrongStorage.storage = foreign
		plan, err = docToolCatalogScopePlanPG(read, &wrongStorage, prepared)
		assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
		assertDocToolCatalogScopeTracePG(test, trace, [5]int{})
		assertDocToolCatalogScopePlanPG(test, read, reader, prepared, expected)
		assertDocToolCatalogScopeTracePG(test, trace, [5]int{0, 0, 0, 1, 1})
	})
	plan, err := docToolCatalogScopePlanPG(original, escaped, prepared)
	assertDocToolCatalogScopeFailurePG(test, plan, err, pgx.ErrTxClosed)
	err = fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
		if err != nil {
			return err
		}
		reader := work.(*postgresDocEventDisplayWork)
		trace, restore := docToolCatalogScopeTracePG(test, read, reader)
		defer restore()
		assertDocToolCatalogScopePlanPG(test, read, reader, prepared, expected)
		trace.queries, trace.arguments = nil, nil
		_, err = reader.owner.Exec(read, `SELECT 1/0`)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "22012" {
			test.Fatal("fixture did not abort the actual warmed transaction", err)
		}
		plan, err := docToolCatalogScopePlanPG(read, reader, prepared)
		assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrConflict)
		assertDocToolCatalogScopeTracePG(test, trace, [5]int{})
		return err
	})
	if !errors.Is(err, domain.ErrConflict) || docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("aborted warm read hid failure or changed durable state", err)
	}
}

func TestDocToolCatalogScopePGSelectedRowsRemainHashChecked(test *testing.T) {
	for _, selected := range []string{"plan_hash", "plan_bytes", "parent_hash"} {
		test.Run(selected, func(test *testing.T) {
			fixture, job, prepared := docCatalogProtocolFixturePG(test)
			scope, err := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
			if err != nil {
				test.Fatal(err)
			}
			table := "doc_tool_order_plans_v1"
			statement := `UPDATE doc_tool_order_plans_v1 SET plan_hash=$2 WHERE scope=$1 AND parent_from=$3`
			arguments := []any{scope, domain.HashContent([]byte(test.Name())), prepared.ToolTree.Root.From}
			if selected == "plan_bytes" {
				statement = `UPDATE doc_tool_order_plans_v1 SET bytes=$2 WHERE scope=$1 AND parent_from=$3`
				arguments[1] = []byte(`{}`)
			} else if selected == "parent_hash" {
				table = "doc_tool_tree_nodes_v1"
				statement = `UPDATE doc_tool_tree_nodes_v1 SET node_hash=$2 WHERE scope=$1 AND node_from=$3`
				arguments[0] = prepared.ToolOrders.Tree
			}
			docToolCatalogScopeMutatePG(test, fixture, job, table, statement, arguments...)
			before := docCatalogUseImagePG(test, fixture)
			readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
				trace, restore := docToolCatalogScopeTracePG(test, read, reader)
				defer restore()
				for attempt := 1; attempt <= 2; attempt++ {
					plan, err := docToolCatalogScopePlanPG(read, reader, prepared)
					assertDocToolCatalogScopeFailurePG(test, plan, err, domain.ErrIntegrity)
					expected := [5]int{1, 1, 1, attempt, attempt}
					if selected == "parent_hash" {
						expected[4] = 0
					}
					assertDocToolCatalogScopeTracePG(test, trace, expected)
				}
			})
			if docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("selected-row rejection changed durable state")
			}
		})
	}
}
