//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func equalDocToolOrderSource(tree domain.DocToolTreeSource, keys domain.DocToolKeyMapSource, left, right domain.DocToolOrderSource) bool {
	leftRaw, leftErr := left.Encode(tree, keys)
	rightRaw, rightErr := right.Encode(tree, keys)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftRaw, rightRaw)
}

func docToolOrdersPGStep(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation, alter func(*domain.DocToolPreparationReader)) domain.DocEventDisplayStep {
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
		tree, keys := *prepared.ToolTree, *prepared.ToolKeys
		tools := domain.DocToolPreparationReader{
			Nodes: func(ctx context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
				return reader.ReadToolTreeNodes(ctx, tree, after, limit)
			},
			OrderSource: func(ctx context.Context, parent int) (domain.DocToolOrderSource, error) {
				return reader.ReadToolOrderSource(ctx, tree, keys, parent)
			},
			OrderEntries: func(ctx context.Context, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
				return reader.ReadToolOrderEntries(ctx, tree, keys, source, after, limit)
			},
		}
		if alter != nil {
			alter(&tools)
		}
		result, err = reader.PrepareStep(read, prepared, func(context.Context, int, int) ([]byte, error) {
			test.Error("orders stage requested original source bytes")
			return nil, domain.ErrIntegrity
		}, tools)
		return err
	}); err != nil {
		test.Fatal(err)
	}
	return result
}

func docToolOrdersPGStart(test *testing.T, output any) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	fixture, job, prepared := docToolKeyMapStoreStart(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: test.Name(), Output: output})
	prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
	step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	prepared, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil || prepared.Phase != "tool_orders" || prepared.ToolOrders == nil {
		test.Fatal("actual orders initialization", err, prepared.Phase)
	}
	return fixture, job, prepared
}

func docToolOrdersPGComplete(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) domain.DocEventDisplayPreparation {
	test.Helper()
	for steps := 0; steps < prepared.ToolKeys.Nodes*2+32; steps++ {
		if prepared.Phase == "tool_orders_done" {
			return prepared
		}
		if steps%100 == 0 {
			if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), time.Minute); err != nil {
				test.Fatal(err)
			}
		}
		step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal("actual orders save", steps, err)
		}
		loaded, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
		if err != nil || !reflect.DeepEqual(loaded, prepared) {
			test.Fatal("orders checkpoint lost on reload", err)
		}
		prepared = loaded
	}
	test.Fatal("orders stage did not converge")
	return prepared
}

func docToolOrdersPGImage(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) string {
	test.Helper()
	owner := mustDocToolKeyMapStoreOwner(test, fixture, job)
	var result string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'prior',$1::text,
 'sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY scope) FROM doc_tool_order_sources_v1 source WHERE source_hash=$2),
 'plans',(SELECT jsonb_agg(to_jsonb(plan) ORDER BY plan.scope,plan.parent_from) FROM doc_tool_order_plans_v1 plan
 JOIN doc_tool_order_sources_v1 source ON source.scope=plan.scope WHERE source.source_hash=$2))::text`,
		docToolKeyMapStoreImage(test, fixture, job), owner).Scan(&result); err != nil {
		test.Fatal(err)
	}
	return result
}

func TestDocToolOrdersPGActualSaveRestartAndSelectedRead(test *testing.T) {
	values := make([]any, 137)
	for index := range values {
		values[index] = index
	}
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"2": true, "10": false, "01": nil, "a": values, "empty": map[string]any{}, "nested": []any{map[string]any{"last": 1}}})
	for range 2 {
		step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	restarted, err := NewPostgresStore(fixture.ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(restarted.Close)
	fixture.storage = restarted
	loaded, err := restarted.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil || !reflect.DeepEqual(loaded, prepared) {
		test.Fatal("new storage process lost pending order state", err)
	}
	prepared = docToolOrdersPGComplete(test, fixture, job, loaded)
	tree, keys, source := *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders
	scope, _ := source.Hash(tree, keys)
	var state string
	var plans, containers int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,plan_count,
 (SELECT count(*) FROM doc_tool_tree_nodes_v1 WHERE scope=$2 AND value_type IN (123,91))
 FROM doc_tool_order_sources_v1 WHERE scope=$1`, scope, source.Tree).Scan(&state, &plans, &containers); err != nil || state != "complete" || plans != 5 || plans != containers {
		test.Fatal("whole set did not cover every original container", state, plans, containers, err)
	}
	before := docToolOrdersPGImage(test, fixture, job)
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		after := tree.Root.From - 1
		for {
			nodes, err := reader.ReadToolTreeNodes(read, tree, after, 16)
			if err != nil {
				test.Fatal(err)
			}
			if len(nodes) == 0 {
				break
			}
			for _, node := range nodes {
				after = node.Value.From
				if node.Type != domain.DocToolValueArray && node.Type != domain.DocToolValueObject {
					continue
				}
				plan, err := reader.ReadToolOrderPlan(read, tree, keys, source, node.Value.From)
				if err != nil {
					test.Fatal("private ready plan", err)
				}
				actual, err := reader.ReadToolOrderSource(read, tree, keys, node.Value.From)
				if err != nil || !equalDocToolOrderSource(tree, keys, plan.Source, actual) {
					test.Fatal("ready descriptor lost its exact canonical source", err)
				}
				var entries []domain.DocToolOrderEntry
				for index := 0; index < plan.Pages.Count; index++ {
					page, err := domain.ReadDocToolOrderPage(read, tree, keys, plan, index, reader.LoadObject)
					if err != nil {
						test.Fatal(err)
					}
					entries = append(entries, page.Entries...)
				}
				var selected []domain.DocToolOrderEntry
				var cursor *domain.DocToolOrderEntry
				for {
					page, err := reader.ReadToolOrderEntries(read, tree, keys, actual, cursor, 7)
					if err != nil {
						test.Fatal("keyset entries", err)
					}
					if len(page) == 0 {
						break
					}
					selected = append(selected, page...)
					cursor = &selected[len(selected)-1]
				}
				if len(entries) != actual.Count || !reflect.DeepEqual(entries, selected) {
					test.Fatal("saved pages diverged from protected ordered entries", node.Value.From)
				}
			}
		}
		root, err := reader.ReadToolOrderPlan(read, tree, keys, source, tree.Root.From)
		if err != nil {
			test.Fatal(err)
		}
		page, err := domain.ReadDocToolOrderPage(read, tree, keys, root, 0, reader.LoadObject)
		if err != nil {
			test.Fatal(err)
		}
		var names []string
		for _, entry := range page.Entries {
			span := entry.Node.Key
			start := job.Target.Event.Offset
			names = append(names, string(fixture.stream[start+span.From:start+span.To]))
		}
		if !reflect.DeepEqual(names, []string{`"2"`, `"10"`, `"01"`, `"a"`, `"empty"`, `"nested"`}) {
			test.Fatal("object order lost numeric or insertion semantics", names)
		}
	})
	if docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("private order reads changed durable state")
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}

