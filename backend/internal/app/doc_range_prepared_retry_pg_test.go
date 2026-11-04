//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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

func docRangePreparedRetryPGSourceStageImage(ctx context.Context, fixture docViewPGFixture, hash domain.ContentHash, wantBlocks int) (string, error) {
	var nextBlock, blocks, parts int
	var complete bool
	var headerVersion, headerBytes, headerNext, headerParts, storedHeaderBytes int
	var headerComplete bool
	var image string
	err := fixture.pool.QueryRow(ctx, `SELECT stage.next_block,stage.blocks,stage.complete,
 (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1),
 stage.header_version,stage.header_bytes,stage.header_next,stage.header_complete,
 (SELECT count(*) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1),
 (SELECT COALESCE(sum(octet_length(bytes)),0) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1),
 jsonb_build_object(
 'header',jsonb_build_array(to_jsonb(stage),stage.xmin::text),
 'envelope',(SELECT jsonb_agg(jsonb_build_array(header.ordinal,header.hash,md5(header.bytes),octet_length(header.bytes),header.xmin::text) ORDER BY header.ordinal)
 FROM doc_legacy_byte_stage_headers_v1 header WHERE header.doc_hash=$1),
 'objects',(SELECT jsonb_agg(jsonb_build_array(object.hash,md5(object.bytes),octet_length(object.bytes),object.xmin::text) ORDER BY object.hash)
 FROM doc_legacy_byte_stage_objects_v1 object WHERE object.doc_hash=$1),
 'parts',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(part),part.xmin::text) ORDER BY part.ordinal)
 FROM doc_legacy_byte_stage_parts_v1 part WHERE part.doc_hash=$1)
 )::text FROM doc_legacy_byte_stages_v1 stage WHERE stage.doc_hash=$1`, hash).
		Scan(&nextBlock, &blocks, &complete, &parts, &headerVersion, &headerBytes, &headerNext, &headerComplete, &headerParts, &storedHeaderBytes, &image)
	if err != nil {
		return "", err
	}
	if !complete || nextBlock != wantBlocks || blocks != wantBlocks || parts != wantBlocks || headerVersion != 1 || !headerComplete ||
		headerBytes < 2 || headerNext != (headerBytes+65535)/65536 || headerParts != headerNext || storedHeaderBytes != headerBytes || fixture.storage.InTransaction(ctx) {
		return "", fmt.Errorf("prepared corruption requires committed stream/header extents: next=%d blocks=%d parts=%d want=%d complete=%t header_version=%d header_bytes=%d header_next=%d header_parts=%d header_complete=%t", nextBlock, blocks, parts, wantBlocks, complete, headerVersion, headerBytes, headerNext, headerParts, headerComplete)
	}
	return image, nil
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

func TestPGDocRangeWorkerAutomaticallyRetriesPreparedArtifactCorruption(test *testing.T) {
	testPGDocRangeWorkerAutomaticallyRetriesPreparedArtifactCorruption(test, false)
}

func TestPGDocRangeWorkerAutomaticallyRetriesPreparedSourceSpoolCorruption(test *testing.T) {
	testPGDocRangeWorkerAutomaticallyRetriesPreparedArtifactCorruption(test, true)
}

func testPGDocRangeWorkerAutomaticallyRetriesPreparedArtifactCorruption(test *testing.T, corruptSource bool) {
	test.Helper()
	fixture := newDocViewPGFixture(test)
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	gate := &resourceAdmission{wait: resourceWaitLimit}
	ctx, cancel := context.WithTimeout(context.WithValue(fixture.ctx, resourceAdmissionKey{}, gate), 30*time.Second)
	defer cancel()
	fixture.ctx = ctx
	directory := test.TempDir()
	for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
		test.Setenv(variable, directory)
	}
	wantChunks, failedPublishCalls := 2, int64(1)
	if corruptSource {
		wantChunks, failedPublishCalls = 3, 0
	}
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("x", (wantChunks-1)*domain.ConversationManifestChunkBytes+128)},
		historyMessage(domain.RoleUser, "first Needle🙂 \"quoted\"\n"+string(fixture.repo)),
		historyMessage(domain.RoleAssistant, "second Needle🙂 <tag> & answer"),
	})
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil || domain.HashContent(canonical) != document.Hash {
		test.Fatal("fixture lost its canonical legacy identity", err)
	}
	streamBytes, sourceQuanta := docRangeWorkPGSourceQuanta(test, canonical)
	if sourceQuanta != wantChunks {
		test.Fatal("retry fixture has an unexpected number of source quanta", streamBytes, sourceQuanta, wantChunks)
	}
	updated, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, canonical)
	if err != nil || updated.RowsAffected() != 1 {
		test.Fatal("could not seed the monolithic original", updated, err)
	}
	sourceImage := legacyByteRangePGSourceImage(test, fixture)
	var sourceVersion int64
	var sourceID string
	if err := fixture.pool.QueryRow(ctx, `SELECT source.doc_range_source_version,incarnation.incarnation::text
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$1`, document.Hash).
		Scan(&sourceVersion, &sourceID); err != nil {
		test.Fatal(err)
	}
	storage := &docRangePreparedRetryPGStore{
		PostgresStore:  fixture.storage,
		repo:           fixture.repo,
		hash:           document.Hash,
		sourceQuanta:   wantChunks,
		streamBytes:    streamBytes,
		rawBytes:       len(canonical),
		spoolDirectory: directory,
		corruptSource:  corruptSource,
		prepared:       make(chan docRangePreparedRetryPGObservation, 2),
		finished:       make(chan docRangePreparedRetryPGObservation, 1),
		retrySeen:      make(chan struct{}),
	}
	storage.noByteStage = func(ctx context.Context) error {
		return docRangeWorkPGNoByteStage(ctx, fixture, document.Hash)
	}
	storage.sourceStage = func(ctx context.Context) (string, error) {
		return docRangePreparedRetryPGSourceStageImage(ctx, fixture, document.Hash, wantChunks)
	}
	storage.liveClaim = func(ctx context.Context, job domain.DocRangeIndexJob) error {
		var live bool
		err := fixture.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_jobs_v1 job
 JOIN snapshots snapshot ON snapshot.repo_id=job.repo_id AND snapshot.id=job.doc_hash AND snapshot.doc_hash=job.doc_hash AND snapshot.doc_identity=''
 JOIN repo_blobs owned ON owned.repo_id=job.repo_id AND owned.kind='doc' AND owned.hash=job.doc_hash
 JOIN blobs source ON source.hash=owned.hash JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE job.repo_id=$1 AND job.doc_hash=$2 AND job.base_hash='' AND job.state='running' AND job.version=$3
 AND job.attempts=$4 AND job.lease_until=$5 AND job.lease_until>clock_timestamp()
 AND source.doc_range_source_version=$6 AND incarnation.incarnation::text=$7)`,
			job.RepoID, job.DocHash, job.Version, job.Attempts, job.LeaseUntil, sourceVersion, sourceID).Scan(&live)
		if err != nil {
			return err
		}
		if !live {
			return errors.New("prepared retry changed source identity, ownership, claim version or its live lease")
		}
		return nil
	}
	if corruptSource {
		storage.sourceSteps = make(chan docRangePreparedRetryPGStepObservation, docRangeWorkPGInitialSteps(len(canonical), streamBytes)+sourceQuanta+3)
	}
	fixture.service.blobs = storage
	page, err := fixture.service.ReadDocViewEvents(ctx, fixture.repo, document.Hash, "", 1, 2, "chat")
	if !errors.Is(err, domain.ErrDocRangeIndexPending) || len(page.Events) != 0 {
		test.Fatal("cold monolithic demand did not queue preparation", page, err)
	}
	queued := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "waiting")
	worker, cancelWorker := context.WithCancel(ctx)
	workerStopped := make(chan struct{})
	resumeWorker := sync.OnceFunc(func() { close(storage.retrySeen) })
	stopWorker := func() {
		resumeWorker()
		cancelWorker()
		select {
		case <-workerStopped:
		case <-time.After(5 * time.Second):
			test.Fatal("document range worker did not stop within its cleanup deadline")
		}
	}
	test.Cleanup(func() {
		stopWorker()
		assertResourcesReleased(test, gate)
		if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
			test.Error("automatic worker retry leaked private spools", files, err)
		}
	})
	go func() {
		defer close(workerStopped)
		fixture.service.RunDocRangeWorker(worker)
	}()
	var scheduled docRangePreparedRetryPGObservation
	select {
	case scheduled = <-storage.finished:
	case <-workerStopped:
		test.Fatal("document range worker stopped before recording its prepared-artifact failure")
	case <-ctx.Done():
		test.Fatal("prepared-artifact failure was not durably recorded", ctx.Err())
	}
	var privateStage string
	initialSteps := docRangeWorkPGInitialSteps(len(canonical), streamBytes)
	decodedSteps := (len(canonical)+(512<<10)-1)/(512<<10) + len(canonical)/(512<<10) + 1
	wantProofSteps := int64(initialSteps - decodedSteps)
	failedSteps, wantDirectorySteps := initialSteps+sourceQuanta+3, int64(6)
	if corruptSource {
		failedSteps, wantDirectorySteps = initialSteps+3, 3
		corruptionStep := initialSteps + 2
		for step := 1; step <= failedSteps; step++ {
			select {
			case observation := <-storage.sourceSteps:
				if observation.step != step || observation.complete || observation.corrupted != (step == corruptionStep) ||
					step < failedSteps && observation.err != nil || step == failedSteps && (!errors.Is(observation.err, domain.ErrPreparedDocRangeObject) || !errors.Is(observation.err, domain.ErrIntegrity)) {
					test.Fatal("source spool corruption did not follow canonical handoff and fail the third metadata source quantum", observation)
				}
				if step == corruptionStep {
					privateStage = observation.privateStage
				}
			default:
				test.Fatal("worker did not observe every canonical proof quantum and the three metadata source quanta", step)
			}
		}
	}
	retainedStage, err := storage.sourceStage(ctx)
	if err != nil || retainedStage == "" || corruptSource && retainedStage != privateStage {
		test.Fatal("natural prepared failure or handle close changed the complete durable stream/header stage", retainedStage, privateStage, err)
	}
	privateStage = retainedStage
	if storage.steps.Load() != int64(failedSteps) || storage.proofSteps.Load() != wantProofSteps || storage.directorySteps.Load() != wantDirectorySteps-3 {
		test.Fatal("first failed attempt skipped canonical source proof or failed in the wrong phase", storage.steps.Load(), storage.proofSteps.Load(), storage.directorySteps.Load(), failedSteps)
	}
	retrying := assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "retrying")
	if retrying.receipt.Reason != "prepared_artifact_corrupt" || retrying.receipt.Attempts != 1 ||
		retrying.receipt.Version != queued.receipt.Version+1 || !retrying.receipt.LeaseUntil.IsZero() ||
		!retrying.receipt.NextAttempt.Equal(scheduled.at.Add(2*time.Second).Truncate(time.Microsecond)) ||
		!retrying.receipt.NextAttempt.Equal(scheduled.job.NextAttempt.Truncate(time.Microsecond)) {
		test.Fatal("prepared corruption did not retain the existing receipt with natural retry backoff", retrying, scheduled)
	}
	if storage.prepares.Load() != 1 || storage.publishCalls.Load() != failedPublishCalls || storage.publications.Load() != 0 || storage.closes.Load() != 1 {
		test.Fatal("failed publication did not discard exactly its first prepared handle",
			storage.prepares.Load(), storage.publishCalls.Load(), storage.publications.Load(), storage.closes.Load())
	}
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("retrying work retained private spools from the failed publication", files, err)
	}
	if ready, err := fixture.storage.HasDocByteRangeIndex(ctx, fixture.repo, document.Hash); err != nil || ready {
		test.Fatal("failed prepared publication advertised byte readiness", ready, err)
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
		test.Fatal("prepared-artifact failure rewrote original source bytes or ownership")
	}
	resumeWorker()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	completed := retrying
	for completed.receipt.State != "completed" || storage.publications.Load() == 0 || storage.closes.Load() < storage.prepares.Load() {
		select {
		case <-ticker.C:
			completed = readDocByteUpgradePGJob(test, fixture, document.Hash)
			if completed.receipt.State == "rejected" {
				test.Fatal("automatic retry rejected the unchanged original", completed)
			}
		case <-workerStopped:
			test.Fatal("document range worker stopped before automatic completion", completed)
		case <-ctx.Done():
			test.Fatal("natural worker retry did not complete before its deadline", completed, ctx.Err())
		}
	}
	stopWorker()
	completed = assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	if completed.receipt.Attempts != 2 || completed.receipt.Version != queued.receipt.Version+2 ||
		completed.receipt.Reason != "" || !completed.receipt.LeaseUntil.IsZero() ||
		!completed.receipt.NextAttempt.Equal(retrying.receipt.NextAttempt) {
		test.Fatal("automatic retry changed its receipt identity or failed to clear the failure", completed)
	}
	if storage.prepares.Load() != 2 || storage.publishCalls.Load() != failedPublishCalls+1 || storage.publications.Load() != 1 ||
		storage.closes.Load() != storage.prepares.Load() || storage.finishes.Load() != 1 {
		test.Fatal("worker did not freshly prepare, publish once and close every handle",
			storage.prepares.Load(), storage.publishCalls.Load(), storage.publications.Load(), storage.closes.Load(), storage.finishes.Load())
	}
	if steps, proofSteps, directorySteps := storage.steps.Load(), storage.proofSteps.Load(), storage.directorySteps.Load(); steps != int64(failedSteps+sourceQuanta+3)+storage.recoveryHandoff.Load() ||
		proofSteps != wantProofSteps || storage.recoveryHandoff.Load() <= 0 || storage.handoffs.Load() != 1 || directorySteps != wantDirectorySteps {
		test.Fatal("automatic retry did not preserve exact raw proof, observed recovery handoff and metadata/directory quanta",
			steps, proofSteps, directorySteps, storage.recoveryHandoff.Load(), storage.handoffs.Load(), failedSteps, sourceQuanta)
	}
	for attempt := range 2 {
		select {
		case preparation := <-storage.prepared:
			resources := preparation.resources
			if preparation.job.Attempts != attempt+1 {
				test.Fatal("real preparation changed the existing worker attempt", preparation)
			}
			if attempt == 0 {
				if err := docRangeWorkPGResourcePhase(streamBytes, len(canonical), sourceQuanta, 0, resources); err != nil || preparation.kind != "*store.preparedLegacyCaptureWork" {
					test.Fatal("first preparation did not retain the exact bounded raw capture", preparation, err)
				}
			} else if preparation.kind != "*store.preparedLegacyRecoveryWork" || resources.SpoolBytes != 0 || resources.HeapBytes <= 0 || resources.HeapBytes > 128<<10 || resources.KeepPermit {
				test.Fatal("ready retry preparation did not retain an empty bounded canonical recovery", preparation)
			}
			if attempt == 1 && (!preparation.job.NextAttempt.Equal(retrying.receipt.NextAttempt) || preparation.at.Before(retrying.receipt.NextAttempt)) {
				test.Fatal("worker prepared its retry before the durable backoff became eligible", preparation, retrying)
			}
			if attempt == 1 && preparation.privateStage != privateStage {
				test.Fatal("fresh preparation changed the complete durable private stage", preparation.privateStage, privateStage)
			}
		default:
			test.Fatal("worker did not observe both real preparations")
		}
	}
	if after, err := storage.sourceStage(ctx); err != nil || after != privateStage {
		test.Fatal("automatic retry rewrote the retained complete durable stream/header stage", after, privateStage, err)
	}
	assertResourcesReleased(test, gate)
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if generation.Version != 5 || generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.Hash != document.Hash ||
		generation.Identity() != domain.DocumentIdentityLegacy || generation.EventCount != int64(len(document.CIR.Events)) || generation.ChunkCount() != wantChunks {
		test.Fatal("automatic retry did not publish the original legacy source as a bounded v5 generation", generation)
	}
	var original domain.DocumentRef
	if err := fixture.pool.QueryRow(ctx, `SELECT doc_hash,doc_identity FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, document.Hash).Scan(&original.Hash, &original.Identity); err != nil ||
		original != (domain.DocumentRef{Hash: document.Hash, Identity: domain.DocumentIdentityLegacy}) {
		test.Fatal("automatic retry changed the original snapshot identity", original, err)
	}
	observed := observeLegacyDerivedPublicationPG(test, fixture, document.Hash)
	if corruptSource {
		assertLegacyDerivedPublicationPGPublicReadsAt(test, fixture, document, observed, wantChunks-1)
	} else {
		assertLegacyDerivedPublicationPGPublicReads(test, fixture, document, observed)
	}
	if after := legacyByteRangePGSourceImage(test, fixture); after != sourceImage {
		test.Fatal("automatic retry or public reads rewrote original source bytes, chunks or ownership")
	}
}
