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

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func newPublishedStringReuseFixturePG(test *testing.T) publishedReuseFixturePG {
	test.Helper()
	isolatedDSN := a11IsolatedPGDSN(test, context.Background())
	test.Setenv("CXT_TEST_DSN", isolatedDSN)
	result := publishedReuseFixturePG{left: newDocDisplayPGFixture(test), right: newDocDisplayPGFixture(test), isolatedDSN: isolatedDSN}
	result.event = domain.CIREvent{Kind: domain.EventToolResult, CallID: "literal-safety", Output: strings.Repeat("marker🙂\n", 4000)}
	result.leftJob = claimPublishedStringReusePG(test, result.left, result.event)
	result.rightJob = claimPublishedStringReusePG(test, result.right, result.event)
	return result
}

func claimPublishedStringReusePG(test *testing.T, fixture docViewPGFixture, event domain.CIREvent) domain.DocDisplayJob {
	test.Helper()
	fixture.service.ConfigureDocumentDisplay(fixture.storage)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{event}})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || job.RepoID != fixture.repo {
		test.Fatal("claim literal", job, err)
	}
	return job
}

func finishPublishedStringReusePeerPG(test *testing.T, fixture publishedReuseFixturePG) domain.DocDisplayStringGeneration {
	test.Helper()
	for batch := 0; batch < 100; batch++ {
		complete, err := fixture.left.service.buildDocDisplay(fixture.left.ctx, fixture.leftJob)
		if err != nil {
			test.Fatal("literal peer builder", batch, err)
		}
		if complete {
			generation, err := readDocDisplayGenerationPG(fixture.left, fixture.leftJob.Target)
			if err != nil {
				test.Fatal(err)
			}
			return generation
		}
	}
	test.Fatal("literal peer did not publish")
	return domain.DocDisplayStringGeneration{}
}

func TestPGDocPublishedStringReuseMissingOrCorruptEvidence(test *testing.T) {
	for _, change := range []string{"missing_generation", "missing_peer", "corrupt_generation", "corrupt_peer_hash", "incomplete_peer"} {
		test.Run(change, func(test *testing.T) {
			fixture := newPublishedStringReuseFixturePG(test)
			generation := finishPublishedStringReusePeerPG(test, fixture)
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
				initial, err := domain.NewDocDisplayStringState(generation.Source)
				if err != nil {
					test.Fatal(err)
				}
				raw, err := initial.Encode(generation.Source)
				if err != nil {
					test.Fatal(err)
				}
				corruptCompletedPublishedReusePeerFixturePG(test, fixture, change, raw)
			}
			before := publishedReuseJobImagePG(test, fixture.right, fixture.rightJob)
			progress, err := fixture.right.storage.LoadDocDisplayJob(fixture.right.ctx, fixture.rightJob)
			if change == "missing_generation" || change == "missing_peer" {
				if err != nil || progress.State.Done || progress.Checkpoint != "" || progress.Source == nil || *progress.Source != generation.Source {
					test.Fatal("missing literal evidence fabricated completion", progress, err)
				}
			} else if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(progress, domain.DocDisplayPreparation{}) {
				test.Fatal("corrupt literal evidence accepted", progress, err)
			}
			if publishedReuseJobImagePG(test, fixture.right, fixture.rightJob) != before {
				test.Fatal("missing/corrupt literal evidence changed own root")
			}
		})
	}
}

func TestPGDocPublishedStringReuseOwnSourceAndLeaseFences(test *testing.T) {
	for _, atComplete := range []bool{false, true} {
		for _, change := range []string{"source", "membership", "lease"} {
			test.Run(fmt.Sprintf("%s/at_complete=%t", change, atComplete), func(test *testing.T) {
				fixture := newPublishedStringReuseFixturePG(test)
				finishPublishedStringReusePeerPG(test, fixture)
				right := fixture.right
				var checkpoint domain.ContentHash
				if atComplete {
					reused, err := right.storage.LoadDocDisplayJob(right.ctx, fixture.rightJob)
					if err != nil || !reused.State.Done {
						test.Fatal(reused, err)
					}
					checkpoint = reused.Checkpoint
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
					err = right.storage.CompleteDocDisplay(right.ctx, fixture.rightJob, checkpoint, time.Now().UTC())
				} else {
					_, err = right.storage.LoadDocDisplayJob(right.ctx, fixture.rightJob)
				}
				if !errors.Is(err, domain.ErrConflict) || before != publishedReuseJobImagePG(test, right, fixture.rightJob) {
					test.Fatal("literal reuse bypassed own source or lease fence", change, err)
				}
			})
		}
	}
}

