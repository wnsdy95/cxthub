//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docToolKeyMapStoreLoader func(context.Context, outbound.DocToolTreeWorkReader, int, int) ([]domain.DocToolTreeNode, error)

func docToolKeyMapStoreStep(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation, alternate docToolKeyMapStoreLoader) domain.DocEventDisplayStep {
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
		nodes := work.(outbound.DocToolTreeWorkReader)
		result, err = work.PrepareStep(read, prepared, func(read context.Context, from, length int) ([]byte, error) {
			if err := read.Err(); err != nil {
				return nil, err
			}
			if from < 0 || length <= 0 || length > domain.DocFieldDiscoveryStepBytes || from > job.Target.Event.Length-length {
				return nil, domain.ErrIntegrity
			}
			start := job.Target.Event.Offset + from
			return bytes.Clone(fixture.stream[start : start+length]), nil
		}, domain.DocToolPreparationReader{Nodes: func(read context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
			if alternate != nil {
				return alternate(read, nodes, after, limit)
			}
			return nodes.ReadToolTreeNodes(read, *prepared.ToolTree, after, limit)
		}})
		return err
	}); err != nil {
		test.Fatal(err)
	}
	return result
}

func docToolKeyMapStoreStart(test *testing.T, event domain.CIREvent) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	fixture, job, prepared := docToolTreeReviewStart(test, event)
	prepared = completeDocToolTreePG(test, fixture, job, prepared)
	step := docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
	prepared, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil || prepared.Phase != "tool_keys" || prepared.ToolKeys == nil {
		test.Fatal("key stage initialization", err, prepared.Phase)
	}
	return fixture, job, prepared
}

func docToolKeyMapStoreComplete(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) domain.DocEventDisplayPreparation {
	test.Helper()
	for steps := 0; steps < 1000; steps++ {
		if prepared.Phase == "tool_keys_done" {
			return prepared
		}
		step := docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
		var err error
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal("actual outer save", steps, err)
		}
		loaded, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
		if err != nil || !reflect.DeepEqual(loaded, prepared) {
			test.Fatal("restart lost the private key checkpoint", err)
		}
		prepared = loaded
	}
	test.Fatal("key map did not converge")
	return prepared
}

