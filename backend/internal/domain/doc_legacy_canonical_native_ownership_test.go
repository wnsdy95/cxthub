package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type l04LegacyCanonicalCapacityMeasurement struct {
	text     string
	capacity int
}

type l04LegacyCanonicalCapacityObservation struct {
	context.Context
	native *legacyNativeObject
	texts  map[legacyCanonicalReplacementOwnershipText]l04LegacyCanonicalCapacityMeasurement
	err    error
}

func (observation *l04LegacyCanonicalCapacityObservation) scalar(scalar *legacyNativeScalar) {
	if scalar == nil || scalar.text == nil || scalar.previous != nil || scalar.position != scalar.end || scalar.buffered != 0 {
		return
	}
	text, capacity := scalar.text.String(), scalar.text.Cap()
	identity := legacyCanonicalReplacementOwnershipTextID(text)
	if capacity < len(text) {
		observation.err = fmt.Errorf("completed native builder capacity is smaller than its text")
		return
	}
	if observation.texts == nil {
		observation.texts = make(map[legacyCanonicalReplacementOwnershipText]l04LegacyCanonicalCapacityMeasurement)
	}
	if prior, present := observation.texts[identity]; present && prior.capacity != capacity {
		observation.err = fmt.Errorf("one native text backing has conflicting observed builder capacities")
		return
	}
	observation.texts[identity] = l04LegacyCanonicalCapacityMeasurement{text: text, capacity: capacity}
}

func (observation *l04LegacyCanonicalCapacityObservation) observe(native *legacyNativeObject) {
	if native == nil || observation.err != nil {
		return
	}
	pending := []*legacyNativeObject{native}
	for len(pending) != 0 {
		object := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		observation.scalar(object.scalar)
		if object.typedNumber != nil {
			observation.scalar(object.typedNumber.scalar)
		}
		for _, list := range object.lists {
			if list != nil {
				observation.scalar(list.scalar)
			}
		}
		for _, parser := range object.containers {
			if parser != nil {
				observation.scalar(parser.scalar)
			}
		}
		for _, child := range object.nested {
			if child != nil {
				pending = append(pending, child)
			}
		}
		for _, list := range object.blocks {
			if list != nil && list.object != nil {
				pending = append(pending, list.object)
			}
		}
		for _, list := range object.events {
			if list != nil && list.object != nil {
				pending = append(pending, list.object)
			}
		}
	}
}

func (observation *l04LegacyCanonicalCapacityObservation) Err() error {
	observation.observe(observation.native)
	return observation.Context.Err()
}

func (observation *l04LegacyCanonicalCapacityObservation) textExcess(test *testing.T, text string) int64 {
	test.Helper()
	if observation.err != nil {
		test.Fatal("actual native builder observation failed", observation.err)
	}
	measurement, present := observation.texts[legacyCanonicalReplacementOwnershipTextID(text)]
	if !present {
		if len(text) > LegacyCanonicalEncodingBytes {
			test.Fatal("retained giant text has no observed builder backing", len(text))
		}
		return 0
	}
	if measurement.text != text || measurement.capacity < len(text) {
		test.Fatal("retained text differs from its observed native allocation")
	}
	return legacyCanonicalReplacementOwnershipAllocation(int64(measurement.capacity), 1) -
		legacyCanonicalReplacementOwnershipAllocation(int64(len(text)), 1)
}

func (observation *l04LegacyCanonicalCapacityObservation) excess(test *testing.T, value reflect.Value, depth int) int64 {
	test.Helper()
	if !value.IsValid() {
		return 0
	}
	if depth > legacyNativeHeapMaxDepth {
		test.Fatal("actual native capacity oracle exceeded its finite reference depth")
	}
	var excess int64
	switch value.Kind() {
	case reflect.String:
		return observation.textExcess(test, value.String())
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			excess += observation.excess(test, value.Elem(), depth+1)
		}
	case reflect.Struct:
		for field := 0; field < value.NumField(); field++ {
			excess += observation.excess(test, value.Field(field), depth+1)
		}
	case reflect.Array, reflect.Slice:
		if value.Kind() == reflect.Slice {
			value = value.Slice(0, value.Cap())
		}
		for position := 0; position < value.Len(); position++ {
			excess += observation.excess(test, value.Index(position), depth+1)
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			excess += observation.excess(test, iterator.Key(), depth+1)
			excess += observation.excess(test, iterator.Value(), depth+1)
		}
	}
	return excess
}

