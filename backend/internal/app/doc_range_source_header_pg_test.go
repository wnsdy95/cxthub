//go:build postgres

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeSourceHeaderPGInspection struct {
	handle         uintptr
	canonical      uintptr
	envelope       uintptr
	native         uintptr
	encoding       uintptr
	scalar         uintptr
	source         docRangeNativePagesPGSourceInspection
	phase          uint64
	nativePosition int64
	scalarPosition int64
	scalarEnd      int64
	scalarCopied   int64
	pending        int
	decodedCwd     int
	encodedCwd     int
	headerPhase    uint64
	headerBytes    int
	headerCopied   int64
	headerEnd      int64
	status         uint64
}

func docRangeSourceHeaderPGInspect(handle outbound.PreparedDocRangeWork) (docRangeSourceHeaderPGInspection, error) {
	capture := reflect.ValueOf(handle).Elem()
	state := docRangeSourceHeaderPGInspection{handle: reflect.ValueOf(handle).Pointer()}
	if !capture.FieldByName("normalization").IsNil() || capture.FieldByName("original").Bool() {
		return state, errors.New("canonical header candidate entered normalization or whole-document fallback")
	}
	canonical := capture.FieldByName("canonical")
	if canonical.IsNil() {
		return state, nil
	}
	preparation := canonical.Elem()
	envelope := preparation.FieldByName("envelope")
	if !envelope.IsValid() || !preparation.FieldByName("headerCopied").IsValid() {
		return state, errors.New("actual canonical PG preparation lacks the strict resumable header helper")
	}
	state.canonical = canonical.Pointer()
	state.headerPhase = preparation.FieldByName("phase").Uint()
	state.headerBytes = preparation.FieldByName("header").Len()
	state.headerCopied = preparation.FieldByName("headerCopied").Int()
	state.headerEnd = preparation.FieldByName("frame").FieldByName("envelopeEnd").Int()
	state.status = preparation.FieldByName("status").Uint()
	source := preparation.FieldByName("source")
	if source.FieldByName("Bytes").Int() == 0 && state.status == uint64(domain.LegacyByteSourceVerified) && envelope.IsNil() {
		return state, nil
	}
	var err error
	state.source, err = docRangeNativePagesPGSourceInspect(source, domain.ConversationManifestChunkBytes)
	if err != nil {
		return state, err
	}
	if envelope.IsNil() {
		return state, nil
	}
	if state.headerBytes != 0 || !capture.FieldByName("work").IsNil() || state.headerPhase != 1 {
		return state, errors.New("strict canonical header helper overlapped raw seed retention or published metadata work")
	}
	state.envelope = envelope.Pointer()
	helper := envelope.Elem()
	state.phase = helper.FieldByName("phase").Uint()
	state.decodedCwd = helper.FieldByName("decoded").FieldByName("Cwd").Len()
	native, encoding := helper.FieldByName("native"), helper.FieldByName("encoding")
	if !native.IsNil() && !encoding.IsNil() {
		return state, errors.New("canonical header retained native and encoder owners simultaneously")
	}
	if !native.IsNil() {
		state.native = native.Pointer()
		state.nativePosition = native.Elem().FieldByName("position").Int()
		scalar := native.Elem().FieldByName("scalar")
		if !scalar.IsNil() {
			state.scalar = scalar.Pointer()
			state.scalarPosition = scalar.Elem().FieldByName("position").Int()
			state.scalarEnd = scalar.Elem().FieldByName("end").Int()
			state.scalarCopied = scalar.Elem().FieldByName("copied").Int()
		}
	}
	if !encoding.IsNil() {
		state.encoding = encoding.Pointer()
		state.encodedCwd = encoding.Elem().FieldByName("envelope").FieldByName("Cwd").Len()
		state.pending = encoding.Elem().FieldByName("pending").Len()
		if state.decodedCwd != 0 || state.pending > domain.LegacyCanonicalEncodingBytes {
			return state, errors.New("canonical header encoder duplicated decoded ownership or exceeded its 64 KiB pending output")
		}
		scalar := encoding.Elem().FieldByName("scalar")
		if !scalar.IsNil() {
			frames := scalar.Elem().FieldByName("frames")
			if count := frames.FieldByName("count").Int(); count > 0 {
				frame, err := docRangeNativePagesPGFrame(frames, count-1)
				if err != nil {
					return state, err
				}
				if frame.FieldByName("kind").Uint() == 's' {
					state.scalar = scalar.Pointer()
					state.scalarPosition = frame.FieldByName("position").Int()
					state.scalarEnd = int64(frame.FieldByName("text").Len())
				}
			}
		}
	}
	return state, nil
}

