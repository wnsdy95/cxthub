//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangePreparedRetryPGObservation struct {
	job          domain.DocRangeIndexJob
	at           time.Time
	resources    outbound.RootPublicationResources
	privateStage string
	kind         string
}

type docRangePreparedRetryPGStepObservation struct {
	step         int
	complete     bool
	err          error
	privateStage string
	corrupted    bool
}

type docRangePreparedRetryPGStore struct {
	*store.PostgresStore
	repo            domain.ContentHash
	hash            domain.ContentHash
	prepares        atomic.Int64
	publishCalls    atomic.Int64
	publications    atomic.Int64
	closes          atomic.Int64
	finishes        atomic.Int64
	steps           atomic.Int64
	proofSteps      atomic.Int64
	recoveryHandoff atomic.Int64
	handoffs        atomic.Int64
	directorySteps  atomic.Int64
	sourceQuanta    int
	streamBytes     int
	rawBytes        int
	prepared        chan docRangePreparedRetryPGObservation
	finished        chan docRangePreparedRetryPGObservation
	retrySeen       chan struct{}
	spoolDirectory  string
	corruptSource   bool
	sourceSteps     chan docRangePreparedRetryPGStepObservation
	sourceStage     func(context.Context) (string, error)
	noByteStage     func(context.Context) error
	liveClaim       func(context.Context, domain.DocRangeIndexJob) error
}

type docRangePreparedRetryPGWork struct {
	outbound.PreparedDocRangeWork
	storage      *docRangePreparedRetryPGStore
	attempt      int64
	steps        int
	privateStage string
	recovery     bool
	handoffStep  int
	metadata     uintptr
	lastSpool    int64
	verification docRangeLegacyRecoveryPGVerification
}

type docRangePreparedRetryPGRecoveryPhase struct {
	input         bool
	proofData     bool
	canonical     bool
	inputBytes    int64
	verifiedBytes int64
	metadata      uintptr
}

func docRangePreparedRetryPGSpoolBytes(spool reflect.Value) (int64, error) {
	if !spool.IsValid() || spool.Kind() != reflect.Pointer {
		return 0, errors.New("prepared retry spool is not an inspectable pointer")
	}
	if spool.IsNil() {
		return 0, nil
	}
	if spool.Elem().Kind() != reflect.Struct {
		return 0, errors.New("prepared retry spool does not contain a struct")
	}
	size := spool.Elem().FieldByName("size")
	if !size.IsValid() || size.Kind() != reflect.Int {
		return 0, errors.New("prepared retry spool has no observable byte extent")
	}
	return size.Int(), nil
}

func docRangePreparedRetryPGInspectRecovery(handle outbound.PreparedDocRangeWork) (docRangePreparedRetryPGRecoveryPhase, error) {
	var state docRangePreparedRetryPGRecoveryPhase
	value := reflect.ValueOf(handle)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() || value.Type().String() != "*store.preparedLegacyRecoveryWork" {
		return state, fmt.Errorf("ready retry did not select durable canonical recovery: %T", handle)
	}
	recovery := value.Elem()
	input, data := recovery.FieldByName("input"), recovery.FieldByName("data")
	canonical, metadata := recovery.FieldByName("canonical"), recovery.FieldByName("work")
	for _, field := range []reflect.Value{input, data, canonical, metadata} {
		if !field.IsValid() || field.Kind() != reflect.Pointer {
			return state, errors.New("ready recovery lacks observable input, proof spool or metadata handoff")
		}
	}
	state.input, state.proofData, state.canonical = !input.IsNil(), !data.IsNil(), !canonical.IsNil()
	var err error
	if state.inputBytes, err = docRangePreparedRetryPGSpoolBytes(input); err != nil {
		return state, err
	}
	if !metadata.IsNil() {
		if metadata.Elem().Kind() != reflect.Struct {
			return state, errors.New("recovery metadata handoff is not an inspectable work struct")
		}
		state.metadata = metadata.Pointer()
		data = metadata.Elem().FieldByName("data")
	}
	state.verifiedBytes, err = docRangePreparedRetryPGSpoolBytes(data)
	return state, err
}