func (observation *l04LegacyCanonicalCapacityObservation) eventRecords(test *testing.T, native *legacyNativeObject) {
	test.Helper()
	for index, list := range native.events {
		if list == nil {
			continue
		}
		values := native.destination.Elem().Field(native.metadata.fields[index].index).Interface().([]CIREvent)
		if list.phase != legacyNativeEventsDone || len(values) != len(list.decoded) || cap(values) != cap(list.decoded) ||
			cap(values) != 0 && unsafe.SliceData(values) != unsafe.SliceData(list.decoded) || list.records.length() > cap(values) {
			test.Fatal("completed native replacement lost its actual source backing or child records")
		}
		var payload docRangeHeaderHeapEstimate
		var excess int64
		backing := values[:cap(values)]
		for position := 0; position < list.records.length(); position++ {
			child := reflect.ValueOf(backing[position])
			minimum, err := legacyNativeValueHeap(context.Background(), child)
			storage := legacyCanonicalReplacementOwnershipAllocation(1, int64(child.Type().Size()))
			if err != nil || minimum.err != nil || minimum.bytes < storage {
				test.Fatal("original typed child storage oracle failed", position, minimum, err)
			}
			minimum.bytes -= storage
			measured := observation.excess(test, child, 0)
			minimum.include(docRangeHeaderHeapEstimate{bytes: measured})
			record := list.records.at(position)
			if minimum.err != nil || record.heap != minimum || record.excess != measured {
				test.Fatal("native child payload or excess differs from actual builder observations", position, record.heap, minimum, record.excess, measured)
			}
			payload.include(minimum)
			excess += measured
		}
		if payload.err != nil || list.payload != payload || list.excess != excess || native.backingExcess[index] != excess {
			test.Fatal("parent replacement ledger lost or duplicated measured child storage", list.payload, payload, list.excess, native.backingExcess[index], excess)
		}
	}
}

func l04LegacyCanonicalCapacityDrain(test *testing.T, fixture *legacyNativeCanonicalOwnerFixture) (CIREvent, *l04LegacyCanonicalCapacityObservation) {
	test.Helper()
	observation := &l04LegacyCanonicalCapacityObservation{Context: context.Background(), native: fixture.native.object}
	for range 256 {
		event, complete, err := fixture.step(test, observation, 0)
		if err != nil || observation.err != nil {
			test.Fatal("observed native owner fixture rejected its event", err, observation.err)
		}
		if complete {
			if !fixture.native.canonicalReady {
				test.Fatal("observed complete native event did not become canonical ready")
			}
			observation.eventRecords(test, fixture.native.object)
			return event, observation
		}
		if fixture.native.canonicalReady {
			test.Fatal("observed incomplete native event became canonical ready")
		}
	}
	test.Fatal("observed native owner fixture exhausted its finite decode allowance")
	return CIREvent{}, observation
}

type legacyCanonicalNativeHandoffObservation struct {
	context.Context
	normalizer       *LegacyNormalizationDecoder
	event            CIREvent
	fields           []docRangeHeaderHeapEstimate
	projectionExcess int64
	capacities       l04LegacyCanonicalCapacityObservation
	err              error
	captured         bool
}

func (observation *legacyCanonicalNativeHandoffObservation) withContext(ctx context.Context) context.Context {
	observation.Context = ctx
	return observation
}

func (observation *legacyCanonicalNativeHandoffObservation) Err() error {
	if observation.normalizer.nativeCache != nil {
		observation.capacities.observe(observation.normalizer.nativeCache.object)
	}
	if !observation.captured && observation.normalizer.nativeCache != nil && observation.normalizer.nativeCache.canonicalReady {
		native := observation.normalizer.nativeCache
		observation.captured = true
		observation.event = CIREvent(native.decoded)
		observation.event.fieldPresence = native.object.presence
		observation.fields = append([]docRangeHeaderHeapEstimate(nil), native.object.heaps...)
		var root map[string]any
		root, observation.err = legacyCanonicalEventJSONObject(observation.event)
		if observation.err == nil {
			observation.projectionExcess, observation.err = native.object.canonicalBackingExcessBytes(root)
		}
	}
	return observation.Context.Err()
}

func (observation *legacyCanonicalNativeHandoffObservation) check(test *testing.T, encoding *LegacyCanonicalEncoding, expectedExcess int64) {
	test.Helper()
	if !observation.captured || observation.err != nil || observation.projectionExcess != expectedExcess {
		test.Fatal("native projected ledger lost independently measured backing capacity", observation.captured, observation.err, observation.projectionExcess, expectedExcess)
	}
	if encoding.preparation == nil || encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 {
		test.Fatal("native handoff eagerly inspected children before ownership accounting")
	}
	lower, upper := legacyCanonicalNativeOwnershipBounds(test, observation.event, observation.fields, &observation.capacities)
	root, valid := encoding.root.(map[string]any)
	if !valid {
		test.Fatal("native handoff lost its projected event root")
	}
	extra := legacyCanonicalNativeOwnershipProjection(test, observation.event, root)
	legacyCanonicalNativeOwnershipLedger(test, encoding, lower, upper, extra)
	legacyCanonicalNativeOwnershipHeldEvent(test, encoding.nativeOwner, observation.event)
}