func docToolKeyMapStoreImage(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) string {
	test.Helper()
	var result string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'job',(SELECT to_jsonb(job) FROM doc_display_jobs_v1 job WHERE repo_id=$1 AND profile='public-event-display-v1'),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY object.hash) FROM doc_display_objects_v1 object WHERE source_hash=$2),
 'sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY source.scope) FROM doc_tool_key_sources_v1 source
 JOIN doc_tool_tree_sources_v1 tree ON tree.scope=source.tree_scope WHERE tree.source_hash=$2),
 'observations',(SELECT jsonb_agg(to_jsonb(observation) ORDER BY observation.scope,observation.node_from)
 FROM doc_tool_key_observations_v1 observation JOIN doc_tool_tree_sources_v1 tree ON tree.scope=observation.tree_scope WHERE tree.source_hash=$2),
 'members',(SELECT jsonb_agg(to_jsonb(member) ORDER BY member.scope,member.parent_from,member.digest)
 FROM doc_tool_key_members_v1 member JOIN doc_tool_key_sources_v1 source ON source.scope=member.scope
 JOIN doc_tool_tree_sources_v1 tree ON tree.scope=source.tree_scope WHERE tree.source_hash=$2))::text`,
		job.RepoID, mustDocToolKeyMapStoreOwner(test, fixture, job)).Scan(&result); err != nil {
		test.Fatal(err)
	}
	return result
}

func mustDocToolKeyMapStoreOwner(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) domain.ContentHash {
	test.Helper()
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	owner, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	return owner
}

func readDocToolKeyMapStore(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation, check func(context.Context, *postgresDocEventDisplayWork)) {
	test.Helper()
	raw, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent(raw))
		if err != nil {
			return err
		}
		check(read, work.(*postgresDocEventDisplayWork))
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestDocToolKeyMapPGStoreActualSaveOrderedScopedRead(test *testing.T) {
	output := map[string]any{"01": false, "1": 1, "10": 10, "2": 2, "4294967294": true, "4294967295": false, "a": map[string]any{"child": "PUBLIC_BODY"}, "z": map[string]any{}}
	fixture, job, prepared := docToolKeyMapStoreStart(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: test.Name(), Output: output})
	prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
	tree, source := *prepared.ToolTree, *prepared.ToolKeys
	scope, _ := source.Hash(tree)
	var members, observations int
	var state string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,member_count,
 (SELECT count(*) FROM doc_tool_key_observations_v1 WHERE scope=$1) FROM doc_tool_key_sources_v1 WHERE scope=$1`, scope).Scan(&state, &members, &observations); err != nil || state != "complete" || members != 9 || observations != 9 {
		test.Fatal("private completed counts", state, members, observations, err)
	}
	var uniqueSlots bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*)=count(DISTINCT first_from)
 FROM doc_tool_key_members_v1 WHERE scope=$1 AND parent_from=$2`, scope, tree.Root.From).Scan(&uniqueSlots); err != nil || !uniqueSlots {
		test.Fatal("valid stage has ambiguous insertion slots for SQL pagination", uniqueSlots, err)
	}
	var escaped *postgresDocEventDisplayWork
	var escapedContext context.Context
	before := docToolKeyMapStoreImage(test, fixture, job)
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		escaped, escapedContext = reader, read
		var after *domain.DocToolKeyMember
		var all []domain.DocToolKeyMember
		for calls := 0; calls < 10; calls++ {
			page, err := reader.ReadToolKeyMembers(read, tree, source, tree.Root.From, after, 2)
			if err != nil || len(page) > 2 {
				test.Fatal("ordered private keyset", err, page)
			}
			if len(page) == 0 {
				break
			}
			all = append(all, page...)
			after = &all[len(all)-1]
		}
		var keys []string
		for index, member := range all {
			span := member.Last.Node.Key
			start := job.Target.Event.Offset
			keys = append(keys, string(fixture.stream[start+span.From:start+span.To]))
			if index > 0 && domain.CompareDocToolKeyMembers(all[index-1], member) >= 0 {
				test.Fatal("SQL order diverged from domain order")
			}
			if member.First != member.Last.Node.Value.From {
				test.Fatal("unique key changed its first raw insertion slot")
			}
		}
		if !reflect.DeepEqual(keys, []string{`"1"`, `"2"`, `"10"`, `"4294967294"`, `"01"`, `"4294967295"`, `"a"`, `"z"`}) {
			test.Fatal("numeric versus raw-insertion ordering", keys)
		}
		empty := all[len(all)-1].Last.Node.Value.From
		if got, err := reader.ReadToolKeyMembers(read, tree, source, empty, nil, 64); err != nil || len(got) != 0 {
			test.Fatal("canonical empty object is not readable", got, err)
		}
		for _, parent := range []int{tree.Root.From + 1, all[0].Last.Node.Value.From} {
			if got, err := reader.ReadToolKeyMembers(read, tree, source, parent, nil, 64); !errors.Is(err, domain.ErrValidation) || got != nil {
				test.Fatal("non-node or scalar masqueraded as empty object", parent, got, err)
			}
		}
		for _, limit := range []int{-1, 0, 65} {
			if got, err := reader.ReadToolKeyMembers(read, tree, source, tree.Root.From, nil, limit); !errors.Is(err, domain.ErrValidation) || got != nil {
				test.Fatal("unbounded limit", limit, got, err)
			}
		}
		forged := all[0]
		forged.Last.Digest = domain.HashContent([]byte("other cursor"))
		if got, err := reader.ReadToolKeyMembers(read, tree, source, tree.Root.From, &forged, 2); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			test.Fatal("cursor escaped exact persisted member", got, err)
		}
		wrongTree := tree
		wrongTree.Evidence = domain.HashContent([]byte("other tree"))
		wrongSource := source
		wrongSource.Nodes++
		for _, wrong := range []struct {
			tree domain.DocToolTreeSource
			src  domain.DocToolKeyMapSource
		}{{wrongTree, source}, {tree, wrongSource}} {
			if got, err := reader.ReadToolKeyMembers(read, wrong.tree, wrong.src, tree.Root.From, nil, 2); !errors.Is(err, domain.ErrConflict) || got != nil {
				test.Fatal("reader escaped its exact tree or key source", got, err)
			}
		}
		wrongOwner := *reader
		wrongOwner.source = domain.HashContent([]byte("other owner"))
		if got, err := wrongOwner.ReadToolKeyMembers(read, tree, source, tree.Root.From, nil, 2); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			test.Fatal("reader escaped its exact display owner", got, err)
		}
		cancelled, cancel := context.WithCancel(read)
		cancel()
		if got, err := reader.ReadToolKeyMembers(cancelled, tree, source, tree.Root.From, nil, 2); !errors.Is(err, context.Canceled) || got != nil {
			test.Fatal("cancellation leaked partial members", got, err)
		}
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(other context.Context) error {
			if got, err := reader.ReadToolKeyMembers(other, tree, source, tree.Root.From, nil, 2); !errors.Is(err, domain.ErrConflict) || got != nil {
				test.Fatal("reader escaped into another transaction", got, err)
			}
			return nil
		}); err != nil {
			test.Fatal(err)
		}
	})
	for _, read := range []context.Context{fixture.ctx, escapedContext} {
		if got, err := escaped.ReadToolKeyMembers(read, tree, source, tree.Root.From, nil, 2); err == nil || got != nil {
			test.Fatal("escaped or closed snapshot yielded private members", got, err)
		}
	}
	if docToolKeyMapStoreImage(test, fixture, job) != before {
		test.Fatal("private reads mutated durable state")
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}

func TestDocToolKeyMapPGStoreLongCurrentRestartAndUnfinishedRead(test *testing.T) {
	fixture, job, prepared := docToolKeyMapStoreStart(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: test.Name(), Output: map[string]any{strings.Repeat("a", 140<<10): "PUBLIC_BODY", "z": 1}})
	step := docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
	_, sealed, found := step.ToolKeyMap()
	if !found || sealed.State().Current == nil || sealed.State().Seen != 1 || len(sealed.Items()) != 0 {
		test.Fatal("long key did not retain an unconsumed current candidate")
	}
	var err error
	prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil {
		test.Fatal(err)
	}
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		if got, err := reader.ReadToolKeyMembers(read, *prepared.ToolTree, *prepared.ToolKeys, prepared.ToolTree.Root.From, nil, 4); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || got != nil {
			test.Fatal("unfinished source leaked members", got, err)
		}
	})
	prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
	readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
		if got, err := reader.ReadToolKeyMembers(read, *prepared.ToolTree, *prepared.ToolKeys, prepared.ToolTree.Root.From, nil, 4); err != nil || len(got) != 2 {
			test.Fatal("long key checkpoints lost the original keyed node", got, err)
		}
	})
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}

func TestDocToolKeyMapPGStoreNoKeyTrees(test *testing.T) {
	for _, output := range []any{map[string]any{}, []any{}, []any{true, nil, 1}, "text", 1} {
		test.Run(fmt.Sprintf("%T_%v", output, output), func(test *testing.T) {
			fixture, job, prepared := docToolKeyMapStoreStart(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: test.Name(), Output: output})
			prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
			scope, _ := prepared.ToolKeys.Hash(*prepared.ToolTree)
			var count int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT member_count FROM doc_tool_key_sources_v1 WHERE scope=$1 AND state='complete'`, scope).Scan(&count); err != nil || count != 0 {
				test.Fatal("no-key tree did not complete with an explicit zero count", count, err)
			}
		})
	}
}

