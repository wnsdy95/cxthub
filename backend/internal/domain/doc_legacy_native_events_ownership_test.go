package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeEventsOwnershipObservation struct {
	context.Context
	object     *legacyNativeObject
	capacities map[legacyCanonicalReplacementOwnershipText]int
	builders   map[*legacyNativeScalar]bool
	paged      bool
}

func legacyNativeEventsOwnershipObserve(object *legacyNativeObject) *legacyNativeEventsOwnershipObservation {
	return &legacyNativeEventsOwnershipObservation{
		Context: context.Background(), object: object,
		capacities: make(map[legacyCanonicalReplacementOwnershipText]int),
		builders:   make(map[*legacyNativeScalar]bool),
	}
}

func (observation *legacyNativeEventsOwnershipObservation) Err() error {
	var scalar func(*legacyNativeScalar)
	scalar = func(value *legacyNativeScalar) {
		if value == nil {
			return
		}
		observation.builders[value] = true
		if value.text != nil && value.previous == nil && value.position == value.end && value.buffered == 0 {
			text := value.text.String()
			observation.capacities[legacyCanonicalReplacementOwnershipTextID(text)] = value.text.Cap()
		}
	}
	var walk func(*legacyNativeObject)
	walk = func(object *legacyNativeObject) {
		if object == nil {
			return
		}
		scalar(object.scalar)
		if object.typedNumber != nil {
			scalar(object.typedNumber.scalar)
			scalar(object.typedNumber.errorText)
		}
		for _, parser := range object.containers {
			if parser != nil {
				scalar(parser.scalar)
			}
		}
		for _, child := range object.nested {
			walk(child)
		}
		for _, list := range object.blocks {
			if list != nil {
				walk(list.object)
			}
		}
		for _, list := range object.events {
			if list != nil {
				observation.paged = observation.paged || list.records.root != nil
				walk(list.object)
			}
		}
	}
	walk(observation.object)
	return observation.Context.Err()
}

func (observation *legacyNativeEventsOwnershipObservation) slack(test *testing.T, value reflect.Value, depth int) int64 {
	test.Helper()
	if !value.IsValid() {
		return 0
	}
	if depth > 10000 {
		test.Fatal("independent event ownership walk exceeded its unchanged depth fence")
	}
	var extra int64
	switch value.Kind() {
	case reflect.String:
		text := value.String()
		if capacity, found := observation.capacities[legacyCanonicalReplacementOwnershipTextID(text)]; found {
			if capacity < len(text) {
				test.Fatal("actual event builder capacity is smaller than its committed string")
			}
			extra += 2 * int64(capacity-len(text))
		}
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			extra += observation.slack(test, value.Elem(), depth+1)
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			extra += observation.slack(test, value.Field(index), depth+1)
		}
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice {
			value = value.Slice(0, value.Cap())
		}
		for index := 0; index < value.Len(); index++ {
			extra += observation.slack(test, value.Index(index), depth+1)
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			extra += observation.slack(test, iterator.Key(), depth+1)
			extra += observation.slack(test, iterator.Value(), depth+1)
		}
	}
	return extra
}

func (observation *legacyNativeEventsOwnershipObservation) source(test *testing.T, value reflect.Value, storage bool) (int64, int64) {
	test.Helper()
	ledger := &legacyCanonicalReplacementOwnershipLedger{
		sourceMaps:   make(map[uintptr]bool),
		sourceSlices: make(map[legacyCanonicalReplacementOwnershipSlice]bool),
		sourceTexts:  make(map[legacyCanonicalReplacementOwnershipText]bool),
	}
	shape := ledger.source(test, value, storage, 0)
	return shape, observation.slack(test, value, 0)
}

func legacyNativeEventsOwnershipScalar(value *legacyNativeScalar) int64 {
	if value == nil {
		return 0
	}
	owned := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*value)))
	for _, builder := range []*strings.Builder{value.text, value.previous} {
		if builder != nil {
			owned += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*builder)))
			owned += legacyCanonicalReplacementOwnershipAllocation(int64(builder.Cap()), 1)
		}
	}
	return owned
}

