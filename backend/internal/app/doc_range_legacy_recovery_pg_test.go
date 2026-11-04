//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeLegacyRecoveryPGVerification struct {
	context.Context
	preparation atomic.Bool
	step        atomic.Bool
	event       atomic.Bool
}

func (ctx *docRangeLegacyRecoveryPGVerification) Err() error {
	if !ctx.preparation.Load() || !ctx.step.Load() || !ctx.event.Load() {
		var callers [32]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			switch {
			case strings.HasSuffix(frame.Function, "domain.NewLegacyByteSourcePreparation"):
				ctx.preparation.Store(true)
			case strings.HasSuffix(frame.Function, "domain.(*LegacyByteSourcePreparation).Step"):
				ctx.step.Store(true)
			case strings.HasSuffix(frame.Function, "domain.verifyCanonicalEvent"):
				ctx.event.Store(true)
			}
			if !more {
				break
			}
		}
	}
	return ctx.Context.Err()
}

type docRangeLegacyRecoveryPGObservation struct {
	job domain.DocRangeIndexJob
	at  time.Time
}

type docRangeLegacyRecoveryPGStore struct {
	*store.PostgresStore
	afterStep     func(context.Context, *docRangeLegacyRecoveryPGWork, outbound.DocRangeWorkProgress) error
	beforePublish func(context.Context, *docRangeLegacyRecoveryPGWork) error
	closeFailure  error
	works         []*docRangeLegacyRecoveryPGWork
	publications  int
	finishCalls   int
	finishes      int
	finished      chan docRangeLegacyRecoveryPGObservation
	resume        <-chan struct{}
	closed        chan *docRangeLegacyRecoveryPGWork
}

type docRangeLegacyRecoveryPGWork struct {
	outbound.PreparedDocRangeWork
	storage      *docRangeLegacyRecoveryPGStore
	job          domain.DocRangeIndexJob
	started      time.Time
	kind         string
	steps        int
	closes       int
	publishCalls int
	published    bool
	failure      error
	verification docRangeLegacyRecoveryPGVerification
}

var _ outbound.DocRangeWorkStore = (*docRangeLegacyRecoveryPGStore)(nil)
var _ outbound.DocRangeJobStore = (*docRangeLegacyRecoveryPGStore)(nil)
var _ outbound.PreparedDocRangeWork = (*docRangeLegacyRecoveryPGWork)(nil)

func (storage *docRangeLegacyRecoveryPGStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	started := time.Now().UTC()
	prepared, err := storage.PostgresStore.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		return prepared, err
	}
	work := &docRangeLegacyRecoveryPGWork{PreparedDocRangeWork: prepared, storage: storage, job: job, started: started,
		kind: fmt.Sprintf("%T", prepared), verification: docRangeLegacyRecoveryPGVerification{Context: ctx}}
	storage.works = append(storage.works, work)
	resources, err := prepared.RetainedResources()
	if err != nil {
		return work, err
	}
	if resources.SpoolBytes != 0 || resources.HeapBytes <= 0 || resources.HeapBytes > 128<<10 || resources.KeepPermit {
		return work, fmt.Errorf("worker preparation did not leave bounded empty capture/recovery state: %+v", resources)
	}
	if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		return work, fmt.Errorf("new worker handle published before fresh verification: %v", err)
	}
	return work, nil
}

func (work *docRangeLegacyRecoveryPGWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	bound := ctx
	if work.kind == "*store.preparedLegacyRecoveryWork" {
		bound = &work.verification
	}
	progress, err := work.PreparedDocRangeWork.Step(bound, job)
	if err == nil {
		work.steps++
		if progress != outbound.DocRangeWorkPending && progress != outbound.DocRangeWorkPublishable {
			err = fmt.Errorf("legacy recovery bypassed bounded worker preparation: %d", progress)
		} else if work.storage.afterStep != nil {
			err = work.storage.afterStep(ctx, work, progress)
		}
	}
	work.failure = err
	return progress, err
}