func TestPGDocPublishedStringReuseIndependentLifetime(test *testing.T) {
	fixture := newPublishedStringReuseFixturePG(test)
	for batch := 0; batch < 2; batch++ {
		if complete, err := fixture.right.service.buildDocDisplay(fixture.right.ctx, fixture.rightJob); err != nil || complete {
			test.Fatal("unfinished literal render", batch, complete, err)
		}
	}
	generation := finishPublishedStringReusePeerPG(test, fixture)
	before := publishedReuseJobImagePG(test, fixture.right, fixture.rightJob)
	if _, err := fixture.right.storage.LoadDocDisplayJob(outbound.WithDocReadOnly(fixture.right.ctx), fixture.rightJob); !errors.Is(err, domain.ErrConflict) || before != publishedReuseJobImagePG(test, fixture.right, fixture.rightJob) {
		test.Fatal("read-only literal reuse changed own checkpoint", err)
	}
	reused, err := fixture.right.storage.LoadDocDisplayJob(fixture.right.ctx, fixture.rightJob)
	if err != nil || !reused.State.Done {
		test.Fatal("unfinished literal render did not adopt completed work", reused, err)
	}
	if _, err := fixture.left.pool.Exec(fixture.left.ctx, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.left.repo); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.left.pool.Exec(fixture.left.ctx, `DELETE FROM doc_display_generations_v1 WHERE event_hash=$1 AND profile=$2`, fixture.leftJob.Target.Event.Hash, fixture.leftJob.Target.DisplayProfile()); err != nil {
		test.Fatal(err)
	}
	drainDocDisplayMaintenancePG(test, fixture.right)
	probe := &docDisplayJobProbe{DocDisplayStore: fixture.right.storage}
	fixture.right.service.ConfigureDocumentDisplay(probe)
	if complete, err := fixture.right.service.buildDocDisplay(fixture.right.ctx, fixture.rightJob); err != nil || !complete || probe.saved != 0 || probe.discovered != 0 || probe.initialized != 0 || probe.completed != 1 {
		test.Fatal("literal own root depended on deleted peer after GC", complete, probe, err)
	}
	actual, err := readDocDisplayGenerationPG(fixture.right, fixture.rightJob.Target)
	if err != nil || !reflect.DeepEqual(actual, generation) {
		test.Fatal("literal independent completion changed its generation", actual, err)
	}
}