func (observation *legacyNativeEventsOwnershipObservation) fields(test *testing.T, object *legacyNativeObject) int64 {
	test.Helper()
	var owned int64
	for index, field := range object.metadata.fields {
		value := object.destination.Elem().Field(field.index)
		actual := object.heaps[index]
		if object.nestedActive && object.field == index && index < len(object.nested) && object.nested[index] != nil {
			continue
		}
		if actual.bytes == 0 && actual.err == nil {
			if !value.IsZero() {
				test.Fatal("nonzero child wire field has no source ownership", field.name)
			}
			continue
		}
		shape, slack := observation.source(test, value, true)
		if actual.err != nil || actual.bytes != shape+slack {
			test.Fatal("native field differs from original shape plus actual builder capacity", field.name, actual, shape, slack)
		}
		owned += shape + slack
	}
	return owned
}

func (observation *legacyNativeEventsOwnershipObservation) values(test *testing.T, list *legacyNativeEventList) int64 {
	test.Helper()
	var payload, added, excess int64
	for index := 0; index < list.records.length(); index++ {
		backing := list.decoded
		if list.previous != nil && index >= list.copied {
			backing = list.previous
		}
		if index >= cap(backing) {
			test.Fatal("event record has no reachable old or new backing slot", index, list.copied)
		}
		shape, slack := observation.source(test, reflect.ValueOf(backing[:cap(backing)][index]), false)
		record := list.records.at(index)
		if record == nil || record.heap.err != nil || record.heap.bytes != shape+slack || record.excess != slack {
			test.Fatal("event record lost original payload or measured builder capacity", index, record, shape, slack)
		}
		payload += shape + slack
		excess += slack
		if index < list.count {
			added += shape + slack
		}
	}
	if list.phase == legacyNativeEventsDone && !list.owned {
		added = 0
	}
	want := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(list.decoded))) +
		legacyCanonicalReplacementOwnershipAllocation(int64(cap(list.decoded)), int64(unsafe.Sizeof(CIREvent{}))) + payload
	if list.payload.err != nil || list.payload.bytes != payload || list.excess != excess ||
		list.estimate.err != nil || list.estimate.bytes != want || list.added.err != nil || list.added.bytes != added {
		test.Fatal("event payload, backing, or conservative current-pass ledger differs from the independent source shape", list.payload, payload, list.excess, excess, list.estimate, want, list.added, added)
	}
	return added
}

func (observation *legacyNativeEventsOwnershipObservation) objectBytes(test *testing.T, object *legacyNativeObject) int64 {
	test.Helper()
	if object == nil {
		return 0
	}
	if object.number != nil || object.typeError != nil || object.lists != nil || object.blocks != nil || object.containers != nil {
		test.Fatal("scoped metadata fixture unexpectedly entered a different native helper")
	}
	owned := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*object)))
	if object.mutation != nil {
		owned += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*object.mutation)))
	}
	owned += legacyCanonicalReplacementOwnershipAllocation(int64(cap(object.key)+cap(object.raw)), 1)
	model := reflect.ValueOf(object).Elem()
	for _, name := range []string{"heaps", "backingExcess", "lists", "blocks", "events", "nested", "containers"} {
		value := model.FieldByName(name)
		owned += legacyCanonicalReplacementOwnershipAllocation(int64(value.Cap()), int64(value.Type().Elem().Size()))
	}
	owned += legacyNativeEventsOwnershipScalar(object.scalar)
	if number := object.typedNumber; number != nil {
		if number.semantic != nil {
			test.Fatal("successful typed-number metadata fixture entered diagnostics")
		}
		owned += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*number)))
		if number.newTarget {
			owned += legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*number.target)))
		}
		owned += legacyNativeEventsOwnershipScalar(number.scalar) + legacyNativeEventsOwnershipScalar(number.errorText)
	}
	observation.fields(test, object)
	for index, field := range object.metadata.fields {
		if index < len(object.nested) && object.nested[index] != nil {
			child := object.nested[index]
			owned += observation.objectBytes(test, child)
			owned += legacyCanonicalReplacementOwnershipAllocation(1, int64(child.destination.Type().Size()))
			owned += legacyCanonicalReplacementOwnershipAllocation(1, int64(child.metadata.model.Size()))
		} else if object.heaps[index].bytes != 0 {
			shape, slack := observation.source(test, object.destination.Elem().Field(field.index), true)
			owned += shape + slack
		}
	}
	for _, list := range object.events {
		if list != nil {
			owned += observation.listBytes(test, list)
		}
	}
	return owned
}

