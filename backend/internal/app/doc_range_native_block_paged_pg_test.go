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

const docRangeNativeBlockPagedPGRecords = 8192
const docRangeNativeBlockPagedPGVisible = 4097

type docRangeNativeBlockPagedPGInspection struct {
	native   docRangeNativePagesPGInspection
	values   docRangeNativePagesPGValuesInspection
	list     uintptr
	position int64
	end      int64
	count    int64
	length   int64
	borrowed int64
	field    int64
	heap     int64
	first    domain.ContentBlock
	last     domain.ContentBlock
}

func docRangeNativeBlockPagedPGValuesInspect(values reflect.Value) (docRangeNativePagesPGValuesInspection, error) {
	if !values.IsValid() || values.Kind() != reflect.Pointer || values.IsNil() || values.Elem().Type().Name() != "legacyNativeBlockValues" {
		return docRangeNativePagesPGValuesInspection{}, errors.New("native blocks lost their actual paged data")
	}
	records := values.Elem().FieldByName("records")
	root, single, inline := records.FieldByName("root"), records.FieldByName("single"), records.FieldByName("inline")
	payload := values.Elem().FieldByName("payload")
	state := docRangeNativePagesPGValuesInspection{
		values: values.Pointer(), root: root.Pointer(), single: single.Pointer(), inline: inline.Pointer(),
		count: values.Elem().FieldByName("count").Int(), records: records.FieldByName("count").Int(),
		pages: records.FieldByName("pages").Int(), nodes: records.FieldByName("nodes").Int(), depth: records.FieldByName("depth").Int(),
		backing: int64(values.Elem().Type().Size()), payload: payload.FieldByName("bytes").Int(),
	}
	if state.count < 0 || state.count > state.records || state.pages < 0 || state.nodes < 0 ||
		state.depth < 0 || state.depth > (int64(reflect.TypeFor[uintptr]().Size())*8-1)/6 ||
		state.payload < 0 || !payload.FieldByName("err").IsNil() {
		return docRangeNativePagesPGValuesInspection{}, errors.New("native block data lost its logical count, high-water records, or payload ledger")
	}
	if root.IsNil() {
		if state.records > 8 || state.pages != 0 || state.nodes != 0 || state.depth != 0 || state.records != 0 && single.IsNil() && inline.IsNil() {
			return docRangeNativePagesPGValuesInspection{}, errors.New("native block data lost its fixed small-list storage")
		}
		if !single.IsNil() {
			if !inline.IsNil() || state.records > 1 || single.Elem().Type() != inline.Type().Elem().Elem() ||
				state.records == 0 && !single.Elem().IsZero() {
				return docRangeNativePagesPGValuesInspection{}, errors.New("native block data lost its exclusive single value or retained an empty value reference")
			}
			state.firstPage, state.lastPage = single.Pointer(), single.Pointer()
			state.backing += int64(single.Elem().Type().Size())
		} else if !inline.IsNil() {
			if inline.Elem().Kind() != reflect.Array || inline.Elem().Len() != 8 {
				return docRangeNativePagesPGValuesInspection{}, errors.New("native blocks retained a variable-size inline array")
			}
			state.firstPage, state.lastPage = inline.Pointer(), inline.Pointer()
			state.backing += int64(inline.Elem().Type().Size())
		}
	} else {
		children, page := root.Elem().FieldByName("children"), root.Elem().FieldByName("values")
		if !single.IsNil() || !inline.IsNil() || state.records <= 8 || state.pages != (state.records+63)/64 || state.nodes < state.pages ||
			children.Kind() != reflect.Array || children.Len() != 64 || page.Type().Elem().Kind() != reflect.Array || page.Type().Elem().Len() != 64 {
			return docRangeNativePagesPGValuesInspection{}, errors.New("native block data is not retained in fixed 64-record pages")
		}
		state.backing += state.nodes*int64(root.Elem().Type().Size()) + state.pages*int64(page.Type().Elem().Size())
	}
	for position := int64(0); position < state.records; position += 64 {
		record, err := docRangeNativePagesPGFrame(records, position)
		if err != nil {
			return docRangeNativePagesPGValuesInspection{}, err
		}
		if record.Type().Name() != "legacyNativeBlockValue" || record.FieldByName("block").Type() != reflect.TypeFor[domain.ContentBlock]() ||
			record.FieldByName("heaps").Kind() != reflect.Array || record.FieldByName("heaps").Len() != 2 ||
			record.FieldByName("excess").Kind() != reflect.Array || record.FieldByName("excess").Len() != 2 {
			return docRangeNativePagesPGValuesInspection{}, errors.New("native block page lost its actual block and fixed field ledgers")
		}
		for field := 0; field < 2; field++ {
			heap := record.FieldByName("heaps").Index(field)
			if heap.FieldByName("bytes").Int() < 0 || !heap.FieldByName("err").IsNil() || record.FieldByName("excess").Index(field).Int() < 0 {
				return docRangeNativePagesPGValuesInspection{}, errors.New("native block page retained an invalid field ledger")
			}
		}
		if position == 0 {
			state.firstPage = record.Addr().Pointer()
		}
		state.lastPage = record.Addr().Pointer()
	}
	if state.pages > 1 && state.firstPage == state.lastPage {
		return docRangeNativePagesPGValuesInspection{}, errors.New("native block pages alias the same backing array")
	}
	return state, nil
}

