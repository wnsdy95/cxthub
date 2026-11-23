//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeNativePagesPGInspection struct {
	handle              uintptr
	encoding            uintptr
	preparation         uintptr
	source              docRangeNativePagesPGSourceInspection
	frames              int64
	actions             uint64
	replacement         uintptr
	replacementCount    int64
	replacementPosition int64
	replacementPhase    uint64
	replacementValues   docRangeNativePagesPGValuesInspection
	decoder             uintptr
	native              uintptr
	object              uintptr
	eventList           uintptr
	eventCursor         uintptr
	eventChild          uintptr
	eventPosition       int64
	eventEnd            int64
	eventCount          int64
	eventPhase          uint64
	eventPaged          bool
	eventValues         docRangeNativePagesPGValuesInspection
	eventBorrowed       int64
	eventHeap           int64
	cursorPosition      int64
	cursorPhase         uint64
	childPosition       int64
}

type docRangeNativePagesPGValuesInspection struct {
	values    uintptr
	root      uintptr
	single    uintptr
	inline    uintptr
	firstPage uintptr
	lastPage  uintptr
	count     int64
	records   int64
	pages     int64
	nodes     int64
	depth     int64
	backing   int64
	payload   int64
}

type docRangeNativePagesPGSourceInspection struct {
	partBytes int64
	bytes     int64
	parts     int
	manifest  uintptr
	hash      domain.ContentHash
}

func docRangeNativePagesPGSourceInspect(source reflect.Value, partBytes int) (docRangeNativePagesPGSourceInspection, error) {
	parts := source.FieldByName("Parts")
	state := docRangeNativePagesPGSourceInspection{
		partBytes: source.FieldByName("PartBytes").Int(), bytes: source.FieldByName("Bytes").Int(),
		parts: parts.Len(), manifest: parts.Pointer(),
	}
	if state.partBytes != int64(partBytes) && (partBytes != domain.ConversationManifestChunkBytes || state.partBytes != 0) ||
		state.bytes <= 0 || int64(state.parts) != (state.bytes+int64(partBytes)-1)/int64(partBytes) || source.FieldByName("Read").IsNil() {
		return docRangeNativePagesPGSourceInspection{}, errors.New("actual PG source lost its expected authenticated part geometry")
	}
	var hashes strings.Builder
	for ordinal := 0; ordinal < parts.Len(); ordinal++ {
		part := parts.Index(ordinal)
		hash := domain.ContentHash(part.FieldByName("Hash").String())
		if part.FieldByName("Bytes").Int() != min(int64(partBytes), state.bytes-int64(ordinal)*int64(partBytes)) || domain.ValidateContentHash(hash) != nil {
			return docRangeNativePagesPGSourceInspection{}, errors.New("actual PG source lost its original part length or hash")
		}
		hashes.WriteString(string(hash))
	}
	state.hash = domain.HashContent([]byte(hashes.String()))
	return state, nil
}

func docRangeNativePagesPGFrame(frames reflect.Value, position int64) (reflect.Value, error) {
	count := frames.FieldByName("count").Int()
	if position < 0 || position >= count {
		return reflect.Value{}, errors.New("native preparation frame position is outside the retained pages")
	}
	node := frames.FieldByName("root")
	single, inline := frames.FieldByName("single"), frames.FieldByName("inline")
	if !single.IsNil() {
		if !node.IsNil() || !inline.IsNil() || count != 1 || frames.FieldByName("pages").Int() != 0 ||
			frames.FieldByName("nodes").Int() != 0 || frames.FieldByName("depth").Int() != 0 {
			return reflect.Value{}, errors.New("native preparation lost its exclusive single frame storage")
		}
		return single.Elem(), nil
	}
	if node.IsNil() {
		if inline.IsNil() || inline.Elem().Kind() != reflect.Array || inline.Elem().Len() != 8 || count > 8 ||
			frames.FieldByName("pages").Int() != 0 || frames.FieldByName("nodes").Int() != 0 || frames.FieldByName("depth").Int() != 0 {
			return reflect.Value{}, errors.New("native preparation lost its inline frame")
		}
		return inline.Elem().Index(int(position)), nil
	}
	depth := frames.FieldByName("depth").Int()
	if !inline.IsNil() || depth < 0 || depth > (int64(reflect.TypeFor[uintptr]().Size())*8-1)/6 {
		return reflect.Value{}, errors.New("native preparation lost its exclusive fixed-page frame storage")
	}
	for level := depth; level > 0; level-- {
		children := node.Elem().FieldByName("children")
		if children.Kind() != reflect.Array || children.Len() != 64 {
			return reflect.Value{}, errors.New("native preparation lost its fixed-width frame children")
		}
		node = children.Index(int((position >> (6 * level)) & 63))
		if node.IsNil() {
			return reflect.Value{}, errors.New("native preparation lost a retained frame page")
		}
	}
	values := node.Elem().FieldByName("values")
	if values.IsNil() || values.Elem().Kind() != reflect.Array || values.Elem().Len() != 64 {
		return reflect.Value{}, errors.New("native preparation lost its retained frame values")
	}
	return values.Elem().Index(int(position & 63)), nil
}

