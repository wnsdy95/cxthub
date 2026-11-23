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
	resumeObserved  bool
	afterObserved   bool
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
	resumedBeforeStep := work.resumeObserved
	work.afterObserved = false
	var err error
	work.before, err = docRangeNativeBlockPagedPGInspect(work.handle)
	if err != nil {
		return 0, err
	}
	work.beforeResources, err = work.handle.RetainedResources()
	if err != nil {
		return 0, err
	}
	stepContext := ctx
	if !work.resumeObserved {
		work.inspection = &docRangeNativeBlockPagedPGContext{Context: ctx, decode: work.decode}
		stepContext = work.inspection
	}
	progress, stepErr := work.PreparedDocRangeWork.Step(stepContext, job)
	if !resumedBeforeStep || stepErr != nil || !work.afterObserved {
		work.after, err = docRangeNativeBlockPagedPGInspect(work.handle)
		if err != nil {
			return 0, errors.Join(stepErr, err)
		}
	}
	work.afterResources, err = work.handle.RetainedResources()
	work.failure = errors.Join(stepErr, err)
	return progress, work.failure
}

func TestPGDocRangeNativeBlockPostInspectionReuse(test *testing.T) {
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
		test.Run(input.name, func(test *testing.T) {
			handle := &docRangeNativePagesPGPostInspectionHandle{preparedDocRangeWorkStub: &preparedDocRangeWorkStub{}}
			stale := docRangeNativeBlockPagedPGInspection{count: 7}
			observed := docRangeNativeBlockPagedPGInspection{count: 11}
			work := &docRangeNativeBlockPagedPGWork{
				PreparedDocRangeWork: handle, handle: handle, before: stale, after: stale,
				resumeObserved: input.resumed, afterObserved: true,
			}
			handle.step = func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
				if input.observed {
					work.after, work.afterObserved, work.resumeObserved = observed, true, true
				}
				return outbound.DocRangeWorkPending, input.failure
			}
			progress, err := work.Step(context.Background(), domain.DocRangeIndexJob{})
			var expected docRangeNativeBlockPagedPGInspection
			if input.wantReuse {
				expected = observed
			}
			if progress != outbound.DocRangeWorkPending || !errors.Is(err, input.failure) || !errors.Is(work.failure, input.failure) ||
				work.before != (docRangeNativeBlockPagedPGInspection{}) || work.after != expected || work.afterObserved != input.observed {
				test.Fatal("block observer reused stale, first-resume, or failed post-state, or lost its full before inspection", progress, err, work)
			}
			resources := outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}
			if handle.resourceChecks != 2 || work.beforeResources != resources || work.afterResources != resources {
				test.Fatal("block post-state reuse changed retained-resource observations", handle.resourceChecks, work)
			}
		})
	}
}