func legacyCanonicalNativeOwnershipBounds(test *testing.T, event CIREvent, fields []docRangeHeaderHeapEstimate, capacities ...*l04LegacyCanonicalCapacityObservation) (docRangeHeaderHeapEstimate, docRangeHeaderHeapEstimate) {
	test.Helper()
	lower, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(event))
	if err != nil || len(fields) != len(legacyEventNativeFields.fields) {
		test.Fatal("independent whole-event heap fixture failed", err, len(fields))
	}
	var eventStorage docRangeHeaderHeapEstimate
	eventStorage.allocation(1, int64(unsafe.Sizeof(event)))
	lower.bytes -= eventStorage.bytes
	var upper docRangeHeaderHeapEstimate
	value := reflect.ValueOf(event)
	for index, field := range legacyEventNativeFields.fields {
		minimum, err := legacyNativeValueHeap(context.Background(), value.Field(field.index))
		if err != nil {
			test.Fatal("independent native field heap failed", field.name, err)
		}
		actual := fields[index]
		if actual.bytes == 0 && actual.err == nil {
			if value.Field(field.index).IsZero() {
				continue
			}
			actual = minimum
		}
		if actual.err != nil || actual.bytes < minimum.bytes {
			test.Fatal("native field ledger lost full-capacity storage", field.name, actual, minimum)
		}
		if len(capacities) != 0 {
			measured := minimum
			measured.include(docRangeHeaderHeapEstimate{bytes: capacities[0].excess(test, value.Field(field.index), 0)})
			if measured.err != nil || actual != measured {
				test.Fatal("native field ledger differs from observed builder backing and original typed storage", field.name, actual, measured)
			}
		}
		lower.include(docRangeHeaderHeapEstimate{bytes: actual.bytes - minimum.bytes})
		upper.include(actual)
	}
	if lower.err != nil || upper.err != nil || lower.bytes > upper.bytes {
		test.Fatal("independent whole-owner bounds disagree", lower, upper)
	}
	return lower, upper
}

func legacyCanonicalNativeOwnershipProjection(test *testing.T, event CIREvent, root map[string]any) docRangeHeaderHeapEstimate {
	test.Helper()
	maps := []map[string]any{root}
	for _, name := range []string{"provider_metadata", "locked", "input"} {
		if name == "input" && event.Input != nil {
			continue
		}
		if fields, exists := root[name].(map[string]any); exists {
			maps = append(maps, fields)
		}
	}
	var extra docRangeHeaderHeapEstimate
	for _, fields := range maps {
		extra.allocation(1, 64)
		extra.allocation(int64(max(8, len(fields))), int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))+8)
		for key, value := range fields {
			extra.allocation(int64(len(key)), 1)
			if value != nil {
				extra.allocation(1, int64(reflect.TypeOf(value).Size()))
			}
		}
	}
	if extra.err != nil {
		test.Fatal("independent fixed projection metadata overflowed", extra)
	}
	return extra
}

func legacyCanonicalIndependentNumberHeap(test testing.TB, numbers *legacyCanonicalNumberValidation) docRangeHeaderHeapEstimate {
	test.Helper()
	heap := legacyCanonicalIndependentPageHeap(test, &numbers.pending)
	registry := &numbers.proven
	seen := make(map[*legacyCanonicalNumberRegistryNode]bool)
	nodes, entries := 0, 0
	var walk func(*legacyCanonicalNumberRegistryNode, int)
	walk = func(node *legacyCanonicalNumberRegistryNode, depth int) {
		if seen[node] || depth > legacyCanonicalNumberDigits {
			test.Fatal("numeric registry aliases a node or exceeds its radix depth")
		}
		seen[node] = true
		nodes++
		heap.allocation(1, int64(unsafe.Sizeof(*node)))
		if node.branched {
			if node.used != 0 || depth == legacyCanonicalNumberDigits {
				test.Fatal("numeric branch retains leaf entries or exceeds its identity width")
			}
		} else if node.used <= 0 || node.used > len(node.entries) {
			test.Fatal("numeric leaf escaped its allocated bucket capacity", node.used)
		}
		for position, entry := range node.entries {
			if position < node.used {
				entries++
			} else if entry != (legacyCanonicalNumberEntry{}) {
				test.Fatal("unused numeric bucket slot retains an identity", position)
			}
		}
		for _, child := range node.children {
			if child != nil {
				if !node.branched {
					test.Fatal("numeric leaf retains child allocations")
				}
				walk(child, depth+1)
			}
		}
	}
	if registry.root != nil {
		if registry.count <= 0 {
			test.Fatal("empty numeric registry retains node allocations")
		}
		walk(registry.root, 0)
	}
	if nodes != registry.nodes || entries != registry.count || entries != numbers.pending.length() || heap.err != nil {
		test.Fatal("independent numeric allocations differ from registry or pending metadata", nodes, registry.nodes, entries, registry.count, numbers.pending.length(), heap)
	}
	return heap
}

