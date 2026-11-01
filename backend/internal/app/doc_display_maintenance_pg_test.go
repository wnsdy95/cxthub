//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docDisplayLeaseMutationPG(ctx context.Context, pool *pgxpool.Pool, repository domain.ContentHash, statement string, arguments ...any) (pgconn.CommandTag, error) {
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	if _, err := transaction.Exec(ctx, `SELECT cxthub_bind_doc_catalog_control(repo_id,doc_hash,event_index,profile,version)
 FROM doc_display_jobs_v1 WHERE repo_id=$1 AND state='running'`, repository); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := transaction.Exec(ctx, statement, arguments...)
	if err != nil {
		return tag, err
	}
	return tag, transaction.Commit(ctx)
}

func docDisplayCatalogBusyPG(err error) bool {
	var failure *pgconn.PgError
	return errors.As(err, &failure) && failure.Code == "55P03" && failure.Message == "document catalog admission is busy"
}

func retryDocDisplayCatalogBusyPG(ctx context.Context, operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := operation(ctx)
		if !docDisplayCatalogBusyPG(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func drainDocDisplayMaintenancePG(test *testing.T, fixture docViewPGFixture) (int, int64) {
	test.Helper()
	deleted := 0
	var deletedBytes int64
	for count := 0; count < 5000; count++ {
		step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
		if errors.Is(err, domain.ErrNotFound) {
			return deleted, deletedBytes
		}
		if err != nil {
			test.Fatal("maintenance", count, err)
		}
		deleted += step.Deleted
		deletedBytes += step.DeletedBytes
	}
	test.Fatal("maintenance failed to converge")
	return 0, 0
}

func docDisplayStorageFootprintPG(test *testing.T, fixture docViewPGFixture) string {
	test.Helper()
	type footprint struct {
		AccountingScope        string `json:"accounting_scope"`
		Rows                   int64  `json:"rows"`
		StoredTupleBytes       int64  `json:"stored_tuple_bytes"`
		LogicalPayloadBytes    int64  `json:"logical_payload_bytes"`
		AllocatedRelationBytes int64  `json:"allocated_relation_bytes"`
		AllocatedIndexBytes    int64  `json:"allocated_index_bytes"`
	}
	result := make(map[string]footprint)
	for _, table := range []string{"doc_display_sources_v1", "doc_display_objects_v1", "doc_display_generations_v1", "doc_display_jobs_v1", "doc_display_object_edges_v1", "doc_display_object_marks_v1", "doc_display_maintenance_v1", "doc_display_maintenance_cursor_v1", "doc_display_catalog_admissions_v1", "doc_display_catalog_census_v1", "doc_display_catalog_lifecycle_v1", "doc_tool_tree_sources_v1", "doc_tool_tree_nodes_v1", "doc_tool_key_sources_v1", "doc_tool_key_observations_v1", "doc_tool_key_members_v1", "doc_tool_order_sources_v1", "doc_tool_order_plans_v1"} {
		payload := "0::bigint"
		value := footprint{AccountingScope: "isolated_fixture"}
		switch table {
		case "doc_display_jobs_v1":
			payload = "coalesce(sum(coalesce(octet_length(checkpoint),0)+coalesce(octet_length(preparation),0)+coalesce(octet_length(discovery),0)),0)"
		case "doc_display_maintenance_cursor_v1":
			payload = "coalesce(sum(octet_length(after_source)),0)"
			value.AccountingScope = "global_singleton"
		case "doc_display_object_edges_v1", "doc_display_object_marks_v1", "doc_display_maintenance_v1", "doc_display_catalog_admissions_v1", "doc_display_catalog_census_v1", "doc_display_catalog_lifecycle_v1", "doc_tool_key_observations_v1", "doc_tool_key_members_v1":
		default:
			payload = "coalesce(sum(octet_length(bytes)),0)"
		}
		statement := `SELECT count(*),coalesce(sum(pg_column_size(entry)),0),` + payload + `,pg_total_relation_size($1::regclass),pg_indexes_size($1::regclass) FROM ` + pgx.Identifier{table}.Sanitize() + ` entry`
		if err := fixture.pool.QueryRow(fixture.ctx, statement, table).Scan(&value.Rows, &value.StoredTupleBytes, &value.LogicalPayloadBytes, &value.AllocatedRelationBytes, &value.AllocatedIndexBytes); err != nil {
			test.Fatal("storage footprint", table, err)
		}
		result[table] = value
	}
	raw, err := json.Marshal(result)
	if err != nil {
		test.Fatal(err)
	}
	return string(raw)
}

func TestPGDocDisplayMaintenanceActualWorkerRestartAndPublishedReads(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, kind := range []string{"generic", "Write", "Edit", "Bash", "result", "reasoning", "message", "PRE"} {
			test.Run(fmt.Sprintf("%s/legacy=%t", kind, legacy), func(test *testing.T) {
				test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, context.Background()))
				fixture := newDocDisplayPGFixture(test)
				fixture.service.ConfigureDocumentDisplay(fixture.storage)
				rows := make([]any, 100)
				for index := range rows {
					rows[index] = map[string]any{"ordinal": index, "text": "marker", "nested": []any{true, nil, "value"}}
				}
				event := domain.CIREvent{CallID: string(fixture.repo)}
				profile := domain.DocDisplayProjectionProfile
				switch kind {
				case "generic":
					event.Kind, event.ToolName, event.Input = domain.EventToolCall, "generic", map[string]any{"rows": rows}
				case "Write":
					event.Kind, event.ToolName, event.Input = domain.EventToolCall, "Write", map[string]any{"file_path": "/a/b/c/d", "content": strings.Repeat("line marker\n", 5000)}
				case "Edit":
					event.Kind, event.ToolName, event.Input = domain.EventToolCall, "Edit", map[string]any{"file_path": "/a/b/c/d", "old_string": strings.Repeat("before marker\n", 5000), "new_string": strings.Repeat("after marker\n", 5000)}
				case "Bash":
					event.Kind, event.ToolName, event.Input = domain.EventToolCall, "Bash", map[string]any{"command": strings.Repeat("echo marker\n", 5000)}
				case "result":
					event.Kind, event.Output = domain.EventToolResult, rows
				case "reasoning":
					event.Kind, event.RedactedSummary = domain.EventReasoning, strings.Repeat("**marker** ", 8000)
				case "message":
					event.Kind, event.Role, event.Blocks = domain.EventMessage, domain.RoleAssistant, []domain.ContentBlock{{Type: "text", Text: strings.Repeat("public marker ", 5000)}}
				case "PRE":
					event.Kind, event.Output = domain.EventToolResult, strings.Repeat("marker\n", 10000)
					profile = ""
				}
				var hash domain.ContentHash
				if legacy {
					hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
					prepareLegacyFindGenerationPG(test, fixture, hash)
				} else {
					hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
				}
				target := docDisplayTargetPG(test, fixture, hash, 0)
				target.Profile = profile
				if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
					test.Fatal(err)
				}
				complete := false
				activeDeleted := 0
				var activeDeletedBytes int64
				for batch := 0; batch < 200; batch++ {
					restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
					restarted.ConfigureDocumentDisplay(fixture.storage)
					if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
						test.Fatal("actual worker", batch, err)
					}
					for maintenance := 0; maintenance < 12; maintenance++ {
						step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
						if errors.Is(err, domain.ErrNotFound) {
							break
						}
						if err != nil {
							test.Fatal("active-job maintenance", batch, err)
						}
						activeDeleted += step.Deleted
						activeDeletedBytes += step.DeletedBytes
					}
					if profile == "" {
						_, err := readDocDisplayGenerationPG(fixture, target)
						complete = err == nil
						if err != nil && !errors.Is(err, domain.ErrNotFound) {
							test.Fatal(err)
						}
					} else {
						_, err := readDocEventDisplayGenerationPG(fixture, target)
						complete = err == nil
						if err != nil && !errors.Is(err, domain.ErrNotFound) {
							test.Fatal(err)
						}
					}
					if complete {
						break
					}
				}
				if !complete {
					test.Fatal("worker did not finish across maintenance/restart")
				}
				var owner domain.ContentHash
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, fixture.repo, hash, target.DisplayProfile()).Scan(&owner); err != nil {
					test.Fatal(err)
				}
				readPages := func() []domain.DocDisplayPage {
					var pages []domain.DocDisplayPage
					_, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (bool, error) {
						if profile == "" {
							generation, err := fixture.storage.ReadDocDisplayGeneration(read, target)
							if err != nil {
								return false, err
							}
							for index := 0; index < generation.Pages.Count; index++ {
								page, err := fixture.storage.ReadDocDisplayPage(read, target, index)
								if err != nil {
									return false, err
								}
								pages = append(pages, page)
							}
						} else {
							generation, err := fixture.storage.ReadDocEventDisplayGeneration(read, target)
							if err != nil {
								return false, err
							}
							for index := 0; index < generation.Pages.Count; index++ {
								page, err := fixture.storage.ReadDocEventDisplayPage(read, target, index)
								if err != nil {
									return false, err
								}
								pages = append(pages, page)
							}
						}
						return true, nil
					})
					if err != nil {
						test.Fatal("published page read", err)
					}
					return pages
				}
				before := readPages()
				test.Logf("storage_before_final_maintenance=%s", docDisplayStorageFootprintPG(test, fixture))
				if _, err := fixture.pool.Exec(fixture.ctx, `SELECT cxthub_queue_doc_display_maintenance($1)`, owner); err != nil {
					test.Fatal(err)
				}
				deleted, deletedBytes := drainDocDisplayMaintenancePG(test, fixture)
				test.Logf("storage_after_final_maintenance=%s", docDisplayStorageFootprintPG(test, fixture))
				after := readPages()
				if len(before) == 0 || !reflect.DeepEqual(before, after) {
					test.Fatal("cleanup changed a published page or its original byte mapping")
				}
				if profile != "" {
					var raw []byte
					if err := fixture.pool.QueryRow(fixture.ctx, `SELECT preparation FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, fixture.repo, hash, profile).Scan(&raw); err != nil {
						test.Fatal(err)
					}
					state, err := domain.DecodeDocEventDisplayPreparation(domain.HashContent(raw), raw)
					if err != nil {
						test.Fatal(err)
					}
					if _, err := domain.ValidateDocEventDisplayPublication(fixture.ctx, state, func(ctx context.Context, object domain.ContentHash) ([]byte, error) {
						var value []byte
						err := fixture.pool.QueryRow(ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, owner, object).Scan(&value)
						return value, err
					}); err != nil {
						test.Fatal("cleanup lost final policy evidence or typed children", err)
					}
				}
				var bytes, objects int64
				if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*),coalesce(sum(octet_length(bytes)),0) FROM doc_display_objects_v1 WHERE source_hash=$1`, owner).Scan(&objects, &bytes); err != nil {
					test.Fatal(err)
				}
				test.Logf("kind=%s legacy=%t active_deleted_objects=%d active_deleted_bytes=%d final_deleted_objects=%d final_deleted_bytes=%d retained_objects=%d retained_logical_bytes=%d", kind, legacy, activeDeleted, activeDeletedBytes, deleted, deletedBytes, objects, bytes)
			})
		}
	}
}