func (observation *legacyNativeEventsOwnershipObservation) listBytes(test *testing.T, list *legacyNativeEventList) int64 {
	test.Helper()
	added := observation.values(test, list)
	pages := legacyCanonicalIndependentPageHeap(test, &list.records)
	owned := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*list))) + pages.bytes + added
	if list.owned {
		owned += legacyCanonicalReplacementOwnershipAllocation(int64(cap(list.decoded)), int64(unsafe.Sizeof(CIREvent{})))
	}
	if list.previousOwned {
		owned += legacyCanonicalReplacementOwnershipAllocation(int64(cap(list.previous)), int64(unsafe.Sizeof(CIREvent{})))
	}
	return owned + observation.objectBytes(test, list.object)
}

func legacyNativeEventsOwnershipCleared(test *testing.T, native *legacyNormalizationNativeEvent) {
	test.Helper()
	object := native.object
	if native.canonicalReady || !reflect.ValueOf(native.decoded).IsZero() || object.events != nil ||
		object.scalar != nil || object.number != nil || object.typedNumber != nil || object.nested != nil || object.blocks != nil ||
		object.containers != nil || object.lists != nil || object.eventActive || object.blockActive || object.nestedActive ||
		object.containerActive || object.listActive || object.presence != 0 {
		test.Fatal("released native cache still reaches event-list values or active helpers")
	}
	for index, heap := range object.heaps {
		if heap != (docRangeHeaderHeapEstimate{}) || object.backingExcess[index] != 0 {
			test.Fatal("released native cache retained event payload accounting", index, heap)
		}
	}
	observation := legacyNativeEventsOwnershipObserve(object)
	want := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*native))) + observation.objectBytes(test, object)
	actual, err := native.retained()
	if err != nil || actual != want {
		test.Fatal("released native cache charges more than its independent reusable metadata", actual, want, err)
	}
}

func legacyNativeEventsOwnershipNestedRaw() string {
	giant := strings.Repeat("x", 2*ConversationManifestChunkBytes+73)
	number := "1." + strings.Repeat("7", 2*LegacyCanonicalEncodingBytes+37)
	child := `{"kind":"turn","id":"` + giant + `","provider_metadata":{"turn_id":"native","create_time":"` + number + `"}}`
	nested := `{"kind":"compaction","replacement":[` + child + `,` + strings.Repeat(`{"kind":"turn","id":"page"},`, 128) + `{}]}`
	return `{"kind":"compaction","replacement":[{"kind":"turn","id":"head"},` + nested + `],"replacement_complete":true}`
}

func TestLegacyNativeEventsOwnershipPartialAndCompleteIndependentLedger(test *testing.T) {
	raw := legacyNativeEventsOwnershipNestedRaw()
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	observation := legacyNativeEventsOwnershipObserve(fixture.native.object)
	var expected CIREvent
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal(err)
	}
	sawCursor, sawPartial := false, false
	for quantum := 0; quantum < 1024; quantum++ {
		event, complete, err := fixture.step(test, observation, 0)
		if err != nil {
			test.Fatal("actual nested event-list driver failed", quantum, err)
		}
		want := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*fixture.native))) + observation.objectBytes(test, fixture.native.object)
		actual, retainedErr := fixture.native.retained()
		if retainedErr != nil || actual != want {
			test.Fatal("native retention differs from independent value, fixed-page, wire, and builder allocations", quantum, actual, want, retainedErr)
		}
		index := legacyEventNativeFields.match("replacement")
		if len(fixture.native.object.events) != 0 && fixture.native.object.events[index] != nil {
			list := fixture.native.object.events[index]
			sawPartial = sawPartial || !complete && list.count > 0
			sawCursor = sawCursor || list.cursor != nil && list.cursor != list && list.cursor.parent != nil
		}
		if !complete {
			legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
			continue
		}
		if !sawPartial || !sawCursor || !observation.paged || !reflect.DeepEqual(event, expected) || !bytes.Equal(fixture.buffer.body, []byte(raw)) {
			test.Fatal("fixture missed a real parent/cursor yield, fixed pages, or changed native values/source", sawPartial, sawCursor, observation.paged)
		}
		capacity := observation.capacities[legacyCanonicalReplacementOwnershipTextID(event.Replacement[1].Replacement[0].ID)]
		number := event.Replacement[1].Replacement[0].ProviderMetadata.CreateTime
		numberCapacity := observation.capacities[legacyCanonicalReplacementOwnershipTextID(string(*number))]
		if capacity <= len(event.Replacement[1].Replacement[0].ID) || numberCapacity <= len(*number) || len(observation.builders) < 2 {
			test.Fatal("child scalar/typed-number excess has no actual builder-capacity witness", capacity, numberCapacity)
		}
		fixture.native.releaseValues()
		legacyNativeEventsOwnershipCleared(test, fixture.native)
		return
	}
	test.Fatal("independent ownership fixture exhausted its finite driver allowance")
}