func TestDocToolKeyMapPGStoreCASAndInputPrefix(test *testing.T) {
	fixture, job, prepared := docToolKeyMapStoreStart(test, docToolTreeReviewEvent(test, 8))
	forged := docToolKeyMapStoreStep(test, fixture, job, prepared, func(read context.Context, reader outbound.DocToolTreeWorkReader, after, limit int) ([]domain.DocToolTreeNode, error) {
		nodes, err := reader.ReadToolTreeNodes(read, *prepared.ToolTree, after, limit+1)
		if err != nil {
			return nil, err
		}
		return append(nodes[:1], nodes[2:]...), nil
	})
	before := docToolKeyMapStoreImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, forged); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("sealed step skipped an interior canonical node", err)
	}
	if docToolKeyMapStoreImage(test, fixture, job) != before {
		test.Fatal("input-prefix rejection was not atomic")
	}
	step := docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
	for _, alter := range []func(*domain.DocDisplayJob){
		func(job *domain.DocDisplayJob) { job.Version++ },
		func(job *domain.DocDisplayJob) { job.Target.Event.Hash = domain.HashContent([]byte("other event")) },
	} {
		wrong := job
		alter(&wrong)
		if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, wrong, step); !errors.Is(err, domain.ErrConflict) || docToolKeyMapStoreImage(test, fixture, job) != before {
			test.Fatal("wrong lease or event scope changed private facts", err)
		}
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
		test.Fatal("outer CAS did not select exactly one saver", committed, conflicted)
	}
	before = docToolKeyMapStoreImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); !errors.Is(err, domain.ErrConflict) || docToolKeyMapStoreImage(test, fixture, job) != before {
		test.Fatal("lost acknowledgement retry changed observations or aggregates", err)
	}
}

func TestDocToolKeyMapPGStoreCompletionRejectsCorruption(test *testing.T) {
	for _, mutation := range []string{
		`DELETE FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from=(SELECT min(node_from) FROM doc_tool_key_observations_v1 WHERE scope=$1)`,
		`DELETE FROM doc_tool_key_members_v1 WHERE scope=$1 AND first_from=(SELECT min(first_from) FROM doc_tool_key_members_v1 WHERE scope=$1)`,
		`UPDATE doc_tool_key_observations_v1 SET node_hash=$2 WHERE scope=$1`,
		`UPDATE doc_tool_key_observations_v1 SET parent_from=parent_from+1 WHERE scope=$1`,
		`UPDATE doc_tool_key_members_v1 SET is_array_index=NOT is_array_index WHERE scope=$1`,
		`INSERT INTO doc_tool_key_members_v1 SELECT scope,parent_from,$2,first_from,last_from,is_array_index,array_index FROM doc_tool_key_members_v1 WHERE scope=$1 LIMIT 1`,
	} {
		test.Run(fmt.Sprintf("case_%x", domain.HashContent([]byte(mutation))[:12]), func(test *testing.T) {
			fixture, job, prepared := docToolKeyMapStoreStart(test, docToolTreeReviewEvent(test, 8))
			var final domain.DocEventDisplayStep
			for {
				step := docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
				if step.Preparation().Phase == "tool_keys_done" {
					final = step
					break
				}
				var err error
				prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
				if err != nil {
					test.Fatal(err)
				}
			}
			scope, _ := prepared.ToolKeys.Hash(*prepared.ToolTree)
			arguments := []any{scope}
			if strings.Contains(mutation, "$2") {
				arguments = append(arguments, domain.HashContent([]byte("corrupted observation")))
			}
			if strings.HasPrefix(mutation, "DELETE FROM doc_tool_key_members_v1") || strings.Contains(mutation, "SET parent_from=parent_from+1") {
				before := docToolKeyMapStoreImage(test, fixture, job)
				var err error
				if strings.HasPrefix(mutation, "DELETE") {
					_, err = execDocToolCatalogPGWithCorruptDeletes(test, fixture, job, []string{"doc_tool_key_observations_v1", "doc_tool_key_members_v1"}, mutation, arguments...)
				} else {
					_, err = execDocToolCatalogPG(fixture, job, mutation, arguments...)
				}
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23503" || docToolKeyMapStoreImage(test, fixture, job) != before {
					test.Fatal("deferred group reference allowed committed observations without an aggregate", err)
				}
				return
			}
			var err error
			if strings.HasPrefix(mutation, "DELETE") {
				_, err = execDocToolCatalogPGWithCorruptDeletes(test, fixture, job, []string{"doc_tool_key_observations_v1", "doc_tool_key_members_v1"}, mutation, arguments...)
			} else {
				_, err = execDocToolCatalogPG(fixture, job, mutation, arguments...)
			}
			if err != nil {
				test.Fatal("corruption fixture", err)
			}
			before := docToolKeyMapStoreImage(test, fixture, job)
			if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, final); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("inconsistent complete aggregate was accepted", err)
			}
			if docToolKeyMapStoreImage(test, fixture, job) != before {
				test.Fatal("failed completion advanced a checkpoint or repaired corrupt facts")
			}
		})
	}
}