func docRangeNativeBlockPagedPGEdges(values reflect.Value, state *docRangeNativeBlockPagedPGInspection) error {
	if state.values.records == 0 {
		return nil
	}
	for _, edge := range []struct {
		position int64
		block    *domain.ContentBlock
	}{{0, &state.first}, {state.values.records - 1, &state.last}} {
		record, err := docRangeNativePagesPGFrame(values.Elem().FieldByName("records"), edge.position)
		if err != nil {
			return err
		}
		block := record.FieldByName("block")
		*edge.block = domain.ContentBlock{Type: block.FieldByName("Type").String(), Text: block.FieldByName("Text").String()}
	}
	return nil
}

func docRangeNativeBlockPagedPGInspect(handle outbound.PreparedDocRangeWork) (docRangeNativeBlockPagedPGInspection, error) {
	capture := reflect.ValueOf(handle).Elem()
	normalization := capture.FieldByName("normalization")
	if normalization.IsNil() {
		proof, err := docRangeNativePagesPGInspect(handle, true)
		return docRangeNativeBlockPagedPGInspection{native: proof}, err
	}
	preparation := normalization.Elem().FieldByName("preparation")
	if preparation.IsNil() || !normalization.Elem().FieldByName("canonical").IsNil() {
		return docRangeNativeBlockPagedPGInspection{}, nil
	}
	decoder := preparation.Elem().FieldByName("decoder")
	if !decoder.IsNil() {
		native := decoder.Elem().FieldByName("native")
		if !native.IsNil() {
			object := native.Elem().FieldByName("object")
			if !object.IsNil() && object.Elem().FieldByName("blockActive").Bool() {
				field := object.Elem().FieldByName("field").Int()
				blocks := object.Elem().FieldByName("blocks")
				if field < 0 || field >= int64(blocks.Len()) || blocks.Index(int(field)).IsNil() {
					return docRangeNativeBlockPagedPGInspection{}, errors.New("active native block decoder lost its actual list")
				}
				list := blocks.Index(int(field))
				data := list.Elem().FieldByName("data")
				paged, borrowed := list.Elem().FieldByName("paged"), list.Elem().FieldByName("borrowed")
				if !paged.IsValid() || !borrowed.IsValid() || !object.Elem().FieldByName("pagedEvents").Bool() || !paged.Bool() {
					return docRangeNativeBlockPagedPGInspection{}, errors.New("actual public block decoder did not select paged data")
				}
				if data.IsValid() && data.Kind() == reflect.Pointer && data.IsNil() {
					return docRangeNativeBlockPagedPGInspection{}, nil
				}
				values, err := docRangeNativeBlockPagedPGValuesInspect(data)
				if err != nil {
					return docRangeNativeBlockPagedPGInspection{}, err
				}
				for _, name := range []string{"decoded", "previous", "payloads", "previousHeaps", "excess", "previousExtra"} {
					if !list.Elem().FieldByName(name).IsNil() {
						return docRangeNativeBlockPagedPGInspection{}, fmt.Errorf("actual paged block decoder retained contiguous %s", name)
					}
				}
				fieldHeap, estimate := object.Elem().FieldByName("heaps").Index(int(field)), list.Elem().FieldByName("estimate")
				if !borrowed.FieldByName("err").IsNil() || !fieldHeap.FieldByName("err").IsNil() || !estimate.FieldByName("err").IsNil() ||
					borrowed.FieldByName("bytes").Int() < 0 || borrowed.FieldByName("bytes").Int() != fieldHeap.FieldByName("bytes").Int() ||
					estimate.FieldByName("bytes").Int() < values.backing+values.payload+int64(list.Elem().FieldByName("decoded").Type().Size()) ||
					!native.Elem().FieldByName("decoded").FieldByName("Blocks").IsNil() || native.Elem().FieldByName("canonicalReady").Bool() ||
					preparation.Elem().FieldByName("pending").Len() != 0 || preparation.Elem().FieldByName("next").Int() != 0 ||
					preparation.Elem().FieldByName("written").Int() != 0 || !capture.FieldByName("work").IsNil() || capture.FieldByName("original").Bool() {
					return docRangeNativeBlockPagedPGInspection{}, errors.New("active native block decoding lost its borrowed field ledger or passed native sealing")
				}
				source, err := docRangeNativePagesPGSourceInspect(decoder.Elem().FieldByName("source"), domain.LegacyNormalizationPartBytes)
				if err != nil {
					return docRangeNativeBlockPagedPGInspection{}, err
				}
				state := docRangeNativeBlockPagedPGInspection{
					native: docRangeNativePagesPGInspection{
						handle: reflect.ValueOf(handle).Pointer(), preparation: preparation.Pointer(), source: source,
						decoder: decoder.Pointer(), native: native.Pointer(), object: object.Pointer(),
					},
					values: values, list: list.Pointer(), position: list.Elem().FieldByName("position").Int(), end: list.Elem().FieldByName("end").Int(),
					count: list.Elem().FieldByName("count").Int(), borrowed: borrowed.FieldByName("bytes").Int(),
					field: fieldHeap.FieldByName("bytes").Int(), heap: estimate.FieldByName("bytes").Int(),
				}
				if state.position < 0 || state.position > state.end || state.end > source.bytes || state.count < 0 || state.count > state.values.records || state.values.count != state.count {
					return docRangeNativeBlockPagedPGInspection{}, errors.New("actual block decoder left its original source or high-water bounds")
				}
				err = docRangeNativeBlockPagedPGEdges(data, &state)
				return state, err
			}
		}
	}
	base, err := docRangeNativePagesPGInspect(handle, false)
	if err != nil || base.frames == 0 {
		return docRangeNativeBlockPagedPGInspection{}, err
	}
	encoding := preparation.Elem().FieldByName("pending").Index(int(preparation.Elem().FieldByName("next").Int())).FieldByName("Encoding")
	frames := encoding.Elem().FieldByName("preparation").Elem().FieldByName("frames")
	for position := int64(0); position < base.frames; position++ {
		frame, err := docRangeNativePagesPGFrame(frames, position)
		if err != nil {
			return docRangeNativeBlockPagedPGInspection{}, err
		}
		if frame.FieldByName("kind").Uint() != 'd' {
			continue
		}
		data := frame.FieldByName("value").Elem()
		values, err := docRangeNativeBlockPagedPGValuesInspect(data)
		if err != nil {
			return docRangeNativeBlockPagedPGInspection{}, err
		}
		owner := encoding.Elem().FieldByName("nativeOwner").Elem()
		owned := owner.FieldByName("blocks")
		if !owned.IsValid() || owned.Pointer() != values.values || !owner.FieldByName("event").FieldByName("Blocks").IsNil() ||
			!owner.FieldByName("heap").FieldByName("err").IsNil() || owner.FieldByName("heap").FieldByName("bytes").Int() < values.backing+values.payload ||
			!frame.FieldByName("prepare").Bool() || frame.FieldByName("count").Int() != values.records || frame.FieldByName("length").Int() != values.count {
			return docRangeNativeBlockPagedPGInspection{}, errors.New("actual d preparation lost native block ownership, high-water accounting, or logical output length")
		}
		state := docRangeNativeBlockPagedPGInspection{
			native: base, values: values, position: frame.FieldByName("position").Int(),
			count: frame.FieldByName("count").Int(), length: frame.FieldByName("length").Int(),
			heap: owner.FieldByName("heap").FieldByName("bytes").Int(),
		}
		if state.position < 0 || state.position > state.count {
			return docRangeNativeBlockPagedPGInspection{}, errors.New("actual d preparation left its retained block records")
		}
		err = docRangeNativeBlockPagedPGEdges(data, &state)
		return state, err
	}
	return docRangeNativeBlockPagedPGInspection{}, nil
}