func TestLegacyNativeEventsOwnershipGrowthKeepsBothBackingsButOnePayload(test *testing.T) {
	raw := `[{"id":"first"},` + strings.Repeat(`{},`, 128) + `{}]` + strings.Repeat(" ", 1024)
	list, err := newLegacyNativeEventList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	var frame legacyJSONValueScanner
	list.frame = &frame
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := buffer.input()
	observation := legacyNativeEventsOwnershipObserve(nil)
	limit := LegacyNormalizationStepEvents * len(legacyEventNativeFields.fields)
	for target := 1; target <= 128; target++ {
		remaining := 1
		if target == 1 {
			remaining = 2
		}
		input := &legacyNormalizationInput{ctx: context.Background(), source: source, fields: limit - remaining}
		complete, err := list.step(context.Background(), input)
		if err != nil || complete || list.count != target || input.fields != limit || input.eventsRunning != nil || input.eventsPending != nil ||
			input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.values > legacyNativeAnyValueActions || input.unbounded {
			test.Fatal("actual event driver did not stop at its caller-owned action boundary", target, list.count, input.fields, err)
		}
		for _, page := range input.pages {
			clear(page)
		}
	}
	width := int(unsafe.Sizeof(CIREvent{}))
	input := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - 4*width - 3}
	complete, err := list.step(context.Background(), input)
	if err != nil || complete || list.previous == nil || !list.previousOwned || !list.owned || list.copied != 1 ||
		cap(list.previous) != 128 || cap(list.decoded) != 256 || unsafe.SliceData(list.previous) == unsafe.SliceData(list.decoded) ||
		list.previous[0].ID != "first" || list.decoded[0].ID != "first" || input.eventsRunning != nil || input.eventsPending != nil {
		test.Fatal("fixture did not park actual old/new event backing migration", complete, err, list.copied)
	}
	want := observation.listBytes(test, list)
	var actual docRangeHeaderHeapEstimate
	list.retained(&actual)
	if actual.err != nil || actual.bytes != want || list.records.root == nil || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 {
		test.Fatal("growth dropped a backing, double-counted copied payload, or missed fixed record pages", actual, want)
	}
	for _, page := range input.pages {
		clear(page)
	}
	for quantum := 0; quantum < 16; quantum++ {
		input = &legacyNormalizationInput{ctx: context.Background(), source: source}
		complete, err = list.step(context.Background(), input)
		if err != nil || input.eventsRunning != nil || input.eventsPending != nil || input.workBytes > ConversationManifestChunkBytes ||
			input.loads > 2 || input.fields > limit || input.values > legacyNativeAnyValueActions || input.unbounded {
			test.Fatal("growth driver failed to resume its actual cursor", err)
		}
		want = observation.listBytes(test, list)
		actual = docRangeHeaderHeapEstimate{}
		list.retained(&actual)
		if actual.err != nil || actual.bytes != want {
			test.Fatal("resumed growth retention differs from independent allocations", actual, want)
		}
		if complete {
			if list.previous != nil || list.previousOwned || list.cursor != nil || len(list.decoded) != 130 || !bytes.Equal(buffer.body, []byte(raw)) {
				test.Fatal("completed growth retained migration state or changed source")
			}
			return
		}
	}
	test.Fatal("growth exhausted its finite resume allowance")
}

