//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeWorkPGProbe struct {
	*store.PostgresStore
	afterPrepare func(context.Context, outbound.RootPublicationResources) error
	afterStep    func(context.Context, int, bool) error
	job          domain.DocRangeIndexJob
	steps        int
	published    int
	closed       int
	initial      []outbound.RootPublicationResources
	resources    []outbound.RootPublicationResources
}

type docRangeWorkPGPrepared struct {
	outbound.PreparedDocRangeWork
	probe *docRangeWorkPGProbe
}

func (probe *docRangeWorkPGProbe) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	prepared, err := probe.PostgresStore.PrepareDocRangeWork(ctx, job)
	if prepared == nil || err != nil {
		return prepared, err
	}
	resources, err := prepared.RetainedResources()
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	probe.initial = append(probe.initial, resources)
	probe.job = job
	work := &docRangeWorkPGPrepared{PreparedDocRangeWork: prepared, probe: probe}
	if probe.afterPrepare != nil {
		return work, probe.afterPrepare(ctx, resources)
	}
	return work, nil
}

func (prepared *docRangeWorkPGPrepared) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	progress, err := prepared.PreparedDocRangeWork.Step(ctx, job)
	if err != nil {
		return 0, err
	}
	if progress != outbound.DocRangeWorkPending && progress != outbound.DocRangeWorkPublishable {
		return 0, fmt.Errorf("monolithic work returned invalid progress: %d", progress)
	}
	complete := progress == outbound.DocRangeWorkPublishable
	if !complete {
		if err := prepared.PreparedDocRangeWork.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			return 0, fmt.Errorf("unfinished work accepted publication: %v", err)
		}
	}
	prepared.probe.steps++
	resources, err := prepared.RetainedResources()
	if err != nil {
		return 0, err
	}
	prepared.probe.resources = append(prepared.probe.resources, resources)
	if prepared.probe.afterStep != nil {
		err = prepared.probe.afterStep(ctx, prepared.probe.steps, complete)
	}
	return progress, err
}

func (prepared *docRangeWorkPGPrepared) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	if err := prepared.PreparedDocRangeWork.Publish(ctx, job); err != nil {
		return err
	}
	prepared.probe.published++
	return nil
}

func (prepared *docRangeWorkPGPrepared) Close() error {
	prepared.probe.closed++
	return prepared.PreparedDocRangeWork.Close()
}