type docRangeNativeBlockPagedPGContext struct {
	context.Context
	entered atomic.Bool
	decode  bool
}

func (ctx *docRangeNativeBlockPagedPGContext) Err() error {
	if !ctx.entered.Load() {
		var callers [12]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			if ctx.decode && (strings.HasSuffix(frame.Function, "domain.(*legacyNativeBlockList).step") ||
				strings.HasSuffix(frame.Function, "domain.(*legacyNativeBlockList).advance")) ||
				!ctx.decode && strings.HasSuffix(frame.Function, "domain.(*legacyCanonicalPreparation).step") {
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

type docRangeNativeBlockPagedPGStore struct {
	*docRangeWorkPGProbe
	work   *docRangeNativeBlockPagedPGWork
	decode bool
}

type docRangeNativeBlockPagedPGWork struct {
	outbound.PreparedDocRangeWork
	handle          outbound.PreparedDocRangeWork
	inspection      *docRangeNativeBlockPagedPGContext
	before          docRangeNativeBlockPagedPGInspection
	after           docRangeNativeBlockPagedPGInspection
	beforeResources outbound.RootPublicationResources
	afterResources  outbound.RootPublicationResources
	failure         error
	decode          bool
}

func (storage *docRangeNativeBlockPagedPGStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	prepared, err := storage.docRangeWorkPGProbe.PrepareDocRangeWork(ctx, job)
	if prepared == nil || err != nil {
		return prepared, err
	}
	work, supported := prepared.(*docRangeWorkPGPrepared)
	if !supported {
		return prepared, errors.New("block inspection did not receive the existing actual PG work probe")
	}
	storage.work = &docRangeNativeBlockPagedPGWork{PreparedDocRangeWork: prepared, handle: work.PreparedDocRangeWork, decode: storage.decode}
	return storage.work, nil
}

func (work *docRangeNativeBlockPagedPGWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	var err error
	work.before, err = docRangeNativeBlockPagedPGInspect(work.handle)
	if err != nil {
		return 0, err
	}
	work.beforeResources, err = work.handle.RetainedResources()
	if err != nil {
		return 0, err
	}
	work.inspection = &docRangeNativeBlockPagedPGContext{Context: ctx, decode: work.decode}
	progress, stepErr := work.PreparedDocRangeWork.Step(work.inspection, job)
	work.after, err = docRangeNativeBlockPagedPGInspect(work.handle)
	if err != nil {
		return 0, errors.Join(stepErr, err)
	}
	work.afterResources, err = work.handle.RetainedResources()
	work.failure = errors.Join(stepErr, err)
	return progress, work.failure
}

func docRangeNativeBlockPagedPGText(prefix string, position int) string {
	return fmt.Sprintf("%s-%04d/%s", prefix, position, strings.Repeat("x", 80))
}

func docRangeNativeBlockPagedPGDocument(test *testing.T, fixture docViewPGFixture) (domain.SessionDoc, []byte, []byte) {
	test.Helper()
	blocks := make([]domain.ContentBlock, docRangeNativeBlockPagedPGVisible)
	for position := range blocks {
		blocks[position] = domain.ContentBlock{Type: "text", Text: docRangeNativeBlockPagedPGText("next", position)}
	}
	document := seedLegacyFindPGDocument(test, fixture, []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: blocks},
		historyMessage(domain.RoleUser, "native block page boundary "+string(fixture.repo)),
	})
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	envelope, err := json.Marshal(document.CIR.Envelope)
	if err != nil {
		test.Fatal(err)
	}
	boundary, err := json.Marshal(document.CIR.Events[1])
	if err != nil {
		test.Fatal(err)
	}
	var raw bytes.Buffer
	raw.WriteString(`{"envelope":` + string(envelope) + `,"events":[{"kind":"message","role":"assistant","blocks":[`)
	for position := 0; position < docRangeNativeBlockPagedPGRecords; position++ {
		if position != 0 {
			raw.WriteByte(',')
		}
		raw.WriteString(`{"type":"text","text":"` + docRangeNativeBlockPagedPGText("seed", position) + `"}`)
	}
	raw.WriteString(`],"blocks":[`)
	for position := 0; position < docRangeNativeBlockPagedPGVisible; position++ {
		if position != 0 {
			raw.WriteByte(',')
		}
		raw.WriteString(`{"text":"` + docRangeNativeBlockPagedPGText("next", position) + `"}`)
	}
	raw.WriteString(`]},` + string(boundary) + `]}`)
	var decoded domain.CIRDocument
	if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
		test.Fatal(err)
	}
	oracle, err := domain.CanonicalBytes(decoded)
	if err != nil || !bytes.Equal(oracle, canonical) || domain.HashContent(oracle) != document.Hash ||
		raw.Len() <= 2*domain.ConversationManifestChunkBytes || bytes.Equal(raw.Bytes(), canonical) {
		test.Fatal("duplicate native block fixture lost typed partial-merge semantics, exact SHA-256, or its original-proof source gate", err)
	}
	return document, raw.Bytes(), canonical
}

func TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFences(test *testing.T) {
	docRangeNativeBlockPagedPGScenario(test, true, "unchanged")
}

func TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFencesLeaseExpiry(test *testing.T) {
	docRangeNativeBlockPagedPGScenario(test, true, "lease expiry")
}

func TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFencesSourceVersion(test *testing.T) {
	docRangeNativeBlockPagedPGScenario(test, true, "source version")
}

func TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFencesPreparation(test *testing.T) {
	docRangeNativeBlockPagedPGScenario(test, false, "unchanged")
}

func TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFencesPreparationLeaseExpiry(test *testing.T) {
	docRangeNativeBlockPagedPGScenario(test, false, "lease expiry")
}

func TestPGDocRangeNativeBlockPagedNormalizationParksAndRechecksFencesPreparationSourceVersion(test *testing.T) {
	docRangeNativeBlockPagedPGScenario(test, false, "source version")
}

func docRangeNativeBlockPagedPGScenario(test *testing.T, decode bool, change string) {
	test.Helper()
	test.Run(fmt.Sprintf("block_decode=%t", decode), func(test *testing.T) {
		test.Run(change, func(test *testing.T) {
			docRangeNativeBlockPagedPGParksAndRechecksFences(test, decode, change)
		})
	})
}

func docRangeNativeBlockPagedPGParksAndRechecksFences(test *testing.T, decode bool, change string) {
	test.Helper()
	started := time.Now()
	fixture := newDocViewPGFixture(test)
	gate := &resourceAdmission{wait: resourceWaitLimit}
	ctx, cancel := context.WithDeadline(context.WithValue(fixture.ctx, resourceAdmissionKey{}, gate), started.Add(30*time.Second))
	defer cancel()
	fixture.ctx = ctx
	if maxParkedRootHeapBytes != 16<<20 || maxParkedRootSpoolBytes != 64<<20 {
		test.Fatal("native block fixture requires the unchanged 16 MiB heap and 64 MiB spool parking policy")
	}
	fixture.storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := fixture.storage.EnsureDocRangeIndex(ctx, fixture.repo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	document, raw, canonical := docRangeNativeBlockPagedPGDocument(test, fixture)
	sourceManifests := make(map[int]domain.ContentHash, 2)
	for _, partBytes := range []int{domain.ConversationManifestChunkBytes, domain.LegacyNormalizationPartBytes} {
		var hashes strings.Builder
		for offset := 0; offset < len(raw); offset += partBytes {
			hashes.WriteString(string(domain.HashContent(raw[offset:min(offset+partBytes, len(raw))])))
		}
		sourceManifests[partBytes] = domain.HashContent([]byte(hashes.String()))
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, raw); err != nil {
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
	observed := &docRangeNativeBlockPagedPGStore{docRangeWorkPGProbe: probe, decode: decode}
	fixture.service.blobs = observed
	foreground := make(chan error, 1)
	var foregroundCompleted, parkedObserved atomic.Bool
	var parked, decoded docRangeNativeBlockPagedPGInspection
	var parkedResources outbound.RootPublicationResources
	var parkedPermit *resourcePermit
	var resumeBefore int64
	proofSourceObserved, nativeHandoffObserved := false, false
	parkStep, resumedStep := 0, 0
	probe.afterPrepare = func(_ context.Context, resources outbound.RootPublicationResources) error {
		if resources.KeepPermit || resources.HeapBytes <= 0 || resources.HeapBytes > 128<<10 || resources.SpoolBytes != 0 {
			return fmt.Errorf("actual block capture exceeded the unchanged initial setup budget: %+v", resources)
		}
		return nil
	}
	probe.afterStep = func(bound context.Context, step int, complete bool) error {
		state, err := docRangeNativeBlockPagedPGInspect(observed.work.handle)
		if err != nil {
			return err
		}
		if state.native.source.manifest != 0 {
			partBytes := domain.ConversationManifestChunkBytes
			if state.native.decoder != 0 {
				partBytes = domain.LegacyNormalizationPartBytes
			}
			if state.native.source.bytes != int64(len(raw)) || state.native.source.hash != sourceManifests[partBytes] {
				return errors.New("actual block source changed the authenticated original bytes or part geometry")
			}
			if state.native.decoder == 0 {
				proofSourceObserved = true
			} else if observed.work.before.native.decoder != 0 && observed.work.before.native.source != state.native.source {
				return errors.New("same block decoder changed its original 64 KiB source manifest across a quantum")
			}
		}
		if state.list != 0 && state.borrowed > 0 && state.values.records == docRangeNativeBlockPagedPGRecords && state.count > 64 {
			decoded = state
		}
		if state.native.encoding != 0 && state.values.values != 0 && decoded.values.values != 0 {
			if state.values.values != decoded.values.values || state.values.firstPage != decoded.values.firstPage || state.values.lastPage != decoded.values.lastPage ||
				state.values.records != docRangeNativeBlockPagedPGRecords || state.values.count != docRangeNativeBlockPagedPGVisible ||
				state.last != (domain.ContentBlock{Type: "text", Text: docRangeNativeBlockPagedPGText("seed", docRangeNativeBlockPagedPGRecords-1)}) {
				return errors.New("native owner replaced actual block pages or lost the duplicate array's hidden tail")
			}
			nativeHandoffObserved = true
		}
		permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
		if parkStep != 0 {
			if change != "unchanged" {
				return errors.New("fenced native block work completed another quantum")
			}
			if resumedStep == 0 {
				if step != parkStep+1 || observed.work.before != parked || observed.work.beforeResources != parkedResources ||
					!observed.work.inspection.entered.Load() || state.native.handle != parked.native.handle || state.native.preparation != parked.native.preparation ||
					permit != parkedPermit || !permit.live.Load() || permit.resumeWait.Load() <= resumeBefore ||
					!foregroundCompleted.Load() || !parkedObserved.Load() || complete ||
					state.values.values != parked.values.values || state.values.firstPage != parked.values.firstPage || state.values.lastPage != parked.values.lastPage ||
					state.values.records != parked.values.records || state.values.pages != parked.values.pages || state.position <= parked.position {
					return errors.New("same prepared handle and actual native block pages did not resume after foreground progress")
				}
				if decode {
					if state.native.decoder != parked.native.decoder || state.native.native != parked.native.native || state.native.object != parked.native.object ||
						state.native.source != parked.native.source || state.list != parked.list || state.end != parked.end ||
						state.borrowed != parked.borrowed || state.field != parked.field || state.count <= parked.count || state.count >= docRangeNativeBlockPagedPGVisible || state.native.encoding != 0 {
						return errors.New("same block decoder did not advance its retained duplicate-array cursor and borrowed ledger")
					}
				} else if state.native.encoding != parked.native.encoding || state.native.actions <= parked.native.actions ||
					state.values != parked.values || state.count != parked.count || state.length != parked.length || state.position >= state.count {
					return errors.New("same d preparation did not resume its actual high-water block records")
				}
				resumedStep = step
			}
			return nil
		}
		if !observed.work.inspection.entered.Load() {
			return nil
		}
		if decode {
			if state.list == 0 || state.borrowed == 0 || state.count <= 64 || state.count >= docRangeNativeBlockPagedPGVisible/2 || state.values.records != docRangeNativeBlockPagedPGRecords {
				return nil
			}
		} else if state.native.encoding == 0 || state.position <= docRangeNativeBlockPagedPGVisible || state.position >= state.count-1024 || !nativeHandoffObserved {
			return nil
		}
		if !proofSourceObserved || state.values.pages != docRangeNativeBlockPagedPGRecords/64 || state.values.firstPage == 0 || state.values.firstPage == state.values.lastPage ||
			state.first != (domain.ContentBlock{Type: "text", Text: docRangeNativeBlockPagedPGText("next", 0)}) ||
			state.last != (domain.ContentBlock{Type: "text", Text: docRangeNativeBlockPagedPGText("seed", docRangeNativeBlockPagedPGRecords-1)}) {
			return errors.New("active block fixture lost the original proof source, actual pages, partial merge, or hidden high-water tail")
		}
		resources := probe.resources[step-1]
		if complete || permit == nil || !permit.live.Load() || resources.KeepPermit || resources.HeapBytes < state.values.backing+state.values.payload ||
			resources.HeapBytes < state.heap || resources.HeapBytes > maxParkedRootHeapBytes || resources.SpoolBytes < 0 || resources.SpoolBytes > maxParkedRootSpoolBytes {
			return fmt.Errorf("actual block work is not parkable under unchanged heap/spool policy: %+v", resources)
		}
		if err := docRangeWorkPGNoByteStage(bound, fixture, document.Hash); err != nil {
			return err
		}
		var generations int
		if err := fixture.pool.QueryRow(bound, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, document.Hash).Scan(&generations); err != nil || generations != 0 {
			return fmt.Errorf("active block preparation exposed a generation: count=%d error=%v", generations, err)
		}
		test.Logf("actual native block decode=%t data=%x count=%d records=%d pages=%d nodes=%d borrowed=%d field_heap=%d retained_heap=%d spool=%d",
			decode, state.values.values, state.values.count, state.values.records, state.values.pages, state.values.nodes, state.borrowed, state.field, resources.HeapBytes, resources.SpoolBytes)
		parkStep, parked, parkedResources = step, state, resources
		parkedPermit, resumeBefore = permit, permit.resumeWait.Load()
		go func() {
			foreground <- resourceWork(ctx, ResourceBulk, func(read context.Context) error {
				gate.mu.Lock()
				parkedNow := gate.retained == permit && permit.parked && permit.live.Load() && gate.bulk == 1
				gate.mu.Unlock()
				if !parkedNow || permit.resumeWait.Load() != resumeBefore {
					return errors.New("foreground did not enter while the actual native block permit was parked")
				}
				current, err := docRangeNativeBlockPagedPGInspect(observed.work.handle)
				if err != nil || current != parked {
					return fmt.Errorf("parked block state changed before foreground read: state=%+v error=%v", current, err)
				}
				page, err := fixture.service.ReadDocEvents(read, fixture.repo, fixture.hash, "", 0, 20)
				if err != nil {
					return err
				}
				if len(page.Events) != 20 || page.Events[0].Seq != 0 || page.Next != 20 || page.Hash != fixture.hash || !reflect.DeepEqual(page.Events, fixture.cir.Events[:20]) {
					return errors.New("foreground lost its actual pinned page while native blocks remained parked")
				}
				switch change {
				case "lease expiry":
					tag, err := fixture.pool.Exec(read, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3`, fixture.repo, document.Hash, probe.job.Version)
					if err != nil || tag.RowsAffected() != 1 {
						return fmt.Errorf("could not expire the parked block worker lease: rows=%d error=%v", tag.RowsAffected(), err)
					}
				case "source version":
					tag, err := fixture.pool.Exec(read, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, document.Hash)
					if err != nil || tag.RowsAffected() != 1 {
						return fmt.Errorf("could not replace the parked block source version: rows=%d error=%v", tag.RowsAffected(), err)
					}
					var currentVersion int64
					if err := fixture.pool.QueryRow(read, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, document.Hash).Scan(&currentVersion); err != nil || currentVersion <= sourceVersion {
						return fmt.Errorf("block source mutation did not advance its real version: before=%d after=%d error=%v", sourceVersion, currentVersion, err)
					}
				}
				current, err = docRangeNativeBlockPagedPGInspect(observed.work.handle)
				if err != nil || current != parked {
					return fmt.Errorf("parked block state advanced during foreground work: state=%+v error=%v", current, err)
				}
				parkedObserved.Store(true)
				foregroundCompleted.Store(true)
				return nil
			})
		}()
		waitResourceQueue(test, gate, ResourceBulk, 1)
		return nil
	}
	err := fixture.service.ProcessDocRangeJobs(ctx, 1)
	if parkStep == 0 {
		test.Fatal("actual block decode or d preparation never remained active at a parkable quantum boundary", err)
	}
	select {
	case foregroundErr := <-foreground:
		if foregroundErr != nil {
			test.Fatal("foreground failed while actual block work remained parked", foregroundErr)
		}
	case <-ctx.Done():
		test.Fatal("foreground did not finish during active block normalization", ctx.Err())
	}
	assertResourcesReleased(test, gate)
	if !parkedObserved.Load() || !foregroundCompleted.Load() || parkedPermit.resumeWait.Load() <= resumeBefore || len(probe.initial) != 1 || probe.closed != 1 {
		test.Fatal("block parking did not retain and close exactly one actual prepared handle", probe)
	}
	var stored []byte
	var currentVersion int64
	if err := fixture.pool.QueryRow(ctx, `SELECT bytes,doc_range_source_version FROM blobs WHERE hash=$1`, document.Hash).Scan(&stored, &currentVersion); err != nil ||
		!bytes.Equal(stored, raw) || change != "source version" && currentVersion != sourceVersion || change == "source version" && currentVersion <= sourceVersion {
		test.Fatal("block normalization changed original source bytes or lost its current version fence", sourceVersion, currentVersion, err)
	}
	if change != "unchanged" {
		work := observed.work
		if !errors.Is(err, domain.ErrConflict) || !errors.Is(work.failure, domain.ErrConflict) || work.inspection.entered.Load() ||
			work.before != parked || work.after != parked || work.beforeResources != parkedResources || work.afterResources != parkedResources || probe.steps != parkStep || probe.published != 0 {
			test.Fatal("lease or source fence resumed actual block work or published stale pages", err, work)
		}
		if err := docRangeWorkPGNoByteStage(ctx, fixture, document.Hash); err != nil {
			test.Fatal(err)
		}
		var generations int
		if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, document.Hash).Scan(&generations); err != nil || generations != 0 {
			test.Fatal("fenced block normalization published a generation", generations, err)
		}
		return
	}
	if err != nil || resumedStep != parkStep+1 || probe.published != 1 || !nativeHandoffObserved {
		test.Fatal("unchanged block work did not resume and publish through the same handle and actual native pages", err, probe, nativeHandoffObserved)
	}
	assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
	generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
	if generation.Version != 5 || generation.Hash != document.Hash || generation.EventCount != int64(len(document.CIR.Events)) {
		test.Fatal("block normalization published a different generation", generation)
	}
	var expected struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &expected); err != nil {
		test.Fatal(err)
	}
	reader := &docRangeNativePagesFragmentStore{legacyFindGenerationPGStore: newLegacyFindGenerationPGStore(fixture)}
	fixture.service.blobs = reader
	actual := make([][]byte, len(expected.Events))
	for position, event := range expected.Events {
		for offset := 0; offset < len(event); {
			end := min(offset+(64<<10), len(event))
			next := end
			if next == len(event) {
				next = -1
			}
			reader.cost = legacyFindGenerationCosts{}
			fragment, err := fixture.service.ReadDocViewFragment(outbound.WithDocReadOnly(ctx), fixture.repo, document.Hash, position, offset, 64<<10)
			if err != nil || fragment.JSON != string(event[offset:end]) || fragment.Index != position || fragment.Offset != offset || fragment.Total != len(event) || fragment.Next != next ||
				len(fragment.JSON) > 64<<10 || reader.cost.whole != 0 || reader.cost.manifests != 0 || reader.cost.indexes != 0 ||
				reader.cost.chunks < 1 || reader.cost.chunks > 2 || reader.cost.chunkBytes > 2*domain.ChunkTarget {
				test.Fatal("published block fragment changed exact bytes or used unbounded source fallback", position, offset, fragment, reader.cost, err)
			}
			actual[position] = append(actual[position], []byte(fragment.JSON)...)
			offset = end
		}
		if !bytes.Equal(actual[position], event) || domain.HashContent(actual[position]) != domain.HashContent(event) {
			test.Fatal("block fragment continuation changed exact event bytes or SHA-256", position)
		}
	}
	published := domain.AssembleCanonicalDoc(generation.Envelope, [][]byte{bytes.Join(actual, []byte(","))})
	if !bytes.Equal(published, canonical) || domain.HashContent(published) != document.Hash {
		test.Fatal("same-handle block resumption changed exact published canonical bytes or SHA-256")
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, document.Hash).Scan(&stored); err != nil || !bytes.Equal(stored, raw) {
		test.Fatal("published block fragment reads rewrote the original duplicate-array source", err)
	}
}