func legacyCanonicalNativeOwnershipLedger(test *testing.T, encoding *LegacyCanonicalEncoding, lower, upper, extra docRangeHeaderHeapEstimate) int64 {
	test.Helper()
	owner := encoding.nativeOwner
	if owner == nil {
		test.Fatal("native encoder lost its whole event owner")
	}
	if owner.heap.err != nil || owner.heap.bytes < lower.bytes || owner.heap.bytes > upper.bytes {
		test.Fatal("native owner omitted event fields or counted their backing slack twice", owner.heap, lower, upper)
	}
	wantRoot := owner.heap
	wantRoot.include(extra)
	if wantRoot.err != nil || encoding.rootHeap != wantRoot {
		test.Fatal("root heap must retain the whole owner plus only fixed projection allocations", encoding.rootHeap, wantRoot)
	}
	var metadata docRangeHeaderHeapEstimate
	metadata.allocation(1, int64(unsafe.Sizeof(*encoding)))
	metadata.allocation(1, int64(unsafe.Sizeof(*owner)))
	if preparation := encoding.preparation; preparation != nil {
		metadata.allocation(1, int64(unsafe.Sizeof(*preparation)))
		metadata.include(legacyCanonicalIndependentPageHeap(test, &preparation.frames))
		for position := 0; position < preparation.frames.length(); position++ {
			frame := preparation.frames.at(position)
			if frame.iterator != nil {
				metadata.allocation(1, int64(unsafe.Sizeof(reflect.MapIter{})))
			}
		}
	}
	singleOwners := make(map[*legacyCanonicalKeyField]bool)
	inlineOwners := make(map[*[legacyCanonicalInlineValues]legacyCanonicalKeyField]bool)
	pageOwners := make(map[*legacyCanonicalPageNode[legacyCanonicalKeyField]]bool)
	fieldStorage := func(fields *legacyCanonicalPages[legacyCanonicalKeyField]) docRangeHeaderHeapEstimate {
		if fields.single != nil {
			if singleOwners[fields.single] {
				test.Fatal("pending and active key owners share single backing")
			}
			singleOwners[fields.single] = true
		}
		if fields.inline != nil {
			if inlineOwners[fields.inline] {
				test.Fatal("pending and active key owners share inline backing")
			}
			inlineOwners[fields.inline] = true
		}
		if fields.root != nil {
			if pageOwners[fields.root] {
				test.Fatal("pending and active key owners share paged backing")
			}
			pageOwners[fields.root] = true
		}
		return legacyCanonicalIndependentPageHeap(test, fields)
	}
	var pending docRangeHeaderHeapEstimate
	for position := 0; position < encoding.mapKeys.length(); position++ {
		keys := encoding.mapKeys.at(position)
		storage := fieldStorage(&keys.fields)
		if storage.err != nil || keys.heap != storage.bytes {
			test.Fatal("pending key ledger lost its full-capacity backing", keys.heap, storage)
		}
		pending.include(storage)
	}
	if pending.err != nil || encoding.pendingKeysHeap != pending {
		test.Fatal("pending key aggregate differs from independent backing allocations", encoding.pendingKeysHeap, pending)
	}
	metadata.include(pending)
	metadata.include(legacyCanonicalIndependentPageHeap(test, &encoding.mapKeys))
	metadata.include(legacyCanonicalIndependentPageHeap(test, &encoding.frames))
	for position := 0; position < encoding.frames.length(); position++ {
		frame := encoding.frames.at(position)
		metadata.include(fieldStorage(&frame.fields))
		if frame.ownedLiteral {
			metadata.allocation(int64(len(frame.text)), 1)
		}
	}
	metadata.include(legacyCanonicalIndependentNumberHeap(test, &encoding.numbers))
	wantRetained := encoding.rootHeap
	wantRetained.include(metadata)
	retained, err := encoding.RetainedBytes()
	if err != nil || wantRetained.err != nil || retained != wantRetained.bytes {
		test.Fatal("owner, preparation, key, numeric, or output metadata is uncharged or duplicated", retained, wantRetained, err)
	}
	if lower.bytes > 16<<20 && retained <= 16<<20 {
		test.Fatal("a greater-than-16MiB owner was incorrectly reported as parkable", retained, lower)
	}
	return retained
}

