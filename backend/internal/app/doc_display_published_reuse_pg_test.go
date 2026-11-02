//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type publishedReuseFixturePG struct {
	left, right       docViewPGFixture
	leftJob, rightJob domain.DocDisplayJob
	event             domain.CIREvent
	isolatedDSN       string
}

func claimPublishedReusePG(test *testing.T, fixture docViewPGFixture, event domain.CIREvent) domain.DocDisplayJob {
	test.Helper()
	return claimPublishedReuseSourcePG(test, fixture, event, false)
}

func claimPublishedReuseSourcePG(test *testing.T, fixture docViewPGFixture, event domain.CIREvent, legacy bool) domain.DocDisplayJob {
	test.Helper()
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	var hash domain.ContentHash
	if legacy {
		hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
		prepareLegacyFindGenerationPG(test, fixture, hash)
	} else {
		hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
	}
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

func newPublishedReuseFixturePG(test *testing.T) publishedReuseFixturePG {
	test.Helper()
	return newPublishedReuseSourcesPG(test, false, false)
}

func newPublishedReuseSourcesPG(test *testing.T, leftLegacy, rightLegacy bool) publishedReuseFixturePG {
	test.Helper()
	isolatedDSN := a11IsolatedPGDSN(test, context.Background())
	test.Setenv("CXT_TEST_DSN", isolatedDSN)
	result := publishedReuseFixturePG{left: newDocDisplayPGFixture(test), right: newDocDisplayPGFixture(test), isolatedDSN: isolatedDSN}
	rows := make([]any, 32)
	for index := range rows {
		rows[index] = map[string]any{"ordinal": index, "text": "shared", "nested": []any{true, nil, "value"}}
	}
	result.event = domain.CIREvent{Kind: domain.EventToolCall, ToolName: "generic", CallID: "published-reuse", Input: map[string]any{"rows": rows}}
	result.leftJob = claimPublishedReuseSourcePG(test, result.left, result.event, leftLegacy)
	result.rightJob = claimPublishedReuseSourcePG(test, result.right, result.event, rightLegacy)
	return result
}

func finishPublishedReusePeerPG(test *testing.T, fixture publishedReuseFixturePG) domain.DocEventDisplayPreparation {
	test.Helper()
	for batch := 0; batch < 100; batch++ {
		complete, err := fixture.left.service.buildDocEventDisplay(fixture.left.ctx, fixture.leftJob)
		if err != nil {
			test.Fatal("peer builder", batch, err)
		}
		if complete {
			receipt := readDocDisplayCatalogReceiptPG(test, fixture.left, fixture.leftJob.Target)
			state, err := domain.DecodeDocEventDisplayPreparation(receipt.Checkpoint, receipt.Preparation)
			if err != nil {
				test.Fatal(err)
			}
			return state
		}
	}
	test.Fatal("peer did not publish")
	return domain.DocEventDisplayPreparation{}
}

func publishedReuseJobImagePG(test *testing.T, fixture docViewPGFixture, job domain.DocDisplayJob) string {
	test.Helper()
	var image string
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT to_jsonb(job)::text FROM doc_display_jobs_v1 job WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile()).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func corruptCompletedPublishedReusePeerFixturePG(test *testing.T, fixture publishedReuseFixturePG, change string, raw []byte) {
	test.Helper()
	if fixture.isolatedDSN == "" {
		test.Fatal("completed-peer corruption requires the newly created isolated database")
	}
	configuration, err := pgxpool.ParseConfig(fixture.isolatedDSN)
	if err != nil || configuration.ConnConfig.Database != fixture.left.pool.Config().ConnConfig.Database {
		test.Fatal("completed-peer corruption pool does not select the owned database", err)
	}
	job, left := fixture.leftJob, fixture.left
	assignment, protocol := "preparation=$6", 5
	switch job.Target.DisplayProfile() {
	case domain.DocDisplayProjectionProfile:
		if change == "incomplete_peer" {
			assignment = "preparation_hash=$7,preparation=$6"
		}
	case domain.DocDisplayStringProfile:
		assignment, protocol = "checkpoint=$6", 2
		if change == "incomplete_peer" {
			assignment = "checkpoint_hash=$7,checkpoint=$6"
		}
	default:
		test.Fatal("unexpected completed-peer corruption profile", job.Target.DisplayProfile())
	}
	if change != "corrupt_peer_hash" && change != "incomplete_peer" {
		test.Fatal("unexpected completed-peer corruption", change)
	}
	transaction, err := left.pool.Begin(left.ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(left.ctx), 5*time.Second)
		defer cancel()
		_ = transaction.Rollback(cleanup)
	}()
	var owned, completed bool
	if err := transaction.QueryRow(left.ctx, `SELECT current_database()=$1`, configuration.ConnConfig.Database).Scan(&owned); err != nil || !owned {
		test.Fatal("completed-peer corruption requires the actual owned database", owned, err)
	}
	if err := transaction.QueryRow(left.ctx, `SELECT state='completed' AND worker_protocol=$6 FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4 AND version=$5 FOR UPDATE NOWAIT`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, protocol).Scan(&completed); err != nil || !completed {
		test.Fatal("corruption fixture must target exactly the completed peer", completed, err)
	}
	const guards = `SELECT count(*) FILTER (WHERE tgname='guard_doc_display_zz_catalog_write')=1 AND bool_and(tgenabled='O'),
 jsonb_agg(jsonb_build_array(tgname,tgenabled::text) ORDER BY tgname)::text
 FROM pg_trigger WHERE tgrelid='doc_display_jobs_v1'::regclass`
	var enabled bool
	var originalGuards, restoredGuards string
	if err := transaction.QueryRow(left.ctx, guards).Scan(&enabled, &originalGuards); err != nil || !enabled {
		test.Fatal("completed-peer corruption requires every actual job guard enabled", enabled, err)
	}
	if _, err := transaction.Exec(left.ctx, `ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	arguments := []any{job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, raw}
	if change == "incomplete_peer" {
		arguments = append(arguments, domain.HashContent(raw))
	}
	if tag, err := transaction.Exec(left.ctx, `UPDATE doc_display_jobs_v1 SET `+assignment+`
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4 AND version=$5 AND state='completed'`, arguments...); err != nil || tag.RowsAffected() != 1 {
		test.Fatal("failed to corrupt exactly one completed peer", tag, err)
	}
	if _, err := transaction.Exec(left.ctx, `ALTER TABLE doc_display_jobs_v1 ENABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		test.Fatal(err)
	}
	if err := transaction.QueryRow(left.ctx, guards).Scan(&enabled, &restoredGuards); err != nil || !enabled || restoredGuards != originalGuards {
		test.Fatal("completed-peer corruption did not restore every guard before commit", enabled, err)
	}
	if err := transaction.Commit(left.ctx); err != nil {
		test.Fatal(err)
	}
}