type docRangeSourceHeaderPGContext struct {
	context.Context
	observe bool
	entered atomic.Bool
}

func (ctx *docRangeSourceHeaderPGContext) Err() error {
	if ctx.observe && !ctx.entered.Load() {
		var callers [20]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, "domain.(*legacySourceEnvelope).step") {
				ctx.entered.Store(true)
				break
			}
			if !more {
				break
			}
		}
	}
	return ctx.Context.Err()
}

type docRangeSourceHeaderPGStore struct {
	*docRangeWorkPGProbe
	work *docRangeSourceHeaderPGWork
}

type docRangeSourceHeaderPGWork struct {
	outbound.PreparedDocRangeWork
	handle          outbound.PreparedDocRangeWork
	inspection      *docRangeSourceHeaderPGContext
	before          docRangeSourceHeaderPGInspection
	after           docRangeSourceHeaderPGInspection
	beforeResources outbound.RootPublicationResources
	afterResources  outbound.RootPublicationResources
	failure         error
}

func (storage *docRangeSourceHeaderPGStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	prepared, err := storage.docRangeWorkPGProbe.PrepareDocRangeWork(ctx, job)
	if prepared == nil || err != nil {
		return prepared, err
	}
	work, supported := prepared.(*docRangeWorkPGPrepared)
	if !supported {
		return prepared, errors.New("canonical header inspection lost the existing actual PG preparation probe")
	}
	storage.work = &docRangeSourceHeaderPGWork{PreparedDocRangeWork: prepared, handle: work.PreparedDocRangeWork}
	return storage.work, nil
}

func (work *docRangeSourceHeaderPGWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	var err error
	work.before, err = docRangeSourceHeaderPGInspect(work.handle)
	if err != nil {
		return 0, err
	}
	work.beforeResources, err = work.handle.RetainedResources()
	if err != nil {
		return 0, err
	}
	work.inspection = &docRangeSourceHeaderPGContext{Context: ctx, observe: work.before.envelope != 0}
	progress, stepErr := work.PreparedDocRangeWork.Step(work.inspection, job)
	work.after, err = docRangeSourceHeaderPGInspect(work.handle)
	if err != nil {
		work.failure = errors.Join(stepErr, err)
		return 0, work.failure
	}
	work.afterResources, err = work.handle.RetainedResources()
	work.failure = errors.Join(stepErr, err)
	return progress, work.failure
}

func TestPGDocRangeSourceHeaderCandidateParksAndRechecksFences(test *testing.T) {
	for _, phase := range []string{"native", "encoding"} {
		for _, change := range []string{"unchanged", "lease expiry", "source version"} {
			test.Run(phase+"/"+change, func(test *testing.T) {
				docRangeSourceHeaderPGParksAndRechecksFences(test, phase, change)
			})
		}
	}
}