func legacyCanonicalNativeOwnershipHeldEvent(test *testing.T, owner *legacyNativeCanonicalOwner, event CIREvent) {
	test.Helper()
	held := owner.event
	if !reflect.DeepEqual(held, event) || len(held.Blocks) != len(event.Blocks) || cap(held.Blocks) != cap(event.Blocks) ||
		unsafe.SliceData(held.Blocks) != unsafe.SliceData(event.Blocks) {
		test.Fatal("native owner did not retain the entire actual event, including union-omitted fields and backing slots")
	}
}

func legacyCanonicalNativeOwnershipOwnerReleased(test *testing.T, owner *legacyNativeCanonicalOwner) {
	test.Helper()
	if owner.heap != (docRangeHeaderHeapEstimate{}) || owner.seal != nil || !reflect.DeepEqual(owner.event, CIREvent{}) {
		test.Fatal("released native owner retained event references or its heap ledger")
	}
}

func legacyCanonicalNativeOwnershipReleased(test *testing.T, encoding *LegacyCanonicalEncoding, owner *legacyNativeCanonicalOwner, terminal error) {
	test.Helper()
	if encoding.nativeOwner != nil || encoding.preparation != nil {
		test.Fatal("terminal encoder retained native ownership or inspection state")
	}
	legacyCanonicalNativeOwnershipOwnerReleased(test, owner)
	assertLegacyCanonicalBlocksOwnershipReleased(test, encoding, terminal)
	if duplicate, err := newLegacyCanonicalEncodingNative(context.Background(), owner); duplicate != nil || err == nil {
		test.Fatal("a consumed native owner was accepted again", err)
	}
}

func legacyCanonicalNativeOwnershipSource(test *testing.T, raw string) (*legacyNormalizationNativeEvent, CIREvent, docRangeHeaderHeapEstimate, docRangeHeaderHeapEstimate) {
	test.Helper()
	typed := l04LegacyCanonicalTypedEvent(test, raw)
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal(err)
	}
	native.object.start(0, len(raw))
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	var frame legacyJSONValueScanner
	observation := &l04LegacyCanonicalCapacityObservation{Context: context.Background(), native: native.object}
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+64*parts+8*strings.Count(raw, "{"); stepIndex++ {
		input := &legacyNormalizationInput{ctx: observation, source: source}
		event, complete, err := native.step(observation, input, &frame)
		legacyNativeBlocksReviewCheckQuantum(test, input)
		if err != nil || input.failure != nil {
			test.Fatal("native ownership fixture failed before handoff", stepIndex, err, input.failure)
		}
		if !complete {
			continue
		}
		if !reflect.DeepEqual(event, typed) {
			test.Fatal("native owner source differs from the original typed event")
		}
		lower, upper := legacyCanonicalNativeOwnershipBounds(test, event, native.object.heaps, observation)
		observation.eventRecords(test, native.object)
		for index, list := range native.object.blocks {
			if list == nil {
				continue
			}
			_, total := legacyCanonicalBlocksHandoffReviewLedger(test, list)
			minimum := legacyNativeBlocksTestHeap(test, list.values())
			if native.object.heaps[index] != list.heap() || list.heap().bytes-minimum.bytes != total {
				test.Fatal("native owner source did not preserve the independent full-capacity block slack ledger")
			}
		}
		return native, event, lower, upper
	}
	test.Fatal("native ownership fixture exhausted its finite allowance")
	return nil, CIREvent{}, docRangeHeaderHeapEstimate{}, docRangeHeaderHeapEstimate{}
}

func legacyCanonicalNativeOwnershipFixture(test *testing.T, raw string) (*LegacyCanonicalEncoding, CIREvent, docRangeHeaderHeapEstimate, docRangeHeaderHeapEstimate, docRangeHeaderHeapEstimate) {
	test.Helper()
	native, event, lower, upper := legacyCanonicalNativeOwnershipSource(test, raw)
	owner, err := native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil {
		test.Fatal("native ownership factory failed", err)
	}
	encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
	if err != nil {
		owner.release()
		test.Fatal("native ownership constructor failed", err)
	}
	native.releaseValues()
	if encoding == nil || encoding.nativeOwner != owner || encoding.preparation == nil || encoding.inspectWork != 0 ||
		encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 || encoding.numbers.pending.length() != 0 || encoding.keyWork != 0 {
		test.Fatal("native constructor visited children, collected keys, or lost its initial owner")
	}
	root, ok := encoding.root.(map[string]any)
	if !ok {
		test.Fatal("native constructor lost its fixed event projection")
	}
	extra := legacyCanonicalNativeOwnershipProjection(test, event, root)
	legacyCanonicalNativeOwnershipLedger(test, encoding, lower, upper, extra)
	legacyCanonicalNativeOwnershipHeldEvent(test, owner, event)
	return encoding, event, lower, upper, extra
}