func docRangeNativePagesPGValuesInspect(values reflect.Value) (docRangeNativePagesPGValuesInspection, error) {
	if !values.IsValid() || values.Kind() != reflect.Pointer || values.IsNil() || values.Elem().Type().Name() != "legacyNativeEventValues" {
		return docRangeNativePagesPGValuesInspection{}, errors.New("native replacement lost its actual paged values")
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
	if state.count < 0 || state.records < state.count || state.pages < 0 || state.nodes < 0 ||
		state.depth < 0 || state.depth > (int64(reflect.TypeFor[uintptr]().Size())*8-1)/6 ||
		state.payload < 0 || !payload.FieldByName("err").IsNil() {
		return docRangeNativePagesPGValuesInspection{}, errors.New("native replacement lost its logical count, high-water records, or payload ledger")
	}
	if root.IsNil() {
		if state.records > 8 || state.pages != 0 || state.nodes != 0 || state.depth != 0 || state.records != 0 && single.IsNil() && inline.IsNil() {
			return docRangeNativePagesPGValuesInspection{}, errors.New("native replacement lost its fixed small-list values")
		}
		if !single.IsNil() {
			if !inline.IsNil() || state.records > 1 || single.Elem().Type() != inline.Type().Elem().Elem() ||
				state.records == 0 && !single.Elem().IsZero() {
				return docRangeNativePagesPGValuesInspection{}, errors.New("native replacement lost its exclusive single value or retained an empty value reference")
			}
			state.firstPage, state.lastPage = single.Pointer(), single.Pointer()
			state.backing += int64(single.Elem().Type().Size())
		} else if !inline.IsNil() {
			if inline.Elem().Kind() != reflect.Array || inline.Elem().Len() != 8 {
				return docRangeNativePagesPGValuesInspection{}, errors.New("native replacement retained a variable-size inline array")
			}
			state.firstPage, state.lastPage = inline.Pointer(), inline.Pointer()
			state.backing += int64(inline.Elem().Type().Size())
		}
	} else {
		children, page := root.Elem().FieldByName("children"), root.Elem().FieldByName("values")
		if !single.IsNil() || !inline.IsNil() || state.records <= 8 || state.pages != (state.records+63)/64 || state.nodes < state.pages ||
			children.Kind() != reflect.Array || children.Len() != 64 || page.Type().Elem().Kind() != reflect.Array || page.Type().Elem().Len() != 64 {
			return docRangeNativePagesPGValuesInspection{}, errors.New("native replacement is not retained in fixed 64-value pages")
		}
		state.backing += state.nodes*int64(root.Elem().Type().Size()) + state.pages*int64(page.Type().Elem().Size())
	}
	for position := int64(0); position < state.records; position += 64 {
		record, err := docRangeNativePagesPGFrame(records, position)
		if err != nil {
			return docRangeNativePagesPGValuesInspection{}, err
		}
		if record.Type().Name() != "legacyNativeEventValue" || !record.FieldByName("event").FieldByName("Replacement").IsNil() {
			return docRangeNativePagesPGValuesInspection{}, errors.New("native value page retained a decoded replacement array")
		}
		if position == 0 {
			state.firstPage = record.Addr().Pointer()
		}
		state.lastPage = record.Addr().Pointer()
	}
	if state.pages > 1 && state.firstPage == state.lastPage {
		return docRangeNativePagesPGValuesInspection{}, errors.New("native replacement pages alias the same backing array")
	}
	return state, nil
}

func docRangeNativePagesPGRawInspect(handle outbound.PreparedDocRangeWork, capture, preparation reflect.Value) (docRangeNativePagesPGInspection, error) {
	decoder := preparation.Elem().FieldByName("decoder")
	if decoder.IsNil() {
		return docRangeNativePagesPGInspection{}, nil
	}
	native := decoder.Elem().FieldByName("native")
	if native.IsNil() {
		return docRangeNativePagesPGInspection{}, nil
	}
	object := native.Elem().FieldByName("object")
	if object.IsNil() {
		return docRangeNativePagesPGInspection{}, errors.New("raw native decoder lost its actual object")
	}
	active := object.Elem().FieldByName("eventActive")
	events := object.Elem().FieldByName("events")
	if !active.IsValid() || !events.IsValid() {
		return docRangeNativePagesPGInspection{}, errors.New("raw native decoder does not expose the integrated event-list state")
	}
	if !active.Bool() {
		return docRangeNativePagesPGInspection{}, nil
	}
	field := object.Elem().FieldByName("field").Int()
	if field < 0 || field >= int64(events.Len()) || events.Index(int(field)).IsNil() {
		return docRangeNativePagesPGInspection{}, errors.New("active raw native decoder lost its event list")
	}
	list := events.Index(int(field))
	cursor := list.Elem().FieldByName("cursor")
	if list.Elem().Type().Name() != "legacyNativeEventList" || cursor.IsNil() {
		return docRangeNativePagesPGInspection{}, errors.New("raw native event list did not retain its active driver cursor")
	}
	child := cursor.Elem().FieldByName("object")
	if child.IsNil() || native.Elem().FieldByName("canonicalReady").Bool() ||
		preparation.Elem().FieldByName("pending").Len() != 0 || preparation.Elem().FieldByName("next").Int() != 0 ||
		preparation.Elem().FieldByName("written").Int() != 0 || !capture.FieldByName("work").IsNil() || capture.FieldByName("original").Bool() {
		return docRangeNativePagesPGInspection{}, errors.New("active raw event-list inspection passed native sealing or canonical encoding creation")
	}
	source, err := docRangeNativePagesPGSourceInspect(decoder.Elem().FieldByName("source"), domain.LegacyNormalizationPartBytes)
	if err != nil {
		return docRangeNativePagesPGInspection{}, err
	}
	values, err := docRangeNativePagesPGValuesInspect(list.Elem().FieldByName("values"))
	if err != nil {
		return docRangeNativePagesPGInspection{}, err
	}
	borrowed, estimate := list.Elem().FieldByName("borrowed"), list.Elem().FieldByName("estimate")
	fieldHeap := object.Elem().FieldByName("heaps").Index(int(field))
	if !object.Elem().FieldByName("pagedEvents").Bool() || !list.Elem().FieldByName("paged").Bool() ||
		!cursor.Elem().FieldByName("paged").Bool() || !child.Elem().FieldByName("pagedEvents").Bool() ||
		!native.Elem().FieldByName("decoded").FieldByName("Replacement").IsNil() ||
		!list.Elem().FieldByName("decoded").IsNil() || !list.Elem().FieldByName("previous").IsNil() ||
		list.Elem().FieldByName("records").FieldByName("count").Int() != 0 ||
		!borrowed.FieldByName("err").IsNil() || !estimate.FieldByName("err").IsNil() || !fieldHeap.FieldByName("err").IsNil() ||
		borrowed.FieldByName("bytes").Int() < 0 || borrowed.FieldByName("bytes").Int() != fieldHeap.FieldByName("bytes").Int() ||
		estimate.FieldByName("bytes").Int() < values.backing+values.payload+int64(list.Elem().FieldByName("decoded").Type().Size()) {
		return docRangeNativePagesPGInspection{}, errors.New("actual source decoding retained a contiguous event array or lost its paged field ledger")
	}
	state := docRangeNativePagesPGInspection{
		handle: reflect.ValueOf(handle).Pointer(), preparation: preparation.Pointer(),
		source:  source,
		decoder: decoder.Pointer(), native: native.Pointer(), object: object.Pointer(),
		eventList: list.Pointer(), eventCursor: cursor.Pointer(), eventChild: child.Pointer(),
		eventPosition: list.Elem().FieldByName("position").Int(), eventEnd: list.Elem().FieldByName("end").Int(),
		eventCount: list.Elem().FieldByName("count").Int(), eventPhase: list.Elem().FieldByName("phase").Uint(),
		eventPaged: list.Elem().FieldByName("paged").Bool(), eventValues: values,
		eventBorrowed: borrowed.FieldByName("bytes").Int(), eventHeap: estimate.FieldByName("bytes").Int(),
		cursorPosition: cursor.Elem().FieldByName("position").Int(), cursorPhase: cursor.Elem().FieldByName("phase").Uint(),
		childPosition: child.Elem().FieldByName("position").Int(),
	}
	if state.eventPosition < 0 || state.eventPosition > state.eventEnd || state.eventEnd > decoder.Elem().FieldByName("source").FieldByName("Bytes").Int() ||
		state.cursorPosition < state.eventPosition || state.cursorPosition > state.eventEnd || state.eventCount < 0 {
		return docRangeNativePagesPGInspection{}, errors.New("active raw event-list cursor left its original source bounds")
	}
	if state.eventValues.count != state.eventCount || state.eventValues.records > state.eventCount+1 || state.eventBorrowed != 0 {
		return docRangeNativePagesPGInspection{}, errors.New("raw replacement lost its committed count, reserved record, or first-field borrowed ledger")
	}
	return state, nil
}

func docRangeNativePagesPGInspect(handle outbound.PreparedDocRangeWork, rawDecode bool) (docRangeNativePagesPGInspection, error) {
	capture := reflect.ValueOf(handle).Elem()
	if rawDecode {
		canonical := capture.FieldByName("canonical")
		if !canonical.IsNil() {
			source := canonical.Elem().FieldByName("source")
			if source.FieldByName("Bytes").Int() != 0 {
				state, err := docRangeNativePagesPGSourceInspect(source, domain.ConversationManifestChunkBytes)
				return docRangeNativePagesPGInspection{source: state}, err
			}
		}
	}
	normalization := capture.FieldByName("normalization")
	if normalization.IsNil() {
		return docRangeNativePagesPGInspection{}, nil
	}
	work := normalization.Elem()
	preparation := work.FieldByName("preparation")
	if preparation.IsNil() || !work.FieldByName("canonical").IsNil() {
		return docRangeNativePagesPGInspection{}, nil
	}
	if rawDecode {
		return docRangeNativePagesPGRawInspect(handle, capture, preparation)
	}
	pending := preparation.Elem().FieldByName("pending")
	next := preparation.Elem().FieldByName("next").Int()
	if next < 0 || next >= int64(pending.Len()) {
		return docRangeNativePagesPGInspection{}, nil
	}
	encoding := pending.Index(int(next)).FieldByName("Encoding")
	if encoding.IsNil() {
		return docRangeNativePagesPGInspection{}, nil
	}
	inspector := encoding.Elem().FieldByName("preparation")
	if inspector.IsNil() {
		return docRangeNativePagesPGInspection{}, nil
	}
	frames := inspector.Elem().FieldByName("frames")
	if frames.Kind() != reflect.Struct || !frames.FieldByName("count").IsValid() {
		return docRangeNativePagesPGInspection{}, errors.New("native inspector does not retain the integrated fixed-page frame container")
	}
	count := frames.FieldByName("count").Int()
	if count == 0 {
		return docRangeNativePagesPGInspection{}, nil
	}
	if count < 0 || encoding.Elem().FieldByName("nativeOwner").IsNil() ||
		!capture.FieldByName("work").IsNil() || capture.FieldByName("original").Bool() {
		return docRangeNativePagesPGInspection{}, errors.New("active inspection is not owned by provisional native normalization")
	}
	preparing := inspector.Elem()
	if preparing.FieldByName("lastActions").Int() < 0 || preparing.FieldByName("lastActions").Int() > 1024 ||
		preparing.FieldByName("lastBytes").Int() < 0 || preparing.FieldByName("lastBytes").Int() > 64<<10 ||
		preparing.FieldByName("lastStorageBytes").Int() < 0 || preparing.FieldByName("lastStorageBytes").Int() > 64<<10 ||
		preparation.Elem().FieldByName("written").Int() != 0 || encoding.Elem().FieldByName("frames").FieldByName("count").Int() != 0 {
		return docRangeNativePagesPGInspection{}, errors.New("active native preparation exceeded its unchanged quantum or emitted output before preparation completed")
	}
	state := docRangeNativePagesPGInspection{
		handle: reflect.ValueOf(handle).Pointer(), encoding: encoding.Pointer(), preparation: inspector.Pointer(),
		frames: count, actions: encoding.Elem().FieldByName("inspectWork").Uint(),
	}
	for position := int64(0); position < count; position++ {
		frame, err := docRangeNativePagesPGFrame(frames, position)
		if err != nil {
			return docRangeNativePagesPGInspection{}, err
		}
		if frame.FieldByName("kind").Uint() != 'r' {
			continue
		}
		replacement := frame.FieldByName("value").Elem()
		if replacement.Kind() != reflect.Pointer || replacement.IsNil() || replacement.Elem().Type().Name() != "legacyCanonicalReplacement" ||
			!frame.FieldByName("prepare").Bool() {
			return docRangeNativePagesPGInspection{}, errors.New("replacement preparation is not backed by the actual native wrapper")
		}
		events := replacement.Elem().FieldByName("events")
		values, err := docRangeNativePagesPGValuesInspect(replacement.Elem().FieldByName("native"))
		if err != nil {
			return docRangeNativePagesPGInspection{}, err
		}
		owner := encoding.Elem().FieldByName("nativeOwner").Elem()
		if !events.IsNil() || values.count != frame.FieldByName("count").Int() || values.records != values.count ||
			owner.FieldByName("replacement").Pointer() != values.values || !owner.FieldByName("event").FieldByName("Replacement").IsNil() ||
			!owner.FieldByName("heap").FieldByName("err").IsNil() || owner.FieldByName("heap").FieldByName("bytes").Int() < values.backing+values.payload {
			return docRangeNativePagesPGInspection{}, errors.New("replacement preparation lost its original paged children or native owner handoff")
		}
		state.replacement = replacement.Pointer()
		state.replacementCount = values.count
		state.replacementPosition = frame.FieldByName("position").Int()
		state.replacementPhase = frame.FieldByName("phase").Uint()
		state.replacementValues = values
	}
	return state, nil
}

type docRangeNativePagesPGContext struct {
	context.Context
	inspected atomic.Bool
	rawDecode bool
}

func (ctx *docRangeNativePagesPGContext) Err() error {
	if !ctx.inspected.Load() {
		var callers [12]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		if ctx.rawDecode {
			for {
				frame, more := frames.Next()
				if strings.HasSuffix(frame.Function, "domain.(*legacyNativeEventList).step") ||
					strings.HasSuffix(frame.Function, "domain.(*legacyNativeEventList).advance") {
					ctx.inspected.Store(true)
					break
				}
				if !more {
					break
				}
			}
			return ctx.Context.Err()
		}
		preparation, _ := frames.Next()
		encoding, _ := frames.Next()
		consumer, _ := frames.Next()
		if strings.HasSuffix(preparation.Function, "domain.(*legacyCanonicalPreparation).step") &&
			strings.HasSuffix(encoding.Function, "domain.(*LegacyCanonicalEncoding).Next") &&
			strings.HasSuffix(consumer.Function, "domain.(*LegacyNormalizationPreparation).stage") {
			ctx.inspected.Store(true)
		}
	}
	return ctx.Context.Err()
}

type docRangeNativePagesPGStore struct {
	*docRangeWorkPGProbe
	work      *docRangeNativePagesPGWork
	rawDecode bool
}

type docRangeNativePagesPGWork struct {
	outbound.PreparedDocRangeWork
	handle          outbound.PreparedDocRangeWork
	inspection      *docRangeNativePagesPGContext
	before          docRangeNativePagesPGInspection
	after           docRangeNativePagesPGInspection
	beforeResources outbound.RootPublicationResources
	afterResources  outbound.RootPublicationResources
	failure         error
	rawDecode       bool
	resumeObserved  bool
	afterObserved   bool
}

func (storage *docRangeNativePagesPGStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	prepared, err := storage.docRangeWorkPGProbe.PrepareDocRangeWork(ctx, job)
	if prepared == nil || err != nil {
		return prepared, err
	}
	work, supported := prepared.(*docRangeWorkPGPrepared)
	if !supported {
		return prepared, errors.New("native inspection did not receive the existing actual PG work probe")
	}
	storage.work = &docRangeNativePagesPGWork{PreparedDocRangeWork: prepared, handle: work.PreparedDocRangeWork, rawDecode: storage.rawDecode}
	return storage.work, nil
}

func (work *docRangeNativePagesPGWork) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	resumedBeforeStep := work.resumeObserved
	work.afterObserved = false
	var err error
	work.before, err = docRangeNativePagesPGInspect(work.handle, work.rawDecode)
	if err != nil {
		return 0, err
	}
	work.beforeResources, err = work.handle.RetainedResources()
	if err != nil {
		return 0, err
	}
	stepContext := ctx
	if !work.resumeObserved {
		work.inspection = &docRangeNativePagesPGContext{Context: ctx, rawDecode: work.rawDecode}
		stepContext = work.inspection
	}
	progress, stepErr := work.PreparedDocRangeWork.Step(stepContext, job)
	if !resumedBeforeStep || stepErr != nil || !work.afterObserved {
		work.after, err = docRangeNativePagesPGInspect(work.handle, work.rawDecode)
		if err != nil {
			return 0, errors.Join(stepErr, err)
		}
	}
	work.afterResources, err = work.handle.RetainedResources()
	work.failure = errors.Join(stepErr, err)
	return progress, work.failure
}

