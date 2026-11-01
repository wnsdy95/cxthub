package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unsafe"
)

type legacyCanonicalReplacementOwnershipText struct {
	data   *byte
	length int
}

type legacyCanonicalReplacementOwnershipSlice struct {
	model    reflect.Type
	data     uintptr
	length   int
	capacity int
}

type legacyCanonicalReplacementOwnershipLedger struct {
	sourceBytes     int64
	sourceExcess    int64
	capacities      *l04LegacyCanonicalCapacityObservation
	projectionBytes int64
	sourceMaps      map[uintptr]bool
	sourceSlices    map[legacyCanonicalReplacementOwnershipSlice]bool
	sourceTexts     map[legacyCanonicalReplacementOwnershipText]bool
	projectionMaps  map[uintptr]map[string]any
	projectionKeys  map[legacyCanonicalReplacementOwnershipText]bool
	wrappers        map[*legacyCanonicalReplacement]bool
	orderingKeys    map[legacyCanonicalReplacementOwnershipText]string
	sharedMaps      map[uintptr]bool
}

func legacyCanonicalReplacementOwnershipAllocation(count, width int64) int64 {
	if count == 0 || width == 0 {
		return 0
	}
	return 64 + 2*count*width
}

func legacyCanonicalReplacementOwnershipTextID(text string) legacyCanonicalReplacementOwnershipText {
	return legacyCanonicalReplacementOwnershipText{data: unsafe.StringData(text), length: len(text)}
}

func legacyCanonicalReplacementOwnershipSliceID(value reflect.Value) legacyCanonicalReplacementOwnershipSlice {
	return legacyCanonicalReplacementOwnershipSlice{model: value.Type(), data: value.Pointer(), length: value.Len(), capacity: value.Cap()}
}

func (ledger *legacyCanonicalReplacementOwnershipLedger) source(test *testing.T, value reflect.Value, storage bool, depth int) int64 {
	test.Helper()
	if !value.IsValid() {
		return 0
	}
	if depth > 10000 {
		test.Fatal("independent replacement source ledger exceeded its finite depth")
	}
	var owned int64
	if storage {
		owned += legacyCanonicalReplacementOwnershipAllocation(1, int64(value.Type().Size()))
	}
	switch value.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
	case reflect.String:
		ledger.sourceTexts[legacyCanonicalReplacementOwnershipTextID(value.String())] = true
		owned += legacyCanonicalReplacementOwnershipAllocation(int64(value.Len()), 1)
		if ledger.capacities != nil {
			excess := ledger.capacities.textExcess(test, value.String())
			owned += excess
			ledger.sourceExcess += excess
		}
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			owned += ledger.source(test, value.Elem(), true, depth+1)
		}
	case reflect.Struct:
		for field := 0; field < value.NumField(); field++ {
			owned += ledger.source(test, value.Field(field), false, depth+1)
		}
	case reflect.Array, reflect.Slice:
		if value.Kind() == reflect.Slice {
			ledger.sourceSlices[legacyCanonicalReplacementOwnershipSliceID(value)] = true
			owned += legacyCanonicalReplacementOwnershipAllocation(int64(value.Cap()), int64(value.Type().Elem().Size()))
			value = value.Slice(0, value.Cap())
		}
		for position := 0; position < value.Len(); position++ {
			owned += ledger.source(test, value.Index(position), false, depth+1)
		}
	case reflect.Map:
		if !value.IsNil() {
			ledger.sourceMaps[uintptr(value.UnsafePointer())] = true
			owned += legacyCanonicalReplacementOwnershipAllocation(1, 64)
			owned += legacyCanonicalReplacementOwnershipAllocation(int64(max(8, value.Len())), int64(value.Type().Key().Size()+value.Type().Elem().Size())+8)
			iterator := value.MapRange()
			for iterator.Next() {
				owned += ledger.source(test, iterator.Key(), false, depth+1)
				owned += ledger.source(test, iterator.Value(), false, depth+1)
			}
		}
	default:
		test.Fatal("unexpected replacement source kind", value.Kind())
	}
	return owned
}