func (work *docRangePreparedRetryPGWork) observeRecovery(before, after docRangePreparedRetryPGRecoveryPhase, resources outbound.RootPublicationResources) (int, error) {
	storage := work.storage
	chunkBytes := int64(domain.ConversationManifestChunkBytes)
	if resources.KeepPermit || resources.HeapBytes <= 0 || resources.SpoolBytes < 0 || resources.SpoolBytes > maxParkedRootSpoolBytes {
		return 0, fmt.Errorf("invalid recovery retained resources: %+v", resources)
	}
	if after.metadata != 0 {
		if after.input || after.proofData || after.canonical || after.verifiedBytes != int64(storage.streamBytes) {
			return 0, errors.New("retry metadata handoff retained its canonical input or proof owner")
		}
		if work.handoffStep == 0 {
			if before.metadata != 0 || !before.canonical || before.inputBytes != int64(storage.rawBytes) || before.verifiedBytes != int64(storage.streamBytes) ||
				!work.verification.preparation.Load() || !work.verification.step.Load() || !work.verification.event.Load() {
				return 0, errors.New("retry handed off metadata without a fresh canonical preparation and actual event verification")
			}
			work.handoffStep, work.metadata = work.steps, after.metadata
			storage.recoveryHandoff.Add(int64(work.steps))
			storage.handoffs.Add(1)
		}
		metadataStep := work.steps - work.handoffStep
		if work.metadata != after.metadata {
			return 0, errors.New("retry replaced the actual metadata work after its observed handoff")
		}
		initialSteps := docRangeWorkPGInitialSteps(storage.rawBytes, storage.streamBytes)
		if err := docRangeWorkPGResourcePhase(storage.streamBytes, storage.rawBytes, storage.sourceQuanta, initialSteps+metadataStep, resources); err != nil {
			return 0, err
		}
		if metadataStep < storage.sourceQuanta && resources.SpoolBytes != int64(storage.streamBytes) ||
			metadataStep == storage.sourceQuanta && resources.SpoolBytes <= int64(storage.streamBytes) ||
			metadataStep > storage.sourceQuanta && resources.SpoolBytes <= work.lastSpool {
			return 0, fmt.Errorf("retry metadata source/directory quantum did not flush its exact next object: step=%d resources=%+v", metadataStep, resources)
		}
		work.lastSpool = resources.SpoolBytes
		return metadataStep, nil
	}
	if work.handoffStep != 0 || before.metadata != 0 || !after.input || after.inputBytes < 0 || after.inputBytes > int64(storage.rawBytes) ||
		after.verifiedBytes < 0 || after.verifiedBytes > int64(storage.streamBytes) || resources.SpoolBytes != after.inputBytes+after.verifiedBytes ||
		after.inputBytes < before.inputBytes || after.inputBytes-before.inputBytes > chunkBytes ||
		after.verifiedBytes < before.verifiedBytes || after.verifiedBytes-before.verifiedBytes > chunkBytes ||
		after.inputBytes-before.inputBytes+after.verifiedBytes-before.verifiedBytes > chunkBytes ||
		after.verifiedBytes > 0 && after.inputBytes != int64(storage.rawBytes) {
		return 0, errors.New("retry canonical recovery lost its exact bounded input/verified spool extents")
	}
	heapLimit := int64(2 << 20)
	if after.verifiedBytes > 0 && after.verifiedBytes < int64(storage.streamBytes) {
		heapLimit += 4 * after.verifiedBytes
	}
	if resources.HeapBytes >= heapLimit {
		return 0, fmt.Errorf("retry canonical input/verified spool retained unbounded heap: %+v", resources)
	}
	return 0, nil
}

var _ outbound.DocRangeWorkStore = (*docRangePreparedRetryPGStore)(nil)
var _ outbound.DocRangeJobStore = (*docRangePreparedRetryPGStore)(nil)
var _ outbound.PreparedDocRangeWork = (*docRangePreparedRetryPGWork)(nil)

func (storage *docRangePreparedRetryPGStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	started := time.Now().UTC()
	prepared, err := storage.PostgresStore.PrepareDocRangeWork(ctx, job)
	if prepared == nil || err != nil || job.RepoID != storage.repo || job.DocHash != storage.hash || job.BaseHash != "" {
		return prepared, err
	}
	work := &docRangePreparedRetryPGWork{PreparedDocRangeWork: prepared, storage: storage,
		verification: docRangeLegacyRecoveryPGVerification{Context: ctx}}
	work.attempt = storage.prepares.Add(1)
	resources, err := prepared.RetainedResources()
	if err != nil {
		return work, err
	}
	observation := docRangePreparedRetryPGObservation{job: job, at: started, resources: resources, kind: fmt.Sprintf("%T", prepared)}
	if work.attempt == 1 {
		if err := docRangeWorkPGResourcePhase(storage.streamBytes, storage.rawBytes, storage.sourceQuanta, 0, resources); err != nil {
			return work, err
		}
		if observation.kind != "*store.preparedLegacyCaptureWork" {
			return work, fmt.Errorf("first attempt bypassed original raw capture: %s", observation.kind)
		}
		if err := storage.noByteStage(ctx); err != nil {
			return work, err
		}
	} else {
		work.recovery = true
		state, err := docRangePreparedRetryPGInspectRecovery(prepared)
		if err != nil {
			return work, err
		}
		if !state.input || state.inputBytes != 0 || state.proofData || state.canonical || state.metadata != 0 ||
			resources.SpoolBytes != 0 || resources.KeepPermit || resources.HeapBytes <= 0 || resources.HeapBytes > 128<<10 {
			return work, errors.New("ready retry did not retain an empty bounded recovery input")
		}
		observation.privateStage, err = storage.sourceStage(ctx)
		if err != nil {
			return work, err
		}
		work.privateStage = observation.privateStage
	}
	if err := storage.liveClaim(ctx, job); err != nil {
		return work, err
	}
	select {
	case storage.prepared <- observation:
	case <-ctx.Done():
		return work, ctx.Err()
	default:
	}
	return work, nil
}

