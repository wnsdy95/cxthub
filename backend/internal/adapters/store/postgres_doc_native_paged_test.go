//go:build postgres

package store

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docNativeEventsPGContext struct {
	context.Context
	loads int
}

func (ctx *docNativeEventsPGContext) Err() error {
	var callers [3]uintptr
	frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
	caller, _ := frames.Next()
	consumer, _ := frames.Next()
	if strings.HasSuffix(caller.Function, "domain.readLegacyByteSourcePart") &&
		strings.HasSuffix(consumer.Function, "domain.(*legacyNormalizationInput).part") {
		ctx.loads++
	}
	return ctx.Context.Err()
}

type docNativeEventsPGPagedState struct {
	values   uintptr
	root     uintptr
	single   uintptr
	inline   uintptr
	count    int64
	records  int64
	pages    int64
	nodes    int64
	depth    int64
	payload  int64
	backing  int64
	estimate int64
	borrowed int64
}

func docNativeEventsPGPagedSnapshot(test *testing.T, work *preparedLegacyCaptureWork, object, list reflect.Value, field int) docNativeEventsPGPagedState {
	test.Helper()
	values := list.FieldByName("values")
	if values.IsNil() {
		test.Fatal("active raw replacement lost its native paged values")
	}
	value := values.Elem()
	records := value.FieldByName("records")
	payload, estimate, borrowed := value.FieldByName("payload"), list.FieldByName("estimate"), list.FieldByName("borrowed")
	fieldHeap := object.FieldByName("heaps").Index(field)
	state := docNativeEventsPGPagedState{values: values.Pointer(), root: records.FieldByName("root").Pointer(),
		single: records.FieldByName("single").Pointer(), inline: records.FieldByName("inline").Pointer(),
		count: value.FieldByName("count").Int(), records: records.FieldByName("count").Int(), pages: records.FieldByName("pages").Int(),
		nodes: records.FieldByName("nodes").Int(), depth: records.FieldByName("depth").Int(), payload: payload.FieldByName("bytes").Int(),
		estimate: estimate.FieldByName("bytes").Int(), borrowed: borrowed.FieldByName("bytes").Int()}
	if state.count != list.FieldByName("count").Int() || state.count < 0 || state.count >= 8192 || state.records < state.count || state.records > state.count+1 ||
		state.records > 8192 || state.depth < 0 || state.depth > 2 || state.payload < 0 || state.borrowed < 0 ||
		!payload.FieldByName("err").IsNil() || !estimate.FieldByName("err").IsNil() || !borrowed.FieldByName("err").IsNil() ||
		!fieldHeap.FieldByName("err").IsNil() || state.borrowed != fieldHeap.FieldByName("bytes").Int() {
		test.Fatal("active raw replacement corrupted its logical count, reserved record, payload, or borrowed field heap", state)
	}
	var payloadBytes, committed int64
	text := strings.Repeat("x", 64)
	backing := docCanonicalPagesPGRecordHeap(test, records, func(record reflect.Value, ordinal int64) {
		if ordinal >= state.count {
			return
		}
		event, heap := record.FieldByName("event"), record.FieldByName("heap")
		blocks := record.FieldByName("blocks")
		if blocks.IsNil() || blocks.Elem().Type().Name() != "legacyNativeBlockValues" || blocks.Elem().FieldByName("count").Int() != 1 {
			test.Fatal("native paged replacement lost its actual child block pages", ordinal)
		}
		blockRecords := blocks.Elem().FieldByName("records")
		if blockRecords.FieldByName("count").Int() != 1 || blockRecords.FieldByName("single").IsNil() ||
			!blockRecords.FieldByName("root").IsNil() || !blockRecords.FieldByName("inline").IsNil() {
			test.Fatal("native paged replacement child lost its actual singleton block record", ordinal)
		}
		blockBacking := docCanonicalPagesPGRecordHeap(test, blockRecords, nil)
		blockBacking.allocation(1, int64(blocks.Elem().Type().Size()))
		blockRecord := blockRecords.FieldByName("single").Elem()
		block := blockRecord.FieldByName("block")
		blockPayload, blockExcess := docNativeBlockPGRecordPayload(test, blockRecord)
		blockLedger := blocks.Elem().FieldByName("payload")
		var eventPayload rootPublicationHeapEstimate
		eventPayload.allocation(int64(event.FieldByName("Kind").Len()), 1)
		eventPayload.allocation(int64(event.FieldByName("Role").Len()), 1)
		if !record.FieldByName("replacement").IsNil() || !event.FieldByName("Replacement").IsNil() ||
			event.FieldByName("Kind").String() != string(domain.EventMessage) || event.FieldByName("Role").String() != string(domain.RoleAssistant) ||
			!event.FieldByName("Blocks").IsNil() || block.FieldByName("Type").String() != "text" || block.FieldByName("Text").String() != text ||
			blockBacking.err != nil || eventPayload.err != nil || !blockLedger.FieldByName("err").IsNil() || blockLedger.FieldByName("bytes").Int() != blockPayload ||
			!heap.FieldByName("err").IsNil() || heap.FieldByName("bytes").Int() < 0 || record.FieldByName("excess").Int() < blockExcess ||
			heap.FieldByName("bytes").Int() != eventPayload.bytes+blockBacking.bytes+blockPayload+record.FieldByName("excess").Int()-blockExcess {
			test.Fatal("native paged replacement lost its original flat child or actual singleton payload ledger", ordinal)
		}
		payloadBytes += heap.FieldByName("bytes").Int()
		committed++
	})
	backing.allocation(1, int64(value.Type().Size()))
	if state.root != 0 && (state.records <= 8 || state.pages != (state.records+63)/64) {
		test.Fatal("native raw replacement page count does not match its reserved highwater", state)
	}
	var storage rootPublicationHeapEstimate
	storage.allocation(1, int64(list.FieldByName("decoded").Type().Size()))
	state.backing = backing.bytes
	if backing.err != nil || storage.err != nil || committed != state.count || payloadBytes != state.payload ||
		state.estimate != storage.bytes+state.backing+state.payload {
		test.Fatal("native raw replacement omitted actual paged backing or committed child payload from its heap ledger", state, backing, payloadBytes, committed)
	}
	if resources, err := work.RetainedResources(); err != nil || resources.HeapBytes < state.estimate {
		test.Fatal("actual PG retained resources omitted active native paged values", resources, state, err)
	}
	return state
}