func TestPGDocPublishedReuseInProgressAndIndependentLifetime(test *testing.T) {
	fixture := newPublishedReuseFixturePG(test)
	if complete, err := fixture.right.service.buildDocEventDisplay(fixture.right.ctx, fixture.rightJob); err != nil || complete {
		test.Fatal("partial own job", complete, err)
	}
	partial, err := fixture.right.storage.LoadDocEventDisplay(fixture.right.ctx, fixture.rightJob)
	if err != nil || partial.Phase == "completed" {
		test.Fatal(partial, err)
	}
	peer := finishPublishedReusePeerPG(test, fixture)
	_, pages := readDocDisplayCatalogPagesPG(test, fixture.left, fixture.leftJob.Target)
	before := publishedReuseJobImagePG(test, fixture.right, fixture.rightJob)
	if _, err := fixture.right.storage.LoadDocEventDisplay(outbound.WithDocReadOnly(fixture.right.ctx), fixture.rightJob); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("read-only reuse wrote a checkpoint", err)
	}
	if before != publishedReuseJobImagePG(test, fixture.right, fixture.rightJob) {
		test.Fatal("read-only reuse changed own job")
	}
	reused, err := fixture.right.storage.LoadDocEventDisplay(fixture.right.ctx, fixture.rightJob)
	if err != nil || !reflect.DeepEqual(reused, peer) {
		test.Fatal("in-progress job reconstructed instead of adopting exact published work", err)
	}
	if _, err := fixture.left.pool.Exec(fixture.left.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, fixture.left.repo, fixture.leftJob.DocHash, fixture.leftJob.Target.DisplayProfile()); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.left.pool.Exec(fixture.left.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, fixture.leftJob.Target.Event.Hash, fixture.leftJob.Target.DisplayProfile()); err != nil {
		test.Fatal(err)
	}
	drainDocDisplayMaintenancePG(test, fixture.right)
	probe := &docDisplayCatalogReuseProbePG{PostgresStore: fixture.right.storage}
	fixture.right.service.ConfigureDocumentDisplay(probe)
	if complete, err := fixture.right.service.buildDocEventDisplay(fixture.right.ctx, fixture.rightJob); err != nil || !complete || probe.saveCalls != 0 || probe.completeCalls != 1 {
		test.Fatal("own copied root failed after peer/generation removal and GC", complete, probe, err)
	}
	_, restored := readDocDisplayCatalogPagesPG(test, fixture.right, fixture.rightJob.Target)
	if !reflect.DeepEqual(pages, restored) {
		test.Fatal("independent completion changed published text or source mapping")
	}
}