func TestDocToolKeyMapPGStoreSharedCompletedSourceReplay(test *testing.T) {
	event := docToolTreeReviewEvent(test, 6)
	first, firstJob, firstPrepared := docToolKeyMapStoreStart(test, event)
	firstPrepared = docToolKeyMapStoreComplete(test, first, firstJob, firstPrepared)
	second, secondJob, secondPrepared := docToolKeyMapStoreStart(test, event)
	docToolTreeReviewSharedCleanup(test, first, second)
	scope, _ := firstPrepared.ToolKeys.Hash(*firstPrepared.ToolTree)
	before := docToolKeyMapStoreImage(test, first, firstJob)
	secondPrepared = docToolKeyMapStoreComplete(test, second, secondJob, secondPrepared)
	if *firstPrepared.ToolKeys != *secondPrepared.ToolKeys || docToolKeyMapStoreImage(test, first, firstJob) != before {
		test.Fatal("same-source replay duplicated or rewrote immutable key facts")
	}
	third, thirdJob, thirdPrepared := docToolKeyMapStoreStart(test, event)
	docToolTreeReviewSharedCleanup(test, first, third)
	docToolTreeReviewSharedCleanup(test, second, third)
	step := docToolKeyMapStoreStep(test, third, thirdJob, thirdPrepared, nil)
	_, sealed, _ := step.ToolKeyMap()
	items := sealed.Items()
	if len(items) == 0 {
		test.Fatal("missing replay observations")
	}
	docToolKeyMapStoreBypassGuards(test, third, &thirdJob, []string{"doc_tool_key_observations_v1", "doc_tool_key_members_v1"},
		`DELETE FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from=$2`, scope, items[0].Node.Value.From)
	before = docToolKeyMapStoreImage(test, third, thirdJob)
	if _, err := third.storage.SaveDocEventDisplay(third.ctx, thirdJob, step); !errors.Is(err, domain.ErrIntegrity) || docToolKeyMapStoreImage(test, third, thirdJob) != before {
		test.Fatal("completed source allowed a new observation or partial repair", err)
	}
}