func TestDocToolOrdersPGEmptyAndScalarSets(test *testing.T) {
	for _, output := range []any{map[string]any{}, []any{}, "text", 1, true, nil} {
		test.Run(fmt.Sprintf("%T_%v", output, output), func(test *testing.T) {
			fixture, job, prepared := docToolOrdersPGStart(test, output)
			prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
			scope, _ := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
			var count int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT plan_count FROM doc_tool_order_sources_v1 WHERE scope=$1 AND state='complete'`, scope).Scan(&count); err != nil {
				test.Fatal(err)
			}
			expected := 0
			if prepared.ToolTree.Type == domain.DocToolValueArray || prepared.ToolTree.Type == domain.DocToolValueObject {
				expected = 1
			}
			if count != expected {
				test.Fatal("empty or leaf-only set did not seal an explicit count", count, expected)
			}
			assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
		})
	}
}

func TestDocToolOrdersPGReadBindingCursorAndUnfinished(test *testing.T) {
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"a": []any{true, false, nil}, "b": 2})
	step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	prepared, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil {
		test.Fatal(err)
	}
	tree, keys, source := *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders
	var escaped *postgresDocEventDisplayWork
	var escapedContext context.Context
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		escaped, escapedContext = reader, read
		if got, err := reader.ReadToolOrderPlan(read, tree, keys, source, tree.Root.From); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || !reflect.DeepEqual(got, domain.DocToolOrderPlan{}) {
			test.Fatal("ready individual plan escaped unfinished whole set", got, err)
		}
		root, err := reader.ReadToolOrderSource(read, tree, keys, tree.Root.From)
		if err != nil {
			test.Fatal(err)
		}
		entries, err := reader.ReadToolOrderEntries(read, tree, keys, root, nil, 64)
		if err != nil || len(entries) != 2 {
			test.Fatal(err, entries)
		}
		array, err := reader.ReadToolOrderSource(read, tree, keys, entries[0].Node.Value.From)
		if err != nil {
			test.Fatal(err)
		}
		children, err := reader.ReadToolOrderEntries(read, tree, keys, array, nil, 64)
		if err != nil || len(children) != 3 {
			test.Fatal(err, children)
		}
		for _, parent := range []int{tree.Root.From + 1, entries[1].Node.Value.From} {
			if got, err := reader.ReadToolOrderSource(read, tree, keys, parent); !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(got, domain.DocToolOrderSource{}) {
				test.Fatal("non-container accepted as source", got, err)
			}
		}
		for _, limit := range []int{-1, 0, 65} {
			if got, err := reader.ReadToolOrderEntries(read, tree, keys, root, nil, limit); !errors.Is(err, domain.ErrValidation) || got != nil {
				test.Fatal("unbounded page accepted", got, err)
			}
		}
		forged := entries[0]
		forged.Digest = domain.HashContent([]byte("forged member cursor"))
		if got, err := reader.ReadToolOrderEntries(read, tree, keys, root, &forged, 1); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			test.Fatal("unproven object cursor accepted", got, err)
		}
		forged = children[0]
		forged.Node.Type = domain.DocToolValueNull
		if got, err := reader.ReadToolOrderEntries(read, tree, keys, array, &forged, 1); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			test.Fatal("unproven array cursor accepted", got, err)
		}
		wrong := *reader
		wrong.orders = nil
		if got, err := wrong.ReadToolOrderSource(read, tree, keys, tree.Root.From); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(got, domain.DocToolOrderSource{}) {
			test.Fatal("reader without order capability accepted", got, err)
		}
		wrong = *reader
		wrong.source = domain.HashContent([]byte("different display source"))
		if got, err := wrong.ReadToolOrderSource(read, tree, keys, tree.Root.From); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.DocToolOrderSource{}) {
			test.Fatal("reader escaped owner", got, err)
		}
		other := source
		other.Nodes++
		if got, err := reader.ReadToolOrderPlan(read, tree, keys, other, tree.Root.From); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(got, domain.DocToolOrderPlan{}) {
			test.Fatal("reader escaped global source", got, err)
		}
		cancelled, cancel := context.WithCancel(read)
		cancel()
		if got, err := reader.ReadToolOrderEntries(cancelled, tree, keys, root, nil, 64); !errors.Is(err, context.Canceled) || got != nil {
			test.Fatal("cancelled page leaked partial data", got, err)
		}
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(other context.Context) error {
			if got, err := reader.ReadToolOrderSource(other, tree, keys, tree.Root.From); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(got, domain.DocToolOrderSource{}) {
				test.Fatal("reader escaped original transaction", got, err)
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	})
	for _, read := range []context.Context{fixture.ctx, escapedContext} {
		if got, err := escaped.ReadToolOrderSource(read, tree, keys, tree.Root.From); err == nil || !reflect.DeepEqual(got, domain.DocToolOrderSource{}) {
			test.Fatal("escaped or closed reader yielded private source", got, err)
		}
	}
}

func TestDocToolOrdersPGCASAndProvenance(test *testing.T) {
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"a": 1, "b": 2, "c": 3})
	forged := docToolOrdersPGStep(test, fixture, job, prepared, func(tools *domain.DocToolPreparationReader) {
		original := tools.OrderSource
		tools.OrderSource = func(ctx context.Context, parent int) (domain.DocToolOrderSource, error) {
			source, err := original(ctx, parent)
			source.Count--
			return source, err
		}
		entries := tools.OrderEntries
		tools.OrderEntries = func(ctx context.Context, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
			return entries(ctx, source, after, limit)
		}
	})
	before := docToolOrdersPGImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, forged); !errors.Is(err, domain.ErrIntegrity) || docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("forged protected source count survived actual save", err)
	}
	forged = docToolOrdersPGStep(test, fixture, job, prepared, func(tools *domain.DocToolPreparationReader) {
		original := tools.OrderEntries
		tools.OrderEntries = func(ctx context.Context, source domain.DocToolOrderSource, after *domain.DocToolOrderEntry, limit int) ([]domain.DocToolOrderEntry, error) {
			entries, err := original(ctx, source, after, limit)
			if len(entries) > 0 {
				entries[0].Digest = domain.HashContent([]byte("unproven key digest"))
			}
			return entries, err
		}
	})
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, forged); !errors.Is(err, domain.ErrIntegrity) || docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("domain-sealed unproven selected entries survived actual save", err)
	}
	step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	wrong := job
	wrong.Version++
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, wrong, step); !errors.Is(err, domain.ErrConflict) || docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("wrong lease mutated orders", err)
	}
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
			results <- err
		})
	}
	workers.Wait()
	close(results)
	committed, conflicted := 0, 0
	for err := range results {
		if err == nil {
			committed++
		} else if errors.Is(err, domain.ErrConflict) {
			conflicted++
		} else {
			test.Fatal(err)
		}
	}
	if committed != 1 || conflicted != 1 {
		test.Fatal("order step CAS selected other than one saver", committed, conflicted)
	}
	before = docToolOrdersPGImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); !errors.Is(err, domain.ErrConflict) || docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("stale save changed catalog or objects", err)
	}
}

func TestDocToolOrdersPGCanonicalPrefixAndNoPartial(test *testing.T) {
	values := make([]any, 80)
	for index := range values {
		values[index] = true
	}
	fixture, job, prepared := docToolOrdersPGStart(test, values)
	step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	prepared, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil {
		test.Fatal(err)
	}
	step = docToolOrdersPGStep(test, fixture, job, prepared, nil)
	_, global, _ := step.ToolOrders()
	inner, found := global.Order()
	if !found || len(inner.Entries()) != 64 {
		test.Fatal("large array did not produce a bounded first page")
	}
	corrupt := inner.Entries()[20].Node
	if _, err := execDocToolCatalogPG(fixture, job, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=$3 WHERE scope=$1 AND node_from=$2`, prepared.ToolKeys.Tree, corrupt.Value.From, domain.HashContent([]byte("corrupt selected node"))); err != nil {
		test.Fatal(err)
	}
	before := docToolOrdersPGImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); !errors.Is(err, domain.ErrIntegrity) || docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("selected page was not re-proven in outer save transaction", err)
	}
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		source, err := reader.ReadToolOrderSource(read, *prepared.ToolTree, *prepared.ToolKeys, prepared.ToolTree.Root.From)
		if err != nil {
			test.Fatal(err)
		}
		if got, err := reader.ReadToolOrderEntries(read, *prepared.ToolTree, *prepared.ToolKeys, source, nil, 64); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			test.Fatal("corruption after valid prefix leaked partial page", got, err)
		}
	})
	raw, _ := corrupt.Encode(*prepared.ToolTree)
	if _, err := execDocToolCatalogPG(fixture, job, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=$3 WHERE scope=$1 AND node_from=$2`, prepared.ToolKeys.Tree, corrupt.Value.From, domain.HashContent(raw)); err != nil {
		test.Fatal(err)
	}
	for range 2 {
		step = docToolOrdersPGStep(test, fixture, job, prepared, nil)
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	forged := docToolOrdersPGStep(test, fixture, job, prepared, func(tools *domain.DocToolPreparationReader) {
		original := tools.Nodes
		tools.Nodes = func(ctx context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
			nodes, err := original(ctx, after, limit+1)
			if err != nil {
				return nil, err
			}
			return append(nodes[:1], nodes[2:]...), nil
		}
	})
	before = docToolOrdersPGImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, forged); !errors.Is(err, domain.ErrIntegrity) || docToolOrdersPGImage(test, fixture, job) != before {
		test.Fatal("global input skipped a canonical source node", err)
	}
}

func TestDocToolOrdersPGCompletionCorruptionAndGuards(test *testing.T) {
	for _, mutation := range []string{
		`DELETE FROM doc_tool_order_plans_v1 WHERE scope=$1 AND parent_from=(SELECT min(parent_from) FROM doc_tool_order_plans_v1 WHERE scope=$1)`,
		`UPDATE doc_tool_order_plans_v1 SET plan_hash=$2 WHERE scope=$1`,
		`UPDATE doc_tool_order_plans_v1 SET bytes=bytes || decode('20','hex') WHERE scope=$1`,
	} {
		test.Run(fmt.Sprintf("%x", domain.HashContent([]byte(mutation))[:12]), func(test *testing.T) {
			fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"a": []any{1}, "z": 2})
			var final domain.DocEventDisplayStep
			for {
				step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
				if step.Preparation().Phase == "tool_orders_done" {
					final = step
					break
				}
				var err error
				prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
				if err != nil {
					test.Fatal(err)
				}
			}
			scope, _ := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
			arguments := []any{scope}
			if strings.Contains(mutation, "$2") {
				arguments = append(arguments, domain.HashContent([]byte("wrong descriptor hash")))
			}
			before := docToolOrdersPGImage(test, fixture, job)
			_, err := execDocToolCatalogPG(fixture, job, mutation, arguments...)
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "23514" || docToolOrdersPGImage(test, fixture, job) != before {
				test.Fatal("ready catalog row was mutable", err)
			}
			docToolKeyMapStoreBypassGuards(test, fixture, &job, []string{"doc_tool_order_plans_v1"}, mutation, arguments...)
			before = docToolOrdersPGImage(test, fixture, job)
			if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, final); !errors.Is(err, domain.ErrIntegrity) || docToolOrdersPGImage(test, fixture, job) != before {
				test.Fatal("corrupt or incomplete catalog completed", err)
			}
		})
	}
}

func TestDocToolOrdersPGCompletedImmutableAndParentCascade(test *testing.T) {
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"a": []any{}, "z": 1})
	prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
	scope, _ := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
	for _, mutation := range []string{
		`UPDATE doc_tool_order_sources_v1 SET bytes=bytes || decode('20','hex') WHERE scope=$1`,
		`UPDATE doc_tool_order_sources_v1 SET state='building',plan_count=NULL WHERE scope=$1`,
		`UPDATE doc_tool_order_plans_v1 SET plan_hash=$2 WHERE scope=$1`,
		`DELETE FROM doc_tool_order_plans_v1 WHERE scope=$1`,
	} {
		arguments := []any{scope}
		if strings.Contains(mutation, "$2") {
			arguments = append(arguments, domain.HashContent([]byte("changed ready plan")))
		}
		before := docToolOrdersPGImage(test, fixture, job)
		_, err := execDocToolCatalogPG(fixture, job, mutation, arguments...)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23514" || docToolOrdersPGImage(test, fixture, job) != before {
			test.Fatal("completed order data mutated", mutation, err)
		}
	}
	for _, replay := range []string{
		`UPDATE doc_tool_order_sources_v1 SET state=state,plan_count=plan_count WHERE scope=$1`,
		`UPDATE doc_tool_order_plans_v1 SET plan_hash=plan_hash,bytes=bytes WHERE scope=$1`,
	} {
		if _, err := execDocToolCatalogPG(fixture, job, replay, scope); err != nil {
			test.Fatal("exact catalog replay rejected", err)
		}
	}
	owner, _ := prepared.Source.Hash()
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, job.RepoID); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, owner); err != nil {
		test.Fatal("display source parent cascade blocked", err)
	}
	var remaining int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 (SELECT count(*) FROM doc_tool_order_sources_v1 WHERE scope=$1)+
 (SELECT count(*) FROM doc_tool_order_plans_v1 WHERE scope=$1)`, scope).Scan(&remaining); err != nil || remaining != 0 {
		test.Fatal("parent cascade retained private order rows", remaining, err)
	}
}