func (work *docRangeLegacyRecoveryPGWork) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	work.publishCalls++
	if work.storage.beforePublish != nil {
		if err := work.storage.beforePublish(ctx, work); err != nil {
			work.failure = err
			return err
		}
	}
	work.failure = work.PreparedDocRangeWork.Publish(ctx, job)
	if work.failure == nil {
		work.published = true
		work.storage.publications++
	}
	return work.failure
}

func (work *docRangeLegacyRecoveryPGWork) Close() error {
	err := work.PreparedDocRangeWork.Close()
	work.closes++
	if work.published {
		err = errors.Join(err, work.storage.closeFailure)
	}
	if work.storage.closed != nil {
		work.storage.closed <- work
	}
	return err
}

func (storage *docRangeLegacyRecoveryPGStore) FinishDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	storage.finishCalls++
	if err := storage.PostgresStore.FinishDocRangeJob(ctx, job, now); err != nil {
		return err
	}
	storage.finishes++
	if storage.finished != nil {
		select {
		case storage.finished <- docRangeLegacyRecoveryPGObservation{job: job, at: now}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-storage.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type docRangeLegacyRecoveryPGFixture struct {
	docViewPGFixture
	document    domain.SessionDoc
	directory   string
	gate        *resourceAdmission
	original    string
	stage       string
	interrupted docByteUpgradePGJob
}

func docRangeLegacyRecoveryPGOriginalImage(ctx context.Context, fixture docViewPGFixture) (string, error) {
	var image string
	err := fixture.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'sources',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(owned),owned.xmin::text,
 encode(source.bytes,'hex'),source.doc_range_source_version,source.xmin::text) ORDER BY owned.kind,owned.hash)
 FROM repo_blobs owned JOIN blobs source ON source.hash=owned.hash WHERE owned.repo_id=$1),
 'snapshots',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) ORDER BY snapshot.id)
 FROM snapshots snapshot WHERE snapshot.repo_id=$1),
 'memory',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(memory),memory.xmin::text) ORDER BY memory.snapshot_id)
 FROM memories memory WHERE memory.repo_id=$1),
 'refs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(ref),ref.xmin::text) ORDER BY ref.kind,ref.name)
 FROM refs ref WHERE ref.repo_id=$1),
 'history',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(history),history.xmin::text) ORDER BY history.id)
 FROM context_history history WHERE history.repo_id=$1)
 )::text`, fixture.repo).Scan(&image)
	return image, err
}

func docRangeLegacyRecoveryPGStageImage(ctx context.Context, fixture docViewPGFixture, hash domain.ContentHash) (bool, string, error) {
	var ready bool
	var image string
	err := fixture.pool.QueryRow(ctx, `SELECT
 COALESCE((SELECT complete AND next_block=blocks AND header_version=1 AND header_complete
 AND header_bytes>131072 AND header_next=(header_bytes+65535)/65536
 AND (SELECT count(*) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1)=header_next
 AND (SELECT sum(octet_length(bytes)) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1)=header_bytes
 AND (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)=blocks
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1),false),
 COALESCE((SELECT jsonb_build_object(
 'stage',jsonb_build_array(to_jsonb(stage),stage.xmin::text),
 'headers',(SELECT jsonb_agg(jsonb_build_array(header.ordinal,header.hash,md5(header.bytes),octet_length(header.bytes),header.xmin::text) ORDER BY header.ordinal)
 FROM doc_legacy_byte_stage_headers_v1 header WHERE header.doc_hash=$1),
 'parts',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(part),part.xmin::text) ORDER BY part.ordinal)
 FROM doc_legacy_byte_stage_parts_v1 part WHERE part.doc_hash=$1),
 'objects',(SELECT jsonb_agg(jsonb_build_array(object.hash,md5(object.bytes),octet_length(object.bytes),object.xmin::text) ORDER BY object.hash)
 FROM doc_legacy_byte_stage_objects_v1 object WHERE object.doc_hash=$1)
 )::text FROM doc_legacy_byte_stages_v1 stage WHERE stage.doc_hash=$1),'')`, hash).Scan(&ready, &image)
	return ready, image, err
}

func docRangeLegacyRecoveryPGNoPublication(ctx context.Context, fixture docViewPGFixture, hash domain.ContentHash) error {
	var count int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, hash).Scan(&count); err != nil {
		return err
	}
	ready, err := fixture.storage.HasDocByteRangeIndex(ctx, fixture.repo, hash)
	if err != nil || ready || count != 0 {
		return fmt.Errorf("private recovery published before its worker transaction: ready=%t generations=%d error=%v", ready, count, err)
	}
	return nil
}

func newDocRangeLegacyRecoveryPGFixture(test *testing.T) docRangeLegacyRecoveryPGFixture {
	test.Helper()
	fixture := docRangeLegacyRecoveryPGFixture{docViewPGFixture: newDocViewPGFixture(test), gate: &resourceAdmission{wait: resourceWaitLimit}}
	ctx, cancel := context.WithTimeout(context.WithValue(fixture.ctx, resourceAdmissionKey{}, fixture.gate), 45*time.Second)
	test.Cleanup(cancel)
	fixture.ctx = ctx
	fixture.directory = test.TempDir()
	test.Setenv("TMPDIR", fixture.directory)
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture.cir.Envelope.Cwd = strings.Repeat("x", (128<<10)+17)
	fixture.document = seedLegacyFindPGDocument(test, fixture.docViewPGFixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("x", domain.ConversationManifestChunkBytes+128)},
		historyMessage(domain.RoleUser, "first Needle🙂 \"quoted\"\n"+string(fixture.repo)),
		historyMessage(domain.RoleAssistant, "second Needle🙂 <tag> & answer"),
	})
	canonical, err := domain.CanonicalBytes(fixture.document.CIR)
	if err != nil || domain.HashContent(canonical) != fixture.document.Hash {
		test.Fatal("recovery fixture lost its original canonical identity", err)
	}
	if updated, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.document.Hash, canonical); err != nil || updated.RowsAffected() != 1 {
		test.Fatal("could not seed the monolithic recovery source", updated, err)
	}
	if err := fixture.storage.PutMemoryMeta(ctx, fixture.repo, domain.MemoryDigest{
		SnapshotID: fixture.document.Hash, Summary: "preserved recovery memory \ud55c🙂", KeyFacts: []string{"immutable source"},
		OpenTasks: []string{"retain pending work"}, Provider: domain.ProviderCodex,
	}); err != nil {
		test.Fatal(err)
	}
	fixture.original, err = docRangeLegacyRecoveryPGOriginalImage(ctx, fixture.docViewPGFixture)
	if err != nil {
		test.Fatal(err)
	}
	page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, fixture.document.Hash, "", 1, 2, "chat")
	if !errors.Is(err, domain.ErrDocRangeIndexPending) || len(page.Events) != 0 {
		test.Fatal("cold recovery demand did not queue the existing byte worker", page, err)
	}
	queued := assertDocByteUpgradePGRequirement(test, fixture.docViewPGFixture, fixture.document.Hash, "waiting")
	worker, stop := context.WithCancel(ctx)
	defer stop()
	probe := &docRangeLegacyRecoveryPGStore{PostgresStore: fixture.storage}
	probe.afterStep = func(bound context.Context, work *docRangeLegacyRecoveryPGWork, progress outbound.DocRangeWorkProgress) error {
		ready, image, err := docRangeLegacyRecoveryPGStageImage(bound, fixture.docViewPGFixture, fixture.document.Hash)
		if err != nil || !ready {
			return err
		}
		if progress != outbound.DocRangeWorkPending || fixture.stage != "" || fixture.storage.InTransaction(bound) {
			return errors.New("completed byte/header checkpoint was not independently committed before worker publication")
		}
		if err := work.PreparedDocRangeWork.Publish(bound, work.job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			return fmt.Errorf("completed stage bypassed unfinished metadata: %v", err)
		}
		if err := docRangeLegacyRecoveryPGNoPublication(bound, fixture.docViewPGFixture, fixture.document.Hash); err != nil {
			return err
		}
		fixture.stage = image
		stop()
		return nil
	}
	fixture.service.blobs = probe
	if err := fixture.service.ProcessDocRangeJobs(worker, 1); !errors.Is(err, context.Canceled) || fixture.stage == "" || len(probe.works) != 1 {
		test.Fatal("worker did not stop at a real completed recovery checkpoint", err, len(probe.works))
	}
	work := probe.works[0]
	if work.kind != "*store.preparedLegacyCaptureWork" || work.closes != 1 || work.publishCalls != 0 || probe.finishCalls != 0 {
		test.Fatal("interruption skipped original preparation, published, scheduled failure or leaked its handle", work, probe.finishCalls)
	}
	fixture.interrupted = assertDocByteUpgradePGRequirement(test, fixture.docViewPGFixture, fixture.document.Hash, "running")
	if fixture.interrupted.receipt.Attempts != 1 || fixture.interrupted.receipt.Version != queued.receipt.Version+1 || fixture.interrupted.receipt.Reason != "" {
		test.Fatal("checkpoint cancellation rewrote the original job identity", queued, fixture.interrupted)
	}
	assertResourcesReleased(test, fixture.gate)
	assertDocRangeLegacyRecoveryPGNoSpools(test, fixture)
	fixture.storage.Close()
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	fixture.storage = peer
	fixture.service = NewService(peer, peer, nil, nil, peer)
	updated, err := fixture.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3`, fixture.repo, fixture.document.Hash, fixture.interrupted.receipt.Version)
	if err != nil || updated.RowsAffected() != 1 {
		test.Fatal("could not expire only the interrupted worker claim", updated, err)
	}
	return fixture
}

