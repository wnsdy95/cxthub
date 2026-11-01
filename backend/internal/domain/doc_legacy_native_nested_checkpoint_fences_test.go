package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

const legacyNativeNestedW83FenceBudget int64 = 64 << 20
const legacyNativeNestedW83FenceSteps = 65536

func legacyNativeNestedW83FenceReady[Value any](test *testing.T, work legacyNativeBlockRecordW81Work[Value], storage *legacyNativeBlockRecordW81Store) int64 {
	test.Helper()
	for range legacyNativeNestedW83FenceSteps {
		reads, writes, transferred := storage.reads, storage.writes, storage.transferred
		current, cancel := context.WithCancel(context.Background())
		ready, failure := work.step(current)
		cancel()
		if failure != nil {
			test.Fatal("nested value rejected an admitted state", failure)
		}
		if storage.reads-reads+storage.writes-writes > 4*(legacyNativeCheckpointTreeHeight+2) || storage.transferred-transferred > ConversationManifestChunkBytes {
			test.Fatal("nested value escaped the original bounded CAS quantum")
		}
		retained, failure := work.retainedBytes()
		if failure != nil || retained <= 0 || retained > legacyNativeNestedW83FenceBudget {
			test.Fatal("nested value escaped current aggregate admission", retained, failure)
		}
		if ready {
			reads, writes = storage.reads, storage.writes
			if repeated, failure := work.step(context.Background()); !repeated || failure != nil || storage.reads != reads || storage.writes != writes {
				test.Fatal("ready nested value performed IO or lost readiness", repeated, failure)
			}
			return retained
		}
	}
	test.Fatal("nested value exceeded its finite source-only fixture schedule")
	return 0
}

func legacyNativeNestedW83FenceStorage(test *testing.T) *legacyNativeBlockRecordW81Store {
	test.Helper()
	storage := legacyNativeBlockRecordW81Storage(test)
	check := func(body []byte) {
		var object map[string]json.RawMessage
		if json.Unmarshal(body, &object) != nil || object["e"] == nil {
			return
		}
		var rows []json.RawMessage
		if failure := json.Unmarshal(object["e"], &rows); failure != nil || len(rows) > legacyNativeCheckpointPageItems || len(rows) > 32 {
			test.Fatal("nested value escaped the closed metadata page bound", len(rows), failure)
		}
	}
	put, read := storage.io.Put, storage.io.Read
	storage.io.Put = func(current context.Context, hash ContentHash, body []byte) error {
		check(body)
		return put(current, hash, body)
	}
	storage.io.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		body, failure := read(current, hash, length)
		if failure == nil {
			check(body)
		}
		return body, failure
	}
	return storage
}

func legacyNativeNestedW83FenceCapture(test *testing.T, value reflect.Value, model uint8, storage *legacyNativeBlockRecordW81Store) legacyNativeCheckpointNestedValue {
	test.Helper()
	constructor, cancel := context.WithCancel(context.Background())
	work, failure := newLegacyNativeCheckpointNestedValueWriter(constructor, value, model, storage.io, legacyNativeNestedW83FenceBudget)
	cancel()
	if failure != nil {
		test.Fatal("nested value capture constructor", failure)
	}
	defer work.close()
	reads := storage.reads
	legacyNativeNestedW83FenceReady[legacyNativeCheckpointNestedValue](test, work, storage)
	saved, failure := work.take(context.Background())
	if failure != nil || saved.Model != model || saved.Version == 0 || saved.validate(model) != nil || storage.reads != reads {
		test.Fatal("nested value capture changed model, read CAS, or produced an invalid descriptor", failure)
	}
	legacyNativeAnyW78Released(test, work)
	return saved
}

func legacyNativeNestedW83FenceRestore(test *testing.T, saved legacyNativeCheckpointNestedValue, model uint8, storage *legacyNativeBlockRecordW81Store) reflect.Value {
	test.Helper()
	constructor, cancel := context.WithCancel(context.Background())
	work, failure := newLegacyNativeCheckpointNestedValueRestore(constructor, saved, model, storage.io, 0, legacyNativeNestedW83FenceBudget)
	cancel()
	if failure != nil {
		test.Fatal("nested value restore constructor", failure)
	}
	defer work.close()
	writes := storage.writes
	retained := legacyNativeNestedW83FenceReady[reflect.Value](test, work, storage)
	value, failure := work.take(context.Background())
	if failure != nil || !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() || storage.writes != writes {
		test.Fatal("nested value restore lost its owned pointer or wrote CAS", failure)
	}
	heap, failure := legacyNativeValueHeap(context.Background(), value)
	if failure != nil || heap.err != nil || heap.bytes <= 0 || retained < heap.bytes {
		test.Fatal("ready nested value omitted live pointer, struct, number, or text ownership", retained, heap, failure)
	}
	legacyNativeAnyW78Released(test, work)
	if duplicate, failure := work.take(context.Background()); !errors.Is(failure, ErrIntegrity) || duplicate.IsValid() {
		test.Fatal("nested value transferred ownership twice", failure)
	}
	return value
}