func legacyCanonicalReplacementOwnershipNewLedger(test *testing.T, raw string, event CIREvent, capacities *l04LegacyCanonicalCapacityObservation) *legacyCanonicalReplacementOwnershipLedger {
	test.Helper()
	ledger := &legacyCanonicalReplacementOwnershipLedger{
		capacities:     capacities,
		sourceMaps:     make(map[uintptr]bool),
		sourceSlices:   make(map[legacyCanonicalReplacementOwnershipSlice]bool),
		sourceTexts:    make(map[legacyCanonicalReplacementOwnershipText]bool),
		projectionMaps: make(map[uintptr]map[string]any),
		projectionKeys: make(map[legacyCanonicalReplacementOwnershipText]bool),
		wrappers:       make(map[*legacyCanonicalReplacement]bool),
		orderingKeys:   make(map[legacyCanonicalReplacementOwnershipText]string),
		sharedMaps:     make(map[uintptr]bool),
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		test.Fatal("independent source membership decode failed", err)
	}
	value := reflect.ValueOf(event)
	for field := 0; field < value.NumField(); field++ {
		definition := value.Type().Field(field)
		name := strings.Split(definition.Tag.Get("json"), ",")[0]
		if _, present := members[name]; definition.PkgPath == "" && present {
			ledger.sourceBytes += ledger.source(test, value.Field(field), true, 0)
		}
	}
	return ledger
}

func (ledger *legacyCanonicalReplacementOwnershipLedger) projection(test *testing.T, input any) {
	test.Helper()
	switch value := input.(type) {
	case map[string]any:
		if value == nil {
			return
		}
		identity := uintptr(reflect.ValueOf(value).UnsafePointer())
		if ledger.sourceMaps[identity] {
			ledger.sharedMaps[identity] = true
			return
		}
		if _, visited := ledger.projectionMaps[identity]; visited {
			return
		}
		ledger.projectionMaps[identity] = value
		ledger.projectionBytes += legacyCanonicalReplacementOwnershipAllocation(1, 64)
		ledger.projectionBytes += legacyCanonicalReplacementOwnershipAllocation(int64(max(8, len(value))), int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))+8)
		for key, child := range value {
			ledger.projectionKeys[legacyCanonicalReplacementOwnershipTextID(key)] = true
			ledger.projectionBytes += legacyCanonicalReplacementOwnershipAllocation(int64(len(key)), 1)
			if child != nil {
				ledger.projectionBytes += legacyCanonicalReplacementOwnershipAllocation(1, int64(reflect.TypeOf(child).Size()))
			}
			ledger.projection(test, child)
		}
	case *legacyCanonicalReplacement:
		if value == nil || value.events == nil || !ledger.sourceSlices[legacyCanonicalReplacementOwnershipSliceID(reflect.ValueOf(value.events))] {
			test.Fatal("replacement wrapper copied, tightened, or lost its native source backing")
		}
		if !ledger.wrappers[value] {
			ledger.wrappers[value] = true
			ledger.projectionBytes += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*value)))
		}
	case []any, []map[string]any, []ContentBlock:
		if !ledger.sourceSlices[legacyCanonicalReplacementOwnershipSliceID(reflect.ValueOf(value))] {
			test.Fatal("native projection introduced an unowned or copied source slice", reflect.TypeOf(value))
		}
	}
}

