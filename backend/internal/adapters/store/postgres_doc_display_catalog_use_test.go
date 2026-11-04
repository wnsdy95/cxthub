//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docCatalogUseBoundPG(test *testing.T, fixture docDisplayPGFixture) docDisplayPGFixture {
	test.Helper()
	if _, owned := docDisplayOwnedFixturesPG.Load(fixture.storage.pool.Config().ConnConfig.Database); !owned {
		test.Fatal("catalog-use tests require an owned isolated database")
	}
	ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
	test.Cleanup(cancel)
	fixture.ctx = ctx
	return fixture
}

func docCatalogUseFixturePG(test *testing.T) (docDisplayPGFixture, domain.DocDisplayJob, domain.DocEventDisplayPreparation) {
	test.Helper()
	isolated, _ := rootProjectionIsolatedPG(test)
	fixture, job, prepared := docToolOrdersPGStart(test, map[string]any{"first": true, "second": "value"})
	if fixture.storage.pool.Config().ConnConfig.Database != isolated.pool.Config().ConnConfig.Database ||
		prepared.Phase != "tool_orders" || prepared.ToolTree == nil || prepared.ToolKeys == nil || prepared.ToolOrders == nil {
		test.Fatal("fixture did not retain its isolated private preparation", prepared.Phase)
	}
	return docCatalogUseBoundPG(test, fixture), job, prepared
}

func docCatalogUseCheckpointPG(test *testing.T, prepared domain.DocEventDisplayPreparation) domain.ContentHash {
	test.Helper()
	raw, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return domain.HashContent(raw)
}

func docCatalogUseBeginPG(test *testing.T, fixture docDisplayPGFixture) pgx.Tx {
	test.Helper()
	transaction, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { rollbackPG(transaction) })
	return transaction
}

func docCatalogUseTryPG(test *testing.T, fixture docDisplayPGFixture, transaction pgx.Tx, target domain.DocDisplayTarget, expected bool) {
	test.Helper()
	var available bool
	err := transaction.QueryRow(fixture.ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended(
 'cxthub-doc-catalog-use:'||$1::text||':'||$2::text,0))`, target.Event.Hash, target.DisplayProfile()).Scan(&available)
	if err != nil || available != expected {
		test.Fatal("independent exclusive catalog-use probe", available, expected, err)
	}
}

func docCatalogUseProbePG(test *testing.T, fixture docDisplayPGFixture, target domain.DocDisplayTarget, expected bool) {
	test.Helper()
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	docCatalogUseTryPG(test, fixture, transaction, target, expected)
}

func docCatalogUseWriterFencePG(test *testing.T, fixture docDisplayPGFixture, target domain.DocDisplayTarget, source domain.ContentHash) {
	test.Helper()
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `SELECT * FROM cxthub_lock_doc_catalog_affinities(
 ARRAY[$1]::text[],ARRAY[$2]::text[],ARRAY[]::text[])`, target.Event.Hash, target.DisplayProfile()); err != nil {
		test.Fatal("read use or skipped collector retained the writer affinity", err)
	}
	var actual domain.ContentHash
	if err := transaction.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_sources_v1
 WHERE source_hash=$1 FOR NO KEY UPDATE NOWAIT`, source).Scan(&actual); err != nil || actual != source {
		test.Fatal("read use or skipped collector retained Source", actual, err)
	}
}

func docCatalogUseReadPG(test *testing.T, read context.Context, reader *postgresDocEventDisplayWork, prepared domain.DocEventDisplayPreparation) {
	test.Helper()
	raw, err := reader.LoadObject(read, prepared.Active)
	if err != nil || domain.HashContent(raw) != prepared.Active {
		test.Fatal("actual work reader lost its immutable checkpoint", err)
	}
	tree, keys := *prepared.ToolTree, *prepared.ToolKeys
	nodes, err := reader.ReadToolTreeNodes(read, tree, tree.Root.From-1, 1)
	if err != nil || len(nodes) != 1 || nodes[0].Value != tree.Root || nodes[0].Depth != 0 {
		test.Fatal("actual work reader lost its private root", nodes, err)
	}
	members, err := reader.ReadToolKeyMembers(read, tree, keys, tree.Root.From, nil, 2)
	if err != nil || len(members) != 2 {
		test.Fatal("actual work reader lost its private key members", members, err)
	}
	order, err := reader.ReadToolOrderSource(read, tree, keys, tree.Root.From)
	if err != nil || order.Parent.Value != tree.Root || order.Count != 2 {
		test.Fatal("actual work reader lost its private order source", order, err)
	}
}