func legacyCanonicalNativeOwnershipDrain(test *testing.T, encoding *LegacyCanonicalEncoding, lower, upper, extra docRangeHeaderHeapEstimate) ([]byte, int) {
	test.Helper()
	owner, rootHeap := encoding.nativeOwner, encoding.rootHeap
	initialOwner := *owner
	var output []byte
	preparationYields := 0
	for stepIndex := 0; stepIndex < 1<<16; stepIndex++ {
		legacyCanonicalNativeOwnershipLedger(test, encoding, lower, upper, extra)
		preparation, beforeWork := encoding.preparation, encoding.inspectWork
		chunk, complete, err := encoding.Next(context.Background())
		if err != nil || len(chunk) > 64<<10 || cap(chunk) > 64<<10 || encoding.inspectWork < beforeWork {
			test.Fatal("native encoding failed or exceeded its output/work contract", stepIndex, len(chunk), cap(chunk), err)
		}
		if preparation != nil {
			preparationYields++
			if encoding.preparation != nil && (len(chunk) != 0 || cap(chunk) != 0 || complete) || encoding.inspectWork-beforeWork == 0 ||
				encoding.inspectWork-beforeWork > 1024 || preparation.lastActions > 1024 || preparation.lastActions <= 0 ||
				preparation.lastBytes > 64<<10 || preparation.lastBytes < 0 ||
				encoding.inspectWork-beforeWork != uint64(preparation.lastActions) {
				test.Fatal("native preparation emitted output, stalled, or exceeded its action/byte quantum", len(chunk), complete, preparation.lastActions, preparation.lastBytes)
			}
		} else if encoding.inspectWork != beforeWork {
			test.Fatal("completed inspection restarted during sorting, numeric validation, or output")
		}
		output = append(output, chunk...)
		if complete {
			legacyCanonicalNativeOwnershipReleased(test, encoding, owner, nil)
			return output, preparationYields
		}
		if encoding.nativeOwner != owner || encoding.rootHeap != rootHeap || !reflect.DeepEqual(*owner, initialOwner) {
			test.Fatal("a yield released or tightened the whole native event owner")
		}
		legacyCanonicalNativeOwnershipLedger(test, encoding, lower, upper, extra)
	}
	test.Fatal("native ownership encoding exhausted its finite allowance")
	return nil, 0
}

func TestLegacyCanonicalNativeOwnershipOmittedInputAndHiddenSlackBeforeEveryYield(test *testing.T) {
	initial := legacyCanonicalBlocksHandoffReviewInitial()
	var projectionHeap docRangeHeaderHeapEstimate
	for _, size := range []int{37, (8 << 20) + 73} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			raw := `{"kind":"message","seq":7,"role":"user","blocks":` + initial +
				`,"blocks":[{}],"input":{"retained":"` + strings.Repeat("i", size) + `"}}`
			encoding, event, lower, upper, extra := legacyCanonicalNativeOwnershipFixture(test, raw)
			root := encoding.root.(map[string]any)
			if _, emitted := root["input"]; emitted || len(event.Input["retained"].(string)) != size || cap(event.Blocks) < 2 {
				test.Fatal("fixture lost union-omitted input or hidden block backing")
			}
			blocks := root["blocks"].([]ContentBlock)
			if len(blocks) != 1 || cap(blocks) != cap(event.Blocks) || unsafe.SliceData(blocks) != unsafe.SliceData(event.Blocks) ||
				blocks[:cap(blocks)][1] != event.Blocks[:cap(event.Blocks)][1] {
				test.Fatal("native handoff copied or dropped full-capacity block ownership")
			}
			if projectionHeap.bytes != 0 && extra != projectionHeap {
				test.Fatal("omitted input size changed fixed projection allocations", extra, projectionHeap)
			}
			projectionHeap = extra
			projected, err := newLegacyCanonicalEncodingPending(context.Background(), root)
			if err != nil || projected.rootHeap.bytes >= encoding.rootHeap.bytes ||
				encoding.rootHeap.bytes-projected.rootHeap.bytes < 2*int64(size) {
				test.Fatal("whole-owner retention was replaced by the smaller exact projected heap", projected, encoding.rootHeap, err)
			}
			projected.release()
			if size > 8<<20 && lower.bytes <= 16<<20 {
				test.Fatal("giant omitted input did not independently exceed the unchanged 16MiB park limit", lower)
			}
			body, yields := legacyCanonicalNativeOwnershipDrain(test, encoding, lower, upper, extra)
			if yields == 0 || string(body) != `{"blocks":[{"text":"visible","type":"text"}],"kind":"message","role":"user","seq":7}` {
				test.Fatal("whole-owner accounting changed literal visible canonical bytes", yields, len(body))
			}
		})
	}
}