func assertDocRangeLegacyRecoveryPGNoSpools(test *testing.T, fixture docRangeLegacyRecoveryPGFixture) {
	test.Helper()
	files, err := os.ReadDir(fixture.directory)
	if err != nil || len(files) != 0 {
		test.Fatal("legacy recovery retained private files after closing its worker handle", files, err)
	}
}

func assertDocRangeLegacyRecoveryPGVerified(test *testing.T, work *docRangeLegacyRecoveryPGWork) {
	test.Helper()
	if work.kind != "*store.preparedLegacyRecoveryWork" || !work.verification.preparation.Load() ||
		!work.verification.step.Load() || !work.verification.event.Load() {
		test.Fatal("recovery did not execute a new source preparation and actual canonical event verification", work.kind,
			work.verification.preparation.Load(), work.verification.step.Load(), work.verification.event.Load())
	}
}

func assertDocRangeLegacyRecoveryPGPublished(test *testing.T, fixture docRangeLegacyRecoveryPGFixture, attempts int, original string) {
	test.Helper()
	completed := assertDocByteUpgradePGRequirement(test, fixture.docViewPGFixture, fixture.document.Hash, "completed")
	if completed.receipt.Attempts != attempts || completed.receipt.Version != fixture.interrupted.receipt.Version+int64(attempts-1) ||
		completed.receipt.Reason != "" || !completed.receipt.LeaseUntil.IsZero() || !completed.receipt.CreatedAt.Equal(fixture.interrupted.receipt.CreatedAt) {
		test.Fatal("recovery did not complete the same fenced job receipt", completed, fixture.interrupted)
	}
	generation := readDocByteUpgradePGGeneration(test, fixture.docViewPGFixture, fixture.document.Hash)
	if generation.Version != 5 || generation.Hash != fixture.document.Hash || generation.Identity() != domain.DocumentIdentityLegacy ||
		generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.EventCount != int64(len(fixture.document.CIR.Events)) || generation.ChunkCount() != 2 {
		test.Fatal("recovery published a different source, identity or byte layout", generation)
	}
	ready, _, err := docRangeLegacyRecoveryPGStageImage(fixture.ctx, fixture.docViewPGFixture, fixture.document.Hash)
	if err != nil || !ready {
		test.Fatal("published recovery lost its complete header/stream checkpoint", ready, err)
	}
	observed := observeLegacyDerivedPublicationPG(test, fixture.docViewPGFixture, fixture.document.Hash)
	assertLegacyDerivedPublicationPGPublicReads(test, fixture.docViewPGFixture, fixture.document, observed)
	if image, err := docRangeLegacyRecoveryPGOriginalImage(fixture.ctx, fixture.docViewPGFixture); err != nil || image != original {
		test.Fatal("recovery or public reads rewrote archival bytes, ownership, snapshots, memory, refs or history", err)
	}
	assertResourcesReleased(test, fixture.gate)
	assertDocRangeLegacyRecoveryPGNoSpools(test, fixture)
}