func TestPGDocDisplayMaintenanceSameSourceRejectedAndActiveJobs(test *testing.T) {
	test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, context.Background()))
	left, right := newDocDisplayPGFixture(test), newDocDisplayPGFixture(test)
	rows := make([]any, 80)
	for index := range rows {
		rows[index] = map[string]any{"ordinal": index, "nested": []any{true, nil, "shared"}}
	}
	event := domain.CIREvent{Kind: domain.EventToolCall, ToolName: "generic", CallID: "same-source", Input: map[string]any{"rows": rows}}
	claim := func(fixture docViewPGFixture) domain.DocDisplayJob {
		fixture.service.ConfigureDocumentDisplay(fixture.storage)
		hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
		target := docDisplayTargetPG(test, fixture, hash, 0)
		target.Profile = domain.DocDisplayProjectionProfile
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
			test.Fatal(err)
		}
		job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
		if err != nil || job.RepoID != fixture.repo {
			test.Fatal("claim", job, err)
		}
		return job
	}
	leftJob, rightJob := claim(left), claim(right)
	for _, current := range []struct {
		fixture docViewPGFixture
		job     domain.DocDisplayJob
	}{{left, leftJob}, {right, rightJob}} {
		if complete, err := current.fixture.service.buildDocEventDisplay(current.fixture.ctx, current.job); err != nil || complete {
			test.Fatal("initial independent batch", complete, err)
		}
	}
	paused, err := right.storage.LoadDocEventDisplay(right.ctx, rightJob)
	if err != nil {
		test.Fatal(err)
	}
	pausedRaw, err := paused.Encode()
	if err != nil {
		test.Fatal(err)
	}
	owner, err := paused.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	rooted := make(map[domain.ContentHash][]byte)
	var walk func([]byte)
	walk = func(raw []byte) {
		candidates, err := domain.DocDisplayHashCandidates(right.ctx, raw)
		if err != nil {
			test.Fatal(err)
		}
		for _, hash := range candidates {
			if _, exists := rooted[hash]; exists {
				continue
			}
			var child []byte
			if err := right.pool.QueryRow(right.ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, owner, hash).Scan(&child); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				test.Fatal(err)
			}
			rooted[hash] = child
			walk(child)
		}
	}
	walk(pausedRaw)
	if len(rooted) == 0 {
		test.Fatal("fixture did not create any independently retained preparation objects")
	}
	rightJob.State, rightJob.Reason, rightJob.NextAttempt = "rejected", "synthetic_pause", time.Now().UTC()
	rightJob.LeaseUntil = time.Time{}
	if err := right.storage.FinishDocDisplay(right.ctx, rightJob, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	complete := false
	for batch := 0; batch < 100 && !complete; batch++ {
		complete, err = left.service.buildDocEventDisplay(left.ctx, leftJob)
		if err != nil {
			test.Fatal("active same-source job", batch, err)
		}
		drainDocDisplayMaintenancePG(test, left)
	}
	if !complete {
		test.Fatal("active job failed to finish")
	}
	for hash, before := range rooted {
		var after []byte
		if err := right.pool.QueryRow(right.ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, owner, hash).Scan(&after); err != nil || !reflect.DeepEqual(before, after) {
			test.Fatal("another repository's rejected checkpoint lost a dependency", hash, err)
		}
	}
	var currentRaw []byte
	var state string
	if err := right.pool.QueryRow(right.ctx, `SELECT preparation,state FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, right.repo, rightJob.DocHash, rightJob.Target.DisplayProfile()).Scan(&currentRaw, &state); err != nil || state != "rejected" || !reflect.DeepEqual(currentRaw, pausedRaw) {
		test.Fatal("cleanup rewrote an unfinished checkpoint", state, err)
	}
	if _, err := readDocEventDisplayGenerationPG(right, rightJob.Target); err != nil {
		test.Fatal("authorized second repository could not read shared completed generation", err)
	}
}

func TestPGDocDisplayMaintenanceChecksRawJobRootAtSweep(test *testing.T) {
	isolatedDSN := a11IsolatedPGDSN(test, context.Background())
	test.Setenv("CXT_TEST_DSN", isolatedDSN)
	fixture := newDocDisplayPGFixture(test)
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	event := domain.CIREvent{Kind: domain.EventReasoning, RedactedSummary: strings.Repeat("**marker** ", 20_000)}
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	target.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	var owner domain.ContentHash
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT source_hash FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, fixture.repo, hash, target.DisplayProfile()).Scan(&owner); err != nil {
		test.Fatal(err)
	}
	for count := 0; count < 1000; count++ {
		step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
		if err != nil {
			test.Fatal("pre-sweep", count, err)
		}
		if step.Source == owner && step.Phase == "sweep" {
			break
		}
		if count == 999 {
			test.Fatal("fixture never reached sweep")
		}
	}
	var before int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$1`, owner).Scan(&before); err != nil || before == 0 {
		test.Fatal("fixture lacks retained objects", before, err)
	}
	configuration, err := pgxpool.ParseConfig(isolatedDSN)
	if err != nil {
		test.Fatal(err)
	}
	transaction, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(context.WithoutCancel(fixture.ctx))
	var owned bool
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_database()=$1`, configuration.ConnConfig.Database).Scan(&owned); err != nil || !owned {
		test.Fatal("deliberate root corruption requires the newly created owned database", owned, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET preparation=$4 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, fixture.repo, hash, target.DisplayProfile(), []byte(`{}`)); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(fixture.ctx, `ALTER TABLE doc_display_jobs_v1 ENABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	var revised bool
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT source.reference_revision>work.source_revision FROM doc_display_sources_v1 source JOIN doc_display_maintenance_v1 work USING(source_hash) WHERE source_hash=$1`, owner).Scan(&revised); err != nil || !revised {
		test.Fatal("raw-root-only mutation did not invalidate sweep", revised, err)
	}
	var maintenanceErr error
	for count := 0; count < 4; count++ {
		step, err := fixture.storage.StepDocDisplayMaintenance(fixture.ctx)
		maintenanceErr = err
		if err != nil || step.Phase != "catalog_census" {
			break
		}
		if step.Deleted != 0 {
			test.Fatal("census step deleted CAS evidence", step)
		}
	}
	if !errors.Is(maintenanceErr, domain.ErrIntegrity) {
		test.Fatal("hash-mismatched raw checkpoint did not fail closed", maintenanceErr)
	}
	var after int
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_objects_v1 WHERE source_hash=$1`, owner).Scan(&after); err != nil || after != before {
		test.Fatal("checkpoint corruption allowed a deletion", before, after, err)
	}
}