func TestLegacyNativeEventsOwnershipCanceledPartialCallKeepsResumableCursor(test *testing.T) {
	raw := legacyNativeEventsOwnershipNestedRaw()
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	observation := legacyNativeEventsOwnershipObserve(fixture.native.object)
	parked := false
	for quantum := 0; quantum < 1024; quantum++ {
		event, complete, err := fixture.step(test, observation, 0)
		if err != nil {
			test.Fatal("partial cancellation fixture failed decode", err)
		}
		index := legacyEventNativeFields.match("replacement")
		if !parked && len(fixture.native.object.events) != 0 {
			list := fixture.native.object.events[index]
			if list != nil && list.cursor != nil && list.cursor != list && list.cursor.parent != nil && list.count > 0 {
				cursor, parent, position := list.cursor, list.cursor.parent, list.cursor.position
				before, retainedErr := fixture.native.retained()
				reads := len(fixture.buffer.reads)
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				rejected, done, failure := fixture.step(test, canceled, 0)
				after, afterErr := fixture.native.retained()
				if retainedErr != nil || afterErr != nil || before != after || done || !errors.Is(failure, context.Canceled) ||
					!reflect.ValueOf(rejected).IsZero() || len(fixture.buffer.reads) != reads ||
					list.cursor != cursor || cursor.parent != parent || cursor.position != position {
					test.Fatal("pre-canceled partial call consumed values, source reads, or the actual parent/cursor", before, after, failure)
				}
				legacyNativeCanonicalOwnerReject(test, fixture.native, ErrIntegrity)
				parked = true
			}
		}
		if !complete {
			continue
		}
		var expected CIREvent
		if err := json.Unmarshal([]byte(raw), &expected); err != nil || !parked || !reflect.DeepEqual(event, expected) {
			test.Fatal("canceled partial driver did not resume the same native decode", parked, err)
		}
		owner, err := fixture.native.takeCanonicalOwner(context.Background())
		if err != nil || owner == nil {
			test.Fatal("resumed native driver could not hand off its complete value", err)
		}
		legacyNativeEventsOwnershipCleared(test, fixture.native)
		owner.release()
		legacyCanonicalNativeOwnershipOwnerReleased(test, owner)
		return
	}
	test.Fatal("partial cancellation fixture exhausted its finite allowance")
}

func TestLegacyNativeEventsOwnershipWholeListResetDropsHiddenCapacity(test *testing.T) {
	for _, reset := range []string{`[]`, `null`} {
		test.Run(reset, func(test *testing.T) {
			hidden := strings.Repeat("z", 2*LegacyCanonicalEncodingBytes+73)
			raw := `{"kind":"compaction","replacement":[{"id":"head"},{"id":"` + hidden + `"}],"replacement":[{}],"replacement":` + reset + `}`
			fixture := legacyNativeCanonicalOwnerStart(test, raw)
			observation := legacyNativeEventsOwnershipObserve(fixture.native.object)
			for quantum := 0; quantum < 256; quantum++ {
				event, complete, err := fixture.step(test, observation, 0)
				if err != nil {
					test.Fatal(err)
				}
				if !complete {
					continue
				}
				list := fixture.native.object.events[legacyEventNativeFields.match("replacement")]
				observation.values(test, list)
				observation.fields(test, fixture.native.object)
				if cap(event.Replacement) != 0 || (event.Replacement == nil) != (reset == `null`) ||
					list.records.inline != nil || list.records.root != nil || list.previous != nil || list.excess != 0 ||
					len(observation.capacities) == 0 {
					test.Fatal("whole-list reset retained hidden source values or discarded actual-builder evidence", reset)
				}
				fixture.native.releaseValues()
				legacyNativeEventsOwnershipCleared(test, fixture.native)
				return
			}
			test.Fatal("whole-list reset exhausted its finite allowance")
		})
	}
}

func TestLegacyNativeEventsOwnershipSourceShapeVersusActualBuilderSlack(test *testing.T) {
	for _, fixtureSize := range []struct {
		name    string
		hidden  int
		visible int
	}{
		{name: "original-small", hidden: 31, visible: 97},
		{name: "original-after-output", hidden: 193, visible: (64 << 10) + 129},
	} {
		test.Run(fixtureSize.name, func(test *testing.T) {
			raw := legacyCanonicalReplacementOwnershipRaw(fixtureSize.hidden, fixtureSize.visible, false)
			fixture := legacyNativeCanonicalOwnerStart(test, raw)
			observation := legacyNativeEventsOwnershipObserve(fixture.native.object)
			for quantum := 0; quantum < 1024; quantum++ {
				event, complete, err := fixture.step(test, observation, 0)
				if err != nil {
					test.Fatal("original replacement ownership fixture failed native decode", err)
				}
				if !complete {
					continue
				}
				shape := legacyCanonicalReplacementOwnershipNewLedger(test, raw, event, nil).sourceBytes
				slack := observation.slack(test, reflect.ValueOf(event), 0)
				want := observation.fields(test, fixture.native.object)
				index := legacyEventNativeFields.match("replacement")
				list := fixture.native.object.events[index]
				observation.values(test, list)
				listShape, listSlack := observation.source(test, reflect.ValueOf(event.Replacement), true)
				if listShape+listSlack != list.estimate.bytes || listSlack != list.excess || fixture.native.object.backingExcess[index] != listSlack ||
					want != shape+slack || slack <= 0 || len(event.Replacement) != 10 || list.records.length() != 12 ||
					event.Replacement[:cap(event.Replacement)][10].Kind != "unknown-hidden-root" {
					test.Fatal("hidden event tail or measured child builder provenance differs from the field/record ledger", shape, slack, want)
				}
				reads := len(fixture.buffer.reads)
				owner, err := fixture.native.takeCanonicalOwner(context.Background())
				if err != nil || owner == nil || owner.heap.err != nil || owner.heap.bytes != shape+slack || len(fixture.buffer.reads) != reads {
					test.Fatal("owner delta is not explained exactly by actual child builder capacity", shape, slack, owner, err)
				}
				legacyNativeEventsOwnershipCleared(test, fixture.native)
				owner.release()
				legacyCanonicalNativeOwnershipOwnerReleased(test, owner)
				test.Logf("original source shape=%d actual builder slack=%d owner=%d; conservative field storage is distinct from unique physical allocations", shape, slack, want)
				return
			}
			test.Fatal("source-shape decomposition exhausted its finite allowance")
		})
	}
}