func TestPGDocRangeLegacyRecoveryWorkerRestartPublishesCompletedStage(test *testing.T) {
	fixture := newDocRangeLegacyRecoveryPGFixture(test)
	probe := &docRangeLegacyRecoveryPGStore{PostgresStore: fixture.storage}
	probe.afterStep = func(ctx context.Context, work *docRangeLegacyRecoveryPGWork, progress outbound.DocRangeWorkProgress) error {
		ready, image, err := docRangeLegacyRecoveryPGStageImage(ctx, fixture.docViewPGFixture, fixture.document.Hash)
		if err != nil || !ready || image != fixture.stage {
			return fmt.Errorf("restarted worker changed its acknowledged completed checkpoint: ready=%t error=%v", ready, err)
		}
		return docRangeLegacyRecoveryPGNoPublication(ctx, fixture.docViewPGFixture, fixture.document.Hash)
	}
	fixture.service.blobs = probe
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal("fresh store/service could not recover the completed stage", err)
	}
	if len(probe.works) != 1 || probe.publications != 1 || probe.finishCalls != 0 || probe.works[0].closes != 1 || probe.works[0].publishCalls != 1 {
		test.Fatal("restart did not prepare once, publish once and close exactly its replacement handle", probe)
	}
	assertDocRangeLegacyRecoveryPGVerified(test, probe.works[0])
	assertDocRangeLegacyRecoveryPGPublished(test, fixture, 2, fixture.original)
	if _, image, err := docRangeLegacyRecoveryPGStageImage(fixture.ctx, fixture.docViewPGFixture, fixture.document.Hash); err != nil || image != fixture.stage {
		test.Fatal("publication rewrote the acknowledged stream/header checkpoint", err)
	}
}