func TestPGDocPublishedReuseMissingOrCorruptEvidence(test *testing.T) {
	for _, change := range []string{"missing_generation", "missing_peer", "corrupt_generation", "corrupt_peer_hash", "incomplete_peer"} {
		test.Run(change, func(test *testing.T) {
			fixture := newPublishedReuseFixturePG(test)
			peer := finishPublishedReusePeerPG(test, fixture)
			left := fixture.left
			switch change {
			case "missing_generation":
				if _, err := left.pool.Exec(left.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, fixture.leftJob.Target.Event.Hash, fixture.leftJob.Target.DisplayProfile()); err != nil {
					test.Fatal(err)
				}
			case "missing_peer":
				if _, err := left.pool.Exec(left.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, left.repo, fixture.leftJob.DocHash, fixture.leftJob.Target.DisplayProfile()); err != nil {
					test.Fatal(err)
				}
			case "corrupt_generation":
				if _, err := left.pool.Exec(left.ctx, `UPDATE doc_display_generations_v1 SET bytes=$3 WHERE event_hash=$1 AND profile=$2`, fixture.leftJob.Target.Event.Hash, fixture.leftJob.Target.DisplayProfile(), []byte(`{}`)); err != nil {
					test.Fatal(err)
				}
			case "corrupt_peer_hash":
				corruptCompletedPublishedReusePeerFixturePG(test, fixture, change, []byte(`{}`))
			case "incomplete_peer":
				initial, err := domain.NewDocEventDisplayPreparation(domain.DocRangeEntry{DocEventIndex: fixture.leftJob.Target.Event, Kind: domain.EventToolCall})
				if err != nil {
					test.Fatal(err)
				}
				raw, err := initial.Encode()
				if err != nil {
					test.Fatal(err)
				}
				corruptCompletedPublishedReusePeerFixturePG(test, fixture, change, raw)
			}
			before := publishedReuseJobImagePG(test, fixture.right, fixture.rightJob)
			state, err := fixture.right.storage.LoadDocEventDisplay(fixture.right.ctx, fixture.rightJob)
			if change == "missing_generation" || change == "missing_peer" {
				if err != nil || state.Phase == "completed" || state.Source != peer.Source {
					test.Fatal("missing evidence fabricated completed state", state, err)
				}
			} else if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(state, domain.DocEventDisplayPreparation{}) || publishedReuseJobImagePG(test, fixture.right, fixture.rightJob) != before {
				test.Fatal("corrupt selected evidence did not fail without own-root effects", err)
			}
		})
	}
}