func legacyNativeNestedW83FenceTexts(value reflect.Value, model uint8) []string {
	if model == legacyNativeCheckpointLockedModel {
		locked := value.Interface().(*LockedBlob)
		return []string{string(locked.Provider), locked.Scheme, locked.Blob}
	}
	provider := value.Interface().(*ProviderMetadata)
	number := ""
	if provider.CreateTime != nil {
		number = provider.CreateTime.String()
	}
	return []string{provider.TurnID, number}
}

func TestLegacyNativeNestedCheckpointW83IndependentValues(test *testing.T) {
	empty, exact := json.Number(""), json.Number("-0.000E+03")
	precise := json.Number("9007199254740993.2300e-9999")
	giant := json.Number("1" + strings.Repeat("0", 2*LegacyCanonicalEncodingBytes+17))
	var builder strings.Builder
	builder.Grow(4096)
	builder.WriteString("short with unused source backing")
	fixtures := []struct {
		name  string
		model uint8
		value any
	}{
		{name: "locked-empty", model: legacyNativeCheckpointLockedModel, value: &LockedBlob{}},
		{name: "locked-opaque-bytes", model: legacyNativeCheckpointLockedModel, value: &LockedBlob{Provider: ProviderKind("opaque\xff"), Scheme: "\x00\xfe\"\\\n", Blob: "\ud55c🙂<&>\u2028\u2029"}},
		{name: "locked-exact-not-builder-capacity", model: legacyNativeCheckpointLockedModel, value: &LockedBlob{Blob: builder.String()}},
		{name: "locked-giant", model: legacyNativeCheckpointLockedModel, value: &LockedBlob{Provider: ProviderKind("claude"), Scheme: "signature", Blob: strings.Repeat("\ud55c🙂", 3*LegacyCanonicalEncodingBytes/7+19)}},
		{name: "provider-empty-nil-number", model: legacyNativeCheckpointProviderModel, value: &ProviderMetadata{}},
		{name: "provider-present-empty-number", model: legacyNativeCheckpointProviderModel, value: &ProviderMetadata{CreateTime: &empty}},
		{name: "provider-negative-zero-lexical", model: legacyNativeCheckpointProviderModel, value: &ProviderMetadata{TurnID: "turn", CreateTime: &exact}},
		{name: "provider-no-float64-roundtrip", model: legacyNativeCheckpointProviderModel, value: &ProviderMetadata{CreateTime: &precise}},
		{name: "provider-giant-number-and-turn-id", model: legacyNativeCheckpointProviderModel, value: &ProviderMetadata{TurnID: strings.Repeat("turn\ud55c🙂", LegacyCanonicalEncodingBytes/7+13), CreateTime: &giant}},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeNestedW83FenceStorage(test)
			original := reflect.ValueOf(fixture.value)
			texts := legacyNativeNestedW83FenceTexts(original, fixture.model)
			saved := legacyNativeNestedW83FenceCapture(test, original, fixture.model, storage)
			schema, failure := legacyNativeCheckpointModelSchema(legacyNativeCheckpointModelMetadata(fixture.model))
			if failure != nil || saved.Schema != schema || saved.Values.Nil || saved.Values.Length != len(texts) || saved.Values.Capacity != len(texts) || saved.Values.Items.Count != uint64(len(texts)) {
				test.Fatal("nested value lost its actual model schema or fixed physical shape", failure)
			}
			expectedNumber := fixture.model == legacyNativeCheckpointProviderModel && fixture.value.(*ProviderMetadata).CreateTime != nil
			if saved.NumberPresent != expectedNumber {
				test.Fatal("nested value conflated nil and present-empty number")
			}
			var page legacyNativeCheckpointListPage[legacyNativeCheckpointStringValueRow]
			if failure := legacyNativeCheckpointReadJSON(context.Background(), storage.io, saved.Values.Items.Ref, &page); failure != nil || saved.Values.Items.Height != 0 || len(page.Values) != len(texts) {
				test.Fatal("fixed nested value did not use a bounded actual string row page", failure)
			}
			for ordinal, row := range page.Values {
				if row.Capacity != len(texts[ordinal]) || row.Text.Bytes != len(texts[ordinal]) {
					test.Fatal("independent nested value persisted unused source backing capacity", ordinal)
				}
			}
			actual := legacyNativeNestedW83FenceRestore(test, saved, fixture.model, storage)
			if actual.Type() != original.Type() || actual.Pointer() == original.Pointer() || !reflect.DeepEqual(actual.Interface(), original.Interface()) || !reflect.DeepEqual(texts, legacyNativeNestedW83FenceTexts(original, fixture.model)) {
				test.Fatal("nested value changed exact opaque or lexical bytes, mutated input, or borrowed its pointer")
			}
			for ordinal, text := range legacyNativeNestedW83FenceTexts(actual, fixture.model) {
				if len(text) > 0 && unsafe.StringData(text) == unsafe.StringData(texts[ordinal]) {
					test.Fatal("nested value restore borrowed original text", ordinal)
				}
			}
			if fixture.model == legacyNativeCheckpointProviderModel && expectedNumber && actual.Interface().(*ProviderMetadata).CreateTime == fixture.value.(*ProviderMetadata).CreateTime {
				test.Fatal("nested value restore borrowed original number header")
			}
			recaptured := legacyNativeNestedW83FenceCapture(test, actual, fixture.model, storage)
			if !reflect.DeepEqual(saved, recaptured) || !reflect.DeepEqual(actual.Interface(), legacyNativeNestedW83FenceRestore(test, recaptured, fixture.model, storage).Interface()) {
				test.Fatal("immediate nested value recapture changed the canonical data descriptor")
			}
			for hash, body := range storage.objects {
				if len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes || HashContent(body) != hash {
					test.Fatal("nested value mutated immutable CAS or escaped its 64KiB body bound")
				}
			}
		})
	}
}