func (ledger *legacyCanonicalReplacementOwnershipLedger) check(test *testing.T, encoding *LegacyCanonicalEncoding) int64 {
	test.Helper()
	owner := encoding.nativeOwner
	if owner == nil {
		test.Fatal("replacement encoding lost its actual native owner")
	}
	if owner.heap.err != nil || owner.heap.bytes != ledger.sourceBytes {
		test.Fatal("native owner lost omitted fields, capacity-tail values, or counted source backing twice", ledger.sourceBytes, owner.heap, "observed builder excess", ledger.sourceExcess)
	}
	ledger.projection(test, encoding.root)
	var metadata int64
	metadata += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*encoding)))
	metadata += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*owner)))
	if preparation := encoding.preparation; preparation != nil {
		metadata += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*preparation)))
		metadata += legacyCanonicalIndependentPageHeap(test, &preparation.frames).bytes
		for position := 0; position < preparation.frames.length(); position++ {
			frame := preparation.frames.at(position)
			ledger.projection(test, frame.value)
			if frame.iterator != nil {
				metadata += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(reflect.MapIter{})))
			}
		}
	}
	singleOwners := make(map[*legacyCanonicalKeyField]bool)
	inlineOwners := make(map[*[legacyCanonicalInlineValues]legacyCanonicalKeyField]bool)
	pageOwners := make(map[*legacyCanonicalPageNode[legacyCanonicalKeyField]]bool)
	var keys []string
	fieldStorage := func(fields *legacyCanonicalPages[legacyCanonicalKeyField]) int64 {
		if fields.single != nil {
			if singleOwners[fields.single] {
				test.Fatal("active and pending replacement keys share single storage")
			}
			singleOwners[fields.single] = true
		}
		if fields.inline != nil {
			if inlineOwners[fields.inline] {
				test.Fatal("active and pending replacement keys share inline storage")
			}
			inlineOwners[fields.inline] = true
		}
		if fields.root != nil {
			if pageOwners[fields.root] {
				test.Fatal("active and pending replacement keys share paged storage")
			}
			pageOwners[fields.root] = true
		}
		storage := legacyCanonicalIndependentPageHeap(test, fields)
		for position := 0; position < fields.length(); position++ {
			field := fields.at(position)
			ledger.projection(test, field.value)
			keys = append(keys, field.key)
		}
		return storage.bytes
	}
	var pending int64
	for position := 0; position < encoding.mapKeys.length(); position++ {
		entry := encoding.mapKeys.at(position)
		storage := fieldStorage(&entry.fields)
		if entry.heap != storage {
			test.Fatal("replacement directory omitted full-capacity field storage", entry.heap, storage)
		}
		pending += storage
	}
	if encoding.pendingKeysHeap.err != nil || encoding.pendingKeysHeap.bytes != pending {
		test.Fatal("replacement pending metadata differs from independent page ownership", encoding.pendingKeysHeap, pending)
	}
	metadata += pending
	metadata += legacyCanonicalIndependentPageHeap(test, &encoding.mapKeys).bytes
	metadata += legacyCanonicalIndependentPageHeap(test, &encoding.frames).bytes
	for position := 0; position < encoding.frames.length(); position++ {
		frame := encoding.frames.at(position)
		ledger.projection(test, frame.value)
		metadata += fieldStorage(&frame.fields)
		if frame.ownedLiteral {
			metadata += legacyCanonicalReplacementOwnershipAllocation(int64(len(frame.text)), 1)
		}
	}
	metadata += legacyCanonicalIndependentNumberHeap(test, &encoding.numbers).bytes
	for _, key := range keys {
		identity := legacyCanonicalReplacementOwnershipTextID(key)
		if ledger.sourceTexts[identity] || ledger.projectionKeys[identity] {
			continue
		}
		if len(key) != 32 {
			test.Fatal("directory retained an unclassified owned key", len(key))
		}
		if _, visited := ledger.orderingKeys[identity]; !visited {
			ledger.orderingKeys[identity] = key
			ledger.projectionBytes += legacyCanonicalReplacementOwnershipAllocation(int64(len(key)), 1)
		}
	}
	wantRoot := ledger.sourceBytes + ledger.projectionBytes
	if encoding.rootHeap.err != nil || encoding.rootHeap.bytes != wantRoot {
		test.Fatal("replacement source, new wrapper/maps, or owned sort keys were omitted or charged twice", encoding.rootHeap, wantRoot, ledger.sourceBytes, ledger.projectionBytes)
	}
	want := wantRoot + metadata
	actual, err := encoding.RetainedBytes()
	if err != nil || actual != want {
		test.Fatal("replacement retained total differs from independent source/projection/metadata ledger", actual, want, err)
	}
	return actual
}