func TestLegacyNativeEventsOwnershipHiddenTailHandoffAndRelease(test *testing.T) {
	for _, cancelEncoding := range []bool{false, true} {
		name := "complete"
		if cancelEncoding {
			name = "cancel-encoding"
		}
		test.Run(name, func(test *testing.T) {
			hidden := strings.Repeat("h", 2*ConversationManifestChunkBytes+73)
			raw := `{"kind":"compaction","replacement":[{"kind":"turn","id":"old"},{"kind":"turn","id":"` + hidden + `"},` + strings.Repeat(`{"kind":"turn"},`, 8) + `{}],"replacement":[{"kind":"turn","id":"fresh"}],"replacement_complete":true}`
			var typed CIREvent
			if err := json.Unmarshal([]byte(raw), &typed); err != nil {
				test.Fatal("installed typed oracle rejected hidden-tail handoff", err)
			}
			fixture := legacyNativeCanonicalOwnerStart(test, raw)
			observation := legacyNativeEventsOwnershipObserve(fixture.native.object)
			for quantum := 0; quantum < 1024; quantum++ {
				event, complete, err := fixture.step(test, observation, 0)
				if err != nil {
					test.Fatal(err)
				}
				if !complete {
					continue
				}
				list := fixture.native.object.events[legacyEventNativeFields.match("replacement")]
				observation.values(test, list)
				want := observation.fields(test, fixture.native.object)
				if !reflect.DeepEqual(event, typed) || len(event.Replacement) != 1 || cap(event.Replacement) <= 11 || list.records.length() != 11 || list.records.root == nil ||
					event.Replacement[:cap(event.Replacement)][1].ID != hidden || list.owned || list.added.bytes != 0 {
					test.Fatal("cached native replacement lost its hidden tail, records, or published ownership")
				}
				before := legacyCanonicalReplacementOwnershipSnapshot(test, event)
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				if owner, err := fixture.native.takeCanonicalOwner(canceled); owner != nil || !errors.Is(err, context.Canceled) || !fixture.native.canonicalReady {
					test.Fatal("pre-canceled handoff consumed native event ownership", err)
				}
				owner, err := fixture.native.takeCanonicalOwner(context.Background())
				if err != nil || owner == nil || owner.heap.bytes != want || unsafe.SliceData(owner.event.Replacement) != unsafe.SliceData(event.Replacement) {
					test.Fatal("native handoff copied backing or lost hidden builder capacity", err)
				}
				legacyNativeEventsOwnershipCleared(test, fixture.native)
				encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
				if err != nil {
					owner.release()
					test.Fatal(err)
				}
				test.Cleanup(encoding.release)
				fixture.native.reset(0, 0)
				if !reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, owner.event)) {
					test.Fatal("native cache reset mutated the transferred visible/hidden backing")
				}
				if cancelEncoding {
					body, complete, err := encoding.Next(canceled)
					if !errors.Is(err, context.Canceled) || complete || len(body) != 0 {
						test.Fatal("canceled encoder exposed transferred event output", complete, err)
					}
					legacyCanonicalNativeOwnershipReleased(test, encoding, owner, context.Canceled)
				} else {
					expected, err := canonicalInputEventJSON(typed)
					if err != nil {
						test.Fatal(err)
					}
					body, _ := collectLegacyCanonicalEncoding(test, encoding)
					if !bytes.Equal(body, expected) || bytes.Contains(body, []byte(hidden)) {
						test.Fatal("hidden event retention changed canonical output")
					}
					legacyCanonicalNativeOwnershipReleased(test, encoding, owner, nil)
				}
				if !reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, event)) {
					test.Fatal("owner release mutated caller-held event backing")
				}
				return
			}
			test.Fatal("hidden-tail handoff exhausted its finite allowance")
		})
	}
}