func TestLegacyNativeNestedCheckpointW83IndependentFences(test *testing.T) {
	storage := legacyNativeNestedW83FenceStorage(test)
	number := json.Number("-0.000E+03")
	for _, fixture := range []struct {
		name  string
		model uint8
		value any
	}{
		{name: "locked", model: legacyNativeCheckpointLockedModel, value: &LockedBlob{Provider: ProviderKind("claude"), Scheme: "signature", Blob: "opaque\ud55c🙂"}},
		{name: "provider", model: legacyNativeCheckpointProviderModel, value: &ProviderMetadata{TurnID: "turn\ud55c🙂", CreateTime: &number}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			saved := legacyNativeNestedW83FenceCapture(test, reflect.ValueOf(fixture.value), fixture.model, storage)
			for _, mutation := range []struct {
				name   string
				change func(*legacyNativeCheckpointNestedValue)
			}{
				{name: "version", change: func(value *legacyNativeCheckpointNestedValue) { value.Version = 255 }},
				{name: "model", change: func(value *legacyNativeCheckpointNestedValue) { value.Model = legacyNativeCheckpointBlockModel }},
				{name: "schema", change: func(value *legacyNativeCheckpointNestedValue) {
					value.Schema = HashContent([]byte("not the actual nested model"))
				}},
				{name: "nil-values", change: func(value *legacyNativeCheckpointNestedValue) { value.Values.Nil = true }},
				{name: "missing-field", change: func(value *legacyNativeCheckpointNestedValue) { value.Values.Length-- }},
				{name: "extra-capacity", change: func(value *legacyNativeCheckpointNestedValue) { value.Values.Capacity++ }},
				{name: "wrong-count", change: func(value *legacyNativeCheckpointNestedValue) { value.Values.Items.Count-- }},
				{name: "missing-tree", change: func(value *legacyNativeCheckpointNestedValue) { value.Values.Items = legacyNativeCheckpointTree{} }},
				{name: "tree-height", change: func(value *legacyNativeCheckpointNestedValue) {
					value.Values.Items.Height = legacyNativeCheckpointTreeHeight + 1
				}},
				{name: "tree-hash", change: func(value *legacyNativeCheckpointNestedValue) { value.Values.Items.Ref.Hash = "bad" }},
				{name: "tree-size", change: func(value *legacyNativeCheckpointNestedValue) {
					value.Values.Items.Ref.Bytes = LegacyCanonicalEncodingBytes + 1
				}},
			} {
				test.Run(mutation.name, func(test *testing.T) {
					changed := saved
					mutation.change(&changed)
					if failure := changed.validate(fixture.model); !errors.Is(failure, ErrIntegrity) {
						test.Fatal("nested descriptor validator accepted malformed shape", failure)
					}
					reads := storage.reads
					work, failure := newLegacyNativeCheckpointNestedValueRestore(context.Background(), changed, fixture.model, storage.io, 0, legacyNativeNestedW83FenceBudget)
					legacyNativeBlockRecordW81Reject[reflect.Value](test, work, failure, ErrIntegrity)
					if storage.reads != reads {
						test.Fatal("nested schema or shape fence read dependent storage")
					}
				})
			}
			for _, expected := range []uint8{0, legacyNativeCheckpointEnvelopeModel, legacyNativeCheckpointEventModel, legacyNativeCheckpointBlockModel, 255, 9 - fixture.model} {
				reads, writes := storage.reads, storage.writes
				writer, failure := newLegacyNativeCheckpointNestedValueWriter(context.Background(), reflect.ValueOf(fixture.value), expected, storage.io, legacyNativeNestedW83FenceBudget)
				legacyNativeBlockRecordW81Reject[legacyNativeCheckpointNestedValue](test, writer, failure, ErrIntegrity)
				restore, failure := newLegacyNativeCheckpointNestedValueRestore(context.Background(), saved, expected, storage.io, 0, legacyNativeNestedW83FenceBudget)
				legacyNativeBlockRecordW81Reject[reflect.Value](test, restore, failure, ErrIntegrity)
				if storage.reads != reads || storage.writes != writes {
					test.Fatal("wrong nested model performed dependent IO", expected)
				}
			}
			for _, sourceBytes := range []int{-1, MaxFinalizedDocBytes + 1} {
				reads := storage.reads
				work, failure := newLegacyNativeCheckpointNestedValueRestore(context.Background(), saved, fixture.model, storage.io, sourceBytes, legacyNativeNestedW83FenceBudget)
				legacyNativeBlockRecordW81Reject[reflect.Value](test, work, failure, ErrIntegrity)
				if storage.reads != reads {
					test.Fatal("invalid nested source extent read dependent CAS")
				}
			}
			var page legacyNativeCheckpointListPage[legacyNativeCheckpointStringValueRow]
			if failure := legacyNativeCheckpointReadJSON(context.Background(), storage.io, saved.Values.Items.Ref, &page); failure != nil || len(page.Values) != saved.Values.Length {
				test.Fatal("actual nested data rows unavailable", failure)
			}
			for _, name := range []string{"unused-capacity", "missing-row", "extra-row"} {
				test.Run(name, func(test *testing.T) {
					changedPage := page
					changedPage.Values = append([]legacyNativeCheckpointStringValueRow(nil), page.Values...)
					switch name {
					case "unused-capacity":
						changedPage.Values[0].Capacity++
					case "missing-row":
						changedPage.Values = changedPage.Values[:len(changedPage.Values)-1]
					case "extra-row":
						changedPage.Values = append(changedPage.Values, changedPage.Values[0])
					}
					ref, failure := legacyNativeCheckpointWriteJSON(context.Background(), storage.io, changedPage)
					if failure != nil {
						test.Fatal(failure)
					}
					changed := saved
					changed.Values.Items.Ref = ref
					work, failure := newLegacyNativeCheckpointNestedValueRestore(context.Background(), changed, fixture.model, storage.io, 0, legacyNativeNestedW83FenceBudget)
					legacyNativeBlockRecordW81Reject[reflect.Value](test, work, failure, ErrIntegrity)
				})
			}
			test.Run("number-presence", func(test *testing.T) {
				changed := saved
				changed.NumberPresent = !changed.NumberPresent
				reads := storage.reads
				work, failure := newLegacyNativeCheckpointNestedValueRestore(context.Background(), changed, fixture.model, storage.io, 0, legacyNativeNestedW83FenceBudget)
				legacyNativeBlockRecordW81Reject[reflect.Value](test, work, failure, ErrIntegrity)
				if fixture.model == legacyNativeCheckpointLockedModel && storage.reads != reads {
					test.Fatal("Locked accepted a number-presence flag until dependent IO")
				}
			})
			for _, name := range []string{"row-hash", "text-hash", "missing", "short-read"} {
				test.Run(name, func(test *testing.T) {
					current := storage.io
					read := current.Read
					target := saved.Values.Items.Ref.Hash
					if name == "text-hash" {
						target = page.Values[0].Text.Tree.Ref.Hash
					}
					current.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
						body, failure := read(ctx, hash, length)
						if failure != nil || hash != target {
							return body, failure
						}
						if name == "missing" {
							return nil, ErrNotFound
						}
						if name == "short-read" {
							return body[:len(body)-1], nil
						}
						body[0] ^= 1
						return body, nil
					}
					expected := ErrPreparedDocRangeObject
					if name == "missing" {
						expected = ErrNotFound
					}
					work, failure := newLegacyNativeCheckpointNestedValueRestore(context.Background(), saved, fixture.model, current, 0, legacyNativeNestedW83FenceBudget)
					legacyNativeBlockRecordW81Reject[reflect.Value](test, work, failure, expected)
				})
			}
		})
	}
	for _, value := range []reflect.Value{reflect.Value{}, reflect.ValueOf((*LockedBlob)(nil)), reflect.ValueOf(LockedBlob{}), reflect.ValueOf(&ContentBlock{}), reflect.ValueOf((*ProviderMetadata)(nil))} {
		reads, writes := storage.reads, storage.writes
		work, failure := newLegacyNativeCheckpointNestedValueWriter(context.Background(), value, legacyNativeCheckpointLockedModel, storage.io, legacyNativeNestedW83FenceBudget)
		legacyNativeBlockRecordW81Reject[legacyNativeCheckpointNestedValue](test, work, failure, ErrIntegrity)
		if storage.reads != reads || storage.writes != writes {
			test.Fatal("invalid or typed-nil nested destination performed CAS IO")
		}
	}
}