func TestDocToolOrdersPGSharedCompletedSourceReplay(test *testing.T) {
	values := make([]any, 80)
	for index := range values {
		values[index] = index
	}
	output := map[string]any{"a": values, "z": map[string]any{"leaf": true}}
	first, firstJob, firstPrepared := docToolOrdersPGStart(test, output)
	firstPrepared = docToolOrdersPGComplete(test, first, firstJob, firstPrepared)
	second, secondJob, secondPrepared := docToolOrdersPGStart(test, output)
	docToolTreeReviewSharedCleanup(test, first, second)
	before := docToolOrdersPGImage(test, first, firstJob)
	for range 2 {
		step := docToolOrdersPGStep(test, second, secondJob, secondPrepared, nil)
		var err error
		secondPrepared, err = second.storage.SaveDocEventDisplay(second.ctx, secondJob, step)
		if err != nil {
			test.Fatal("bounded partial replay over a completed shared set", err)
		}
	}
	restarted, err := NewPostgresStore(second.ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(restarted.Close)
	second.storage = restarted
	secondPrepared, err = restarted.LoadDocEventDisplay(second.ctx, secondJob)
	if err != nil {
		test.Fatal(err)
	}
	secondPrepared = docToolOrdersPGComplete(test, second, secondJob, secondPrepared)
	if *secondPrepared.ToolOrders != *firstPrepared.ToolOrders || docToolOrdersPGImage(test, first, firstJob) != before {
		test.Fatal("shared completed-source replay rewrote or duplicated catalog facts")
	}
	third, thirdJob, thirdPrepared := docToolOrdersPGStart(test, output)
	docToolTreeReviewSharedCleanup(test, first, third)
	docToolTreeReviewSharedCleanup(test, second, third)
	step := docToolOrdersPGStep(test, third, thirdJob, thirdPrepared, nil)
	scope, _ := thirdPrepared.ToolOrders.Hash(*thirdPrepared.ToolTree, *thirdPrepared.ToolKeys)
	docToolKeyMapStoreBypassGuards(test, third, &thirdJob, []string{"doc_tool_order_plans_v1"},
		`DELETE FROM doc_tool_order_plans_v1 WHERE scope=$1 AND parent_from=$2`, scope, thirdPrepared.ToolTree.Root.From)
	before = docToolOrdersPGImage(test, third, thirdJob)
	if _, err := third.storage.SaveDocEventDisplay(third.ctx, thirdJob, step); !errors.Is(err, domain.ErrIntegrity) || docToolOrdersPGImage(test, third, thirdJob) != before {
		test.Fatal("completed set replay repaired a missing ready row", err)
	}
}

func TestDocToolOrdersPGReadCorruptDescriptors(test *testing.T) {
	for _, mutation := range []struct {
		table string
		query string
	}{
		{"doc_tool_order_sources_v1", `UPDATE doc_tool_order_sources_v1 SET bytes=bytes || decode('20','hex') WHERE scope=$1`},
		{"doc_tool_order_sources_v1", `UPDATE doc_tool_order_sources_v1 SET plan_count=0 WHERE scope=$1`},
		{"doc_tool_order_plans_v1", `UPDATE doc_tool_order_plans_v1 SET bytes=bytes || decode('20','hex') WHERE scope=$1`},
		{"doc_tool_order_plans_v1", `UPDATE doc_tool_order_plans_v1 SET plan_hash=plan_hash || repeat('x',1048576) WHERE scope=$1`},
	} {
		test.Run(fmt.Sprintf("%x", domain.HashContent([]byte(mutation.query))[:12]), func(test *testing.T) {
			fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"a": 1})
			prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
			scope, _ := prepared.ToolOrders.Hash(*prepared.ToolTree, *prepared.ToolKeys)
			docToolKeyMapStoreBypassGuards(test, fixture, &job, []string{mutation.table}, mutation.query, scope)
			readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
				got, err := reader.ReadToolOrderPlan(read, *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders, prepared.ToolTree.Root.From)
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.DocToolOrderPlan{}) {
					test.Fatal("corrupt descriptor leaked a plan", got, err)
				}
			})
		})
	}
}