func TestPGDocPublishedStringReuseConcurrentCompletionsAndGC(test *testing.T) {
	fixture := newPublishedStringReuseFixturePG(test)
	generation := finishPublishedStringReusePeerPG(test, fixture)
	third := newDocDisplayPGFixture(test)
	thirdJob := claimPublishedStringReusePG(test, third, fixture.event)
	if fixture.left.repo == fixture.right.repo || third.repo == fixture.left.repo || third.repo == fixture.right.repo ||
		fixture.leftJob.DocHash == fixture.rightJob.DocHash || thirdJob.DocHash == fixture.leftJob.DocHash || thirdJob.DocHash == fixture.rightJob.DocHash ||
		fixture.leftJob.Target.Event.Hash != fixture.rightJob.Target.Event.Hash || fixture.leftJob.Target.Event.Hash != thirdJob.Target.Event.Hash || generation.Pages.Count < 2 {
		test.Fatal("literal fixture must have independent jobs sharing an exact multi-page event")
	}
	owner, err := generation.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	raw, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	generationHash := domain.HashContent(raw)
	var peerCheckpoint domain.ContentHash
	if err := fixture.left.pool.QueryRow(fixture.left.ctx, `SELECT checkpoint_hash FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4 AND state='completed'`,
		fixture.leftJob.RepoID, fixture.leftJob.DocHash, fixture.leftJob.Target.Event.Index, domain.DocDisplayStringProfile).Scan(&peerCheckpoint); err != nil {
		test.Fatal("literal peer is not ready", err)
	}
	receivers := []struct {
		fixture docViewPGFixture
		job     domain.DocDisplayJob
	}{{fixture.right, fixture.rightJob}, {third, thirdJob}}
	ctx, cancel := context.WithTimeout(fixture.left.ctx, 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	ready := make(chan struct{}, len(receivers))
	complete := make(chan struct{})
	results := make(chan error, len(receivers)+1)
	report := func(err error) {
		results <- err
		if err != nil {
			cancel()
		}
	}
	var workers sync.WaitGroup
	for _, current := range receivers {
		workers.Go(func() {
			<-start
			var reused domain.DocDisplayPreparation
			err := retryDocDisplayCatalogBusyPG(ctx, func(retry context.Context) error {
				var err error
				reused, err = current.fixture.storage.LoadDocDisplayJob(retry, current.job)
				return err
			})
			if err != nil {
				report(fmt.Errorf("literal receiver %s load: %w", current.job.RepoID, err))
				return
			}
			if reused.Source == nil || *reused.Source != generation.Source || !reused.State.Done || reused.Checkpoint != peerCheckpoint || reused.State.Source != owner || reused.Discovery != nil || reused.DiscoveryCheckpoint != "" {
				report(fmt.Errorf("literal receiver %s did not adopt exact completed peer", current.job.RepoID))
				return
			}
			ready <- struct{}{}
			select {
			case <-complete:
			case <-ctx.Done():
				report(ctx.Err())
				return
			}
			report(retryDocDisplayCatalogBusyPG(ctx, func(retry context.Context) error {
				return current.fixture.storage.CompleteDocDisplay(retry, current.job, reused.Checkpoint, time.Now().UTC())
			}))
		})
	}
	maintenanceSteps := 0
	workers.Go(func() {
		<-start
		if _, err := fixture.left.storage.StepDocDisplayMaintenance(ctx); err != nil && !errors.Is(err, domain.ErrNotFound) && !docDisplayCatalogBusyPG(err) {
			report(err)
			return
		}
		for range receivers {
			select {
			case <-ready:
			case <-ctx.Done():
				report(ctx.Err())
				return
			}
		}
		step, err := fixture.left.storage.StepDocDisplayMaintenance(ctx)
		if err != nil || step.Source != owner {
			report(fmt.Errorf("maintenance missed both attached literal roots: step=%+v error=%v", step, err))
			return
		}
		maintenanceSteps++
		close(complete)
		for range 64 {
			_, err := fixture.left.storage.StepDocDisplayMaintenance(ctx)
			if errors.Is(err, domain.ErrNotFound) || docDisplayCatalogBusyPG(err) {
				continue
			}
			if err != nil {
				report(err)
				return
			}
			maintenanceSteps++
		}
		report(nil)
	})
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			test.Fatal("concurrent literal reuse/GC", err)
		}
	}
	drainDocDisplayMaintenancePG(test, fixture.left)
	for _, current := range receivers {
		var state string
		var source, checkpoint domain.ContentHash
		var discovery []byte
		if err := current.fixture.pool.QueryRow(current.fixture.ctx, `SELECT state,source_hash,checkpoint_hash,discovery FROM doc_display_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3 AND profile=$4`, current.job.RepoID, current.job.DocHash,
			current.job.Target.Event.Index, domain.DocDisplayStringProfile).Scan(&state, &source, &checkpoint, &discovery); err != nil || state != "completed" || source != owner || checkpoint != peerCheckpoint || discovery != nil {
			test.Fatal("concurrent literal reuse lost its independent completed root", current.job.RepoID, state, source, checkpoint, err)
		}
		actual, err := readDocDisplayGenerationPG(current.fixture, current.job.Target)
		if err != nil || !reflect.DeepEqual(actual, generation) {
			test.Fatal("maintenance changed the reused literal generation", current.job.RepoID, actual, err)
		}
		encoded, err := actual.Encode()
		if err != nil || domain.HashContent(encoded) != generationHash {
			test.Fatal("maintenance changed literal generation identity", current.job.RepoID, err)
		}
		_, err = repositoryReadForRepo(current.fixture.ctx, current.fixture.service, current.fixture.repo, func(read context.Context) (bool, error) {
			var text strings.Builder
			for index := 0; index < actual.Pages.Count; index++ {
				page, err := current.fixture.storage.ReadDocDisplayPage(read, current.job.Target, index)
				if err != nil {
					return false, err
				}
				if page.From != text.Len() || page.Source != owner {
					return false, fmt.Errorf("literal page %d changed its source or text coordinates", index)
				}
				text.WriteString(page.Text)
			}
			if text.String() != fixture.event.Output.(string) || text.Len() != generation.TextBytes {
				return false, errors.New("maintenance changed the exact reused PRE page text")
			}
			return true, nil
		})
		if err != nil {
			test.Fatal("literal pages after concurrent completion and GC", current.job.RepoID, err)
		}
	}
	test.Logf("literal_receivers=%d concurrent_maintenance_steps=%d pages=%d generation=%s", len(receivers), maintenanceSteps, generation.Pages.Count, generationHash)
}