func legacyNativeNestedW83FenceClose[Value any](test *testing.T, factory func() (legacyNativeBlockRecordW81Work[Value], error), storage *legacyNativeBlockRecordW81Store) {
	test.Helper()
	for _, boundary := range []string{"constructor", "partial", "ready"} {
		test.Run("close-"+boundary, func(test *testing.T) {
			work, failure := factory()
			if failure != nil {
				test.Fatal(failure)
			}
			if boundary == "partial" {
				if _, failure := work.step(context.Background()); failure != nil {
					test.Fatal(failure)
				}
			}
			if boundary == "ready" {
				legacyNativeNestedW83FenceReady(test, work, storage)
			}
			reads, writes := storage.reads, storage.writes
			legacyNativeAnyW78Released(test, work)
			if ready, failure := work.step(context.Background()); ready || !errors.Is(failure, ErrIntegrity) {
				test.Fatal("closed nested value accepted another step", ready, failure)
			}
			var empty Value
			if value, failure := work.take(context.Background()); !errors.Is(failure, ErrIntegrity) || !reflect.DeepEqual(value, empty) || storage.reads != reads || storage.writes != writes {
				test.Fatal("closed nested value transferred state or performed IO", failure)
			}
		})
	}
}

func TestLegacyNativeNestedCheckpointW83Lifecycle(test *testing.T) {
	storage := legacyNativeNestedW83FenceStorage(test)
	number := json.Number("1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+23))
	for _, fixture := range []struct {
		name  string
		model uint8
		value any
	}{
		{name: "locked", model: legacyNativeCheckpointLockedModel, value: &LockedBlob{Blob: strings.Repeat("body\ud55c🙂", LegacyCanonicalEncodingBytes/7+29)}},
		{name: "provider", model: legacyNativeCheckpointProviderModel, value: &ProviderMetadata{TurnID: "lifecycle", CreateTime: &number}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			value := reflect.ValueOf(fixture.value)
			saved := legacyNativeNestedW83FenceCapture(test, value, fixture.model, storage)
			writer := func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeBlockRecordW81Work[legacyNativeCheckpointNestedValue], error) {
				work, failure := newLegacyNativeCheckpointNestedValueWriter(ctx, value, fixture.model, current, budget)
				if failure != nil {
					return nil, failure
				}
				return work, nil
			}
			restore := func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeBlockRecordW81Work[reflect.Value], error) {
				work, failure := newLegacyNativeCheckpointNestedValueRestore(ctx, saved, fixture.model, current, 0, budget)
				if failure != nil {
					return nil, failure
				}
				return work, nil
			}
			test.Run("writer", func(test *testing.T) {
				legacyNativeBlockRecordW81Lifecycle(test, storage, writer)
				legacyNativeNestedW83FenceClose(test, func() (legacyNativeBlockRecordW81Work[legacyNativeCheckpointNestedValue], error) {
					return writer(context.Background(), storage.io, legacyNativeNestedW83FenceBudget)
				}, storage)
			})
			test.Run("restore", func(test *testing.T) {
				legacyNativeBlockRecordW81Lifecycle(test, storage, restore)
				legacyNativeNestedW83FenceClose(test, func() (legacyNativeBlockRecordW81Work[reflect.Value], error) {
					return restore(context.Background(), storage.io, legacyNativeNestedW83FenceBudget)
				}, storage)
			})
			test.Run("missing-io", func(test *testing.T) {
				writerWork, failure := writer(context.Background(), legacyNativeCheckpointIO{}, legacyNativeNestedW83FenceBudget)
				legacyNativeBlockRecordW81Reject(test, writerWork, failure, ErrIntegrity)
				restoreWork, failure := restore(context.Background(), legacyNativeCheckpointIO{}, legacyNativeNestedW83FenceBudget)
				legacyNativeBlockRecordW81Reject(test, restoreWork, failure, ErrIntegrity)
			})
			test.Run("tiny-budget-before-borrowed-copy", func(test *testing.T) {
				reads, writes := storage.reads, storage.writes
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				work, failure := newLegacyNativeCheckpointNestedValueWriter(context.Background(), value, fixture.model, storage.io, 1)
				runtime.ReadMemStats(&after)
				legacyNativeBlockRecordW81Reject[legacyNativeCheckpointNestedValue](test, work, failure, ErrDocumentResourceLimit)
				if after.TotalAlloc-before.TotalAlloc > LegacyCanonicalEncodingBytes || storage.reads != reads || storage.writes != writes {
					test.Fatal("tiny-budget constructor copied borrowed payload or performed IO before aggregate admission")
				}
			})
		})
	}
}