func (storage *docRangePreparedRetryPGStore) FinishDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	if err := storage.PostgresStore.FinishDocRangeJob(ctx, job, now); err != nil {
		return err
	}
	if job.RepoID != storage.repo || job.DocHash != storage.hash || job.BaseHash != "" || storage.finishes.Add(1) != 1 {
		return nil
	}
	storage.finished <- docRangePreparedRetryPGObservation{job: job, at: now}
	select {
	case <-storage.retrySeen:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (storage *docRangePreparedRetryPGStore) corruptSpool(pattern string, offset int64) (result error) {
	paths, err := filepath.Glob(filepath.Join(storage.spoolDirectory, pattern))
	if err != nil || len(paths) != 1 {
		return fmt.Errorf("expected one owned prepared spool: pattern=%s paths=%v error=%v", pattern, paths, err)
	}
	file, err := os.OpenFile(paths[0], os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	var body [1]byte
	if count, err := file.ReadAt(body[:], offset); err != nil || count != len(body) {
		return fmt.Errorf("read prepared spool corruption byte: count=%d error=%v", count, err)
	}
	body[0] ^= 1
	if count, err := file.WriteAt(body[:], offset); err != nil || count != len(body) {
		return fmt.Errorf("write prepared spool corruption byte: count=%d error=%v", count, err)
	}
	return nil
}

func (work *docRangePreparedRetryPGWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	var before docRangePreparedRetryPGRecoveryPhase
	bound := ctx
	if work.recovery {
		var err error
		before, err = docRangePreparedRetryPGInspectRecovery(work.PreparedDocRangeWork)
		if err != nil {
			return 0, err
		}
		bound = &work.verification
	}
	progress, err := work.PreparedDocRangeWork.Step(bound, job)
	complete := progress == outbound.DocRangeWorkPublishable
	if err == nil && progress != outbound.DocRangeWorkPending && progress != outbound.DocRangeWorkPublishable {
		err = fmt.Errorf("monolithic retry work returned invalid progress: %d", progress)
	}
	work.steps++
	work.storage.steps.Add(1)
	initialSteps := docRangeWorkPGInitialSteps(work.storage.rawBytes, work.storage.streamBytes)
	decodedSteps := (work.storage.rawBytes+(512<<10)-1)/(512<<10) + work.storage.rawBytes/(512<<10) + 1
	metadataStep := work.steps - initialSteps
	if err == nil {
		resources, resourcesErr := work.RetainedResources()
		err = resourcesErr
		if err == nil {
			if work.recovery {
				var after docRangePreparedRetryPGRecoveryPhase
				after, err = docRangePreparedRetryPGInspectRecovery(work.PreparedDocRangeWork)
				if err == nil {
					metadataStep, err = work.observeRecovery(before, after, resources)
				}
			} else {
				err = docRangeWorkPGResourcePhase(work.storage.streamBytes, work.storage.rawBytes, work.storage.sourceQuanta, work.steps, resources)
			}
		}
	}
	if err == nil && complete != (metadataStep == work.storage.sourceQuanta+3) {
		err = fmt.Errorf("prepared retry metadata completed outside its observed final chunk directory: step=%d metadata_step=%d source_quanta=%d complete=%t", work.steps, metadataStep, work.storage.sourceQuanta, complete)
	}
	if err == nil && !work.recovery && work.steps <= initialSteps {
		if work.steps > decodedSteps {
			work.storage.proofSteps.Add(1)
		}
		err = work.storage.noByteStage(ctx)
	}
	if err == nil && work.recovery {
		var image string
		image, err = work.storage.sourceStage(ctx)
		if err == nil && image != work.privateStage {
			err = errors.New("retry canonical proof or metadata rewrote the retained complete SQL stream/header stage")
		}
	}
	if err == nil {
		err = work.storage.liveClaim(ctx, job)
	}
	if err == nil {
		if metadataStep > work.storage.sourceQuanta {
			work.storage.directorySteps.Add(1)
		}
		if !complete {
			if pending := work.PreparedDocRangeWork.Publish(ctx, job); !errors.Is(pending, domain.ErrDocRangeIndexPending) {
				err = fmt.Errorf("prepared retry published before canonical proof, metadata and directory completion: %v", pending)
			}
		}
		if ready, readinessErr := work.storage.HasDocByteRangeIndex(ctx, job.RepoID, job.DocHash); readinessErr != nil || ready {
			err = fmt.Errorf("prepared retry step advertised public byte readiness: ready=%t error=%v", ready, readinessErr)
		}
	}
	if !work.storage.corruptSource || work.attempt != 1 {
		return progress, err
	}
	observation := docRangePreparedRetryPGStepObservation{step: work.steps, complete: complete}
	if work.steps == initialSteps+2 && err == nil {
		if complete {
			err = errors.New("second metadata source quantum completed work before source corruption")
		} else {
			observation.privateStage, err = work.storage.sourceStage(ctx)
			if err == nil {
				err = work.storage.corruptSpool("cxthub-legacy-doc-*", int64(2*domain.ConversationManifestChunkBytes))
				observation.corrupted = err == nil
			}
		}
	}
	observation.err = err
	select {
	case work.storage.sourceSteps <- observation:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return progress, err
}

func (work *docRangePreparedRetryPGWork) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	if work.storage.publishCalls.Add(1) == 1 && !work.storage.corruptSource {
		if err := work.storage.corruptSpool("cxthub-doc-range-*", 0); err != nil {
			return err
		}
	}
	if err := work.PreparedDocRangeWork.Publish(ctx, job); err != nil {
		return err
	}
	work.storage.publications.Add(1)
	return nil
}

func (work *docRangePreparedRetryPGWork) Close() error {
	err := work.PreparedDocRangeWork.Close()
	work.storage.closes.Add(1)
	return err
}