func TestPGDocRangeLegacyRecoveryWorkerAutomaticallyRetriesCurrentSourceAndPreparedFailures(tests *testing.T) {
	for _, failure := range []string{"metadata spool", "canonical spool", "source version"} {
		tests.Run(failure, func(test *testing.T) {
			fixture := newDocRangeLegacyRecoveryPGFixture(test)
			resume := make(chan struct{})
			probe := &docRangeLegacyRecoveryPGStore{PostgresStore: fixture.storage,
				finished: make(chan docRangeLegacyRecoveryPGObservation, 1), resume: resume,
				closed: make(chan *docRangeLegacyRecoveryPGWork, 4)}
			expectedOriginal := fixture.original
			injected := false
			corrupt := &docRangePreparedRetryPGStore{spoolDirectory: fixture.directory}
			probe.afterStep = func(ctx context.Context, work *docRangeLegacyRecoveryPGWork, progress outbound.DocRangeWorkProgress) error {
				if len(probe.works) != 1 || work.steps != 1 || failure == "metadata spool" {
					return nil
				}
				if work.kind != "*store.preparedLegacyRecoveryWork" || progress != outbound.DocRangeWorkPending {
					return errors.New("fault did not interrupt the first real recovery input quantum")
				}
				injected = true
				if failure == "canonical spool" {
					return corrupt.corruptSpool("cxthub-legacy-doc-*", 0)
				}
				if image, err := docRangeLegacyRecoveryPGOriginalImage(ctx, fixture.docViewPGFixture); err != nil || image != fixture.original {
					return fmt.Errorf("recovery changed the original before the deliberate source version change: %v", err)
				}
				updated, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.document.Hash)
				if err != nil || updated.RowsAffected() != 1 {
					return fmt.Errorf("could not advance only the current source version: updated=%v error=%v", updated, err)
				}
				expectedOriginal, err = docRangeLegacyRecoveryPGOriginalImage(ctx, fixture.docViewPGFixture)
				return err
			}
			probe.beforePublish = func(ctx context.Context, work *docRangeLegacyRecoveryPGWork) error {
				if len(probe.works) != 1 || failure != "metadata spool" {
					return nil
				}
				injected = true
				return corrupt.corruptSpool("cxthub-doc-range-*", 0)
			}
			fixture.service.blobs = probe
			worker, cancel := context.WithCancel(fixture.ctx)
			stopped := make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			stop := func() {
				release()
				cancel()
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					test.Fatal("automatic legacy recovery worker did not stop")
				}
			}
			test.Cleanup(stop)
			go func() {
				defer close(stopped)
				fixture.service.RunDocRangeWorker(worker)
			}()
			var scheduled docRangeLegacyRecoveryPGObservation
			select {
			case scheduled = <-probe.finished:
			case <-stopped:
				test.Fatal("legacy recovery worker stopped before recording its natural retry")
			case <-fixture.ctx.Done():
				test.Fatal("legacy recovery did not naturally record its failure", fixture.ctx.Err())
			}
			retrying := readDocByteUpgradePGJob(test, fixture.docViewPGFixture, fixture.document.Hash)
			wantReason := "prepared_artifact_corrupt"
			wantFailure := domain.ErrPreparedDocRangeObject
			if failure == "source version" {
				wantReason, wantFailure = "temporary_failure", domain.ErrConflict
			}
			if !injected || len(probe.works) != 1 || !errors.Is(probe.works[0].failure, wantFailure) ||
				retrying.receipt.State != "retrying" || retrying.receipt.Reason != wantReason || retrying.receipt.Attempts != 2 ||
				retrying.receipt.Version != fixture.interrupted.receipt.Version+1 || !retrying.receipt.LeaseUntil.IsZero() ||
				!retrying.receipt.NextAttempt.Equal(scheduled.at.Add(4*time.Second).Truncate(time.Microsecond)) ||
				!retrying.receipt.NextAttempt.Equal(scheduled.job.NextAttempt.Truncate(time.Microsecond)) {
				test.Fatal("real recovery failure was rejected, hidden or lost its existing receipt/backoff", failure, retrying, scheduled)
			}
			if probe.finishCalls != 1 || probe.finishes != 1 || probe.publications != 0 || probe.works[0].closes != 1 {
				test.Fatal("failed recovery leaked a handle or published instead of scheduling its one retry", probe)
			}
			ready, image, err := docRangeLegacyRecoveryPGStageImage(fixture.ctx, fixture.docViewPGFixture, fixture.document.Hash)
			if err != nil || !ready || image != fixture.stage {
				test.Fatal("recovery discarded a fenced stage or retained a failed canonical checkpoint", failure, ready, err)
			}
			if err := docRangeLegacyRecoveryPGNoPublication(fixture.ctx, fixture.docViewPGFixture, fixture.document.Hash); err != nil {
				test.Fatal(err)
			}
			assertDocRangeLegacyRecoveryPGNoSpools(test, fixture)
			release()
			completed := false
			for !completed {
				select {
				case work := <-probe.closed:
					completed = work.published
				case unexpected := <-probe.finished:
					test.Fatal("automatic retry failed again instead of replacing stale/corrupt recovery state", unexpected)
				case <-stopped:
					test.Fatal("legacy recovery worker stopped before final publication")
				case <-fixture.ctx.Done():
					test.Fatal("natural worker backoff did not reach final publication", fixture.ctx.Err())
				}
			}
			stop()
			if len(probe.works) != 2 || probe.publications != 1 || probe.finishCalls != 1 || probe.finishes != 1 ||
				probe.works[1].closes != 1 || !probe.works[1].job.NextAttempt.Equal(retrying.receipt.NextAttempt) ||
				probe.works[1].started.Before(retrying.receipt.NextAttempt) {
				test.Fatal("automatic worker retry did not reclaim once and publish the same job exactly once", probe)
			}
			if completed := readDocByteUpgradePGJob(test, fixture.docViewPGFixture, fixture.document.Hash); !completed.receipt.NextAttempt.Equal(retrying.receipt.NextAttempt) {
				test.Fatal("final publication changed the durable retry deadline", completed, retrying)
			}
			if failure == "metadata spool" || failure == "canonical spool" {
				if failure == "metadata spool" {
					assertDocRangeLegacyRecoveryPGVerified(test, probe.works[0])
				}
				assertDocRangeLegacyRecoveryPGVerified(test, probe.works[1])
				if _, image, err := docRangeLegacyRecoveryPGStageImage(fixture.ctx, fixture.docViewPGFixture, fixture.document.Hash); err != nil || image != fixture.stage {
					test.Fatal("automatic metadata retry rewrote the complete reusable checkpoint", err)
				}
			} else if probe.works[1].kind != "*store.preparedLegacyCaptureWork" {
				test.Fatal("discarded or initially stale stage did not fall back to fresh original capture", probe.works[1].kind)
			}
			assertDocRangeLegacyRecoveryPGPublished(test, fixture, 3, expectedOriginal)
		})
	}
}