func legacyNativeNestedW83FenceChild(native *legacyNativeObject, field int) *legacyNativeObject {
	if field < 0 || field >= len(native.nested) {
		return nil
	}
	return native.nested[field]
}

func legacyNativeNestedW83FenceDoneCut(test *testing.T, raw string, field int) (legacyNativeAnyW78Request, *legacyNativeObject, *legacyJSONValueScanner, LegacyByteSource) {
	test.Helper()
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, nil, func(native *legacyNativeObject) bool {
		return native.phase == legacyNativeObjectApply && native.field == field && native.valueFirst == '{' && !native.nestedActive
	})
	bundle := legacyNativeObjectRestored{Object: native, Scanner: *scanner, Source: source}
	stop := func() bool {
		child := legacyNativeNestedW83FenceChild(native, field)
		return native.nestedActive && child != nil && child.phase == legacyNativeObjectDone
	}
	for range legacyNativeNestedW83FenceSteps {
		current, cancel := context.WithCancel(context.Background())
		cut := &legacyNativeAnyW78CutContext{Context: current, cancel: cancel, stop: stop}
		complete, failure := legacyNativeEventW82Step(test, cut, &bundle, false, false)
		cancel()
		if stop() {
			child := legacyNativeNestedW83FenceChild(native, field)
			if complete || failure != nil && !errors.Is(failure, context.Canceled) && !errors.Is(failure, child.typeError) {
				test.Fatal("actual Done-child cut returned an unrelated failure", failure)
			}
			return request, native, &bundle.Scanner, source
		}
		if complete || failure != nil {
			test.Fatal("actual source missed its active Done-child handoff cut", complete, failure)
		}
	}
	test.Fatal("active Done-child cut exceeded its finite schedule")
	return request, native, &bundle.Scanner, source
}