func legacyCanonicalReplacementOwnershipSnapshot(test *testing.T, event CIREvent) []string {
	test.Helper()
	var snapshot []string
	var walk func(reflect.Value, int)
	walk = func(value reflect.Value, depth int) {
		if depth > 10000 {
			test.Fatal("replacement immutability snapshot exceeded its finite depth")
		}
		snapshot = append(snapshot, value.Type().String())
		switch value.Kind() {
		case reflect.Struct:
			for field := 0; field < value.NumField(); field++ {
				walk(value.Field(field), depth+1)
			}
		case reflect.Pointer, reflect.Interface:
			if value.IsNil() {
				snapshot = append(snapshot, "nil")
			} else {
				if value.Kind() == reflect.Pointer {
					snapshot = append(snapshot, fmt.Sprint(value.Pointer()))
				}
				walk(value.Elem(), depth+1)
			}
		case reflect.Array, reflect.Slice:
			if value.Kind() == reflect.Slice {
				snapshot = append(snapshot, fmt.Sprint(value.Pointer(), ":", value.Len(), ":", value.Cap()))
				value = value.Slice(0, value.Cap())
			}
			for position := 0; position < value.Len(); position++ {
				walk(value.Index(position), depth+1)
			}
		case reflect.Map:
			snapshot = append(snapshot, fmt.Sprint(uintptr(value.UnsafePointer())))
			keys := value.MapKeys()
			sort.Slice(keys, func(left, right int) bool { return keys[left].String() < keys[right].String() })
			for _, key := range keys {
				walk(key, depth+1)
				walk(value.MapIndex(key), depth+1)
			}
		case reflect.String:
			snapshot = append(snapshot, value.String())
		case reflect.Bool:
			snapshot = append(snapshot, fmt.Sprint(value.Bool()))
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			snapshot = append(snapshot, fmt.Sprint(value.Int()))
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			snapshot = append(snapshot, fmt.Sprint(value.Uint()))
		case reflect.Float32, reflect.Float64:
			snapshot = append(snapshot, fmt.Sprint(value.Float()))
		default:
			test.Fatal("unexpected immutable source kind", value.Kind())
		}
	}
	walk(reflect.ValueOf(event), 0)
	return snapshot
}

func legacyCanonicalReplacementOwnershipRaw(hiddenBytes, visibleBytes int, invalid bool) string {
	hidden := strings.Repeat("h", hiddenBytes)
	visible := strings.Repeat("v", visibleBytes)
	message := fmt.Sprintf(`{"kind":"message","seq":6,"role":"user","blocks":[{"type":"text","text":%q},{"type":"hidden-type","text":%q}],"blocks":[null],"input":{"omitted-child":%q},"output":{"omitted-output":[true,%q]}}`, visible, hidden, hidden, hidden)
	nested := fmt.Sprintf(`{"kind":"compaction","seq":1,"replacement":[null,{"kind":"unknown-hidden-nested","input":{"hidden-nested":%q}}],"replacement":[{"kind":"message","seq":2,"role":"assistant","blocks":[{"type":"text","text":"nested-visible"}]}]}`, hidden)
	children := []string{
		message,
		`{"kind":"tool_call","seq":-3,"call_id":"call","tool_name":"tool","input":{"shared-input":[false,{"payload":"kept"}]}}`,
		`{"kind":"tool_result","seq":4,"call_id":"call","output":{"shared-output":[true,{"payload":"kept"}]},"is_error":true}`,
		`{"kind":"reasoning","seq":0,"provider_metadata":{"turn_id":"turn","create_time":123.450e-8},"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`,
		`{"kind":"tool_call","seq":2,"call_id":"nil-input","tool_name":"tool"}`,
		nested,
		fmt.Sprintf(`{"kind":"turn","seq":1,"role":"user","input":{"omitted-turn":%q}}`, hidden),
		`{"kind":"message","seq":3,"role":"assistant","agent_message":true,"blocks":[],"provider_metadata":{"turn_id":"agent","create_time":1.0},"locked":{"provider":"codex","scheme":"encrypted_content","blob":"agent-opaque"}}`,
		`{"kind":"compaction","seq":1,"replacement":[]}`,
		`{"kind":"message","seq":-3,"role":"assistant","blocks":[{"type":"text","text":"last-visible"}]}`,
	}
	if invalid {
		children[len(children)-1] = `{"kind":"unknown-visible","seq":-3}`
	}
	return fmt.Sprintf(`{"kind":"compaction","seq":7,"status":"omitted-root","blocks":[{"type":"ignored","text":"ignored-root"},{"type":"hidden-root-type","text":%q}],"blocks":[null],"replacement":[%s{"kind":"unknown-hidden-root","input":{"hidden-root":%q}},{"kind":"unknown-hidden-last","output":[%q]}],"replacement":[%s],"replacement_complete":true}`, hidden, strings.Repeat("null,", len(children)), hidden, hidden, strings.Join(children, ","))
}