func TestPGDocRangeWorkYieldsBetweenCommittedBatchesAndResumesAfterCancellation(tests *testing.T) {
	for _, interrupted := range []bool{false, true} {
		tests.Run(fmt.Sprintf("cancelled=%t", interrupted), func(test *testing.T) {
			fixture := newDocViewPGFixture(test)
			fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
			if err := fixture.storage.EnsureDocRangeIndex(fixture.ctx, fixture.repo, fixture.hash); err != nil {
				test.Fatal(err)
			}
			document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
				{Kind: domain.EventToolResult, Output: strings.Repeat("x", 5<<20)},
				historyMessage(domain.RoleUser, "work boundary "+string(fixture.repo)),
			})
			canonical, err := domain.CanonicalBytes(document.CIR)
			if err != nil {
				test.Fatal(err)
			}
			streamBytes, sourceSteps := docRangeWorkPGSourceQuanta(test, canonical)
			if sourceSteps != 11 {
				test.Fatal("fixture did not require eleven metadata source quanta", streamBytes, sourceSteps)
			}
			initialSteps := docRangeWorkPGInitialSteps(len(canonical), streamBytes)
			checkpoint := initialSteps + 1
			wantSteps := initialSteps + sourceSteps + 3
			if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, canonical); err != nil {
				test.Fatal(err)
			}
			if _, err := fixture.storage.EnqueueDocByteRangeJob(fixture.ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
				test.Fatal(err)
			}
			probe := &docRangeWorkPGProbe{PostgresStore: fixture.storage}
			fixture.service.blobs = probe
			gate := &resourceAdmission{wait: resourceWaitLimit}
			ctx, cancel := context.WithTimeout(context.WithValue(fixture.ctx, resourceAdmissionKey{}, gate), 30*time.Second)
			defer cancel()
			worker, cancelWorker := context.WithCancel(ctx)
			defer cancelWorker()
			foreground := make(chan error, 1)
			var readCompleted atomic.Bool
			var prefix string
			readPrefix := func() (string, error) {
				var image string
				err := fixture.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(to_jsonb(part),part.xmin::text,object.hash,md5(object.bytes),octet_length(object.bytes),object.xmin::text) ORDER BY part.ordinal),'[]'::jsonb)::text
 FROM doc_legacy_byte_stage_parts_v1 part JOIN doc_legacy_byte_stage_objects_v1 object
 ON object.doc_hash=part.doc_hash AND object.hash=part.hash WHERE part.doc_hash=$1 AND part.ordinal<8`, document.Hash).Scan(&image)
				return image, err
			}
			probe.afterPrepare = func(bound context.Context, resources outbound.RootPublicationResources) error {
				if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceSteps, 0, resources); err != nil {
					return err
				}
				return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
			}
			probe.afterStep = func(bound context.Context, step int, complete bool) error {
				if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceSteps, step, probe.resources[step-1]); err != nil {
					return err
				}
				if complete != (step == wantSteps) {
					return fmt.Errorf("metadata completed before its final chunk directory: step=%d source_steps=%d complete=%t", step, sourceSteps, complete)
				}
				if step == initialSteps+sourceSteps && probe.resources[step-1].SpoolBytes <= int64(streamBytes) {
					return errors.New("final source quantum did not flush its final event page")
				}
				if step > initialSteps+sourceSteps && probe.resources[step-1].SpoolBytes <= probe.resources[step-2].SpoolBytes {
					return errors.New("directory finalization did not flush its next metadata object")
				}
				if ready, err := fixture.storage.HasDocByteRangeIndex(bound, fixture.repo, document.Hash); err != nil || ready {
					return fmt.Errorf("private metadata quantum advertised public byte readiness: ready=%t error=%v", ready, err)
				}
				if step <= initialSteps {
					return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
				}
				if step > checkpoint {
					if !readCompleted.Load() {
						return errors.New("worker advanced before queued foreground read")
					}
					permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
					if permit == nil || permit.resumeWait.Load() <= 0 {
						return errors.New("worker did not resume its owned permit after foreground progress")
					}
					return nil
				}
				var nextBlock, blocks, openTransactions int
				if err := fixture.pool.QueryRow(ctx, `SELECT next_block,blocks FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, document.Hash).Scan(&nextBlock, &blocks); err != nil {
					return err
				}
				if complete || nextBlock != 8 || blocks != 11 || fixture.storage.InTransaction(bound) {
					return fmt.Errorf("step did not close an eight-block checkpoint: next=%d blocks=%d complete=%t", nextBlock, blocks, complete)
				}
				if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='idle in transaction'`).Scan(&openTransactions); err != nil {
					return err
				}
				if openTransactions != 0 {
					return fmt.Errorf("yield boundary retained %d idle transactions", openTransactions)
				}
				var err error
				prefix, err = readPrefix()
				if err != nil {
					return err
				}
				if len(probe.initial) != 1 || probe.initial[0].SpoolBytes != 0 ||
					probe.resources[step-1].SpoolBytes != int64(streamBytes) || probe.resources[step-1].HeapBytes < domain.ConversationManifestChunkBytes {
					return errors.New("first giant-record quantum hid its actual buffer or eagerly emitted metadata objects")
				}
				go func() {
					page, err := fixture.service.ReadDocEvents(ctx, fixture.repo, fixture.hash, "", 0, 20)
					if err == nil && (len(page.Events) != 20 || page.Events[0].Seq != 0 || page.Next != 20 || page.Hash != fixture.hash) {
						err = errors.New("foreground page lost its pinned range")
					}
					readCompleted.Store(true)
					foreground <- err
				}()
				waitResourceQueue(test, gate, ResourceBulk, 1)
				page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, fixture.hash, "", 0, 20, "chat")
				if err != nil {
					return err
				}
				if len(page.Events) != 20 || page.Events[0].Index != 0 || page.Next != 20 {
					return errors.New("ready interactive page lost its independent admission slot")
				}
				if interrupted {
					cancelWorker()
				}
				return nil
			}
			err = fixture.service.ProcessDocRangeJobs(worker, 1)
			if interrupted {
				if !errors.Is(err, context.Canceled) || probe.steps != checkpoint || probe.published != 0 || probe.closed != 1 {
					test.Fatal("cancelled work advanced or leaked its prepared handle", err, probe)
				}
			} else if err != nil || probe.steps != wantSteps || probe.published != 1 || probe.closed != 1 {
				test.Fatal("bounded work failed to yield, resume or publish", err, probe)
			}
			select {
			case err := <-foreground:
				if err != nil {
					test.Fatal("queued foreground page failed", err)
				}
			case <-ctx.Done():
				test.Fatal("queued foreground page did not finish", ctx.Err())
			}
			assertResourcesReleased(test, gate)
			if interrupted {
				probe.afterPrepare = func(bound context.Context, resources outbound.RootPublicationResources) error {
					return docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceSteps, 0, resources)
				}
				probe.afterStep = nil
				if _, err := fixture.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running'`, fixture.repo, document.Hash); err != nil {
					test.Fatal(err)
				}
				if err := fixture.service.ProcessDocRangeJobs(ctx, 1); err != nil || probe.steps != wantSteps+checkpoint || probe.published != 1 || probe.closed != 2 {
					test.Fatal("replacement worker did not restart metadata while resuming the durable byte prefix", err, probe)
				}
				assertResourcesReleased(test, gate)
			}
			if after, err := readPrefix(); err != nil || after != prefix {
				test.Fatal("continued or replacement work rewrote its acknowledged eight-block byte prefix", err)
			}
			if len(probe.initial) != probe.closed || len(probe.resources) != probe.steps {
				test.Fatal("resource probe missed a prepared phase or metadata quantum", probe)
			}
			for _, resources := range probe.initial {
				if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceSteps, 0, resources); err != nil {
					test.Fatal("preparation did not leave an empty, bounded stored capture", resources, err)
				}
			}
			for ordinal, resources := range probe.resources {
				step := ordinal + 1
				if interrupted && step > checkpoint {
					step -= checkpoint
				}
				if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceSteps, step, resources); err != nil {
					test.Fatal("actual prepared resources do not match their canonical source or metadata phase", resources, err)
				}
			}
			generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
			if generation.Version != 5 || generation.ChunkCount() != 11 || generation.Hash != document.Hash {
				test.Fatal("cooperative worker published a different byte generation", generation)
			}
			assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
			test.Logf("steps=%d publications=%d closes=%d retained=%+v", probe.steps, probe.published, probe.closed, probe.resources)
		})
	}
}