func TestLegacyNativeNestedCheckpointW83EventRows(test *testing.T) {
	number := json.Number("-0.000E+03")
	lockedField := legacyEventNativeFields.match("locked")
	providerField := legacyEventNativeFields.match("provider_metadata")
	for _, fixture := range []struct {
		name    string
		raw     string
		initial *wire
		cut     string
		field   int
	}{
		{name: "absent", raw: `{}`},
		{name: "explicit-null", raw: `{"locked":null,"provider_metadata":null}`},
		{name: "empty-owned-locked-and-provider", raw: `{}`, initial: &wire{Locked: &LockedBlob{}, ProviderMetadata: &ProviderMetadata{}}},
		{name: "independent-initial-pointers", raw: `{}`, initial: &wire{Locked: &LockedBlob{Provider: ProviderKind("claude"), Scheme: "signature", Blob: "prior"}, ProviderMetadata: &ProviderMetadata{TurnID: "prior", CreateTime: &number}, fieldPresence: eventFieldLocked}},
		{name: "inactive-caches-on-different-root-key", raw: `{"locked":{"blob":"kept"},"provider_metadata":{"create_time":-0.000E+03},"id":"later"}`},
		{name: "null-retires-caches", raw: `{"locked":{"blob":"discard"},"provider_metadata":{"create_time":1},"locked":null,"provider_metadata":null}`},
		{name: "pre-dispatch", raw: `{"locked":{"blob":"new"}}`, cut: "dispatch", field: lockedField},
		{name: "active-uninitialized-parent", raw: `{"provider_metadata":{"turn_id":"new","create_time":1}}`, cut: "active", field: providerField},
		{name: "active-done-before-parent-handoff", raw: `{"locked":{"blob":"new"},"id":"later"}`, cut: "done", field: lockedField},
		{name: "active-done-remembered-type-error", raw: `{"provider_metadata":{"turn_id":false,"create_time":1},"id":"later"}`, cut: "done", field: providerField},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var request legacyNativeAnyW78Request
			var native *legacyNativeObject
			var scanner *legacyJSONValueScanner
			var source LegacyByteSource
			if fixture.cut == "done" {
				request, native, scanner, source = legacyNativeNestedW83FenceDoneCut(test, fixture.raw, fixture.field)
			} else {
				var stop func(*legacyNativeObject) bool
				if fixture.cut != "" {
					stop = func(native *legacyNativeObject) bool {
						if fixture.cut == "active" {
							return native.nestedActive && legacyNativeNestedW83FenceChild(native, fixture.field) != nil
						}
						return native.phase == legacyNativeObjectApply && native.field == fixture.field && native.valueFirst == '{' && !native.nestedActive
					}
				}
				request, native, scanner, source = legacyNativeEventW82FencePrepare(test, fixture.raw, fixture.initial, stop)
			}
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
			var state legacyNativeObjectCheckpointControl
			if failure := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); failure != nil || state.Model != legacyNativeCheckpointEventModel || state.Version != 3 || state.NestedActive != native.nestedActive || state.HasNestedCache != (native.nested != nil) {
				test.Fatal("actual Event lost its nested controller/cache flags", failure)
			}
			if fixture.name == "absent" || fixture.name == "explicit-null" {
				body, failure := json.Marshal(state)
				if failure != nil || bytes.Contains(body, []byte(`"nested":`)) || bytes.Contains(body, []byte(`"nested_active":`)) || bytes.Contains(body, []byte(`"has_nested_cache":`)) {
					test.Fatal("version-three nonnested Event bytes acquired an optional nested extension", failure)
				}
			}
			original := native.destination.Interface().(*wire)
			for _, member := range []struct {
				field int
				model uint8
			}{
				{field: lockedField, model: legacyNativeCheckpointLockedModel},
				{field: providerField, model: legacyNativeCheckpointProviderModel},
			} {
				row := state.Fields[member.field]
				value := native.destination.Elem().Field(native.metadata.fields[member.field].index)
				child := legacyNativeNestedW83FenceChild(native, member.field)
				if row.Present != !value.IsNil() || row.Initialized != (native.heaps[member.field].bytes != 0) || row.Nested == nil && (!value.IsNil() || child != nil) {
					test.Fatal("actual nested row lost separate destination and initialization state", member.field)
				}
				if row.Nested == nil {
					continue
				}
				if failure := row.Nested.validate(member.model, row.Present, row.Initialized); failure != nil || row.Nested.Model != member.model || (row.Nested.Parser != (legacyNativeCheckpointRef{})) != (child != nil) || (row.Nested.Value != nil) != (!value.IsNil() && child == nil) {
					test.Fatal("actual nested row lost its nil/independent/cache distinction", member.field, failure)
				}
				current := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
				rawReads := 0
				current.Read = func(context.Context, int, int) ([]byte, error) {
					rawReads++
					return nil, ErrConflict
				}
				constructor, cancel := context.WithCancel(context.Background())
				work, failure := newLegacyNativeObjectNestedCheckpointRestore(constructor, *row.Nested, row.Initialized, row.Present, request.Binding, current, storage.io(), legacyNativeNestedW83FenceBudget)
				cancel()
				if failure != nil {
					test.Fatal("actual nested field restore constructor", failure)
				}
				defer work.close()
				legacyNativeEventW82Drain(test, work, storage)
				retained, heapFailure := work.retainedBytes()
				restored, failure := work.take(context.Background())
				if failure != nil || heapFailure != nil || retained <= 0 || restored.Source.authority != current.authority || rawReads != 0 || restored.Heap.err != nil || restored.Heap.bytes < 0 || restored.Excess < 0 || !row.Initialized && restored.Heap.bytes != 0 || (restored.Parser != nil) != (child != nil) || !restored.Value.IsValid() || !reflect.DeepEqual(value.Interface(), restored.Value.Interface()) {
					test.Fatal("actual nested restore lost current authority, owned value, or initialized heap", failure, rawReads)
				}
				if child != nil {
					active := native.nestedActive && native.field == member.field
					beforeScanner, beforeFailure := scanner.checkpoint(context.Background())
					afterScanner, afterFailure := restored.Scanner.checkpoint(context.Background())
					if beforeFailure != nil || afterFailure != nil || restored.Parser.destination.Pointer() != restored.Value.Pointer() || restored.Parser.phase != child.phase || restored.Parser.metadata != legacyNativeCheckpointModelMetadata(member.model) || restored.Parser.parentDepth != child.parentDepth || restored.Parser.prefix != child.prefix || restored.Parser.trackPresence != child.trackPresence || restored.Parser.presence != child.presence || !reflect.DeepEqual(restored.Parser.typeError, child.typeError) || active && !bytes.Equal(beforeScanner, afterScanner) || !active && restored.Scanner != (legacyJSONValueScanner{}) {
						test.Fatal("nested parser lost pointer alias, Done/error state, or active/shared versus inactive/zero framing")
					}
					childRetained, failure := restored.Parser.retained()
					owned := docRangeHeaderHeapEstimate{bytes: childRetained, err: failure}
					owned.allocation(1, int64(restored.Value.Type().Size()))
					owned.allocation(1, int64(restored.Value.Elem().Type().Size()))
					if owned.err != nil || retained < owned.bytes {
						test.Fatal("nested restore omitted owned child storage before take, including an uninitialized parent", retained, owned)
					}
				} else if row.Initialized {
					heap, failure := legacyNativeValueHeap(context.Background(), restored.Value)
					if failure != nil || heap.err != nil || restored.Heap.bytes != heap.bytes || restored.Excess != 0 {
						test.Fatal("independent nested field did not recompute exact current ownership", failure, restored.Heap, heap)
					}
				}
				legacyNativeAnyW78Released(test, work)
			}
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			actual := restored.Object.destination.Interface().(*wire)
			if !reflect.DeepEqual(original, actual) || restored.Object.presence != native.presence || actual.fieldPresence != original.fieldPresence || restored.Object.nestedActive != native.nestedActive || original.Locked != nil && original.Locked == actual.Locked || original.ProviderMetadata != nil && original.ProviderMetadata == actual.ProviderMetadata {
				test.Fatal("Event restore changed nested destination or distinct root presence")
			}
			for _, field := range []int{lockedField, providerField} {
				child := legacyNativeNestedW83FenceChild(restored.Object, field)
				if child != nil && child.destination.Pointer() != restored.Object.destination.Elem().Field(restored.Object.metadata.fields[field].index).Pointer() {
					test.Fatal("root restore detached nested cache from its destination pointer")
				}
			}
			request = legacyNativeEventW82FenceCapture(test, restored.Object, &restored.Scanner, request, restored.Source, storage)
			recaptured := legacyNativeEventW82FenceRestore(test, request, storage)
			if !reflect.DeepEqual(actual, recaptured.Object.destination.Interface()) || recaptured.Scanner != restored.Scanner || recaptured.Object.presence != restored.Object.presence {
				test.Fatal("immediate nested Event recapture lost actual accepted state")
			}
			if fixture.cut == "done" {
				child := legacyNativeNestedW83FenceChild(recaptured.Object, fixture.field)
				if child == nil || child.phase != legacyNativeObjectDone || !recaptured.Object.nestedActive {
					test.Fatal("active Done child was discarded or silently handed off during capture")
				}
			}
			_, continuationFailure := legacyNativeEventW82Continue(test, &recaptured)
			if fixture.name == "active-done-remembered-type-error" {
				child := legacyNativeNestedW83FenceChild(recaptured.Object, fixture.field)
				var remembered *json.UnmarshalTypeError
				if child == nil || !errors.As(continuationFailure, &remembered) || !reflect.DeepEqual(remembered, child.typeError) {
					test.Fatal("active Done child with remembered type error became empty success", continuationFailure)
				}
			} else {
				expected := legacyNativeEventW82FenceCopy(fixture.initial)
				var fields map[string]json.RawMessage
				if failure := json.Unmarshal([]byte(fixture.raw), expected); failure != nil {
					test.Fatal("independent full Event oracle rejected a success fixture", failure)
				}
				if failure := json.Unmarshal([]byte(fixture.raw), &fields); failure != nil || continuationFailure != nil || !reflect.DeepEqual(expected, recaptured.Object.destination.Interface()) || recaptured.Object.presence != eventPresence(fields) {
					test.Fatal("continued nested Event differs from the full-model oracle or exact root presence", failure, continuationFailure)
				}
			}
			body, failure := storage.io().Read(context.Background(), request.Root.Hash, request.Root.Bytes)
			if failure != nil {
				test.Fatal(failure)
			}
			for _, name := range []string{"presence", "wrong-model", "scalar-nested", "missing-payload", "dual-payload", "cache-flag", "active-phase", "active-range", "no-authority", "source-size", "source-part", "source-read", "binding"} {
				test.Run("reject-"+name, func(test *testing.T) {
					var changed legacyNativeObjectCheckpointControl
					if failure := json.Unmarshal(body, &changed); failure != nil {
						test.Fatal(failure)
					}
					field := lockedField
					if changed.Fields[field].Nested == nil || !changed.Fields[field].Present {
						field = providerField
					}
					row := &changed.Fields[field]
					current, binding := source, request.Binding
					switch name {
					case "presence":
						row.Present = !row.Present
					case "wrong-model", "missing-payload", "dual-payload":
						if row.Nested == nil || !row.Present {
							return
						}
						if name == "wrong-model" {
							row.Nested.Model = legacyNativeCheckpointBlockModel
						} else if name == "missing-payload" {
							row.Nested.Value, row.Nested.Parser = nil, legacyNativeCheckpointRef{}
						} else if row.Nested.Value != nil {
							row.Nested.Parser = request.Root
						} else {
							row.Nested.Value = &legacyNativeCheckpointNestedValue{Model: row.Nested.Model}
						}
					case "scalar-nested":
						changed.Fields[legacyEventNativeFields.match("id")].Nested = &legacyNativeObjectCheckpointNestedField{Model: legacyNativeCheckpointLockedModel}
					case "cache-flag":
						hasParser := false
						for _, currentRow := range changed.Fields {
							hasParser = hasParser || currentRow.Nested != nil && currentRow.Nested.Parser != (legacyNativeCheckpointRef{})
						}
						if !hasParser {
							return
						}
						changed.HasNestedCache = false
					case "active-phase", "active-range":
						if !changed.NestedActive {
							return
						}
						if name == "active-phase" {
							changed.Phase = legacyNativeObjectDone
						} else {
							changed.Position++
						}
					case "no-authority":
						current.authority = 0
					case "source-size":
						current.Bytes--
					case "source-part":
						current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
						current.Parts[0].Bytes--
					case "source-read":
						current.Read = nil
					case "binding":
						binding = HashContent([]byte("not this actual Event"))
					}
					ref, failure := legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), changed)
					if failure != nil {
						test.Fatal(failure)
					}
					currentIO := storage.io()
					read := currentIO.Read
					dependentReads := 0
					currentIO.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
						if hash != ref.Hash {
							dependentReads++
							return nil, ErrConflict
						}
						return read(ctx, hash, length)
					}
					failure = legacyNativeEventW82FenceReject(test, ref, current, binding, legacyNativeCheckpointEventModel, currentIO, legacyNativeNestedW83FenceBudget)
					if !errors.Is(failure, ErrIntegrity) || dependentReads != 0 {
						test.Fatal("nested Event cross-row fence followed dependent storage", failure, dependentReads)
					}
				})
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
		})
	}
}