func legacyCanonicalReplacementOwnershipStart(test *testing.T, hiddenBytes, visibleBytes int, invalid bool) (*LegacyCanonicalEncoding, *legacyNativeCanonicalOwner, CIREvent, *legacyCanonicalReplacementOwnershipLedger) {
	test.Helper()
	raw := legacyCanonicalReplacementOwnershipRaw(hiddenBytes, visibleBytes, invalid)
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	event, capacities := l04LegacyCanonicalCapacityDrain(test, fixture)
	if len(event.Replacement) != 10 || cap(event.Replacement) <= len(event.Replacement) ||
		len(event.Blocks) != 1 || cap(event.Blocks) < 2 ||
		event.Blocks[:cap(event.Blocks)][1].Text != strings.Repeat("h", hiddenBytes) ||
		event.Replacement[:cap(event.Replacement)][10].Kind != "unknown-hidden-root" ||
		len(event.Replacement[0].Blocks) != 1 || cap(event.Replacement[0].Blocks) < 2 ||
		len(event.Replacement[5].Replacement) != 1 || cap(event.Replacement[5].Replacement) < 2 {
		test.Fatal("actual native fixture lost omitted or duplicate-shrink capacity-tail references")
	}
	ledger := legacyCanonicalReplacementOwnershipNewLedger(test, raw, event, capacities)
	if ledger.sourceExcess <= 0 {
		test.Fatal("replacement fixture did not retain independently observed child builder capacity")
	}
	test.Logf("L04 original typed source=%d observed builder excess=%d expected whole source=%d", ledger.sourceBytes-ledger.sourceExcess, ledger.sourceExcess, ledger.sourceBytes)
	for identity := range ledger.sourceTexts {
		if measurement, observed := capacities.texts[identity]; observed && len(measurement.text) > LegacyCanonicalEncodingBytes {
			test.Logf("L04 retained actual builder len=%d cap=%d excess=%d", len(measurement.text), measurement.capacity, capacities.textExcess(test, measurement.text))
		}
	}
	before := legacyCanonicalReplacementOwnershipSnapshot(test, event)
	owner, err := fixture.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil {
		test.Fatal("actual replacement native owner handoff failed", err)
	}
	encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
	if err != nil || encoding == nil {
		owner.release()
		test.Fatal("actual replacement owner constructor failed", err)
	}
	test.Cleanup(encoding.release)
	fixture.native.reset(0, 0)
	if !reflect.ValueOf(fixture.native.decoded).IsZero() || fixture.native.canonicalReady ||
		encoding.nativeOwner != owner || encoding.preparation == nil || encoding.inspectWork != 0 ||
		encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 ||
		!reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, owner.event)) {
		test.Fatal("native transfer mutated source, retained cache values, or eagerly inspected replacements")
	}
	ledger.check(test, encoding)
	return encoding, owner, event, ledger
}