func docRangeSourceHeaderPGParksAndRechecksFences(test *testing.T, phase, change string) {
	test.Helper()
	started := time.Now()
	fixture := newDocViewPGFixture(test)
	gate := &resourceAdmission{wait: resourceWaitLimit}
	ctx, cancel := context.WithDeadline(context.WithValue(fixture.ctx, resourceAdmissionKey{}, gate), started.Add(30*time.Second))
	defer cancel()
	fixture.ctx = ctx
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := fixture.storage.EnsureDocRangeIndex(ctx, fixture.repo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	const cwdBytes = 192 << 10
	fixture.cir.Envelope.Cwd = strings.Repeat("x", cwdBytes)
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventToolResult, Output: strings.Repeat("y", 1<<20)},
		historyMessage(domain.RoleUser, "strict source header "+string(fixture.repo)),
	})
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	var expected struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &expected); err != nil || len(expected.Envelope) <= cwdBytes || len(expected.Envelope) > domain.MaxConversationManifestBytes ||
		len(canonical) <= 2*domain.ConversationManifestChunkBytes || domain.HashContent(canonical) != document.Hash {
		test.Fatal("large canonical header lost its exact source oracle or original 512 KiB geometry", err)
	}
	eventBodies := make([][]byte, len(expected.Events))
	for position, event := range expected.Events {
		if len(event) < 4 || position == 0 && len(event) <= 1<<20 || position != 0 && len(event) > 64<<10 {
			test.Fatal("canonical header fixture lost its large source event or small boundary event", position, len(event))
		}
		eventBodies[position] = event
	}
	var manifest strings.Builder
	for offset := 0; offset < len(canonical); offset += domain.ConversationManifestChunkBytes {
		manifest.WriteString(string(domain.HashContent(canonical[offset:min(offset+domain.ConversationManifestChunkBytes, len(canonical))])))
	}
	wantManifest := domain.HashContent([]byte(manifest.String()))
	if _, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, canonical); err != nil {
		test.Fatal(err)
	}
	var sourceVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, document.Hash).Scan(&sourceVersion); err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocByteRangeJob(ctx, fixture.repo, document.Hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	probe := &docRangeWorkPGProbe{PostgresStore: fixture.storage}
	observed := &docRangeSourceHeaderPGStore{docRangeWorkPGProbe: probe}
	fixture.service.blobs = observed
	foreground := make(chan error, 1)
	var foregroundCompleted, parkedObserved atomic.Bool
	var parked docRangeSourceHeaderPGInspection
	var parkedResources outbound.RootPublicationResources
	var parkedPermit *resourcePermit
	var resumeBefore int64
	parkStep, resumedStep, verifiedStep := 0, 0, 0
	handoffObserved, headerCopyObserved := false, false
	probe.afterPrepare = func(bound context.Context, resources outbound.RootPublicationResources) error {
		if resources.HeapBytes <= 0 || resources.HeapBytes > 128<<10 || resources.SpoolBytes != 0 || resources.KeepPermit {
			return fmt.Errorf("canonical header setup exceeded its unchanged 128 KiB bound: %+v", resources)
		}
		return docRangeWorkPGNoByteStage(bound, fixture, document.Hash)
	}
	probe.afterStep = func(bound context.Context, step int, complete bool) error {
		work := observed.work
		state, err := docRangeSourceHeaderPGInspect(work.handle)
		if err != nil {
			return err
		}
		before := work.before
		if state.canonical != 0 {
			if before.canonical != 0 && state.canonical != before.canonical ||
				state.source.manifest != 0 && (state.source.bytes != int64(len(canonical)) || state.source.hash != wantManifest ||
					before.source.manifest != 0 && state.source != before.source) {
				return errors.New("canonical header changed its original authenticated 512 KiB source or preparation handle")
			}
			if err := docRangeWorkPGNoByteStage(bound, fixture, document.Hash); err != nil {
				return err
			}
		}
		if before.envelope != 0 && state.envelope != 0 && before.envelope != state.envelope {
			return errors.New("strict canonical header restarted its helper between quanta")
		}
		if before.native != 0 && state.encoding != 0 {
			if state.envelope != before.envelope || state.native != 0 || before.decodedCwd != cwdBytes ||
				state.decodedCwd != 0 || state.encodedCwd != cwdBytes || probe.resources[step-1].HeapBytes < cwdBytes {
				return errors.New("canonical header native-to-encoder handoff lost its actual value owner or retained accounting")
			}
			handoffObserved = true
		}
		if state.headerPhase == 5 {
			if state.envelope != 0 || state.headerCopied < 0 || state.headerCopied > int64(state.headerBytes) ||
				state.headerBytes != 0 && state.headerBytes != len(expected.Envelope) || state.headerEnd != int64(len(`{"envelope":`)+len(expected.Envelope)) {
				return errors.New("final seed header overlapped strict helper ownership or changed its exact span")
			}
			if before.headerPhase == 5 && before.headerBytes > 0 {
				if state.headerCopied-before.headerCopied != min(int64(domain.LegacyCanonicalEncodingBytes), int64(state.headerBytes)-before.headerCopied) {
					return errors.New("final seed header was not copied in unchanged 64 KiB quanta")
				}
				headerCopyObserved = true
			}
		}
		if state.status == uint64(domain.LegacyByteSourceVerified) {
			capture := reflect.ValueOf(work.handle).Elem()
			proof := capture.FieldByName("canonicalProof")
			seed := proof.FieldByName("seed")
			header := capture.FieldByName("canonical").Elem().FieldByName("header").Bytes()
			if change != "unchanged" || resumedStep != parkStep+1 || !handoffObserved || !headerCopyObserved || complete ||
				state.envelope != 0 || state.headerCopied != int64(len(expected.Envelope)) ||
				!proof.FieldByName("plan").FieldByName("sealed").Bool() || !seed.FieldByName("sealed").Bool() ||
				!bytes.Equal(header, expected.Envelope) || !bytes.Equal(seed.FieldByName("envelope").Bytes(), expected.Envelope) {
				return errors.New("same canonical header handle did not seal the exact original final metadata seed")
			}
			assembled := domain.AssembleCanonicalDoc(seed.FieldByName("envelope").Bytes(), [][]byte{bytes.Join(eventBodies, []byte(","))})
			if !bytes.Equal(assembled, canonical) || domain.HashContent(assembled) != document.Hash {
				return errors.New("resumed strict header final seed changed exact canonical bytes or SHA-256")
			}
			verifiedStep = step
		}
		permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
		if parkStep != 0 {
			if change != "unchanged" {
				return errors.New("fenced canonical header completed another preparation quantum")
			}
			if resumedStep == 0 {
				if step != parkStep+1 || before != parked || !work.inspection.entered.Load() || complete ||
					state.handle != parked.handle || state.canonical != parked.canonical || state.envelope != parked.envelope ||
					state.source != parked.source || permit != parkedPermit || !permit.live.Load() || permit.resumeWait.Load() <= resumeBefore ||
					!foregroundCompleted.Load() || !parkedObserved.Load() {
					return errors.New("same canonical header handle did not resume its owned permit after foreground progress")
				}
				if phase == "native" {
					if state.native != parked.native || state.encoding != 0 ||
						state.scalar != parked.scalar && (state.scalar != 0 || state.decodedCwd != cwdBytes) ||
						state.scalar == parked.scalar && state.nativePosition <= parked.nativePosition &&
							state.scalarPosition <= parked.scalarPosition && state.scalarCopied <= parked.scalarCopied {
						return errors.New("same strict native header decoder did not continue its retained scalar")
					}
				} else if state.encoding != parked.encoding || state.native != 0 || state.scalar != parked.scalar ||
					state.scalarPosition <= parked.scalarPosition || state.encodedCwd != cwdBytes {
					return errors.New("same strict header encoder did not continue its owned large scalar")
				}
				resumedStep = step
			}
			return nil
		}
		if state.envelope == 0 || !work.inspection.entered.Load() || state.scalar == 0 || state.scalarPosition <= 0 ||
			state.scalarEnd <= state.scalarPosition || phase == "native" && state.native == 0 ||
			phase == "encoding" && (state.encoding == 0 || state.pending != 0 || state.scalarEnd != cwdBytes ||
				state.scalarEnd-state.scalarPosition <= domain.LegacyCanonicalEncodingBytes) {
			return nil
		}
		resources := probe.resources[step-1]
		if complete || permit == nil || !permit.live.Load() || resources.KeepPermit || resources.HeapBytes <= 0 ||
			resources.HeapBytes > maxParkedRootHeapBytes || resources.SpoolBytes != int64(len(canonical)) || resources.SpoolBytes > maxParkedRootSpoolBytes {
			return fmt.Errorf("active canonical header exceeded unchanged 16 MiB/64 MiB parking limits: %+v", resources)
		}
		var generations int
		if err := fixture.pool.QueryRow(bound, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, document.Hash).Scan(&generations); err != nil || generations != 0 {
			return fmt.Errorf("active canonical header exposed a generation: count=%d error=%v", generations, err)
		}
		parkStep, parked, parkedResources = step, state, resources
		parkedPermit, resumeBefore = permit, permit.resumeWait.Load()
		test.Logf("active canonical header phase=%s heap=%d spool=%d part_bytes=%d", phase, resources.HeapBytes, resources.SpoolBytes, domain.ConversationManifestChunkBytes)
		go func() {
			foreground <- resourceWork(ctx, ResourceBulk, func(read context.Context) error {
				gate.mu.Lock()
				parkedNow := gate.retained == permit && permit.parked && permit.live.Load() && gate.bulk == 1
				gate.mu.Unlock()
				if !parkedNow || permit.resumeWait.Load() != resumeBefore {
					return errors.New("foreground was not admitted while the actual canonical header permit was parked")
				}
				current, err := docRangeSourceHeaderPGInspect(work.handle)
				if err != nil || current != parked {
					return fmt.Errorf("parked canonical header changed before foreground read: state=%+v error=%v", current, err)
				}
				page, err := fixture.service.ReadDocEvents(read, fixture.repo, fixture.hash, "", 0, 20)
				if err != nil {
					return err
				}
				if len(page.Events) != 20 || page.Events[0].Seq != 0 || page.Next != 20 || page.Hash != fixture.hash ||
					!reflect.DeepEqual(page.Events, fixture.cir.Events[:20]) {
					return errors.New("foreground lost its actual pinned page during canonical header preparation")
				}
				switch change {
				case "lease expiry":
					tag, err := fixture.pool.Exec(read, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3`, fixture.repo, document.Hash, probe.job.Version)
					if err != nil || tag.RowsAffected() != 1 {
						return fmt.Errorf("could not expire parked canonical header lease: rows=%d error=%v", tag.RowsAffected(), err)
					}
				case "source version":
					tag, err := fixture.pool.Exec(read, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, document.Hash)
					if err != nil || tag.RowsAffected() != 1 {
						return fmt.Errorf("could not replace parked canonical header source: rows=%d error=%v", tag.RowsAffected(), err)
					}
					var currentVersion int64
					if err := fixture.pool.QueryRow(read, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, document.Hash).Scan(&currentVersion); err != nil || currentVersion <= sourceVersion {
						return fmt.Errorf("canonical header source version did not advance: before=%d after=%d error=%v", sourceVersion, currentVersion, err)
					}
				}
				current, err = docRangeSourceHeaderPGInspect(work.handle)
				if err != nil || current != parked {
					return fmt.Errorf("parked canonical header advanced during foreground work: state=%+v error=%v", current, err)
				}
				parkedObserved.Store(true)
				foregroundCompleted.Store(true)
				return nil
			})
		}()
		waitResourceQueue(test, gate, ResourceBulk, 1)
		return nil
	}
	err = fixture.service.ProcessDocRangeJobs(ctx, 1)
	if parkStep == 0 {
		test.Fatal("actual canonical candidate header never reached the requested active parking phase", phase, err)
	}
	select {
	case foregroundErr := <-foreground:
		if foregroundErr != nil {
			test.Fatal("foreground failed while the canonical header was parked", foregroundErr)
		}
	case <-ctx.Done():
		test.Fatal("foreground did not complete during canonical header preparation", ctx.Err())
	}
	assertResourcesReleased(test, gate)
	if !parkedObserved.Load() || !foregroundCompleted.Load() || parkedPermit.resumeWait.Load() <= resumeBefore ||
		len(probe.initial) != 1 || probe.closed != 1 {
		test.Fatal("canonical header parking did not retain and close exactly one real prepared handle", probe)
	}
	var stored []byte
	var currentVersion int64
	if sourceErr := fixture.pool.QueryRow(ctx, `SELECT bytes,doc_range_source_version FROM blobs WHERE hash=$1`, document.Hash).Scan(&stored, &currentVersion); sourceErr != nil ||
		!bytes.Equal(stored, canonical) || change != "source version" && currentVersion != sourceVersion || change == "source version" && currentVersion <= sourceVersion {
		test.Fatal("canonical header work changed original source bytes or lost the current source version", sourceVersion, currentVersion, sourceErr)
	}
	if change != "unchanged" {
		work := observed.work
		if !errors.Is(err, domain.ErrConflict) || !errors.Is(work.failure, domain.ErrConflict) || work.inspection.entered.Load() ||
			work.before != parked || work.after != parked || work.beforeResources != parkedResources || work.afterResources != parkedResources ||
			probe.steps != parkStep || probe.published != 0 {
			test.Fatal("lease or source fence reentered the strict header helper or published stale work", err, work)
		}
		if err := docRangeWorkPGNoByteStage(ctx, fixture, document.Hash); err != nil {
			test.Fatal(err)
		}
		var generations int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, document.Hash).Scan(&generations); err != nil || generations != 0 {
			test.Fatal("fenced canonical header published a generation", generations, err)
		}
		return
	}
	if err != nil || resumedStep != parkStep+1 || !handoffObserved || !headerCopyObserved ||
		verifiedStep <= resumedStep || probe.steps <= verifiedStep || probe.published != 1 {
		test.Fatal("same canonical header handle did not resume, transfer ownership, seal the exact seed, and publish", err, probe, handoffObserved, headerCopyObserved, verifiedStep)
	}
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if generation.Version != 5 || generation.Hash != document.Hash || generation.EventCount != int64(len(expected.Events)) || !bytes.Equal(generation.Envelope, expected.Envelope) {
		test.Fatal("canonical header publication changed the exact final envelope seed", generation.Version, generation.Hash, generation.EventCount)
	}
	reader := &docRangeNativePagesFragmentStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
	fixture.service.blobs = reader
	actual := make([][]byte, len(expected.Events))
	for position, event := range expected.Events {
		budget := min(len(event), domain.LegacyCanonicalEncodingBytes)
		for offset := 0; offset < len(event); {
			end := min(offset+budget, len(event))
			next := end
			if next == len(event) {
				next = -1
			}
			reader.cost = legacyFindGenerationCosts{}
			fragment, err := fixture.service.ReadDocViewFragment(outbound.WithDocReadOnly(ctx), fixture.repo, document.Hash, position, offset, budget)
			if err != nil || fragment.JSON != string(event[offset:end]) || fragment.Index != position || fragment.Offset != offset || fragment.Total != len(event) || fragment.Next != next ||
				len(fragment.JSON) > domain.LegacyCanonicalEncodingBytes || reader.cost.whole != 0 || reader.cost.manifests != 0 || reader.cost.indexes != 0 ||
				reader.cost.chunks < 1 || reader.cost.chunks > 2 || reader.cost.chunkBytes > 2*domain.ChunkTarget {
				test.Fatal("resumed canonical header changed exact event bytes or used whole-source fallback", position, offset, fragment, reader.cost, err)
			}
			actual[position] = append(actual[position], []byte(fragment.JSON)...)
			offset = end
		}
		if !bytes.Equal(actual[position], event) || domain.HashContent(actual[position]) != domain.HashContent(event) {
			test.Fatal("canonical header continuation changed exact event bytes or SHA-256", position)
		}
	}
	assembled := domain.AssembleCanonicalDoc(generation.Envelope, [][]byte{bytes.Join(actual, []byte(","))})
	if !bytes.Equal(assembled, canonical) || domain.HashContent(assembled) != document.Hash {
		test.Fatal("same-handle canonical header resumption changed exact published bytes or SHA-256")
	}
}