func TestPGDocRangeLegacyRecoveryCompletedJobSurvivesCloseErrorAndLateWorker(test *testing.T) {
	fixture := newDocRangeLegacyRecoveryPGFixture(test)
	cleanupFailure := errors.Join(domain.ErrPreparedDocRangeObject, errors.New("completed recovery cleanup failed"))
	probe := &docRangeLegacyRecoveryPGStore{PostgresStore: fixture.storage, closeFailure: cleanupFailure}
	fixture.service.blobs = probe
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); !errors.Is(err, cleanupFailure) {
		test.Fatal("worker hid the post-commit cleanup failure", err)
	}
	if len(probe.works) != 1 || probe.publications != 1 || probe.finishCalls != 0 || probe.works[0].closes != 1 {
		test.Fatal("successful publication was demoted through failure scheduling after Close", probe)
	}
	assertDocRangeLegacyRecoveryPGVerified(test, probe.works[0])
	completed := readDocByteUpgradePGJob(test, fixture.docViewPGFixture, fixture.document.Hash)
	image := docByteUpgradePGImage(test, fixture.docViewPGFixture)
	if receipt, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, fixture.document.Hash, time.Now().UTC()); err != nil || !reflect.DeepEqual(receipt, completed.receipt) {
		test.Fatal("same completed byte demand was downgraded or requeued", receipt, completed, err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil || len(probe.works) != 1 {
		test.Fatal("completed recovery job was claimed again", err, len(probe.works))
	}
	if err := fixture.service.runDocRangeJob(fixture.ctx, probe, probe.works[0].job, docJobLease); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("late worker did not reject the obsolete running claim", err)
	}
	if probe.finishCalls != 1 || probe.finishes != 0 || probe.publications != 1 {
		test.Fatal("late worker changed the completed job through a stale failure CAS", probe)
	}
	if after := docByteUpgradePGImage(test, fixture.docViewPGFixture); after != image {
		test.Fatal("cleanup error, duplicate demand or late failure changed the completed job or generation")
	}
	assertDocRangeLegacyRecoveryPGPublished(test, fixture, 2, fixture.original)
}