func legacyCanonicalReplacementOwnershipReleased(test *testing.T, encoding *LegacyCanonicalEncoding, owner *legacyNativeCanonicalOwner, terminal error) {
	test.Helper()
	legacyCanonicalNativeOwnershipOwnerReleased(test, owner)
	var walk func(reflect.Value)
	walk = func(value reflect.Value) {
		switch value.Kind() {
		case reflect.Struct:
			for field := 0; field < value.NumField(); field++ {
				walk(value.Field(field))
			}
		case reflect.Array:
			for position := 0; position < value.Len(); position++ {
				walk(value.Index(position))
			}
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.String, reflect.UnsafePointer:
			if !value.IsZero() {
				test.Fatal("terminal replacement encoder still reaches source, wrapper, projection, or metadata", value.Type())
			}
		}
	}
	value := reflect.ValueOf(encoding).Elem()
	for field := 0; field < value.NumField(); field++ {
		if value.Type().Field(field).Name != "err" {
			walk(value.Field(field))
		}
	}
	assertLegacyCanonicalBlocksOwnershipReleased(test, encoding, terminal)
	if duplicate, err := newLegacyCanonicalEncodingNative(context.Background(), owner); duplicate != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("terminal replacement owner was admitted again", err)
	}
}

func TestLegacyCanonicalReplacementOwnershipHiddenCapacityAndProjectionLedger(test *testing.T) {
	const initialHiddenBytes = 31
	const hiddenReferences = 8
	var firstBody []byte
	var firstSource, firstSourceExcess, firstInitial, firstPrepared, firstProjection int64
	for _, hiddenBytes := range []int{initialHiddenBytes, 4093, 2*LegacyCanonicalEncodingBytes + 73} {
		test.Run(fmt.Sprint(hiddenBytes), func(test *testing.T) {
			encoding, owner, event, ledger := legacyCanonicalReplacementOwnershipStart(test, hiddenBytes, 97, false)
			typed := l04LegacyCanonicalTypedEvent(test, legacyCanonicalReplacementOwnershipRaw(hiddenBytes, 97, false))
			if !reflect.DeepEqual(event, typed) {
				test.Fatal("replacement owner differs from the original typed input")
			}
			typed.Replacement = canonicalEvents(typed.Replacement)
			want := legacyCanonicalNativePreparationOracle(test, typed)
			before := legacyCanonicalReplacementOwnershipSnapshot(test, event)
			initial := ledger.check(test, encoding)
			var body []byte
			var prepared, projection int64
			for step := 0; step < 1<<12; step++ {
				ledger.check(test, encoding)
				preparing := encoding.preparation != nil
				chunk, complete, err := encoding.Next(context.Background())
				if err != nil {
					test.Fatal("ownership fixture failed before completion", step, err)
				}
				if preparing && (len(chunk) != 0 || cap(chunk) != 0 || complete) {
					test.Fatal("replacement ownership preparation exposed canonical output")
				}
				if !reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, event)) ||
					!complete && !reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, owner.event)) {
					test.Fatal("replacement projection, ordering, or output mutated visible or hidden source backing")
				}
				body = append(body, chunk...)
				if preparing && encoding.preparation == nil {
					prepared = ledger.check(test, encoding)
					projection = ledger.projectionBytes
					if len(ledger.wrappers) != 3 || len(ledger.orderingKeys) != 11 || len(ledger.sharedMaps) < 2 || len(ledger.projectionMaps) <= len(event.Replacement) {
						test.Fatal("independent ledger missed nested/empty wrappers, owned keys, fixed projections, or source aliases", len(ledger.wrappers), len(ledger.orderingKeys), len(ledger.sharedMaps), len(ledger.projectionMaps))
					}
				}
				if !complete {
					continue
				}
				legacyCanonicalReplacementOwnershipReleased(test, encoding, owner, nil)
				if prepared == 0 || !json.Valid(body) || bytes.Contains(body, []byte("unknown-hidden")) || bytes.Contains(body, []byte("omitted-child")) ||
					!bytes.Equal(body, want) || HashContent(body) != HashContent(want) {
					test.Fatal("hidden source retention escaped into canonical output or preparation was never observed")
				}
				if firstBody == nil {
					firstBody, firstSource, firstInitial, firstPrepared, firstProjection = body, ledger.sourceBytes, initial, prepared, projection
					firstSourceExcess = ledger.sourceExcess
				} else {
					growth := ledger.sourceBytes - firstSource
					capacityGrowth := ledger.sourceExcess - firstSourceExcess
					if growth != 2*hiddenReferences*int64(hiddenBytes-initialHiddenBytes)+capacityGrowth || initial-firstInitial != growth || prepared-firstPrepared != growth || projection != firstProjection ||
						!bytes.Equal(body, firstBody) || HashContent(body) != HashContent(firstBody) {
						test.Fatal("hidden source growth was dropped, projected twice, or changed visible bytes/hash", growth, initial-firstInitial, prepared-firstPrepared, projection, firstProjection)
					}
				}
				return
			}
			test.Fatal("replacement ownership fixture exhausted its finite allowance")
		})
	}
}

