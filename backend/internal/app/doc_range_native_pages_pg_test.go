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
	var err error
	work.before, err = docRangeNativePagesPGInspect(work.handle, work.rawDecode)
	if err != nil {
		return 0, err
	}
	work.beforeResources, err = work.handle.RetainedResources()
	if err != nil {
		return 0, err
	}
	work.inspection = &docRangeNativePagesPGContext{Context: ctx, rawDecode: work.rawDecode}
	progress, stepErr := work.PreparedDocRangeWork.Step(work.inspection, job)
	work.after, err = docRangeNativePagesPGInspect(work.handle, work.rawDecode)
	if err != nil {
		return 0, errors.Join(stepErr, err)
	}
	work.afterResources, err = work.handle.RetainedResources()
	work.failure = errors.Join(stepErr, err)
	return progress, work.failure
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

type docRangeNativePagesPGInput struct {
	name             string
	version          string
	events           func(domain.ContentHash) []domain.CIREvent
	replacementCount int64
	rawDecode        bool
}

func docRangeNativePagesPGDenseEvents(repo domain.ContentHash) []domain.CIREvent {
	fields := make(map[string]any, 4096)
	for position := 0; position < 4096; position++ {
		fields[fmt.Sprintf("k%04d", position)] = "x"
	}
	return []domain.CIREvent{
		{Kind: domain.EventToolCall, CallID: "active-native-pages", ToolName: "dense", Input: fields},
		historyMessage(domain.RoleUser, "native inspector boundary "+string(repo)),
	}
}

func docRangeNativePagesPGReplacementEvents(repo domain.ContentHash) []domain.CIREvent {
	children := make([]domain.CIREvent, 4097)
	for position := range children {
		children[position] = domain.CIREvent{
			Kind: domain.EventToolCall, Seq: (len(children) - 1 - position) / 2,
			CallID: fmt.Sprintf("child-%04d", position), ToolName: "replacement",
			Input: map[string]any{"z": map[string]any{"z": "last", "a": "first"}, "a": fmt.Sprintf("ordinal-%04d", position)},
		}
	}
	return []domain.CIREvent{
		{Kind: domain.EventCompaction, Replacement: children, ReplacementComplete: true},
		historyMessage(domain.RoleUser, "native inspector boundary "+string(repo)),
	}
}

func docRangeNativePagesPGRawEvents(repo domain.ContentHash) []domain.CIREvent {
	children := make([]domain.CIREvent, 8192)
	for position := range children {
		children[position] = historyMessage(domain.RoleAssistant, strings.Repeat("x", 64))
	}
	return []domain.CIREvent{
		{Kind: domain.EventCompaction, Replacement: children, ReplacementComplete: true},
		historyMessage(domain.RoleUser, "native inspector boundary "+string(repo)),
	}
}

func docRangeNativePagesPGRawSource(test *testing.T, document domain.CIRDocument) []byte {
	test.Helper()
	if len(document.Events) != 2 || len(document.Events[0].Replacement) != 8192 {
		test.Fatal("raw native fixture lost the original 8192-child source gate")
	}
	child := `{"kind":"message","role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("x", 64) + `"}]}`
	replacement := `{"kind":"compaction","replacement":[` + strings.Repeat(child+",", 8191) + child + `],"replacement_complete":true}`
	if len(replacement) != 1138753 || len(replacement) <= 2*domain.ConversationManifestChunkBytes {
		test.Fatal("raw native fixture shrank the original large replacement source gate", len(replacement))
	}
	envelope, err := json.Marshal(document.Envelope)
	if err != nil {
		test.Fatal(err)
	}
	boundary, err := json.Marshal(document.Events[1])
	if err != nil {
		test.Fatal(err)
	}
	return []byte(`{"envelope":` + string(envelope) + `,"events":[` + replacement + "," + string(boundary) + "]}")
}