func TestDocToolKeyMapPGStoreCascadeRollback(test *testing.T) {
	fixture, job, prepared := docToolKeyMapStoreStart(test, docToolTreeReviewEvent(test, 3))
	prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
	scope, _ := prepared.ToolKeys.Hash(*prepared.ToolTree)
	before := docToolKeyMapStoreImage(test, fixture, job)
	for _, parent := range []struct {
		table string
		scope domain.ContentHash
	}{{"doc_tool_key_sources_v1", scope}, {"doc_tool_tree_sources_v1", prepared.ToolKeys.Tree}} {
		_, denied := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM `+pgx.Identifier{parent.table}.Sanitize()+` WHERE scope=$1`, parent.scope)
		var rejected *pgconn.PgError
		if !errors.As(denied, &rejected) || rejected.Code != "23514" || docToolKeyMapStoreImage(test, fixture, job) != before {
			test.Fatal("live catalog parent deletion bypassed retirement authority", denied)
		}
		transaction, err := fixture.storage.pool.Begin(fixture.ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(transaction)
		setDocToolCatalogPGDeleteGuards(test, fixture, transaction, docCatalogWriterTablesPG, false)
		if _, err := transaction.Exec(fixture.ctx, `DELETE FROM `+pgx.Identifier{parent.table}.Sanitize()+` WHERE scope=$1`, parent.scope); err != nil {
			test.Fatal(err)
		}
		var cleared bool
		if err := transaction.QueryRow(fixture.ctx, `SELECT NOT EXISTS(SELECT 1 FROM doc_tool_key_sources_v1 WHERE scope=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_key_observations_v1 WHERE scope=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_tool_key_members_v1 WHERE scope=$1)`, scope).Scan(&cleared); err != nil || !cleared {
			test.Fatal("private source deletion did not cascade through observations and both member references", cleared, err)
		}
		if err := transaction.Rollback(fixture.ctx); err != nil || docToolKeyMapStoreImage(test, fixture, job) != before {
			test.Fatal("cascade rollback lost private key facts", err)
		}
	}
}

func TestDocToolKeyMapPGStoreReadCorruptionAndBoundedHashes(test *testing.T) {
	for _, mutation := range []string{
		`UPDATE doc_tool_tree_nodes_v1 SET bytes='{}'::bytea WHERE scope=$2 AND node_from=(SELECT max(last_from) FROM doc_tool_key_members_v1 WHERE scope=$1)`,
		`UPDATE doc_tool_tree_nodes_v1 SET node_hash=node_hash||repeat('x',16777216) WHERE scope=$2 AND parent_from=-1 AND $1::text IS NOT NULL`,
		`UPDATE doc_tool_key_members_v1 SET digest=digest||repeat('x',1024) WHERE scope=$1 AND first_from=(SELECT max(first_from) FROM doc_tool_key_members_v1 WHERE scope=$1)`,
		`UPDATE doc_tool_key_observations_v1 SET node_hash=node_hash||repeat('x',16777216) WHERE scope=$1`,
		`UPDATE doc_tool_key_members_v1 SET first_from=(SELECT min(first_from) FROM doc_tool_key_members_v1 WHERE scope=$1) WHERE scope=$1`,
	} {
		test.Run(fmt.Sprintf("case_%x", domain.HashContent([]byte(mutation))[:12]), func(test *testing.T) {
			fixture, job, prepared := docToolKeyMapStoreStart(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: test.Name(), Output: map[string]any{"a": 1, "b": 2, "c": 3}})
			prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
			tree, source := *prepared.ToolTree, *prepared.ToolKeys
			scope, _ := source.Hash(tree)
			arguments := []any{scope}
			if strings.Contains(mutation, "$2") {
				arguments = append(arguments, source.Tree)
			}
			if strings.Contains(mutation, "SET digest=") {
				before := docToolKeyMapStoreImage(test, fixture, job)
				_, err := execDocToolCatalogPG(fixture, job, mutation, arguments...)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23514" || docToolKeyMapStoreImage(test, fixture, job) != before {
					test.Fatal("completed digest mutation was not rejected atomically", err)
				}
				return
			}
			if strings.HasPrefix(mutation, "UPDATE doc_tool_key_members_v1") {
				docToolKeyMapStoreBypassGuards(test, fixture, &job, []string{"doc_tool_key_members_v1"}, mutation, arguments...)
			} else if strings.HasPrefix(mutation, "UPDATE doc_tool_key_observations_v1") {
				docToolKeyMapStoreBypassGuards(test, fixture, &job, []string{"doc_tool_key_observations_v1"}, mutation, arguments...)
			} else if _, err := execDocToolCatalogPG(fixture, job, mutation, arguments...); err != nil {
				test.Fatal(err)
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			readDocToolKeyMapStore(test, fixture, job, prepared, func(read context.Context, reader *postgresDocEventDisplayWork) {
				if got, err := reader.ReadToolKeyMembers(read, tree, source, tree.Root.From, nil, 64); !errors.Is(err, domain.ErrIntegrity) || got != nil {
					test.Fatal("corrupt selected metadata leaked partial members", got, err)
				}
			})
			runtime.ReadMemStats(&after)
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
				test.Fatal("oversized stored text escaped bounded selection", allocated)
			}
		})
	}
}

func TestDocToolKeyMapPGStoreInjectedObservationFailureAndCurrentBinding(test *testing.T) {
	fixture, job, prepared := docToolKeyMapStoreStart(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: test.Name(), Output: map[string]any{strings.Repeat("a", 200): 1, "z": 2}})
	step := docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
	var err error
	prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil {
		test.Fatal(err)
	}
	step = docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
	_, sealed, _ := step.ToolKeyMap()
	current := sealed.Inputs()[0]
	if _, err := execDocToolCatalogPG(fixture, job, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=$3 WHERE scope=$1 AND node_from=$2`, prepared.ToolKeys.Tree, current.Value.From, domain.HashContent([]byte("wrong current"))); err != nil {
		test.Fatal(err)
	}
	before := docToolKeyMapStoreImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); !errors.Is(err, domain.ErrIntegrity) || docToolKeyMapStoreImage(test, fixture, job) != before {
		test.Fatal("pending current was not revalidated against canonical storage", err)
	}
	raw, _ := current.Encode(*prepared.ToolTree)
	if _, err := execDocToolCatalogPG(fixture, job, `UPDATE doc_tool_tree_nodes_v1 SET node_hash=$3 WHERE scope=$1 AND node_from=$2`, prepared.ToolKeys.Tree, current.Value.From, domain.HashContent(raw)); err != nil {
		test.Fatal(err)
	}
	for {
		step = docToolKeyMapStoreStep(test, fixture, job, prepared, nil)
		_, sealed, _ = step.ToolKeyMap()
		if len(sealed.Items()) > 0 {
			break
		}
		prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	var schema string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		test.Fatal(err)
	}
	function := pgx.Identifier{schema, domain.NewID("fail_keys_")}.Sanitize()
	trigger := pgx.Identifier{domain.NewID("keys_failure_")}.Sanitize()
	table := pgx.Identifier{schema, "doc_tool_key_observations_v1"}.Sanitize()
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION 'injected key observation failure'; END $$;
 CREATE TRIGGER `+trigger+` BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION `+function+`() `); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if _, err := fixture.storage.pool.Exec(fixture.ctx, `DROP TRIGGER IF EXISTS `+trigger+` ON `+table+`; DROP FUNCTION IF EXISTS `+function+`() `); err != nil {
			test.Error(err)
		}
	})
	before = docToolKeyMapStoreImage(test, fixture, job)
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); err == nil || docToolKeyMapStoreImage(test, fixture, job) != before {
		test.Fatal("observation failure left objects, aggregates or a new checkpoint", err)
	}
}

func TestDocToolKeyMapStoreDomainOrderMatchesUniqueInsertionSlots(test *testing.T) {
	tree := domain.DocToolTreeSource{Version: 1, Profile: domain.DocToolTreeProfile, EventHash: domain.HashContent([]byte("event")), EventBytes: 1000, Evidence: domain.HashContent([]byte("fields")), Root: domain.DocFindSpan{From: 0, To: 1000}, Type: domain.DocToolValueObject}
	var members []domain.DocToolKeyMember
	for _, item := range []struct {
		from  int
		index bool
		value uint32
	}{{50, false, 0}, {40, true, 10}, {30, true, 2}, {20, false, 0}} {
		node := domain.DocToolTreeNode{Value: domain.DocFindSpan{From: item.from, To: item.from + 1}, Type: domain.DocToolValueNumber, Parent: 0, Ordinal: 0, Key: &domain.DocFindSpan{From: item.from - 4, To: item.from - 1}, Depth: 1}
		member, err := domain.MergeDocToolKeyMember(tree, nil, domain.DocToolKeyItem{Node: node, Digest: domain.HashContent([]byte(fmt.Sprint(item.from))), IsArrayIndex: item.index, ArrayIndex: item.value})
		if err != nil {
			test.Fatal(err)
		}
		members = append(members, member)
	}
	slices.SortFunc(members, domain.CompareDocToolKeyMembers)
	var positions []int
	for _, member := range members {
		positions = append(positions, member.First)
	}
	if !reflect.DeepEqual(positions, []int{30, 40, 20, 50}) {
		test.Fatal("valid unique slots require no digest tie-break for SQL pagination", positions)
	}
}

func docToolKeyMapStoreBypassGuards(test *testing.T, fixture docDisplayPGFixture, job *domain.DocDisplayJob, tables []string, mutation string, arguments ...any) {
	test.Helper()
	requireDocToolCatalogPGIsolation(test, fixture)
	mutate := func(bound context.Context, transaction pgx.Tx) error {
		setDocToolCatalogPGDeleteGuards(test, fixture, transaction, tables, false)
		for _, table := range tables {
			if _, err := transaction.Exec(bound, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` DISABLE TRIGGER `+pgx.Identifier{table + "_complete_guard"}.Sanitize()); err != nil {
				return err
			}
		}
		if _, err := transaction.Exec(bound, mutation, arguments...); err != nil {
			return err
		}
		if _, err := transaction.Exec(bound, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		for _, table := range tables {
			if _, err := transaction.Exec(bound, `ALTER TABLE `+pgx.Identifier{table}.Sanitize()+` ENABLE TRIGGER `+pgx.Identifier{table + "_complete_guard"}.Sanitize()); err != nil {
				return err
			}
		}
		setDocToolCatalogPGDeleteGuards(test, fixture, transaction, tables, true)
		return nil
	}
	if job != nil {
		if err := fixture.storage.docDisplayWrite(fixture.ctx, *job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
			if err := bindDocToolCatalogCalculationPG(bound, transaction, *job); err != nil {
				return err
			}
			return mutate(bound, transaction)
		}); err != nil {
			test.Fatal(err)
		}
		return
	}
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	setDocToolCatalogPGWriterGuards(test, fixture, transaction, tables, false)
	if err := mutate(fixture.ctx, transaction); err != nil {
		test.Fatal(err)
	}
	setDocToolCatalogPGWriterGuards(test, fixture, transaction, tables, true)
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
}

func docToolKeyMapStorePrivateImage(test *testing.T, fixture docDisplayPGFixture, scope domain.ContentHash) string {
	test.Helper()
	var result string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'source',(SELECT to_jsonb(source) FROM doc_tool_key_sources_v1 source WHERE scope=$1),
 'observations',(SELECT jsonb_agg(to_jsonb(observation) ORDER BY node_from) FROM doc_tool_key_observations_v1 observation WHERE scope=$1),
 'members',(SELECT jsonb_agg(to_jsonb(member) ORDER BY parent_from,digest) FROM doc_tool_key_members_v1 member WHERE scope=$1))::text`, scope).Scan(&result); err != nil {
		test.Fatal(err)
	}
	return result
}