func TestPGDocPublishedStringReuseRejectsStaleComputedStep(test *testing.T) {
	fixture := newPublishedStringReuseFixturePG(test)
	if complete, err := fixture.right.service.buildDocDisplay(fixture.right.ctx, fixture.rightJob); err != nil || complete {
		test.Fatal("initial literal discovery", complete, err)
	}
	var adopted domain.DocDisplayPreparation
	probe := &docDisplayJobProbe{DocDisplayStore: fixture.right.storage}
	probe.beforeSave = func() error {
		probe.beforeSave = nil
		finishPublishedStringReusePeerPG(test, fixture)
		var err error
		adopted, err = fixture.right.storage.LoadDocDisplayJob(fixture.right.ctx, fixture.rightJob)
		if err != nil || !adopted.State.Done {
			test.Fatal("literal compute/save reuse", adopted, err)
		}
		return nil
	}
	fixture.right.service.ConfigureDocumentDisplay(probe)
	if _, err := fixture.right.service.buildDocDisplay(fixture.right.ctx, fixture.rightJob); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale literal step overwrote adopted state", err)
	}
	current, err := fixture.right.storage.LoadDocDisplayJob(fixture.right.ctx, fixture.rightJob)
	if err != nil || !reflect.DeepEqual(current, adopted) {
		test.Fatal("stale literal step changed winning checkpoint", err)
	}
	if err := fixture.right.storage.CompleteDocDisplay(fixture.right.ctx, fixture.rightJob, adopted.Checkpoint, time.Now().UTC()); err != nil {
		test.Fatal("literal completion after stale step", err)
	}
}

func TestPGDocPublishedStringReuseKeepsFinalPageValidator(test *testing.T) {
	fixture := newPublishedStringReuseFixturePG(test)
	generation := finishPublishedStringReusePeerPG(test, fixture)
	reused, err := fixture.right.storage.LoadDocDisplayJob(fixture.right.ctx, fixture.rightJob)
	if err != nil || !reused.State.Done {
		test.Fatal(reused, err)
	}
	load := func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		var raw []byte
		err := fixture.left.pool.QueryRow(ctx, `SELECT bytes FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2`, reused.State.Source, hash).Scan(&raw)
		return raw, err
	}
	hash, err := domain.ResolveDocRangeLeaf(fixture.left.ctx, generation.Pages, generation.Pages.Count-1, load)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.left.pool.Exec(fixture.left.ctx, `WITH replaced AS (DELETE FROM doc_display_objects_v1 WHERE source_hash=$1 AND hash=$2 RETURNING source_hash,hash)
 INSERT INTO doc_display_objects_v1(source_hash,hash,bytes) SELECT source_hash,hash,$3 FROM replaced`, reused.State.Source, hash, []byte(`{}`)); err != nil {
		test.Fatal(err)
	}
	before := publishedReuseJobImagePG(test, fixture.right, fixture.rightJob)
	if err := fixture.right.storage.CompleteDocDisplay(fixture.right.ctx, fixture.rightJob, reused.Checkpoint, time.Now().UTC()); !errors.Is(err, domain.ErrIntegrity) || before != publishedReuseJobImagePG(test, fixture.right, fixture.rightJob) {
		test.Fatal("literal reuse skipped current selected-page validation", err)
	}
}