func TestLegacyCanonicalNativeOwnershipDensePreparationAndMetadata(test *testing.T) {
	var fields strings.Builder
	fields.WriteByte('{')
	for index := 0; index < 2049; index++ {
		if index != 0 {
			fields.WriteByte(',')
		}
		fmt.Fprintf(&fields, `"key-%04d":{"nested":[true,null,%d,"payload"]}`, index, index)
	}
	fields.WriteByte('}')
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "nested-maps", raw: `{"kind":"tool_result","seq":7,"call_id":"call","output":` + fields.String() + `}`},
		{name: "typed-blocks", raw: `{"kind":"message","seq":7,"role":"user","blocks":[` + strings.Repeat(`{"type":"text","text":"payload"},`, 4096) + `{}]}`},
		{name: "provider-boxes", raw: `{"kind":"reasoning","seq":7,"provider_metadata":{"turn_id":"turn","create_time":123.450e-8},"locked":{"provider":"codex","scheme":"encrypted_content","blob":"blob"}}`},
		{name: "provider-pending-number", raw: `{"kind":"reasoning","seq":7,"provider_metadata":{"turn_id":"turn","create_time":1.` + strings.Repeat("7", 2*LegacyCanonicalEncodingBytes+37) + `}}`},
		{name: "nil-input-new-map", raw: `{"kind":"tool_call","seq":7,"call_id":"call","tool_name":"tool"}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			encoding, _, lower, upper, extra := legacyCanonicalNativeOwnershipFixture(test, fixture.raw)
			want, err := canonicalInputEventJSON(l04LegacyCanonicalTypedEvent(test, fixture.raw))
			if err != nil {
				test.Fatal("typed canonical byte oracle rejected ownership fixture", err)
			}
			body, yields := legacyCanonicalNativeOwnershipDrain(test, encoding, lower, upper, extra)
			if !bytes.Equal(body, want) || yields == 0 || (fixture.name == "nested-maps" || fixture.name == "typed-blocks") && yields < 2 {
				test.Fatal("native metadata or multi-yield inspection changed canonical bytes", yields, len(body), len(want))
			}
		})
	}
}

func TestLegacyCanonicalNativeOwnershipZeroAndConsumedOwners(test *testing.T) {
	for _, owner := range []*legacyNativeCanonicalOwner{nil, {}} {
		encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
		if encoding != nil || err == nil {
			test.Fatal("native constructor accepted a missing or zero owner", encoding, err)
		}
	}
	encoding, _, lower, upper, extra := legacyCanonicalNativeOwnershipFixture(test, `{"kind":"message","seq":7,"role":"user"}`)
	legacyCanonicalNativeOwnershipDrain(test, encoding, lower, upper, extra)
}

func TestLegacyCanonicalNativeOwnershipClaimFailurePreservesTheCurrentOwner(test *testing.T) {
	raw := `{"kind":"message","seq":7,"role":"user","blocks":[{"type":"text","text":"visible"}],"input":{"retained":"omitted"}}`
	encoding, _, lower, upper, extra := legacyCanonicalNativeOwnershipFixture(test, raw)
	owner := encoding.nativeOwner
	before := *owner
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{context.Background(), canceled} {
		if duplicate, err := newLegacyCanonicalEncodingNative(ctx, owner); duplicate != nil || err == nil || !reflect.DeepEqual(*owner, before) {
			test.Fatal("a rejected second constructor changed the active encoder's native owner", err)
		}
		legacyCanonicalNativeOwnershipLedger(test, encoding, lower, upper, extra)
	}
	legacyCanonicalNativeOwnershipDrain(test, encoding, lower, upper, extra)

	native, event, lower, upper := legacyCanonicalNativeOwnershipSource(test, raw)
	fresh, err := native.takeCanonicalOwner(context.Background())
	if err != nil || fresh == nil {
		test.Fatal("fresh owner factory failed", err)
	}
	native.releaseValues()
	freshBefore := *fresh
	if rejected, err := newLegacyCanonicalEncodingNative(canceled, fresh); rejected != nil || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(*fresh, freshBefore) {
		test.Fatal("pre-canceled direct constructor consumed or released its caller-owned token", err)
	}
	retried, err := newLegacyCanonicalEncodingNative(context.Background(), fresh)
	if err != nil || retried == nil || retried.inspectWork != 0 || retried.mapKeys.length() != 0 {
		test.Fatal("pre-canceled fresh owner could not be retried without eager traversal", err)
	}
	extra = legacyCanonicalNativeOwnershipProjection(test, event, retried.root.(map[string]any))
	legacyCanonicalNativeOwnershipHeldEvent(test, fresh, event)
	legacyCanonicalNativeOwnershipDrain(test, retried, lower, upper, extra)
}

func TestLegacyCanonicalNativeOwnershipConstructorFailureAfterClaimReleases(test *testing.T) {
	for _, stage := range []string{"invalid-union", "canceled-after-claim"} {
		test.Run(stage, func(test *testing.T) {
			raw := `{"kind":"unknown","seq":7,"input":{"retained":"omitted"}}`
			if stage == "canceled-after-claim" {
				raw = `{"kind":"message","seq":7,"role":"user","input":{"retained":"omitted"}}`
			}
			native, _, _, _ := legacyCanonicalNativeOwnershipSource(test, raw)
			owner, err := native.takeCanonicalOwner(context.Background())
			if err != nil || owner == nil {
				test.Fatal("constructor failure fixture did not obtain a fresh native owner", err)
			}
			ctx := context.Context(context.Background())
			if stage == "canceled-after-claim" {
				ctx = legacyCanonicalBlocksOwnershipCancellationContext{Context: ctx, cancelWhen: func() bool {
					return owner.seal == nil
				}}
			}
			encoding, err := newLegacyCanonicalEncodingNative(ctx, owner)
			if encoding != nil || err == nil || stage == "canceled-after-claim" && !errors.Is(err, context.Canceled) {
				test.Fatal("native constructor did not fail atomically after its successful claim", stage, err)
			}
			legacyCanonicalNativeOwnershipOwnerReleased(test, owner)
			native.releaseValues()
		})
	}
}

func TestLegacyCanonicalNativeOwnershipCancellationAndErrorsReleaseEverything(test *testing.T) {
	raw := `{"kind":"message","seq":7,"role":"user","blocks":[` +
		strings.Repeat(`{"type":"text","text":"payload"},`, 2048) +
		`{"type":"text","text":"` + strings.Repeat("x", 2*LegacyCanonicalEncodingBytes+73) + `"}],"input":{"retained":"omitted"}}`
	for _, stage := range []string{"before-first", "during-preparation", "after-preparation-yield", "after-output-yield", "unsupported-child", "invalid-number"} {
		test.Run(stage, func(test *testing.T) {
			encoding, _, lower, upper, extra := legacyCanonicalNativeOwnershipFixture(test, raw)
			owner := encoding.nativeOwner
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var terminal error = context.Canceled
			switch stage {
			case "before-first":
				cancel()
			case "unsupported-child":
				encoding.root.(map[string]any)["role"] = struct{}{}
				terminal = ErrValidation
			case "invalid-number":
				encoding.root.(map[string]any)["role"] = json.Number("1e+")
				terminal = ErrValidation
			}
			active := context.Context(ctx)
			if stage == "during-preparation" {
				active = legacyCanonicalBlocksOwnershipCancellationContext{Context: ctx, cancelWhen: func() bool {
					return encoding.inspectWork > 0
				}}
			}
			triggered := stage == "before-first" || stage == "during-preparation" || terminal == ErrValidation
			for stepIndex := 0; stepIndex < 1<<12; stepIndex++ {
				legacyCanonicalNativeOwnershipLedger(test, encoding, lower, upper, extra)
				preparing := encoding.preparation != nil
				chunk, complete, err := encoding.Next(active)
				if err != nil {
					if !triggered || !errors.Is(err, terminal) || complete || len(chunk) != 0 || cap(chunk) != 0 {
						test.Fatal("native ownership failure exposed output or changed precedence", stage, err, complete, len(chunk))
					}
					legacyCanonicalNativeOwnershipReleased(test, encoding, owner, terminal)
					return
				}
				if complete || len(chunk) > 64<<10 || cap(chunk) > 64<<10 || preparing && encoding.preparation != nil && len(chunk) != 0 {
					test.Fatal("native failure fixture completed early or escaped its quantum", stage, complete, len(chunk))
				}
				if stage == "after-preparation-yield" && preparing || stage == "after-output-yield" && len(chunk) != 0 {
					triggered = true
					cancel()
				}
			}
			test.Fatal("native ownership failure exhausted its finite allowance", stage)
		})
	}
}