func docToolKeyMapStoreDuplicates(test *testing.T) (docDisplayPGFixture, domain.DocEventDisplayPreparation, domain.ContentHash, []domain.DocToolKeyItem) {
	test.Helper()
	return docToolKeyMapStoreOutput(test, `{"a":{"old":1},"z":9,"\u0061":{"new":2},"a":{"last":3}}`)
}

func docToolKeyMapStoreOutput(test *testing.T, output string) (docDisplayPGFixture, domain.DocEventDisplayPreparation, domain.ContentHash, []domain.DocToolKeyItem) {
	test.Helper()
	fixture, job, _ := docToolTreeReviewStart(test, domain.CIREvent{Kind: domain.EventToolResult, CallID: test.Name(), Output: map[string]any{"fixture": true}})
	raw := []byte(fmt.Sprintf(`{"kind":"tool_result","call_id":%q,"output":%s}`, test.Name(), output))
	prepared, err := domain.NewDocEventDisplayPreparation(domain.DocRangeEntry{DocEventIndex: domain.DocEventIndex{Hash: domain.HashContent(raw), Length: len(raw)}, Kind: domain.EventToolResult})
	if err != nil {
		test.Fatal(err)
	}
	owner, _ := prepared.Source.Hash()
	sourceRaw, _ := prepared.Source.Encode()
	test.Cleanup(func() {
		if _, err := fixture.storage.pool.Exec(context.Background(), `DELETE FROM doc_display_sources_v1 WHERE source_hash=$1`, owner); err != nil {
			test.Error(err)
		}
	})
	objects := make(map[domain.ContentHash][]byte)
	var nodes []domain.DocToolTreeNode
	var items []domain.DocToolKeyItem
	for attempts := 0; attempts < 100; attempts++ {
		if prepared.Phase == "tool_keys_done" {
			return fixture, prepared, owner, items
		}
		step, err := domain.StepDocEventDisplay(fixture.ctx, prepared, func(read context.Context, from, length int) ([]byte, error) {
			if err := read.Err(); err != nil {
				return nil, err
			}
			if from < 0 || length <= 0 || from > len(raw)-length {
				return nil, domain.ErrIntegrity
			}
			return bytes.Clone(raw[from : from+length]), nil
		}, func(read context.Context, hash domain.ContentHash) ([]byte, error) {
			if err := read.Err(); err != nil {
				return nil, err
			}
			if encoded, found := objects[hash]; found {
				return bytes.Clone(encoded), nil
			}
			return nil, domain.ErrIntegrity
		}, domain.DocToolPreparationReader{Nodes: func(read context.Context, after, limit int) ([]domain.DocToolTreeNode, error) {
			if err := read.Err(); err != nil {
				return nil, err
			}
			var result []domain.DocToolTreeNode
			for _, node := range nodes {
				if node.Value.From > after && len(result) < limit {
					result = append(result, node)
				}
			}
			return result, nil
		}})
		if err != nil {
			test.Fatal(err)
		}
		next := step.Preparation()
		if err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
			tables := []string{"doc_tool_tree_sources_v1", "doc_tool_tree_nodes_v1", "doc_tool_key_sources_v1", "doc_tool_key_observations_v1", "doc_tool_key_members_v1"}
			setDocToolCatalogPGWriterGuards(test, fixture, transaction, tables, false)
			if _, err := transaction.Exec(bound, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, prepared.Source.EventHash, prepared.Source.Profile, owner, sourceRaw); err != nil {
				return err
			}
			if err := putDocDisplayObjects(bound, transaction, owner, step.Objects()); err != nil {
				return err
			}
			if err := fixture.storage.saveDocToolTreeScan(bound, transaction, owner, prepared, next, step); err != nil {
				return err
			}
			if err := fixture.storage.saveDocToolKeyMap(bound, transaction, owner, prepared, next, step); err != nil {
				return err
			}
			setDocToolCatalogPGWriterGuards(test, fixture, transaction, tables, true)
			return nil
		}); err != nil {
			test.Fatal("sealed duplicate-key persistence in the leased job transaction", err)
		}
		for _, object := range step.Objects() {
			objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		if _, scan, found := step.ToolTreeScan(); found {
			nodes = append(nodes, scan.Nodes()...)
			slices.SortFunc(nodes, func(left, right domain.DocToolTreeNode) int { return left.Value.From - right.Value.From })
		}
		if _, keys, found := step.ToolKeyMap(); found {
			items = append(items, keys.Items()...)
		}
		prepared = next
	}
	test.Fatal("sealed duplicate-key fixture did not complete")
	return fixture, prepared, owner, items
}