func docRangeNativePagesPGUnpublishedNormalization(ctx context.Context, fixture docViewPGFixture, hash domain.ContentHash, sourceVersion int64, job domain.DocRangeIndexJob, handle outbound.PreparedDocRangeWork) error {
	if fixture.storage.InTransaction(ctx) || job.Validate() != nil || job.RepoID != fixture.repo || job.DocHash != hash || job.BaseHash != "" || job.State != "running" || job.RepositoryEpoch == "" {
		return errors.New("active normalization lost its unborrowed current claim")
	}
	capture := reflect.ValueOf(handle).Elem()
	normalization := capture.FieldByName("normalization")
	if normalization.IsNil() || normalization.Elem().FieldByName("preparation").IsNil() || !normalization.Elem().FieldByName("canonical").IsNil() {
		return errors.New("checkpoint allowance requires active native normalization before canonical publication")
	}
	checkpoint := capture.FieldByName("checkpoint")
	if checkpoint.IsNil() {
		return docRangeWorkPGNoByteStage(ctx, fixture, hash)
	}
	state := checkpoint.Elem().FieldByName("state")
	stage := state.FieldByName("stage")
	if stage.FieldByName("incarnation").String() == "" || stage.FieldByName("sourceID").String() == "" ||
		stage.FieldByName("sourceVersion").Int() != sourceVersion || stage.FieldByName("plan").String() != "" ||
		stage.FieldByName("blocks").Int() != 0 || stage.FieldByName("next").Int() != 0 || stage.FieldByName("complete").Bool() {
		return errors.New("active normalization checkpoint lost its provisional source binding")
	}
	var sourceID, stageID, checkpointHash string
	var version, sequence int64
	var checkpointBytes int
	var provisional, current, checkpointValid, unpublished bool
	err := fixture.pool.QueryRow(ctx, `SELECT stage.source_id::text,stage.source_version,stage.stage_id::text,
 stage.checkpoint_sequence,COALESCE(stage.checkpoint_hash,''),stage.checkpoint_bytes,
 stage.plan_hash IS NULL AND stage.spec IS NULL AND stage.blocks IS NULL AND stage.next_block=0 AND NOT stage.complete
 AND stage.header_version=0 AND stage.header_bytes IS NULL AND stage.header_next=0 AND NOT stage.header_complete,
 EXISTS(SELECT 1 FROM snapshots snapshot
 JOIN repo_blobs owned ON owned.repo_id=snapshot.repo_id AND owned.hash=snapshot.doc_hash AND owned.kind='doc'
 JOIN blobs original ON original.hash=owned.hash
 JOIN blob_source_incarnations incarnation ON incarnation.hash=original.hash
 JOIN repository_catalog_state catalog ON catalog.repo_id=snapshot.repo_id
 JOIN doc_range_jobs_v1 job ON job.repo_id=snapshot.repo_id AND job.doc_hash=snapshot.doc_hash AND job.base_hash=''
 WHERE snapshot.repo_id=$1 AND snapshot.id=$2 AND snapshot.doc_hash=$2 AND snapshot.doc_identity=''
 AND original.doc_range_source_version=$3 AND stage.source_version=$3 AND incarnation.incarnation=stage.source_id
 AND catalog.epoch::text=$5 AND job.version=$4 AND job.state='running' AND job.lease_until>clock_timestamp()
 AND job.minimum_range_version=4 AND job.byte_source_version=$3),
 stage.checkpoint_sequence>=0 AND ((stage.checkpoint_hash IS NULL AND stage.checkpoint_bytes=0)
 OR (stage.checkpoint_sequence>0 AND EXISTS(SELECT 1 FROM doc_legacy_byte_stage_checkpoint_objects_v1 object
 WHERE object.doc_hash=stage.doc_hash AND object.stage_id=stage.stage_id AND object.hash=stage.checkpoint_hash
 AND octet_length(object.bytes)=stage.checkpoint_bytes AND object.hash='sha256:'||encode(sha256(object.bytes),'hex'))))
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_checkpoint_objects_v1 object WHERE object.doc_hash=$2 AND object.stage_id<>stage.stage_id),
 NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$2)
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$2)
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$2)
 AND NOT EXISTS(SELECT 1 FROM doc_range_generations_v2 WHERE doc_hash=$2)
 AND NOT EXISTS(SELECT 1 FROM doc_range_objects_v2 WHERE doc_hash=$2)
 AND NOT EXISTS(SELECT 1 FROM doc_range_indexes_v1 WHERE hash=$2)
 AND NOT EXISTS(SELECT 1 FROM doc_range_pages_v1 WHERE doc_hash=$2)
 AND NOT EXISTS(SELECT 1 FROM doc_range_boundaries_v1 WHERE doc_hash=$2 OR base_hash=$2)
 FROM doc_legacy_byte_stages_v1 stage WHERE stage.doc_hash=$2`, fixture.repo, hash, sourceVersion, job.Version, job.RepositoryEpoch).Scan(
		&sourceID, &version, &stageID, &sequence, &checkpointHash, &checkpointBytes,
		&provisional, &current, &checkpointValid, &unpublished)
	if err != nil {
		return err
	}
	if sourceID != stage.FieldByName("sourceID").String() || version != sourceVersion || stageID != stage.FieldByName("incarnation").String() ||
		sequence != state.FieldByName("sequence").Int() || checkpointHash != state.FieldByName("hash").String() || int64(checkpointBytes) != state.FieldByName("bytes").Int() ||
		!provisional || !current || !checkpointValid || !unpublished {
		return fmt.Errorf("normalization checkpoint lost current private state or exposed publication: stage=%s sequence=%d provisional=%t current=%t checkpoint=%t unpublished=%t",
			stageID, sequence, provisional, current, checkpointValid, unpublished)
	}
	return nil
}