func TestLegacyCanonicalReplacementOwnershipCancellationAndFailureRelease(test *testing.T) {
	for _, stage := range []string{"before-first", "during-projection", "after-projection-yield", "after-preparation", "after-output", "invalid-child"} {
		test.Run(stage, func(test *testing.T) {
			visibleBytes := 97
			if stage == "after-output" {
				visibleBytes = (64 << 10) + 129
			}
			encoding, owner, event, ledger := legacyCanonicalReplacementOwnershipStart(test, 193, visibleBytes, stage == "invalid-child")
			before := legacyCanonicalReplacementOwnershipSnapshot(test, event)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			active := context.Context(ctx)
			triggered := stage == "before-first" || stage == "invalid-child" || stage == "during-projection"
			if stage == "before-first" {
				cancel()
			}
			if stage == "during-projection" {
				active = legacyCanonicalBlocksOwnershipCancellationContext{Context: ctx, cancelWhen: func() bool {
					for position := 0; position < encoding.mapKeys.length(); position++ {
						fields := &encoding.mapKeys.at(position).fields
						for index := 0; index < fields.length(); index++ {
							if child, ok := fields.at(index).value.(map[string]any); ok && child["kind"] != nil {
								return true
							}
						}
					}
					return false
				}}
			}
			for step := 0; step < 1<<12; step++ {
				ledger.check(test, encoding)
				preparing := encoding.preparation != nil
				chunk, complete, err := encoding.Next(active)
				if !reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, event)) {
					test.Fatal("replacement cancellation or failure mutated native capacity-tail backing")
				}
				if err != nil {
					if !triggered || complete || len(chunk) != 0 || cap(chunk) != 0 ||
						stage != "invalid-child" && !errors.Is(err, context.Canceled) ||
						stage == "invalid-child" && !strings.Contains(err.Error(), "unknown event kind") {
						test.Fatal("replacement terminal failure exposed output or missed its checkpoint", stage, complete, len(chunk), err)
					}
					legacyCanonicalReplacementOwnershipReleased(test, encoding, owner, err)
					return
				}
				if complete || preparing && (len(chunk) != 0 || cap(chunk) != 0) {
					test.Fatal("replacement terminal fixture completed before its checkpoint or emitted during preparation", stage, complete, len(chunk))
				}
				ledger.check(test, encoding)
				if stage == "after-projection-yield" && len(ledger.orderingKeys) != 0 ||
					stage == "after-preparation" && encoding.preparation == nil ||
					stage == "after-output" && len(chunk) != 0 {
					triggered = true
					cancel()
				}
			}
			test.Fatal("replacement terminal ownership fixture exhausted its finite allowance", stage)
		})
	}
}