func TestDocToolKeyMapPGStoreCompletedGuards(test *testing.T) {
	fixture, prepared, _, items := docToolKeyMapStoreDuplicates(test)
	scope, _ := prepared.ToolKeys.Hash(*prepared.ToolTree)
	var originals []domain.DocToolKeyItem
	for _, item := range items {
		if item.Node.Parent == prepared.ToolTree.Root.From && item.Digest == domain.HashContent([]byte(`"a"`)) {
			originals = append(originals, item)
		}
	}
	if len(originals) != 3 {
		test.Fatal("expected three original observations for the same semantic key", len(originals))
	}
	before := docToolKeyMapStorePrivateImage(test, fixture, scope)
	for _, mutation := range []string{
		`DELETE FROM doc_tool_key_members_v1 WHERE scope=$1`,
		`UPDATE doc_tool_key_members_v1 SET first_from=last_from WHERE scope=$1`,
		`UPDATE doc_tool_key_members_v1 SET last_from=first_from WHERE scope=$1`,
		`INSERT INTO doc_tool_key_members_v1 SELECT scope,parent_from,$2,first_from,last_from,is_array_index,array_index FROM doc_tool_key_members_v1 WHERE scope=$1 LIMIT 1`,
		`UPDATE doc_tool_key_observations_v1 SET node_hash=$2 WHERE scope=$1 AND node_from=$3`,
		`UPDATE doc_tool_key_observations_v1 SET digest=$2 WHERE scope=$1 AND node_from=$3`,
		`DELETE FROM doc_tool_key_observations_v1 WHERE scope=$1 AND node_from=$3 AND $2::text IS NOT NULL`,
		`UPDATE doc_tool_key_sources_v1 SET member_count=0 WHERE scope=$1`,
		`UPDATE doc_tool_key_sources_v1 SET member_count=member_count-1 WHERE scope=$1`,
		`UPDATE doc_tool_key_sources_v1 SET bytes=bytes||decode('00','hex') WHERE scope=$1`,
		`UPDATE doc_tool_key_sources_v1 SET scope=scope||'x' WHERE scope=$1`,
		`UPDATE doc_tool_key_sources_v1 SET tree_scope=tree_scope||'x' WHERE scope=$1`,
		`UPDATE doc_tool_key_sources_v1 SET state='scanning',member_count=NULL WHERE scope=$1`,
		`INSERT INTO doc_tool_key_sources_v1 SELECT $2,tree_scope,bytes,'complete',member_count FROM doc_tool_key_sources_v1 WHERE scope=$1`,
	} {
		arguments := []any{scope}
		if strings.Contains(mutation, "$2") {
			arguments = append(arguments, domain.HashContent([]byte("forbidden completed mutation")))
		}
		if strings.Contains(mutation, "$3") {
			arguments = append(arguments, originals[1].Node.Value.From)
		}
		var table string
		switch {
		case strings.Contains(mutation, "doc_tool_key_members_v1"):
			table = "doc_tool_key_members_v1"
		case strings.Contains(mutation, "doc_tool_key_observations_v1"):
			table = "doc_tool_key_observations_v1"
		default:
			table = "doc_tool_key_sources_v1"
		}
		_, err := execDocToolCatalogPGWithoutWriter(test, fixture, []string{table}, mutation, arguments...)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "23514" || docToolKeyMapStorePrivateImage(test, fixture, scope) != before {
			test.Fatal("completed mutation was not rejected with unchanged private data", mutation, err)
		}
	}
	for _, table := range []string{"doc_tool_key_sources_v1", "doc_tool_key_members_v1", "doc_tool_key_observations_v1"} {
		if _, err := execDocToolCatalogPGWithoutWriter(test, fixture, []string{table}, `UPDATE `+pgx.Identifier{table}.Sanitize()+` SET scope=scope WHERE scope=$1`, scope); err != nil {
			test.Fatal("exact completed replay was rejected", table, err)
		}
	}
	if docToolKeyMapStorePrivateImage(test, fixture, scope) != before {
		test.Fatal("exact completed replay changed private data")
	}
}