func TestLegacyNativeEventsOwnershipCachedHelperIsNotForeignSliceAuthority(test *testing.T) {
	backing := make([]CIREvent, 1, 2)
	backing[0] = CIREvent{Kind: EventTurn, ID: "foreign"}
	before := legacyCanonicalReplacementOwnershipSnapshot(test, CIREvent{Replacement: backing})
	for _, prior := range [][]CIREvent{backing, backing[:0]} {
		list, err := newLegacyNativeEventList(prior, docRangeHeaderHeapEstimate{}, 0, 2)
		if list != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("arbitrary external Go backing was mistaken for cached native event-list ownership", err)
		}
	}
	var decoded wire
	decoded.Replacement = backing
	object, err := newLegacyNativeObject(reflect.ValueOf(&decoded), &legacyEventNativeFields, 3, true)
	if err != nil {
		test.Fatal(err)
	}
	object.field = legacyEventNativeFields.match("replacement")
	if object.eventListField(legacyEventNativeFields.fields[object.field]) ||
		!reflect.DeepEqual(before, legacyCanonicalReplacementOwnershipSnapshot(test, CIREvent{Replacement: backing})) {
		test.Fatal("foreign destination silently gained native helper authority or was mutated")
	}
}

func TestLegacyNativeEventsOwnershipPartialSourceAndCancellationRelease(test *testing.T) {
	for _, stage := range []string{"source-error", "cancel-during-read"} {
		test.Run(stage, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", legacyNativeEventsOwnershipNestedRaw())
			normalizer, source := legacyNativeScalarReviewStart(test, raw)
			for quantum := 0; quantum < 1024; quantum++ {
				step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, source)
				if err != nil || step.Complete || len(step.Events) != 0 {
					test.Fatal("source lifetime fixture ended before an actual nested-driver yield", quantum, err)
				}
				if normalizer.nativeCache == nil || len(normalizer.nativeCache.object.events) == 0 {
					continue
				}
				list := normalizer.nativeCache.object.events[legacyEventNativeFields.match("replacement")]
				if list == nil || list.cursor == nil || list.cursor == list || list.cursor.parent == nil || list.count == 0 {
					continue
				}
				native := normalizer.nativeCache
				legacyNativeCanonicalOwnerReject(test, native, ErrIntegrity)
				before, err := normalizer.RetainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				normalizer.source.Read = func(context.Context, int, int) ([]byte, error) {
					calls++
					if stage == "cancel-during-read" {
						cancel()
						return nil, context.Canceled
					}
					return nil, io.ErrClosedPipe
				}
				step, err = normalizer.Step(ctx)
				legacyNativeEventZero(test, step)
				if calls == 0 || stage == "source-error" && (!errors.Is(err, io.ErrClosedPipe) || !errors.Is(err, ErrPreparedDocRangeObject)) ||
					stage == "cancel-during-read" && !errors.Is(err, context.Canceled) || normalizer.terminal == nil || normalizer.native != nil {
					test.Fatal("terminal source/cancel failure changed precedence or retained an active driver", stage, err, calls)
				}
				legacyNativeEventsOwnershipCleared(test, native)
				after, retainedErr := normalizer.RetainedBytes()
				if retainedErr != nil || after >= before || !bytes.Equal(source.body, raw) {
					test.Fatal("terminal decoder retained partial payload or changed authenticated source", before, after, retainedErr)
				}
				priorCalls := calls
				retry, retryErr := normalizer.Step(context.Background())
				legacyNativeEventZero(test, retry)
				if retryErr != err || calls != priorCalls {
					test.Fatal("terminal decoder restarted its released source/driver", retryErr, err, calls)
				}
				return
			}
			test.Fatal("terminal source lifetime fixture never parked an actual parent/cursor")
		})
	}
}