func docRangeNativePagesPGPrivateImage(ctx context.Context, fixture docViewPGFixture, hash domain.ContentHash) (string, error) {
	if fixture.storage.InTransaction(ctx) {
		return "", errors.New("normalization checkpoint retained a transaction")
	}
	var image string
	err := fixture.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'stage',(SELECT jsonb_build_array(to_jsonb(stage),stage.xmin::text) FROM doc_legacy_byte_stages_v1 stage WHERE doc_hash=$1),
 'checkpoints',(SELECT jsonb_agg(jsonb_build_array(stage_id,hash,octet_length(bytes),encode(sha256(bytes),'hex'),object.xmin::text)
 ORDER BY stage_id,hash) FROM doc_legacy_byte_stage_checkpoint_objects_v1 object WHERE doc_hash=$1),
 'parts',(SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1),
 'objects',(SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1),
 'headers',(SELECT count(*) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1),
 'generations',(SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1),
 'verified_objects',(SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$1),
 'indexes',(SELECT count(*) FROM doc_range_indexes_v1 WHERE hash=$1),
 'pages',(SELECT count(*) FROM doc_range_pages_v1 WHERE doc_hash=$1),
 'boundaries',(SELECT count(*) FROM doc_range_boundaries_v1 WHERE doc_hash=$1 OR base_hash=$1)
)::text`, hash).Scan(&image)
	return image, err
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFences(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "dense-map", version: domain.CIRVersionV1, events: docRangeNativePagesPGDenseEvents}, "unchanged")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesLeaseExpiry(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "dense-map", version: domain.CIRVersionV1, events: docRangeNativePagesPGDenseEvents}, "lease expiry")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesSourceVersion(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "dense-map", version: domain.CIRVersionV1, events: docRangeNativePagesPGDenseEvents}, "source version")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacement(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "replacement-unsorted-tie-4097", version: domain.CIRVersionV2, events: docRangeNativePagesPGReplacementEvents, replacementCount: 4097}, "unchanged")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacementLeaseExpiry(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "replacement-unsorted-tie-4097", version: domain.CIRVersionV2, events: docRangeNativePagesPGReplacementEvents, replacementCount: 4097}, "lease expiry")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesReplacementSourceVersion(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "replacement-unsorted-tie-4097", version: domain.CIRVersionV2, events: docRangeNativePagesPGReplacementEvents, replacementCount: 4097}, "source version")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesRawReplacement(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "raw-replacement-8192-text64", version: domain.CIRVersionV2, events: docRangeNativePagesPGRawEvents, replacementCount: 8192, rawDecode: true}, "unchanged")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesRawReplacementLeaseExpiry(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "raw-replacement-8192-text64", version: domain.CIRVersionV2, events: docRangeNativePagesPGRawEvents, replacementCount: 8192, rawDecode: true}, "lease expiry")
}

func TestPGDocRangeNativePagesActiveNormalizationParksAndRechecksFencesRawReplacementSourceVersion(test *testing.T) {
	docRangeNativePagesPGParksAndRechecksFences(test, docRangeNativePagesPGInput{name: "raw-replacement-8192-text64", version: domain.CIRVersionV2, events: docRangeNativePagesPGRawEvents, replacementCount: 8192, rawDecode: true}, "source version")
}

func docRangeNativePagesPGParksAndRechecksFences(test *testing.T, input docRangeNativePagesPGInput, change string) {
	test.Helper()
	test.Run(input.name, func(test *testing.T) {
		test.Run(change, func(test *testing.T) {
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
			fixture.cir.Envelope.CIRVersion = input.version
			document := seedLegacyFindPGDocument(test, fixture, input.events(fixture.repo))
			canonical, err := domain.CanonicalBytes(document.CIR)
			if err != nil {
				test.Fatal(err)
			}
			source := canonical
			if input.replacementCount != 0 && !input.rawDecode {
				children := document.CIR.Events[0].Replacement
				if int64(len(children)) != input.replacementCount || len(children) != 4097 ||
					children[0].Seq <= children[len(children)-1].Seq || children[1].Seq != children[2].Seq || children[1].CallID == children[2].CallID {
					test.Fatal("fixture does not retain 4097 unsorted replacement children with distinct same-sequence ordinals")
				}
				source, err = json.Marshal(document.CIR)
				if err != nil {
					test.Fatal(err)
				}
				if bytes.Equal(source, canonical) {
					test.Fatal("replacement source was already sorted by the canonical oracle")
				}
			}
			var raw bytes.Buffer
			if input.rawDecode {
				raw.Write(docRangeNativePagesPGRawSource(test, document.CIR))
				var decoded domain.CIRDocument
				if err := json.Unmarshal(raw.Bytes(), &decoded); err != nil {
					test.Fatal(err)
				}
				decodedCanonical, err := domain.CanonicalBytes(decoded)
				if err != nil || !bytes.Equal(decodedCanonical, canonical) || domain.HashContent(decodedCanonical) != document.Hash {
					test.Fatal("raw native fixture differs from the unchanged typed canonical oracle", err)
				}
			} else if err := json.Indent(&raw, source, "", "  "); err != nil {
				test.Fatal(err)
			}
			if input.replacementCount == 0 && len(document.CIR.Events[0].Input) <= 1024 ||
				bytes.Equal(raw.Bytes(), canonical) || domain.HashContent(raw.Bytes()) == document.Hash {
				test.Fatal("fixture does not require active native noncanonical normalization")
			}
			sourceManifests := make(map[int]domain.ContentHash, 2)
			if input.rawDecode {
				for _, partBytes := range []int{domain.ConversationManifestChunkBytes, domain.LegacyNormalizationPartBytes} {
					var hashes strings.Builder
					for offset := 0; offset < raw.Len(); offset += partBytes {
						hashes.WriteString(string(domain.HashContent(raw.Bytes()[offset:min(offset+partBytes, raw.Len())])))
					}
					sourceManifests[partBytes] = domain.HashContent([]byte(hashes.String()))
				}
			}
			if _, err := fixture.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, document.Hash, raw.Bytes()); err != nil {
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
			observed := &docRangeNativePagesPGStore{docRangeWorkPGProbe: probe, rawDecode: input.rawDecode}
			fixture.service.blobs = observed
			foreground := make(chan error, 1)
			var foregroundCompleted, parkedObserved atomic.Bool
			var parked docRangeNativePagesPGInspection
			var parkedPrivateImage string
			var parkedResources outbound.RootPublicationResources
			var parkedPermit *resourcePermit
			var resumeBefore int64
			proofSourceObserved := false
			nativeHandoffObserved := false
			parkStep, resumedStep := 0, 0
			probe.afterStep = func(bound context.Context, step int, complete bool) error {
				state, err := docRangeNativePagesPGInspect(observed.work.handle, input.rawDecode)
				if err != nil {
					return err
				}
				if input.rawDecode && state.source.manifest != 0 {
					partBytes := domain.ConversationManifestChunkBytes
					if state.decoder != 0 {
						partBytes = domain.LegacyNormalizationPartBytes
					}
					if state.source.bytes != int64(raw.Len()) || state.source.hash != sourceManifests[partBytes] {
						return errors.New("actual PG source manifest differs from the original raw fixture")
					}
					if state.decoder == 0 {
						proofSourceObserved = true
					} else if observed.work.before.decoder != 0 && observed.work.before.source != state.source {
						return errors.New("same raw decoder changed source geometry or manifest identity across a quantum")
					}
				}
				if input.rawDecode && change == "unchanged" && parkStep != 0 && state.eventList == 0 {
					projection, err := docRangeNativePagesPGInspect(observed.work.handle, false)
					if err != nil {
						return err
					}
					if projection.replacement != 0 {
						if projection.handle != parked.handle || projection.replacementCount != input.replacementCount ||
							projection.replacementValues.values != parked.eventValues.values || projection.replacementValues.firstPage != parked.eventValues.firstPage {
							return errors.New("raw source value pages were replaced before native owner projection")
						}
						nativeHandoffObserved = true
					}
				}
				permit, _ := bound.Value(resourcePermitKey{}).(*resourcePermit)
				if parkStep != 0 {
					if change != "unchanged" {
						return errors.New("fenced native inspector completed another quantum")
					}
					if resumedStep == 0 {
						if step != parkStep+1 || observed.work.before != parked || !observed.work.inspection.inspected.Load() ||
							state.handle != parked.handle || state.preparation != parked.preparation ||
							permit != parkedPermit || !permit.live.Load() || permit.resumeWait.Load() <= resumeBefore ||
							!foregroundCompleted.Load() || !parkedObserved.Load() || complete {
							return errors.New("same active native inspector did not resume its owned permit after foreground progress")
						}
						if input.rawDecode {
							if state.decoder != parked.decoder || state.native != parked.native || state.object != parked.object ||
								state.source != parked.source ||
								state.eventList != parked.eventList || state.eventCursor != parked.eventCursor || state.eventChild != parked.eventChild ||
								!state.eventPaged || state.eventValues.values != parked.eventValues.values || state.eventValues.firstPage != parked.eventValues.firstPage ||
								state.eventValues.pages < parked.eventValues.pages || state.eventValues.records < parked.eventValues.records || state.eventBorrowed != parked.eventBorrowed ||
								state.eventEnd != parked.eventEnd || state.eventPosition <= parked.eventPosition || state.cursorPosition <= parked.cursorPosition ||
								state.eventCount <= parked.eventCount || state.eventCount >= input.replacementCount || state.encoding != 0 || state.frames != 0 {
								return errors.New("same raw decoder, event list, and driver cursor did not resume before native sealing")
							}
						} else if state.encoding != parked.encoding || state.actions <= parked.actions ||
							state.replacement != parked.replacement || state.replacementCount != parked.replacementCount ||
							state.replacementValues != parked.replacementValues ||
							parked.replacement != 0 && (state.replacementPhase != 0 || state.replacementPosition <= parked.replacementPosition || state.replacementPosition >= state.replacementCount) {
							return errors.New("same active canonical inspector did not resume its retained projection")
						}
						resumedStep = step
					}
					return nil
				}
				if !observed.work.inspection.inspected.Load() {
					return nil
				}
				if input.rawDecode {
					if state.eventList == 0 || state.eventCount <= 64 || state.eventCount >= input.replacementCount {
						return nil
					}
					if !proofSourceObserved {
						return errors.New("raw normalization did not retain the original 512 KiB capture proof source")
					}
					if !state.eventPaged || state.eventValues.pages < 2 || state.eventValues.firstPage == 0 || state.eventValues.lastPage == state.eventValues.firstPage {
						return errors.New("actual raw replacement did not retain distinct fixed native value pages")
					}
				} else {
					if state.frames == 0 || input.replacementCount != 0 && (state.replacement == 0 || state.replacementPhase != 0 ||
						state.replacementPosition <= 0 || state.replacementPosition >= state.replacementCount) {
						return nil
					}
					if state.replacementCount != input.replacementCount {
						return errors.New("active native preparation does not retain the fixture's original replacement children")
					}
				}
				resources := probe.resources[step-1]
				if complete || permit == nil || !permit.live.Load() || resources.KeepPermit || resources.HeapBytes <= 0 ||
					resources.HeapBytes > maxParkedRootHeapBytes || resources.SpoolBytes < 0 || resources.SpoolBytes > maxParkedRootSpoolBytes ||
					input.rawDecode && resources.HeapBytes < state.eventHeap {
					return fmt.Errorf("active native inspector is not eligible for unchanged parking budgets: %+v", resources)
				}
				test.Logf("active native mode=%s raw_decode=%t retained_heap=%d spool=%d", input.name, input.rawDecode, resources.HeapBytes, resources.SpoolBytes)
				if input.rawDecode {
					test.Logf("actual paged source values=%x count=%d records=%d pages=%d nodes=%d borrowed=%d field_heap=%d", state.eventValues.values,
						state.eventValues.count, state.eventValues.records, state.eventValues.pages, state.eventValues.nodes, state.eventBorrowed, state.eventHeap)
				}
				if err := docRangeNativePagesPGUnpublishedNormalization(bound, fixture, document.Hash, sourceVersion, probe.job, observed.work.handle); err != nil {
					return err
				}
				parkedPrivateImage, err = docRangeNativePagesPGPrivateImage(bound, fixture, document.Hash)
				if err != nil {
					return err
				}
				var generations int
				if err := fixture.pool.QueryRow(bound, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, document.Hash).Scan(&generations); err != nil || generations != 0 {
					return fmt.Errorf("active native preparation exposed a generation before completion: count=%d error=%v", generations, err)
				}
				parkStep, parked, parkedResources = step, state, resources
				parkedPermit, resumeBefore = permit, permit.resumeWait.Load()
				go func() {
					foreground <- resourceWork(ctx, ResourceBulk, func(read context.Context) error {
						gate.mu.Lock()
						parkedNow := gate.retained == permit && permit.parked && permit.live.Load() && gate.bulk == 1
						gate.mu.Unlock()
						if !parkedNow || permit.resumeWait.Load() != resumeBefore {
							return errors.New("foreground was not admitted while the actual native worker permit was parked")
						}
						current, err := docRangeNativePagesPGInspect(observed.work.handle, input.rawDecode)
						if err != nil || current != parked {
							return fmt.Errorf("parked inspector changed before foreground read: state=%+v error=%v", current, err)
						}
						page, err := fixture.service.ReadDocEvents(read, fixture.repo, fixture.hash, "", 0, 20)
						if err != nil {
							return err
						}
						if len(page.Events) != 20 || page.Events[0].Seq != 0 || page.Next != 20 || page.Hash != fixture.hash ||
							!reflect.DeepEqual(page.Events, fixture.cir.Events[:20]) {
							return errors.New("foreground lost its actual pinned page during native inspection")
						}
						switch change {
						case "lease expiry":
							tag, err := fixture.pool.Exec(read, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3`, fixture.repo, document.Hash, probe.job.Version)
							if err != nil || tag.RowsAffected() != 1 {
								return fmt.Errorf("could not expire the parked native worker lease: rows=%d error=%v", tag.RowsAffected(), err)
							}
						case "source version":
							tag, err := fixture.pool.Exec(read, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, document.Hash)
							if err != nil || tag.RowsAffected() != 1 {
								return fmt.Errorf("could not replace the parked native source version: rows=%d error=%v", tag.RowsAffected(), err)
							}
							var currentVersion int64
							if err := fixture.pool.QueryRow(read, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, document.Hash).Scan(&currentVersion); err != nil || currentVersion <= sourceVersion {
								return fmt.Errorf("source mutation did not advance its real version: before=%d after=%d error=%v", sourceVersion, currentVersion, err)
							}
						}
						current, err = docRangeNativePagesPGInspect(observed.work.handle, input.rawDecode)
						if err != nil || current != parked {
							return fmt.Errorf("parked native inspector advanced during foreground work: state=%+v error=%v", current, err)
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
			if err != nil && observed.work != nil {
				last := observed.work.before
				test.Logf("normalization failure: steps=%d published=%d event_count=%d event_position=%d records=%d replacement_position=%d actions=%d error=%v",
					probe.steps, probe.published, last.eventCount, last.eventPosition, last.eventValues.records,
					last.replacementPosition, last.actions, err)
			}
			if parkStep == 0 {
				test.Fatal("actual native preparation never remained active at a parkable quantum boundary", err)
			}
			select {
			case foregroundErr := <-foreground:
				if foregroundErr != nil {
					test.Fatal("foreground failed while the native inspector remained parked", foregroundErr)
				}
			case <-ctx.Done():
				test.Fatal("foreground did not finish during active native normalization", ctx.Err())
			}
			assertResourcesReleased(test, gate)
			if !parkedObserved.Load() || !foregroundCompleted.Load() || parkedPermit.resumeWait.Load() <= resumeBefore ||
				len(probe.initial) != 1 || probe.closed != 1 {
				test.Fatal("native parking did not retain and close exactly one actual prepared handle", probe)
			}
			var stored []byte
			var currentVersion int64
			if err := fixture.pool.QueryRow(ctx, `SELECT bytes,doc_range_source_version FROM blobs WHERE hash=$1`, document.Hash).Scan(&stored, &currentVersion); err != nil ||
				!bytes.Equal(stored, raw.Bytes()) || change != "source version" && currentVersion != sourceVersion || change == "source version" && currentVersion <= sourceVersion {
				test.Fatal("native normalization changed source bytes or lost its current version fence", sourceVersion, currentVersion, err)
			}
			if change != "unchanged" {
				work := observed.work
				if !errors.Is(err, domain.ErrConflict) || !errors.Is(work.failure, domain.ErrConflict) || work.inspection.inspected.Load() ||
					work.before != parked || work.after != parked || work.beforeResources != parkedResources ||
					work.afterResources != parkedResources || probe.steps != parkStep || probe.published != 0 {
					test.Fatal("lease or current-source fence resumed native preparation or published stale work", err, work)
				}
				if image, err := docRangeNativePagesPGPrivateImage(ctx, fixture, document.Hash); err != nil || image != parkedPrivateImage {
					test.Fatal("fenced native normalization changed its private checkpoint or published canonical state", err)
				}
				var generations int
				if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, document.Hash).Scan(&generations); err != nil || generations != 0 {
					test.Fatal("fenced native normalization published a generation", generations, err)
				}
				return
			}
			if err != nil || resumedStep != parkStep+1 || probe.published != 1 || input.rawDecode && !nativeHandoffObserved {
				test.Fatal("unchanged native inspector did not resume and publish through the same handle and native value pages", err, probe, nativeHandoffObserved)
			}
			assertDocByteUpgradePGRequirement(test, fixture, document.Hash, "completed")
			generation := readDocByteUpgradePGGeneration(test, fixture, document.Hash)
			if generation.Version != 5 || generation.Hash != document.Hash || generation.EventCount != int64(len(document.CIR.Events)) {
				test.Fatal("native normalization published a different generation", generation)
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
				if len(event) < 4 || input.replacementCount == 0 && len(event) > 64<<10 {
					test.Fatal("native fixture exceeded the unchanged fragment budget", len(event))
				}
				budget := min(len(event), 64<<10)
				for offset := 0; offset < len(event); {
					end := min(offset+budget, len(event))
					next := end
					if next == len(event) {
						next = -1
					}
					reader.cost = legacyFindGenerationCosts{}
					fragment, err := fixture.service.ReadDocViewFragment(outbound.WithDocReadOnly(ctx), fixture.repo, document.Hash, position, offset, budget)
					if err != nil || fragment.JSON != string(event[offset:end]) || fragment.Index != position || fragment.Offset != offset ||
						fragment.Total != len(event) || fragment.Next != next || len(fragment.JSON) > 64<<10 ||
						reader.cost.whole != 0 || reader.cost.manifests != 0 || reader.cost.indexes != 0 ||
						reader.cost.chunks < 1 || reader.cost.chunks > 2 || reader.cost.chunkBytes > 2*domain.ChunkTarget {
						test.Fatal("published native bytes differ from the independent canonical oracle or use unbounded reads or whole-source fallback", position, offset, fragment, reader.cost, err)
					}
					actual[position] = append(actual[position], []byte(fragment.JSON)...)
					offset = end
				}
				if !bytes.Equal(actual[position], event) || domain.HashContent(actual[position]) != domain.HashContent(event) {
					test.Fatal("native fragment continuation changed exact event bytes or SHA-256", position)
				}
			}
			published := domain.AssembleCanonicalDoc(generation.Envelope, [][]byte{bytes.Join(actual, []byte(","))})
			if !bytes.Equal(published, canonical) || domain.HashContent(published) != document.Hash {
				test.Fatal("same-handle native resumption changed exact published canonical bytes or SHA-256")
			}
			if err := fixture.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, document.Hash).Scan(&stored); err != nil || !bytes.Equal(stored, raw.Bytes()) {
				test.Fatal("published native fragment reads rewrote the original noncanonical PG source", err)
			}
		})
	})
}