func docCatalogUseImagePG(test *testing.T, fixture docDisplayPGFixture) string {
	test.Helper()
	var result string
	err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'display',$1::jsonb,
 'census',(SELECT jsonb_agg(to_jsonb(census) ORDER BY source_hash) FROM doc_display_catalog_census_v1 census),
 'trees',(SELECT jsonb_agg(to_jsonb(tree) ORDER BY scope) FROM doc_tool_tree_sources_v1 tree),
 'nodes',(SELECT jsonb_agg(to_jsonb(node) ORDER BY scope,node_from) FROM doc_tool_tree_nodes_v1 node),
 'keys',(SELECT jsonb_agg(to_jsonb(keys) ORDER BY scope) FROM doc_tool_key_sources_v1 keys),
 'observations',(SELECT jsonb_agg(to_jsonb(observation) ORDER BY scope,node_from) FROM doc_tool_key_observations_v1 observation),
 'members',(SELECT jsonb_agg(to_jsonb(member) ORDER BY scope,parent_from,digest) FROM doc_tool_key_members_v1 member),
 'orders',(SELECT jsonb_agg(to_jsonb(orders) ORDER BY scope) FROM doc_tool_order_sources_v1 orders),
 'plans',(SELECT jsonb_agg(to_jsonb(plan) ORDER BY scope,parent_from) FROM doc_tool_order_plans_v1 plan),
 'edges',(SELECT jsonb_agg(to_jsonb(edge) ORDER BY source_hash,parent_hash,child_hash) FROM doc_display_object_edges_v1 edge),
 'marks',(SELECT jsonb_agg(to_jsonb(mark) ORDER BY source_hash,hash) FROM doc_display_object_marks_v1 mark))::text`,
		docCatalogAdmissionImagePG(test, fixture.ctx, fixture.storage.pool)).Scan(&result)
	if err != nil {
		test.Fatal(err)
	}
	return result
}

func docCatalogUseQueuePG(test *testing.T, fixture docDisplayPGFixture, prepared domain.DocEventDisplayPreparation) domain.ContentHash {
	test.Helper()
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, source); err != nil {
		test.Fatal(err)
	}
	return source
}

func docCatalogUseSkippedPG(test *testing.T, fixture docDisplayPGFixture, source domain.ContentHash, before string) {
	test.Helper()
	step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if !errors.Is(err, domain.ErrNotFound) || step != (domain.DocDisplayMaintenanceStep{}) {
		test.Fatal("actual collector did not skip pinned read use", step, err)
	}
	docMaintenanceSelectionCursorPG(test, fixture, source)
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("skipped collector changed Source, census, work, or private data beyond its cursor")
	}
}

func docCatalogUseProgressPG(test *testing.T, fixture docDisplayPGFixture, source domain.ContentHash) {
	test.Helper()
	step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
	if err != nil || step.Source != source || step.Phase != "catalog_census" ||
		step.CatalogJobs != 1 || step.CatalogCensus != "protected" {
		test.Fatal("actual collector did not resume after read use ended", step, err)
	}
}

func TestDocDisplayCatalogUsePGSharedReadersAndIndependentWriters(test *testing.T) {
	fixture, job, prepared := docCatalogUseFixturePG(test)
	if key := docDisplayCatalogUseKey(job.Target.Event.Hash, job.Target.DisplayProfile()); key != "cxthub-doc-catalog-use:"+string(job.Target.Event.Hash)+":"+job.Target.DisplayProfile() {
		test.Fatal("catalog-use key changed namespaces", key)
	}
	checkpoint := docCatalogUseCheckpointPG(test, prepared)
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	step := docToolOrdersPGStep(test, fixture, job, prepared, nil)
	var saved domain.DocEventDisplayPreparation
	err = fixture.storage.WithinReadSnapshot(fixture.ctx, func(first context.Context) error {
		work, err := fixture.storage.OpenDocEventDisplayWork(first, job, checkpoint)
		if err != nil {
			return err
		}
		firstReader := work.(*postgresDocEventDisplayWork)
		docCatalogUseReadPG(test, first, firstReader, prepared)
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(second context.Context) error {
			work, err := fixture.storage.OpenDocEventDisplayWork(second, job, checkpoint)
			if err != nil {
				return err
			}
			secondReader := work.(*postgresDocEventDisplayWork)
			if firstReader.owner.Tx.Conn().PgConn().PID() == secondReader.owner.Tx.Conn().PgConn().PID() {
				test.Fatal("readers did not use independent PostgreSQL transactions")
			}
			docCatalogUseReadPG(test, second, secondReader, prepared)
			docCatalogUseProbePG(test, fixture, job.Target, false)
			docCatalogUseWriterFencePG(test, fixture, job.Target, source)
			if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), time.Minute); err != nil {
				return err
			}
			saved, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(saved, step.Preparation()) || docCatalogUseCheckpointPG(test, saved) == checkpoint {
				test.Fatal("valid writer did not advance the real preparation during read use")
			}
			docCatalogUseReadPG(test, first, firstReader, prepared)
			docCatalogUseReadPG(test, second, secondReader, prepared)
			return nil
		}); err != nil {
			return err
		}
		docCatalogUseProbePG(test, fixture, job.Target, false)
		return nil
	})
	if err != nil {
		test.Fatal("shared readers serialized each other or a valid writer", err)
	}
	docCatalogUseProbePG(test, fixture, job.Target, true)
	next := docToolOrdersPGStep(test, fixture, job, saved, nil)
	holder := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(holder)
	docCatalogUseTryPG(test, fixture, holder, job.Target, true)
	if err := fixture.storage.RenewDocDisplay(fixture.ctx, job, time.Now(), time.Minute); err != nil {
		test.Fatal("writer renewal acquired catalog read use", err)
	}
	loaded, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil || !reflect.DeepEqual(loaded, saved) {
		test.Fatal("writer-side preparation load acquired catalog read use", err)
	}
	if _, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, next); err != nil {
		test.Fatal("valid save acquired shared or exclusive catalog read use", err)
	}
}

func TestDocDisplayCatalogUsePGCollectorSkipsUntilCommitOrRollback(test *testing.T) {
	for _, finish := range []string{"commit", "rollback"} {
		test.Run(finish, func(test *testing.T) {
			fixture, job, prepared := docCatalogUseFixturePG(test)
			source := docCatalogUseQueuePG(test, fixture, prepared)
			before := docCatalogUseImagePG(test, fixture)
			abort := errors.New("rollback actual catalog reader")
			err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
				if err != nil {
					return err
				}
				docCatalogUseReadPG(test, read, work.(*postgresDocEventDisplayWork), prepared)
				docCatalogUseSkippedPG(test, fixture, source, before)
				docCatalogUseWriterFencePG(test, fixture, job.Target, source)
				docCatalogUseProbePG(test, fixture, job.Target, false)
				if finish == "rollback" {
					return abort
				}
				return nil
			})
			if finish == "rollback" && !errors.Is(err, abort) || finish == "commit" && err != nil {
				test.Fatal("unexpected snapshot completion", finish, err)
			}
			docCatalogUseProbePG(test, fixture, job.Target, true)
			docCatalogUseProgressPG(test, fixture, source)
		})
	}
}

func TestDocDisplayCatalogUsePGCollectorExclusiveGate(test *testing.T) {
	fixture, job, prepared := docCatalogUseFixturePG(test)
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	before := docCatalogUseImagePG(test, fixture)
	collector := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(collector)
	locked, _, err := lockDocDisplayMaintenanceSource(fixture.ctx, collector, source)
	if err != nil || locked.Hash != source || locked.EventHash != job.Target.Event.Hash || locked.Profile != job.Target.DisplayProfile() {
		test.Fatal("actual collector did not acquire its source and use gate", locked, err)
	}
	checkpoint := docCatalogUseCheckpointPG(test, prepared)
	if reader, err := fixture.storage.OpenDocEventDisplayWork(fixture.ctx, job, checkpoint); !errors.Is(err, domain.ErrConflict) || reader != nil {
		test.Fatal("use gate bypassed the owned read-only transaction requirement", reader, err)
	}
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		reader, err := fixture.storage.OpenDocEventDisplayWork(read, job, checkpoint)
		if !errors.Is(err, domain.ErrResourceOverloaded) || reader != nil {
			test.Fatal("collector use gate returned private data instead of overload", reader, err)
		}
		stale := job
		stale.Version++
		if reader, err := fixture.storage.OpenDocEventDisplayWork(read, stale, checkpoint); !errors.Is(err, domain.ErrConflict) || reader != nil {
			test.Fatal("use gate weakened live-job authority", reader, err)
		}
		if reader, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent([]byte("foreign preparation"))); !errors.Is(err, domain.ErrConflict) || reader != nil {
			test.Fatal("use gate weakened checkpoint authority", reader, err)
		}
		foreign := &PostgresStore{pool: fixture.storage.pool}
		if reader, err := foreign.OpenDocEventDisplayWork(read, job, checkpoint); !errors.Is(err, domain.ErrConflict) || reader != nil {
			test.Fatal("use gate accepted another store's transaction", reader, err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("collector gate or rejected opens changed durable state")
	}
	if err := collector.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	docCatalogUseProbePG(test, fixture, job.Target, true)
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		work, err := fixture.storage.OpenDocEventDisplayWork(read, job, checkpoint)
		if err != nil {
			return err
		}
		docCatalogUseReadPG(test, read, work.(*postgresDocEventDisplayWork), prepared)
		return nil
	}); err != nil {
		test.Fatal("private open failed after collector rollback", err)
	}
	docCatalogUseProbePG(test, fixture, job.Target, true)
}

func TestDocDisplayCatalogUsePGCancellationReleasesPins(test *testing.T) {
	fixture, job, prepared := docCatalogUseFixturePG(test)
	source := docCatalogUseQueuePG(test, fixture, prepared)
	before := docCatalogUseImagePG(test, fixture)
	checkpoint := docCatalogUseCheckpointPG(test, prepared)
	reading, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	opened := make(chan error, 1)
	finished := make(chan error, 1)
	cancelled := make(chan struct{})
	unwind := make(chan struct{})
	release := sync.OnceFunc(func() { close(unwind) })
	defer release()
	go func() {
		finished <- fixture.storage.WithinReadSnapshot(reading, func(read context.Context) error {
			work, err := fixture.storage.OpenDocEventDisplayWork(read, job, checkpoint)
			if err == nil {
				var raw []byte
				raw, err = work.LoadObject(read, prepared.Active)
				if err == nil && domain.HashContent(raw) != prepared.Active {
					err = domain.ErrIntegrity
				}
			}
			opened <- err
			if err != nil {
				return err
			}
			<-read.Done()
			close(cancelled)
			select {
			case <-unwind:
			case <-fixture.ctx.Done():
			}
			return read.Err()
		})
	}()
	select {
	case err := <-opened:
		if err != nil {
			test.Fatal("actual cancellable work reader did not open", err)
		}
	case err := <-finished:
		test.Fatal("read transaction ended before opening", err)
	case <-fixture.ctx.Done():
		test.Fatal(fixture.ctx.Err())
	}
	docCatalogUseProbePG(test, fixture, job.Target, false)
	docCatalogUseSkippedPG(test, fixture, source, before)
	cancel()
	select {
	case <-cancelled:
	case <-fixture.ctx.Done():
		test.Fatal("read callback did not observe cancellation", fixture.ctx.Err())
	}
	docCatalogUseProbePG(test, fixture, job.Target, false)
	docCatalogUseSkippedPG(test, fixture, source, before)
	release()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			test.Fatal("actual read cancellation was swallowed", err)
		}
	case <-fixture.ctx.Done():
		test.Fatal("cancelled read transaction retained its pin", fixture.ctx.Err())
	}
	docCatalogUseProbePG(test, fixture, job.Target, true)
	docCatalogUseWriterFencePG(test, fixture, job.Target, source)
	docCatalogUseProgressPG(test, fixture, source)
}

func TestDocDisplayCatalogUsePGNonPrivateWorkSkipsUse(test *testing.T) {
	for _, phase := range []string{"discovery", "cas_only"} {
		test.Run(phase, func(test *testing.T) {
			rootProjectionIsolatedPG(test)
			fixture := docDisplayPG(test, domain.CIREvent{Kind: domain.EventReasoning, Seq: 1, RedactedSummary: "small immutable-only reasoning"})
			enqueueDocEventDisplayQueue(test, fixture, 0)
			job := fixture.claim(test)
			prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
			if err != nil {
				test.Fatal(err)
			}
			fixture = docCatalogUseBoundPG(test, fixture)
			if phase == "cas_only" {
				step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
				prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
				if err != nil || prepared.Active == "" {
					test.Fatal("fixture did not create a real immutable checkpoint", prepared.Phase, err)
				}
			}
			if prepared.ToolTree != nil || prepared.ToolKeys != nil || prepared.ToolOrders != nil {
				test.Fatal("non-private fixture unexpectedly requires a catalog")
			}
			before := docCatalogUseImagePG(test, fixture)
			holder := docCatalogUseBeginPG(test, fixture)
			defer rollbackPG(holder)
			docCatalogUseTryPG(test, fixture, holder, job.Target, true)
			if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
				work, err := fixture.storage.OpenDocEventDisplayWork(read, job, docCatalogUseCheckpointPG(test, prepared))
				if err != nil {
					return err
				}
				if phase == "cas_only" {
					raw, err := work.LoadObject(read, prepared.Active)
					if err != nil || domain.HashContent(raw) != prepared.Active {
						test.Fatal("immutable-only work lost source-scoped data under the use gate", err)
					}
				}
				return nil
			}); err != nil {
				test.Fatal("non-private actual work open acquired catalog use", phase, err)
			}
			if docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("non-private work open changed durable state")
			}
		})
	}
}

func TestDocDisplayCatalogUsePGFailedSourceProofDoesNotPin(test *testing.T) {
	for _, failure := range []string{"foreign_event", "corrupt_source", "source_hash", "noncanonical_preparation"} {
		test.Run(failure, func(test *testing.T) {
			fixture, job, prepared := docCatalogUseFixturePG(test)
			source, err := prepared.Source.Hash()
			if err != nil {
				test.Fatal(err)
			}
			checkpoint := docCatalogUseCheckpointPG(test, prepared)
			foreign := job.Target
			foreign.Event.Hash = domain.HashContent([]byte("foreign catalog event"))
			switch failure {
			case "foreign_event":
				docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool,
					`UPDATE doc_display_sources_v1 SET event_hash=$2 WHERE source_hash=$1`, source, foreign.Event.Hash)
			case "corrupt_source":
				docCatalogAdmissionExecPG(test, fixture.ctx, fixture.storage.pool,
					`UPDATE doc_display_sources_v1 SET bytes=$2 WHERE source_hash=$1`, source, []byte(`{}`))
			case "source_hash", "noncanonical_preparation":
				changed := prepared
				if failure == "source_hash" {
					changed.Source.Kind = domain.EventToolCall
				}
				raw, err := changed.Encode()
				if err != nil {
					test.Fatal(err)
				}
				if failure == "noncanonical_preparation" {
					raw = append(bytes.Clone(raw), '\n')
				}
				checkpoint = domain.HashContent(raw)
				tag, err := execDocToolCatalogPG(fixture, job, `UPDATE doc_display_jobs_v1
 SET preparation_hash=$4,preparation=$5 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index, checkpoint, raw)
				if err != nil || tag.RowsAffected() != 1 {
					test.Fatal("bound writer could not seed invalid preparation with guards intact", tag, err)
				}
			}
			before := docCatalogUseImagePG(test, fixture)
			for _, busy := range []bool{false, true} {
				holder := docCatalogUseBeginPG(test, fixture)
				if busy {
					docCatalogUseTryPG(test, fixture, holder, job.Target, true)
				}
				if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
					reader, err := fixture.storage.OpenDocEventDisplayWork(read, job, checkpoint)
					if !errors.Is(err, domain.ErrIntegrity) || reader != nil {
						test.Fatal("invalid source proof became read data or a use-gate overload", busy, reader, err)
					}
					if !busy {
						docCatalogUseProbePG(test, fixture, job.Target, true)
						docCatalogUseProbePG(test, fixture, foreign, true)
					}
					return nil
				}); err != nil {
					test.Fatal(err)
				}
				if err := holder.Rollback(fixture.ctx); err != nil {
					test.Fatal(err)
				}
				docCatalogUseProbePG(test, fixture, job.Target, true)
			}
			if docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("failed source proof changed durable state")
			}
		})
	}
}