func TestDocToolOrdersPGDuplicateSemanticMembers(test *testing.T) {
	fixture, prepared, owner, items := docToolKeyMapStoreDuplicates(test)
	tree, keys := *prepared.ToolTree, *prepared.ToolKeys
	source, err := domain.NewDocToolOrdersSource(tree, keys)
	if err != nil {
		test.Fatal(err)
	}
	var duplicates []domain.DocToolKeyItem
	for _, item := range items {
		if item.Node.Parent == tree.Root.From && item.Digest == domain.HashContent([]byte(`"a"`)) {
			duplicates = append(duplicates, item)
		}
	}
	if len(duplicates) != 3 {
		test.Fatal("duplicate fixture lacks original overwritten containers")
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: read.Value(repositoryTxKey{}).(*repositoryTx), source: owner, tree: &tree, keys: &keys, orders: &source}
		container, err := reader.ReadToolOrderSource(read, tree, keys, tree.Root.From)
		if err != nil || container.Parent.Children != 4 || container.Count != 2 {
			test.Fatal("object count used original occurrences rather than semantic members", container, err)
		}
		entries, err := reader.ReadToolOrderEntries(read, tree, keys, container, nil, 64)
		if err != nil || len(entries) != 2 || entries[0].First != duplicates[0].Node.Value.From || entries[0].Node.Value.From != duplicates[2].Node.Value.From {
			test.Fatal("semantic member lost first insertion or last value", entries, err)
		}
		for _, original := range duplicates {
			if _, err := reader.ReadToolOrderSource(read, tree, keys, original.Node.Value.From); err != nil {
				test.Fatal("overwritten original container disappeared from source traversal", err)
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

type docToolOrdersPGTrace struct {
	pgx.Tx
	queries   []string
	arguments [][]any
	rows      int
	objects   int
}

type docToolOrdersPGRows struct {
	pgx.Rows
	trace *docToolOrdersPGTrace
}

func (rows *docToolOrdersPGRows) Next() bool {
	if rows.Rows.Next() {
		rows.trace.rows++
		return true
	}
	return false
}

func (trace *docToolOrdersPGTrace) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	trace.queries = append(trace.queries, query)
	trace.arguments = append(trace.arguments, arguments)
	rows, err := trace.Tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &docToolOrdersPGRows{Rows: rows, trace: trace}, nil
}

func (trace *docToolOrdersPGTrace) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	trace.queries = append(trace.queries, query)
	trace.arguments = append(trace.arguments, arguments)
	if strings.Contains(query, "doc_display_objects_v1") {
		trace.objects++
	}
	return trace.Tx.QueryRow(ctx, query, arguments...)
}

func docToolOrdersPGPlanArguments(test *testing.T, arguments []any) string {
	test.Helper()
	literals := make([]string, len(arguments))
	for index, argument := range arguments {
		switch value := argument.(type) {
		case domain.ContentHash:
			literals[index] = "'" + strings.ReplaceAll(string(value), "'", "''") + "'"
		case string:
			literals[index] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
		case []byte:
			literals[index] = "pg_catalog.decode('" + hex.EncodeToString(value) + "','hex')"
		case []string:
			if value == nil {
				literals[index] = "NULL::text[]"
				continue
			}
			elements := make([]any, len(value))
			for offset, element := range value {
				elements[offset] = element
			}
			literals[index] = "ARRAY[" + docToolOrdersPGPlanArguments(test, elements) + "]::text[]"
		case [][]byte:
			if value == nil {
				literals[index] = "NULL::bytea[]"
				continue
			}
			elements := make([]any, len(value))
			for offset, element := range value {
				elements[offset] = element
			}
			literals[index] = "ARRAY[" + docToolOrdersPGPlanArguments(test, elements) + "]::bytea[]"
		case []int64:
			if value == nil {
				literals[index] = "NULL::bigint[]"
				continue
			}
			elements := make([]any, len(value))
			for offset, element := range value {
				elements[offset] = element
			}
			literals[index] = "ARRAY[" + docToolOrdersPGPlanArguments(test, elements) + "]::bigint[]"
		case int:
			literals[index] = fmt.Sprint(value)
		case int64:
			literals[index] = fmt.Sprint(value)
		case bool:
			literals[index] = fmt.Sprint(value)
		default:
			test.Fatalf("unsupported traced plan argument %T", argument)
		}
	}
	return strings.Join(literals, ",")
}

func explainDocToolOrdersPGTraceQuery(test *testing.T, read context.Context, transaction pgx.Tx, query string, arguments []any, mode string) []byte {
	test.Helper()
	if mode != "force_custom_plan" && mode != "force_generic_plan" {
		test.Fatal("invalid plan cache mode", mode)
	}
	if _, err := transaction.Exec(read, `SET LOCAL plan_cache_mode=`+mode); err != nil {
		test.Fatal(err)
	}
	statement := domain.NewID("orders_plan_")
	name := pgx.Identifier{statement}.Sanitize()
	if _, err := transaction.Exec(read, `PREPARE `+name+` AS `+query); err != nil {
		test.Fatal("prepare actual traced query", mode, query, err)
	}
	defer func() {
		if _, err := transaction.Exec(read, `DEALLOCATE `+name); err != nil {
			test.Error("deallocate traced query", err)
		}
	}()
	var encoded []byte
	if err := transaction.QueryRow(read, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON,COSTS OFF) EXECUTE `+name+`(`+
		docToolOrdersPGPlanArguments(test, arguments)+`)`).Scan(&encoded); err != nil {
		test.Fatal("explain actual traced query", mode, query, err)
	}
	var generic, custom int64
	if err := transaction.QueryRow(read, `SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1`, statement).Scan(&generic, &custom); err != nil {
		test.Fatal("verify traced statement plan mode", mode, err)
	}
	if mode == "force_custom_plan" && (custom != 1 || generic != 0) || mode == "force_generic_plan" && (generic != 1 || custom != 0) {
		test.Fatal("traced statement used wrong plan mode", mode, generic, custom)
	}
	return encoded
}

func assertDocToolOrdersPGPointPlan(test *testing.T, encoded []byte, mode string, index int, query string) float64 {
	test.Helper()
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if json.Unmarshal(encoded, &plans) != nil || len(plans) != 1 {
		test.Fatal("invalid actual query plan", mode, index, query)
	}
	blocks := float64(0)
	for _, field := range []string{"Shared Hit Blocks", "Shared Read Blocks", "Local Hit Blocks", "Local Read Blocks", "Temp Read Blocks", "Temp Written Blocks"} {
		if count, found := plans[0].Plan[field].(float64); found {
			blocks += count
		}
	}
	if blocks > 64 {
		test.Fatal("one-entry fixture query exceeded its 64-block local read budget", mode, index, query, string(encoded))
	}
	leading := map[string][]string{
		"doc_tool_key_sources_v1_scope_tree_scope_key":         {"(scope ="},
		"doc_tool_tree_sources_v1_pkey":                        {"(scope ="},
		"doc_tool_key_sources_v1_pkey":                         {"(scope ="},
		"doc_tool_tree_nodes_v1_pkey":                          {"(scope =", "(node_from ="},
		"doc_tool_tree_nodes_v1_scope_parent_from_ordinal_key": {"(scope =", "(parent_from =", "(ordinal"},
		"doc_tool_key_members_v1_pkey":                         {"(scope =", "(parent_from =", "(digest ="},
		"doc_tool_key_members_v1_first":                        {"(scope =", "(first_from =", "(parent_from =", "(digest ="},
		"doc_tool_key_members_v1_last":                         {"(scope =", "(last_from =", "(parent_from =", "(digest ="},
		"doc_tool_key_members_v1_order":                        {"(scope =", "(parent_from =", "NOT is_array_index", "array_index", "first_from"},
		"doc_tool_key_observations_v1_pkey":                    {"(scope =", "(node_from ="},
		"doc_tool_key_observations_v1_groups":                  {"(scope =", "(parent_from =", "(digest =", "(node_from"},
	}
	var inspect func(map[string]any)
	inspect = func(plan map[string]any) {
		relation, _ := plan["Relation Name"].(string)
		smallCatalogScan := strings.HasSuffix(mode, "/seqscan=on") && plan["Node Type"] == "Seq Scan" &&
			(relation == "doc_tool_tree_sources_v1" || relation == "doc_tool_key_sources_v1")
		if smallCatalogScan {
			filter, _ := plan["Filter"].(string)
			if !strings.Contains(filter, "(scope =") {
				test.Fatal("small catalog scan lost exact source identity", mode, string(encoded))
			}
		}
		if name, found := plan["Index Name"].(string); found {
			fields, known := leading[name]
			condition, _ := plan["Index Cond"].(string)
			if !known {
				test.Fatal("unreviewed access path in point-query fixture", mode, name, string(encoded))
			}
			for _, field := range fields {
				if !strings.Contains(condition, field) {
					test.Fatal("point-query index lacks a leading identity/range condition", mode, name, field, string(encoded))
				}
			}
		}
		if relation != "" || plan["Index Name"] != nil {
			if rows, found := plan["Actual Rows"].(float64); found && rows > 1 {
				test.Fatal("entry-read trace read a relation prefix", mode, index, query, string(encoded))
			}
			if loops, found := plan["Actual Loops"].(float64); found && loops > 1 {
				test.Fatal("entry-read trace repeated a relation probe", mode, index, query, string(encoded))
			}
		}
		for _, field := range []string{"Rows Removed by Filter", "Rows Removed by Join Filter", "Rows Removed by Index Recheck"} {
			if removed, found := plan[field].(float64); found && removed > 1 && !(smallCatalogScan && field == "Rows Removed by Filter") {
				test.Fatal("entry-read trace filtered a prefix", mode, index, query, string(encoded))
			}
		}
		if children, found := plan["Plans"].([]any); found {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(plans[0].Plan)
	return blocks
}

func TestDocToolOrdersPGBoundedSelectedPath(test *testing.T) {
	for _, object := range []bool{false, true} {
		test.Run(fmt.Sprint(object), func(test *testing.T) {
			var output any
			if object {
				members := make(map[string]any, 513)
				for index := range 513 {
					members[fmt.Sprintf("key_%04d", index)] = index
				}
				output = members
			} else {
				values := make([]any, 8193)
				for index := range values {
					values[index] = index
				}
				output = values
			}
			fixture, job, prepared := docToolOrdersPGStart(test, output)
			ctx, cancel := context.WithTimeout(context.WithoutCancel(fixture.ctx), 2*time.Minute)
			defer cancel()
			fixture.ctx = ctx
			prepared = docToolOrdersPGComplete(test, fixture, job, prepared)
			raw, _ := prepared.Encode()
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				original := read.Value(repositoryTxKey{}).(*repositoryTx)
				trace := &docToolOrdersPGTrace{Tx: original.Tx}
				bound := *original
				bound.Tx = trace
				read = context.WithValue(read, repositoryTxKey{}, &bound)
				work, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent(raw))
				if err != nil {
					return err
				}
				reader := work.(*postgresDocEventDisplayWork)
				trace.queries, trace.arguments, trace.rows, trace.objects = nil, nil, 0, 0
				plan, err := reader.ReadToolOrderPlan(read, *prepared.ToolTree, *prepared.ToolKeys, *prepared.ToolOrders, prepared.ToolTree.Root.From)
				if err != nil {
					return err
				}
				page, err := domain.ReadDocToolOrderPage(read, *prepared.ToolTree, *prepared.ToolKeys, plan, (plan.Source.Count-1)/64-1, reader.LoadObject)
				if err != nil || len(page.Entries) != 64 || trace.objects > 4 || trace.rows != 0 {
					test.Fatal("selected page reread a prefix or the whole child list", err, trace.objects, trace.rows)
				}
				trace.queries, trace.arguments, trace.rows = nil, nil, 0
				after := page.Entries[len(page.Entries)-1]
				entries, err := reader.ReadToolOrderEntries(read, *prepared.ToolTree, *prepared.ToolKeys, plan.Source, &after, 1)
				if err != nil || len(entries) != 1 || trace.rows != 1 {
					test.Fatal("late keyset page scanned old entries", err, trace.rows)
				}
				selectedIndex := -1
				maximumQueries := 4
				if object {
					maximumQueries = 7
				}
				if len(trace.queries) == 0 || len(trace.queries) > maximumQueries || len(trace.queries) != len(trace.arguments) {
					test.Fatal("entry-read trace exceeded its bounded proof queries", len(trace.queries), maximumQueries)
				}
				catalogProof, parentProof, cursorProof := false, false, false
				for index, query := range trace.queries {
					upper := strings.ToUpper(query)
					if strings.Contains(upper, "OFFSET") || strings.Contains(upper, "COUNT(") || strings.Contains(upper, " OR ") || strings.Contains(upper, "DOC_TOOL_ORDER_PLANS_V1") {
						test.Fatal("selected entry page used a prefix/count/catalog scan", query)
					}
					if strings.Contains(query, "ORDER BY node.ordinal LIMIT") || strings.Contains(query, "ORDER BY NOT member.is_array_index") {
						selectedIndex = index
					}
					if strings.Contains(query, "FROM doc_tool_tree_sources_v1 tree LEFT JOIN doc_tool_key_sources_v1 keys") {
						catalogProof = true
					}
					arguments := trace.arguments[index]
					if strings.Contains(query, "requested(scope,node_from)") && len(arguments) == 2 && arguments[0] == prepared.ToolKeys.Tree {
						parentProof = parentProof || arguments[1] == prepared.ToolTree.Root.From
						cursorProof = cursorProof || arguments[1] == after.Node.Value.From
					}
				}
				if selectedIndex < 0 || !catalogProof || !parentProof || !cursorProof {
					test.Fatal("selected page omitted indexed ordering or catalog/parent/cursor proof", selectedIndex, catalogProof, parentProof, cursorProof)
				}
				indexName := "doc_tool_tree_nodes_v1_scope_parent_from_ordinal_key"
				if object {
					indexName = "doc_tool_key_members_v1_order"
				}
				for _, sequential := range []string{"on", "off"} {
					if _, err := original.Tx.Exec(read, `SET LOCAL enable_seqscan=`+sequential); err != nil {
						return err
					}
					for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
						for index, query := range trace.queries {
							encoded := explainDocToolOrdersPGTraceQuery(test, read, original.Tx, query, trace.arguments[index], mode)
							assertDocToolOrdersPGPointPlan(test, encoded, mode+"/seqscan="+sequential, index, query)
							if index == selectedIndex && !strings.Contains(string(encoded), indexName) {
								test.Fatal("late selected page did not use parent/order index", mode, sequential, string(encoded))
							}
						}
					}
				}
				test.Logf("object=%t traced_queries=%d prepared_modes=custom,generic seqscan=on,off max_returned_rows=1 max_probe_loops=1 max_body_filtered_rows=1 max_query_blocks=64", object, len(trace.queries))
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}