func TestDocToolKeyMapPGStoreReadRequiresExtrema(test *testing.T) {
	for _, field := range []string{"first_from", "last_from"} {
		test.Run(field, func(test *testing.T) {
			fixture, prepared, owner, items := docToolKeyMapStoreDuplicates(test)
			tree, source := *prepared.ToolTree, *prepared.ToolKeys
			scope, _ := source.Hash(tree)
			var originals []domain.DocToolKeyItem
			for _, item := range items {
				if item.Node.Parent == tree.Root.From && item.Digest == domain.HashContent([]byte(`"a"`)) {
					originals = append(originals, item)
				}
			}
			if len(originals) != 3 {
				test.Fatal("missing duplicate-key extrema fixture")
			}
			cursor := domain.DocToolKeyMember{First: originals[0].Node.Value.From, Last: originals[2]}
			if field == "first_from" {
				cursor.First = originals[1].Node.Value.From
			} else {
				cursor.Last = originals[1]
			}
			docToolKeyMapStoreBypassGuards(test, fixture, nil, []string{"doc_tool_key_members_v1"}, `UPDATE doc_tool_key_members_v1 SET `+pgx.Identifier{field}.Sanitize()+`=$2
 WHERE scope=$1 AND parent_from=$3 AND digest=$4`, scope, originals[1].Node.Value.From, tree.Root.From, originals[0].Digest)
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				transaction := read.Value(repositoryTxKey{}).(*repositoryTx)
				reader := &postgresDocEventDisplayWork{storage: fixture.storage, owner: transaction, source: owner, tree: &tree, keys: &source}
				for _, after := range []*domain.DocToolKeyMember{nil, &cursor} {
					if got, err := reader.ReadToolKeyMembers(read, tree, source, tree.Root.From, after, 64); !errors.Is(err, domain.ErrIntegrity) || got != nil {
						test.Fatal("non-extremal canonical observation was accepted as a member or cursor", after, got, err)
					}
				}
				return nil
			}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestDocToolKeyMapPGStoreGuardsRejectDeleteDuringCompletion(test *testing.T) {
	for _, table := range []string{"doc_tool_key_members_v1", "doc_tool_key_observations_v1"} {
		test.Run(table, func(test *testing.T) {
			fixture, job, prepared := docToolKeyMapStoreStart(test, docToolTreeReviewEvent(test, 3))
			prepared = docToolKeyMapStoreComplete(test, fixture, job, prepared)
			scope, _ := prepared.ToolKeys.Hash(*prepared.ToolTree)
			before := docToolKeyMapStorePrivateImage(test, fixture, scope)
			var count int
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT member_count FROM doc_tool_key_sources_v1 WHERE scope=$1`, scope).Scan(&count); err != nil {
				test.Fatal(err)
			}
			docToolKeyMapStoreBypassGuards(test, fixture, &job, []string{"doc_tool_key_sources_v1"},
				`UPDATE doc_tool_key_sources_v1 SET state='scanning',member_count=NULL WHERE scope=$1`, scope)
			writerContext, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
			defer cancel()
			writer, err := fixture.storage.pool.Begin(writerContext)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(writer)
			if err := fixture.storage.docDisplayWrite(fixture.ctx, job, time.Time{}, func(bound context.Context, publisher pgx.Tx) error {
				if err := bindDocToolCatalogCalculationPG(bound, publisher, job); err != nil {
					return err
				}
				var state string
				if err := publisher.QueryRow(bound, `SELECT state FROM doc_tool_key_sources_v1 WHERE scope=$1 FOR UPDATE`, scope).Scan(&state); err != nil || state != "scanning" {
					test.Fatal("publication did not lock the scanning source", state, err)
				}
				_, err := writer.Exec(writerContext, `DELETE FROM `+pgx.Identifier{table}.Sanitize()+` WHERE scope=$1`, scope)
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23514" || failure.Message != "document catalog deletes require a bound collector" {
					test.Fatal("concurrent raw deletion was not rejected before publication", err)
				}
				_, err = publisher.Exec(bound, `UPDATE doc_tool_key_sources_v1 SET state='complete',member_count=$2 WHERE scope=$1`, scope, count)
				return err
			}); err != nil {
				test.Fatal(err)
			}
			rollbackPG(writer)
			if docToolKeyMapStorePrivateImage(test, fixture, scope) != before {
				test.Fatal("late ready-state guard left partial observations or members")
			}
		})
	}
}