type docRangeNativePagesPGPostInspectionHandle struct {
	*preparedDocRangeWorkStub
	normalization *struct{}
	canonical     *struct{}
}

func TestPGDocRangeNativePagesPostInspectionReuse(test *testing.T) {
	for _, input := range []struct {
		name      string
		resumed   bool
		observed  bool
		failure   error
		wantReuse bool
	}{
		{name: "before resume"},
		{name: "first resume", observed: true},
		{name: "resumed", resumed: true, observed: true, wantReuse: true},
		{name: "missing callback", resumed: true},
		{name: "callback failure", resumed: true, observed: true, failure: domain.ErrConflict},
		{name: "step failure", resumed: true, failure: domain.ErrConflict},
		{name: "fenced", failure: domain.ErrConflict},
	} {
		for _, rawDecode := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/raw=%t", input.name, rawDecode), func(test *testing.T) {
				handle := &docRangeNativePagesPGPostInspectionHandle{preparedDocRangeWorkStub: &preparedDocRangeWorkStub{}}
				stale := docRangeNativePagesPGInspection{eventCount: 7}
				observed := docRangeNativePagesPGInspection{eventCount: 11}
				work := &docRangeNativePagesPGWork{
					PreparedDocRangeWork: handle, handle: handle, before: stale, after: stale,
					rawDecode: rawDecode, resumeObserved: input.resumed, afterObserved: true,
				}
				handle.step = func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
					if input.observed {
						work.after, work.afterObserved, work.resumeObserved = observed, true, true
					}
					return outbound.DocRangeWorkPending, input.failure
				}
				progress, err := work.Step(context.Background(), domain.DocRangeIndexJob{})
				var expected docRangeNativePagesPGInspection
				if input.wantReuse {
					expected = observed
				}
				if progress != outbound.DocRangeWorkPending || !errors.Is(err, input.failure) || !errors.Is(work.failure, input.failure) ||
					work.before != (docRangeNativePagesPGInspection{}) || work.after != expected || work.afterObserved != input.observed {
					test.Fatal("native observer reused stale, first-resume, or failed post-state, or lost its full before inspection", progress, err, work)
				}
				resources := outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}
				if handle.resourceChecks != 2 || work.beforeResources != resources || work.afterResources != resources {
					test.Fatal("native post-state reuse changed retained-resource observations", handle.resourceChecks, work)
				}
			})
		}
	}
}

type docRangeNativePagesFragmentStore struct {
	*legacyFindGenerationPGStore
}

func (storage *docRangeNativePagesFragmentStore) ReadLegacyDerivedByteChunk(ctx context.Context, repo, hash domain.ContentHash, binding domain.DocReadGenerationBinding, ordinal int, descriptor domain.ConversationManifestChunk) ([]byte, error) {
	storage.cost.chunks++
	content, err := storage.PostgresStore.ReadLegacyDerivedByteChunk(ctx, repo, hash, binding, ordinal, descriptor)
	storage.cost.chunkBytes += len(content)
	if len(content) > domain.ChunkTarget {
		return nil, errors.Join(err, errors.New("native fragment read exceeded the unchanged chunk budget"))
	}
	return content, err
}