func TestPGDocPublishedReuseOwnSourceAndLeaseFences(test *testing.T) {
	for _, atComplete := range []bool{false, true} {
		for _, change := range []string{"source", "membership", "lease"} {
			test.Run(fmt.Sprintf("%s/at_complete=%t", change, atComplete), func(test *testing.T) {
				fixture := newPublishedReuseFixturePG(test)
				finishPublishedReusePeerPG(test, fixture)
				right := fixture.right
				var checkpoint domain.ContentHash
				if atComplete {
					reused, err := right.storage.LoadDocEventDisplay(right.ctx, fixture.rightJob)
					if err != nil || reused.Phase != "completed" {
						test.Fatal(err)
					}
					raw, err := reused.Encode()
					if err != nil {
						test.Fatal(err)
					}
					checkpoint = domain.HashContent(raw)
				}
				switch change {
				case "source":
					if _, err := right.pool.Exec(right.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.rightJob.DocHash); err != nil {
						test.Fatal(err)
					}
				case "membership":
					if _, err := right.pool.Exec(right.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, right.repo, fixture.rightJob.DocHash); err != nil {
						test.Fatal(err)
					}
				case "lease":
					if _, err := docDisplayLeaseMutationPG(right.ctx, right.pool, right.repo, `UPDATE doc_display_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, right.repo); err != nil {
						test.Fatal(err)
					}
				}
				before := publishedReuseJobImagePG(test, right, fixture.rightJob)
				var err error
				if atComplete {
					err = right.storage.CompleteDocEventDisplay(right.ctx, fixture.rightJob, checkpoint, time.Now().UTC())
				} else {
					_, err = right.storage.LoadDocEventDisplay(right.ctx, fixture.rightJob)
				}
				if !errors.Is(err, domain.ErrConflict) {
					test.Fatal("peer completion bypassed own target or lease", change, err)
				}
				if before != publishedReuseJobImagePG(test, right, fixture.rightJob) {
					test.Fatal("failed own fence changed checkpoint")
				}
			})
		}
	}
}

func TestPGDocPublishedReuseRejectsStaleComputedStep(test *testing.T) {
	fixture := newPublishedReuseFixturePG(test)
	probe := &docEventDisplayAcknowledgementProbe{DocDisplayStore: fixture.right.storage, DocEventDisplayStore: fixture.right.storage}
	var adopted domain.DocEventDisplayPreparation
	probe.beforeSave = func() error {
		probe.beforeSave = nil
		peer := finishPublishedReusePeerPG(test, fixture)
		var err error
		adopted, err = fixture.right.storage.LoadDocEventDisplay(fixture.right.ctx, fixture.rightJob)
		if err != nil || !reflect.DeepEqual(adopted, peer) {
			test.Fatal("reuse between compute and save", err)
		}
		return nil
	}
	fixture.right.service.ConfigureDocumentDisplay(probe)
	if _, err := fixture.right.service.buildDocEventDisplay(fixture.right.ctx, fixture.rightJob); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old sealed step overwrote adopted completed state", err)
	}
	current, err := fixture.right.storage.LoadDocEventDisplay(fixture.right.ctx, fixture.rightJob)
	if err != nil || !reflect.DeepEqual(current, adopted) {
		test.Fatal("stale step changed the winning reused checkpoint", err)
	}
	restarted := &docDisplayCatalogReuseProbePG{PostgresStore: fixture.right.storage}
	fixture.right.service.ConfigureDocumentDisplay(restarted)
	if complete, err := fixture.right.service.buildDocEventDisplay(fixture.right.ctx, fixture.rightJob); err != nil || !complete || restarted.saveCalls != 0 || restarted.completeCalls != 1 {
		test.Fatal("reused checkpoint could not finish after stale save", complete, restarted, err)
	}
}

func TestPGDocPublishedReuseKeepsFinalPublicationValidator(test *testing.T) {
	fixture := newPublishedReuseFixturePG(test)
	peer := finishPublishedReusePeerPG(test, fixture)
	reused, err := fixture.right.storage.LoadDocEventDisplay(fixture.right.ctx, fixture.rightJob)
	if err != nil || !reflect.DeepEqual(reused, peer) {
		test.Fatal(err)
	}
	owner, err := peer.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.left.pool.Exec(fixture.left.ctx, `WITH replaced AS (DELETE FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2 RETURNING source_hash,hash)
 INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) SELECT source_hash,hash,$3 FROM replaced`, owner, peer.Generation, []byte(`{}`)); err != nil {
		test.Fatal(err)
	}
	before := publishedReuseJobImagePG(test, fixture.right, fixture.rightJob)
	raw, err := reused.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.right.storage.CompleteDocEventDisplay(fixture.right.ctx, fixture.rightJob, domain.HashContent(raw), time.Now().UTC()); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("reused state skipped final current-byte evidence validation", err)
	}
	if before != publishedReuseJobImagePG(test, fixture.right, fixture.rightJob) {
		test.Fatal("failed final validation completed a reused job")
	}
}

func TestPGDocPublishedReuseConcurrentCompletionsAndGC(test *testing.T) {
	fixture := newPublishedReuseFixturePG(test)
	finishPublishedReusePeerPG(test, fixture)
	third := newDocDisplayPGFixture(test)
	thirdJob := claimPublishedReusePG(test, third, fixture.event)
	start := make(chan struct{})
	results := make(chan error, 3)
	var workers sync.WaitGroup
	for _, current := range []struct {
		fixture docViewPGFixture
		job     domain.DocDisplayJob
	}{{fixture.right, fixture.rightJob}, {third, thirdJob}} {
		workers.Go(func() {
			<-start
			err := retryDocDisplayCatalogBusyPG(current.fixture.ctx, func(retry context.Context) error {
				complete, err := current.fixture.service.buildDocEventDisplay(retry, current.job)
				if err == nil && !complete {
					return fmt.Errorf("reuse did not complete in one preparation batch")
				}
				return err
			})
			results <- err
		})
	}
	workers.Go(func() {
		<-start
		for range 64 {
			_, err := fixture.left.storage.StepDocDisplayMaintenance(fixture.left.ctx)
			if err != nil && !errors.Is(err, domain.ErrNotFound) && !docDisplayCatalogBusyPG(err) {
				results <- err
				return
			}
		}
		results <- nil
	})
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			test.Fatal(err)
		}
	}
	drainDocDisplayMaintenancePG(test, fixture.left)
	for _, current := range []struct {
		fixture docViewPGFixture
		job     domain.DocDisplayJob
	}{{fixture.right, fixture.rightJob}, {third, thirdJob}} {
		if receipt := readDocDisplayCatalogReceiptPG(test, current.fixture, current.job.Target); receipt.State != "completed" {
			test.Fatal("concurrent reuse lost a completion", receipt.State)
		}
		readDocDisplayCatalogPagesPG(test, current.fixture, current.job.Target)
	}
}

func TestPGDocPublishedReuseAcrossRootAndLegacySources(test *testing.T) {
	for _, leftLegacy := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			test.Run(fmt.Sprintf("peer_legacy=%t/own_partial=%t", leftLegacy, partial), func(test *testing.T) {
				fixture := newPublishedReuseSourcesPG(test, leftLegacy, !leftLegacy)
				if fixture.leftJob.Target.Event.Hash != fixture.rightJob.Target.Event.Hash || fixture.leftJob.Target.Binding == fixture.rightJob.Target.Binding {
					test.Fatal("fixture must share an exact event but not document identity or binding")
				}
				if partial {
					if complete, err := fixture.right.service.buildDocEventDisplay(fixture.right.ctx, fixture.rightJob); err != nil || complete {
						test.Fatal("partial other-identity demand", complete, err)
					}
				}
				finishPublishedReusePeerPG(test, fixture)
				_, expected := readDocDisplayCatalogPagesPG(test, fixture.left, fixture.leftJob.Target)
				probe := &docDisplayCatalogReuseProbePG{PostgresStore: fixture.right.storage}
				fixture.right.service.ConfigureDocumentDisplay(probe)
				if complete, err := fixture.right.service.buildDocEventDisplay(fixture.right.ctx, fixture.rightJob); err != nil || !complete || probe.saveCalls != 0 || probe.completeCalls != 1 {
					test.Fatal("root/legacy exact event reconstructed or skipped normal completion", complete, probe, err)
				}
				_, actual := readDocDisplayCatalogPagesPG(test, fixture.right, fixture.rightJob.Target)
				if !reflect.DeepEqual(expected, actual) {
					test.Fatal("root/legacy reuse changed exact text or original-byte mappings")
				}
			})
		}
	}

}

func TestPGDocPublishedStringReuseRootLegacyAndDiscovery(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			test.Run(fmt.Sprintf("legacy=%t/partial_discovery=%t", legacy, partial), func(test *testing.T) {
				test.Setenv("CXT_TEST_DSN", a11IsolatedPGDSN(test, context.Background()))
				left, right := newDocDisplayPGFixture(test), newDocDisplayPGFixture(test)
				event := domain.CIREvent{Kind: domain.EventToolResult, CallID: "literal-shared", Output: strings.Repeat("marker🙂\n", 4000)}
				claim := func(fixture docViewPGFixture) domain.DocDisplayJob {
					fixture.service.ConfigureDocumentDisplay(fixture.storage)
					var hash domain.ContentHash
					if legacy {
						hash = seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{event}).Hash
						prepareLegacyFindGenerationPG(test, fixture, hash)
					} else {
						hash = seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
					}
					target := docDisplayTargetPG(test, fixture, hash, 0)
					if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
						test.Fatal(err)
					}
					job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
					if err != nil || job.RepoID != fixture.repo {
						test.Fatal(job, err)
					}
					return job
				}
				leftJob, rightJob := claim(left), claim(right)
				if partial {
					initial, err := domain.NewDocFieldDiscoveryState(rightJob.Target.Event.Hash, rightJob.Target.Event.Length)
					if err != nil {
						test.Fatal(err)
					}
					step, err := repositoryReadForRepo(right.ctx, right.service, right.repo, func(read context.Context) (domain.DocFieldDiscoveryStep, error) {
						source, err := right.service.docDisplaySource(read, rightJob.Target)
						if err != nil {
							return domain.DocFieldDiscoveryStep{}, err
						}
						raw, err := source.read(rightJob.Target.Event.Offset, 64)
						if err != nil {
							return domain.DocFieldDiscoveryStep{}, err
						}
						return domain.AdvanceDocFieldDiscovery(read, initial, raw)
					})
					if err != nil {
						test.Fatal(err)
					}
					progress, err := right.storage.SaveDocDisplayDiscovery(right.ctx, rightJob, step)
					if err != nil || progress.Discovery == nil || progress.Discovery.Done || progress.Checkpoint != "" {
						test.Fatal("fixture did not save an actual unfinished discovery checkpoint", progress, err)
					}
				}
				completed := false
				batches := 0
				for ; batches < 100; batches++ {
					var err error
					completed, err = left.service.buildDocDisplay(left.ctx, leftJob)
					if err != nil {
						test.Fatal(err)
					}
					if completed {
						break
					}
				}
				if !completed {
					test.Fatal("literal peer never published")
				}
				generation, err := readDocDisplayGenerationPG(left, leftJob.Target)
				if err != nil {
					test.Fatal(err)
				}
				probe := &docDisplayJobProbe{DocDisplayStore: right.storage}
				right.service.ConfigureDocumentDisplay(probe)
				if complete, err := right.service.buildDocDisplay(right.ctx, rightJob); err != nil || !complete || probe.saved != 0 || probe.discovered != 0 || probe.initialized != 0 || probe.completed != 1 {
					test.Fatal("literal reuse rebuilt or skipped ordinary completion", complete, probe, err)
				}
				shared, err := readDocDisplayGenerationPG(right, rightJob.Target)
				if err != nil || !reflect.DeepEqual(shared, generation) {
					test.Fatal("literal reuse changed the published source or generation", err)
				}
				progress, err := right.storage.LoadDocDisplayJob(right.ctx, rightJob)
				if !errors.Is(err, domain.ErrConflict) {
					test.Fatal("completed literal job accepted its old running lease", progress, err)
				}
				var state string
				var discovery []byte
				if err := right.pool.QueryRow(right.ctx, `SELECT state,discovery FROM doc_display_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND profile=$3`, right.repo, rightJob.DocHash, rightJob.Target.DisplayProfile()).Scan(&state, &discovery); err != nil || state != "completed" || discovery != nil {
					test.Fatal("reuse retained stale discovery or an unfinished receipt", state, err)
				}
				_, err = repositoryReadForRepo(right.ctx, right.service, right.repo, func(read context.Context) (bool, error) {
					var text strings.Builder
					for page := 0; page < shared.Pages.Count; page++ {
						value, err := right.storage.ReadDocDisplayPage(read, rightJob.Target, page)
						if err != nil {
							return false, err
						}
						text.WriteString(value.Text)
					}
					if text.String() != event.Output.(string) {
						test.Fatal("literal reuse changed exact PRE text")
					}
					return true, nil
				})
				if err != nil {
					test.Fatal(err)
				}
				test.Logf("legacy=%t partial_discovery=%t first_batches=%d reused_batches=1 discovery_steps=0 render_steps=0 normal_completions=1", legacy, partial, batches+1)
			})
		}
	}
}
