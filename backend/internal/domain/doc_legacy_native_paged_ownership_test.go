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

type legacyNativePagedOwnershipObservation struct {
	context.Context
	normalizer   *LegacyNormalizationDecoder
	capacities   l04LegacyCanonicalCapacityObservation
	native       *legacyNormalizationNativeEvent
	event        CIREvent
	replacement  *legacyNativeEventValues
	fields       []docRangeHeaderHeapEstimate
	nativeBytes  int64
	metadata     int64
	captured     bool
	singleton    bool
	paged        bool
	cancelSingle bool
	cancelPages  bool
	cancelReady  bool
	canceled     bool
	err          error
}

func legacyNativePagedOwnershipObjectMetadata(object *legacyNativeObject) int64 {
	if object == nil {
		return 0
	}
	allocation := legacyCanonicalReplacementOwnershipAllocation
	owned := allocation(1, int64(unsafe.Sizeof(*object)))
	if object.mutation != nil {
		owned += allocation(1, int64(unsafe.Sizeof(*object.mutation)))
	}
	owned += allocation(int64(cap(object.key)+cap(object.raw)), 1)
	value := reflect.ValueOf(object).Elem()
	for _, name := range []string{"heaps", "backingExcess", "lists", "blocks", "events", "nested", "containers"} {
		field := value.FieldByName(name)
		owned += allocation(int64(field.Cap()), int64(field.Type().Elem().Size()))
	}
	for _, list := range object.events {
		if list != nil {
			owned += allocation(1, int64(unsafe.Sizeof(*list)))
			owned += legacyNativePagedOwnershipObjectMetadata(list.object)
		}
	}
	return owned
}

func (observation *legacyNativePagedOwnershipObservation) Err() error {
	if err := observation.Context.Err(); err != nil {
		return err
	}
	if observation.canceled {
		return context.Canceled
	}
	native := observation.normalizer.nativeCache
	if native == nil {
		return nil
	}
	observation.native = native
	observation.capacities.observe(native.object)
	var walk func(*legacyNativeObject)
	walk = func(object *legacyNativeObject) {
		if object == nil {
			return
		}
		for _, list := range object.events {
			if list == nil {
				continue
			}
			if !object.pagedEvents || !list.paged || list.decoded != nil || list.previous != nil || list.records != (legacyCanonicalPages[legacyNativeEventRecord]{}) {
				observation.err = fmt.Errorf("public normalizer retained an old typed replacement backing")
			}
			if list.values != nil {
				records := &list.values.records
				if records.single != nil && (records.inline != nil || records.root != nil) || records.inline != nil && records.root != nil {
					observation.err = fmt.Errorf("public replacement singleton, inline, and tree storage overlap")
				}
				if records.length() == 1 {
					if records.single == nil || records.at(0) != records.single || records.inline != nil || records.root != nil ||
						records.nodes != 0 || records.pages != 0 || records.depth != 0 {
						observation.err = fmt.Errorf("first actual public replacement record is not singleton-backed")
					} else {
						allocation := legacyCanonicalReplacementOwnershipAllocation
						want := allocation(1, int64(unsafe.Sizeof(*list.values))) + allocation(1, int64(unsafe.Sizeof(*records.single)))
						if actual := list.values.backing(); actual.err != nil || actual.bytes != want {
							observation.err = fmt.Errorf("actual public singleton backing differs from its value and container allocations: %v, want %d", actual, want)
						}
						observation.singleton = true
					}
				}
				if records.length() > legacyCanonicalPageValues {
					observation.paged = true
				}
			}
			walk(list.object)
		}
	}
	walk(native.object)
	if native.canonicalReady && !observation.captured {
		observation.captured = true
		observation.event = CIREvent(native.decoded)
		observation.event.fieldPresence = native.object.presence
		observation.fields = append([]docRangeHeaderHeapEstimate(nil), native.object.heaps...)
		var err error
		observation.replacement, err = native.object.pagedReplacement()
		if err != nil {
			observation.err = err
		}
		observation.nativeBytes, err = native.retained()
		if err != nil {
			observation.err = err
		}
		observation.metadata = legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*native))) +
			legacyNativePagedOwnershipObjectMetadata(native.object)
	}
	if observation.cancelSingle && observation.singleton || observation.cancelPages && observation.paged || observation.cancelReady && observation.captured {
		observation.canceled = true
		return context.Canceled
	}
	return nil
}

type legacyNativePagedOwnershipLedger struct {
	source      *legacyCanonicalReplacementOwnershipLedger
	values      map[*legacyNativeEventValues]bool
	backings    map[*legacyNativeEventValues]int64
	storages    map[*legacyNativeEventValues]legacyCanonicalPages[legacyNativeEventValue]
	lengths     map[*legacyNativeEventValues]int
	snapshots   map[*legacyNativeEventValues][]legacyNativePagedOwnershipValue
	projections map[uintptr]map[string]any
	wrappers    map[*legacyCanonicalReplacement]bool
	prepared    map[*legacyCanonicalReplacement]bool
	keys        map[legacyCanonicalReplacementOwnershipText]bool
	sourceBytes int64
	projection  int64
	excess      int64
}

type legacyNativePagedOwnershipValue struct {
	record      *legacyNativeEventValue
	event       []string
	replacement *legacyNativeEventValues
	heap        docRangeHeaderHeapEstimate
	excess      int64
}

type legacyNativePagedOwnershipEncodingContext struct {
	context.Context
	test     *testing.T
	encoding *LegacyCanonicalEncoding
	ledger   *legacyNativePagedOwnershipLedger
	cancel   bool
	canceled bool
}

func (observation *legacyNativePagedOwnershipEncodingContext) Err() error {
	if err := observation.Context.Err(); err != nil {
		return err
	}
	if observation.canceled {
		return context.Canceled
	}
	if preparation := observation.encoding.preparation; preparation != nil {
		for position := 0; position < preparation.frames.length(); position++ {
			frame := preparation.frames.at(position)
			if frame.kind == 'r' && frame.phase == 0 && frame.position == 0 {
				observation.ledger.preparationFrame(observation.test, frame)
				if observation.cancel {
					observation.canceled = true
					return context.Canceled
				}
			}
		}
	}
	return nil
}

func legacyNativePagedOwnershipNewLedger(capacities *l04LegacyCanonicalCapacityObservation) *legacyNativePagedOwnershipLedger {
	return &legacyNativePagedOwnershipLedger{
		source: &legacyCanonicalReplacementOwnershipLedger{
			capacities: capacities,
			sourceMaps: make(map[uintptr]bool), sourceSlices: make(map[legacyCanonicalReplacementOwnershipSlice]bool),
			sourceTexts: make(map[legacyCanonicalReplacementOwnershipText]bool),
		},
		values: make(map[*legacyNativeEventValues]bool), backings: make(map[*legacyNativeEventValues]int64),
		storages: make(map[*legacyNativeEventValues]legacyCanonicalPages[legacyNativeEventValue]),
		lengths:  make(map[*legacyNativeEventValues]int), snapshots: make(map[*legacyNativeEventValues][]legacyNativePagedOwnershipValue),
		projections: make(map[uintptr]map[string]any), wrappers: make(map[*legacyCanonicalReplacement]bool),
		prepared: make(map[*legacyCanonicalReplacement]bool), keys: make(map[legacyCanonicalReplacementOwnershipText]bool),
	}
}

func (ledger *legacyNativePagedOwnershipLedger) pageValues(test *testing.T, values *legacyNativeEventValues) (int64, int64) {
	test.Helper()
	if values == nil {
		return 0, 0
	}
	if ledger.values[values] || values.count < 0 || values.count > values.records.length() || values.length() != values.count {
		test.Fatal("actual replacement pages alias another owner or have invalid logical/highwater lengths")
	}
	ledger.values[values] = true
	ledger.storages[values] = values.records
	ledger.lengths[values] = values.length()
	allocation := legacyCanonicalReplacementOwnershipAllocation
	backing := allocation(1, int64(unsafe.Sizeof(*values))) + legacyCanonicalIndependentPageHeap(test, &values.records).bytes
	ledger.backings[values] = backing
	var payload, excess int64
	for position := 0; position < values.records.length(); position++ {
		record := values.records.at(position)
		if record == nil || len(record.event.Replacement) != 0 || cap(record.event.Replacement) != 0 {
			test.Fatal("actual paged child is missing or owns a contiguous typed replacement", position)
		}
		if position < values.count {
			if values.at(position) != record {
				test.Fatal("visible paged access copied or replaced its backing slot", position)
			}
		} else if values.at(position) != nil {
			test.Fatal("logical paged access exposed a hidden highwater slot", position)
		}
		before := ledger.source.sourceExcess
		owned := ledger.source.source(test, reflect.ValueOf(record.event), false, 0)
		childBytes, childExcess := ledger.pageValues(test, record.replacement)
		owned += childBytes
		measured := ledger.source.sourceExcess - before
		if record.heap.err != nil || record.heap.bytes != owned || record.excess != measured || childExcess > measured {
			test.Fatal("paged child payload differs from actual references and observed builder capacity", position, record.heap, owned, record.excess, measured)
		}
		payload += owned
		excess += measured
		ledger.snapshots[values] = append(ledger.snapshots[values], legacyNativePagedOwnershipValue{
			record: record, event: legacyCanonicalReplacementOwnershipSnapshot(test, record.event),
			replacement: record.replacement, heap: record.heap, excess: record.excess,
		})
	}
	if values.at(-1) != nil || values.at(values.count) != nil || values.payload.err != nil || values.payload.bytes != payload {
		test.Fatal("paged payload omits hidden/nested references or exposes an out-of-range slot", values.payload, payload)
	}
	if actual := values.backing(); actual.err != nil || actual.bytes != backing {
		test.Fatal("paged backing differs from independently walked struct/singleton/inline/node/page allocations", actual, backing)
	}
	if actual := values.heap(); actual.err != nil || actual.bytes != backing+payload {
		test.Fatal("paged heap lost or duplicated full backing and highwater payload", actual, backing+payload)
	}
	return backing + payload, excess
}

func (ledger *legacyNativePagedOwnershipLedger) root(test *testing.T, raw string, observation *legacyNativePagedOwnershipObservation) {
	test.Helper()
	if !observation.captured || !observation.singleton || !observation.paged || observation.err != nil || observation.capacities.err != nil ||
		len(observation.event.Replacement) != 0 || cap(observation.event.Replacement) != 0 || observation.replacement == nil {
		test.Fatal("ownership fixture did not observe the actual public paged native handoff", observation.err, observation.capacities.err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		test.Fatal(err)
	}
	value := reflect.ValueOf(observation.event)
	for index, field := range legacyEventNativeFields.fields {
		var expected int64
		if _, present := members[field.name]; present {
			expected = ledger.source.source(test, value.Field(field.index), true, 0)
			if field.eventList {
				paged, excess := ledger.pageValues(test, observation.replacement)
				expected += paged
				ledger.excess += excess
			}
		}
		actual := observation.fields[index]
		if actual.err != nil || actual.bytes != expected {
			test.Fatal("native root field lost paged payload or charged typed storage twice", field.name, actual, expected)
		}
		ledger.sourceBytes += expected
	}
	if ledger.excess <= 0 || ledger.source.sourceExcess < ledger.excess || observation.nativeBytes != observation.metadata+ledger.sourceBytes {
		test.Fatal("root native retained ownership differs from independent source plus actual metadata", observation.nativeBytes, observation.metadata, ledger.sourceBytes, ledger.excess)
	}
}

func (ledger *legacyNativePagedOwnershipLedger) project(test *testing.T, input any) {
	test.Helper()
	allocation := legacyCanonicalReplacementOwnershipAllocation
	switch value := input.(type) {
	case map[string]any:
		identity := uintptr(reflect.ValueOf(value).UnsafePointer())
		if value == nil || ledger.source.sourceMaps[identity] || ledger.projections[identity] != nil {
			return
		}
		ledger.projections[identity] = value
		ledger.projection += allocation(1, 64) + allocation(int64(max(8, len(value))), int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))+8)
		for key, child := range value {
			ledger.keys[legacyCanonicalReplacementOwnershipTextID(key)] = true
			ledger.projection += allocation(int64(len(key)), 1)
			if child != nil {
				ledger.projection += allocation(1, int64(reflect.TypeOf(child).Size()))
			}
			ledger.project(test, child)
		}
	case *legacyCanonicalReplacement:
		if value == nil || value.events != nil || value.native == nil || !ledger.values[value.native] {
			test.Fatal("public replacement wrapper copied pages or substituted the old typed consumer")
		}
		if !ledger.wrappers[value] {
			ledger.wrappers[value] = true
			ledger.projection += allocation(1, int64(unsafe.Sizeof(*value)))
		}
	case []CIREvent, []any, []map[string]any:
		test.Fatal("scoped public paged projection materialized a replacement array", reflect.TypeOf(input))
	}
}

func (ledger *legacyNativePagedOwnershipLedger) preparationFrame(test *testing.T, frame *legacyCanonicalPreparationFrame) {
	test.Helper()
	wrapper, ok := frame.value.(*legacyCanonicalReplacement)
	if !ok || wrapper == nil || !ledger.values[wrapper.native] {
		test.Fatal("replacement preparation lost its actual native page owner")
	}
	want := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*wrapper))) + ledger.backings[wrapper.native]
	if frame.heap.err != nil || frame.heap.bytes != want {
		test.Fatal("replacement preparation omitted actual native backing or charged its payload twice", frame.heap, want)
	}
	ledger.prepared[wrapper] = true
}

func (ledger *legacyNativePagedOwnershipLedger) check(test *testing.T, encoding *LegacyCanonicalEncoding, observation *legacyNativePagedOwnershipObservation) {
	test.Helper()
	owner := encoding.nativeOwner
	if owner == nil || owner.replacement != observation.replacement || len(owner.event.Replacement) != 0 || cap(owner.event.Replacement) != 0 || owner.seal != nil ||
		owner.heap.err != nil || owner.heap.bytes != ledger.sourceBytes || !reflect.DeepEqual(owner.event, observation.event) {
		test.Fatal("one-use owner lost actual source pages, root fields, or the independent heap ledger")
	}
	root, ok := encoding.root.(map[string]any)
	if !ok {
		test.Fatal("actual public encoding lost its projected root")
	}
	wrapper, ok := root["replacement"].(*legacyCanonicalReplacement)
	if !ok || wrapper == nil || wrapper.native != owner.replacement {
		test.Fatal("root replacement wrapper did not borrow the exact transferred native pages")
	}
	for values, records := range ledger.snapshots {
		if values.records != ledger.storages[values] || values.records.length() != len(records) || values.length() != ledger.lengths[values] {
			test.Fatal("preparation changed actual source storage, backing addresses, or highwater")
		}
		for position, before := range records {
			after := values.records.at(position)
			if after != before.record || after.replacement != before.replacement || after.heap != before.heap || after.excess != before.excess ||
				!reflect.DeepEqual(legacyCanonicalReplacementOwnershipSnapshot(test, after.event), before.event) {
				test.Fatal("preparation/output mutated a visible or hidden paged source slot", position)
			}
		}
	}
	allocation := legacyCanonicalReplacementOwnershipAllocation
	metadata := allocation(1, int64(unsafe.Sizeof(*encoding))) + allocation(1, int64(unsafe.Sizeof(*owner)))
	ledger.project(test, encoding.root)
	if preparation := encoding.preparation; preparation != nil {
		metadata += allocation(1, int64(unsafe.Sizeof(*preparation))) + legacyCanonicalIndependentPageHeap(test, &preparation.frames).bytes
		for position := 0; position < preparation.frames.length(); position++ {
			frame := preparation.frames.at(position)
			ledger.project(test, frame.value)
			if frame.iterator != nil {
				metadata += allocation(1, int64(unsafe.Sizeof(reflect.MapIter{})))
			}
			if frame.kind == 'r' && frame.phase == 0 && frame.position == 0 {
				ledger.preparationFrame(test, frame)
			}
		}
	}
	var pending int64
	fieldStorage := func(fields *legacyCanonicalPages[legacyCanonicalKeyField]) int64 {
		owned := legacyCanonicalIndependentPageHeap(test, fields).bytes
		for position := 0; position < fields.length(); position++ {
			field := fields.at(position)
			ledger.project(test, field.value)
			identity := legacyCanonicalReplacementOwnershipTextID(field.key)
			if !ledger.keys[identity] && !ledger.source.sourceTexts[identity] {
				if len(field.key) != 32 {
					test.Fatal("replacement preparation retained an unclassified owned key")
				}
				ledger.keys[identity] = true
				ledger.projection += allocation(int64(len(field.key)), 1)
			}
		}
		return owned
	}
	for position := 0; position < encoding.mapKeys.length(); position++ {
		entry := encoding.mapKeys.at(position)
		owned := fieldStorage(&entry.fields)
		if entry.heap != owned {
			test.Fatal("pending replacement keys lost actual page ownership", entry.heap, owned)
		}
		pending += owned
	}
	metadata += pending + legacyCanonicalIndependentPageHeap(test, &encoding.mapKeys).bytes + legacyCanonicalIndependentPageHeap(test, &encoding.frames).bytes
	for position := 0; position < encoding.frames.length(); position++ {
		frame := encoding.frames.at(position)
		ledger.project(test, frame.value)
		metadata += fieldStorage(&frame.fields)
		if frame.ownedLiteral {
			metadata += allocation(int64(len(frame.text)), 1)
		}
	}
	metadata += legacyCanonicalIndependentNumberHeap(test, &encoding.numbers).bytes
	wantRoot := ledger.sourceBytes + ledger.projection
	if encoding.pendingKeysHeap.err != nil || encoding.pendingKeysHeap.bytes != pending || encoding.rootHeap.err != nil || encoding.rootHeap.bytes != wantRoot {
		test.Fatal("preparation/output lost or duplicated source, wrapper, projection, or pending storage", encoding.rootHeap, wantRoot, encoding.pendingKeysHeap, pending)
	}
	actual, err := encoding.RetainedBytes()
	if err != nil || actual != wantRoot+metadata {
		test.Fatal("encoder retained total differs from independently observed page/source/projection metadata", actual, wantRoot+metadata, err)
	}
}

func legacyNativePagedOwnershipRaw(invalid bool) string {
	children := make([]string, legacyCanonicalPageValues+6)
	for position := range children {
		children[position] = fmt.Sprintf(`{"kind":"turn","id":"hidden-%d","seq":%d,"role":"user"}`, position, position)
	}
	children[len(children)-1] = fmt.Sprintf(`{"kind":"unknown-hidden","input":{"payload":%q}}`, strings.Repeat("h", LegacyCanonicalEncodingBytes+17))
	visible := fmt.Sprintf(`{"kind":"turn","id":%q,"seq":3,"role":"assistant"}`, strings.Repeat("v", LegacyCanonicalEncodingBytes+19))
	if invalid {
		visible = `{"kind":"unknown-visible","id":"reject"}`
	}
	nested := fmt.Sprintf(`{"kind":"compaction","id":"nested","seq":1,"replacement":[%s],"replacement":[%s],"replacement_complete":true}`, strings.Join(children, ","), visible)
	return fmt.Sprintf(`{"kind":"compaction","id":"root","seq":7,"status":"omitted-root","replacement":[%s],"replacement":[%s],"replacement_complete":true}`, strings.Join(children, ","), nested)
}

func legacyNativePagedOwnershipStart(test *testing.T, raw string) (*LegacyCanonicalEncoding, *legacyNativePagedOwnershipObservation, *legacyNativePagedOwnershipLedger) {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(`{"envelope":{"cir_version":"2"},"events":[` + raw + `]}` + strings.Repeat(" ", ConversationManifestChunkBytes))}
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	observation := &legacyNativePagedOwnershipObservation{Context: context.Background(), normalizer: normalizer}
	for range 256 {
		step, err := normalizer.Step(observation)
		if err != nil || step.Complete || len(step.Events) > 1 {
			test.Fatal("actual public ownership fixture failed before its sole event handoff", err)
		}
		if len(step.Events) == 0 {
			continue
		}
		encoding := step.Events[0].Encoding
		if encoding == nil || encoding.preparation == nil || encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 {
			test.Fatal("actual public native handoff eagerly prepared or lost its owner")
		}
		test.Cleanup(encoding.release)
		ledger := legacyNativePagedOwnershipNewLedger(&observation.capacities)
		ledger.root(test, raw, observation)
		legacyNativeEventsOwnershipCleared(test, observation.native)
		ledger.check(test, encoding, observation)
		return encoding, observation, ledger
	}
	test.Fatal("actual public ownership fixture exhausted its finite source allowance")
	return nil, observation, nil
}

func legacyNativePagedOwnershipReleased(test *testing.T, encoding *LegacyCanonicalEncoding, owner *legacyNativeCanonicalOwner, terminal error) {
	test.Helper()
	if owner.replacement != nil {
		test.Fatal("released one-use owner still reaches actual replacement pages")
	}
	legacyCanonicalReplacementOwnershipReleased(test, encoding, owner, terminal)
}

func TestLegacyNativePagedOwnershipPublicHandoffHiddenTailAndOutput(test *testing.T) {
	raw := legacyNativePagedOwnershipRaw(false)
	encoding, observation, ledger := legacyNativePagedOwnershipStart(test, raw)
	owner := encoding.nativeOwner
	for _, values := range []*legacyNativeEventValues{owner.replacement, owner.replacement.at(0).replacement} {
		if values == nil || values.length() != 1 || values.records.length() != legacyCanonicalPageValues+6 || values.records.pages != 2 ||
			values.records.at(values.records.length()-1).event.Kind != "unknown-hidden" {
			test.Fatal("actual ownership fixture lost nested two-page highwater tails")
		}
	}
	if _, _, err := owner.claim(context.Background()); !errors.Is(err, ErrIntegrity) || owner.replacement != observation.replacement {
		test.Fatal("claimed public owner was reusable or a rejected second claim changed its pages", err)
	}
	ctx := &legacyNativePagedOwnershipEncodingContext{Context: context.Background(), test: test, encoding: encoding, ledger: ledger}
	var output []byte
	sawPreparation, sawOutput := false, false
	for range 256 {
		if encoding.preparation != nil {
			sawPreparation = true
		}
		chunk, complete, err := encoding.Next(ctx)
		if err != nil {
			test.Fatal("actual paged ownership output failed", err)
		}
		output = append(output, chunk...)
		if complete {
			var typed CIREvent
			if err := json.Unmarshal([]byte(raw), &typed); err != nil {
				test.Fatal(err)
			}
			want, err := json.Marshal(canonicalEvents([]CIREvent{typed})[0])
			if err != nil || !bytes.Equal(output, want) || !sawPreparation || !sawOutput || len(ledger.wrappers) != 2 || len(ledger.prepared) != 2 {
				test.Fatal("ownership transfer changed output, exposed hidden children, or skipped nested wrappers", err, len(output), len(want), sawPreparation, sawOutput, len(ledger.wrappers))
			}
			legacyNativePagedOwnershipReleased(test, encoding, owner, nil)
			return
		}
		sawOutput = sawOutput || len(chunk) != 0
		ledger.check(test, encoding, observation)
	}
	test.Fatal("actual paged ownership output exhausted its finite allowance")
}

func TestLegacyNativePagedOwnershipEncodingCancellationAndFailure(test *testing.T) {
	for _, phase := range []string{"handoff", "preparation", "output", "invalid-child", "release"} {
		test.Run(phase, func(test *testing.T) {
			encoding, observation, ledger := legacyNativePagedOwnershipStart(test, legacyNativePagedOwnershipRaw(phase == "invalid-child"))
			owner := encoding.nativeOwner
			if phase == "release" {
				encoding.release()
				encoding.release()
				owner.release()
				legacyNativePagedOwnershipReleased(test, encoding, owner, nil)
				return
			}
			preparationContext := &legacyNativePagedOwnershipEncodingContext{
				Context: context.Background(), test: test, encoding: encoding, ledger: ledger, cancel: phase == "preparation",
			}
			ready := phase == "handoff"
			for range 256 {
				if ready {
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					chunk, complete, err := encoding.Next(ctx)
					if !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 {
						test.Fatal("cancellation exposed owned paged output", complete, err)
					}
					legacyNativePagedOwnershipReleased(test, encoding, owner, context.Canceled)
					return
				}
				chunk, complete, err := encoding.Next(preparationContext)
				if err != nil {
					if phase == "preparation" {
						if !preparationContext.canceled || !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 || len(ledger.prepared) == 0 {
							test.Fatal("preparation cancellation did not observe and release the actual native backing", complete, err)
						}
						legacyNativePagedOwnershipReleased(test, encoding, owner, context.Canceled)
						return
					}
					if phase != "invalid-child" || complete || len(chunk) != 0 || !strings.Contains(err.Error(), `unknown event kind "unknown-visible"`) {
						test.Fatal("unexpected actual paged preparation failure", complete, err)
					}
					legacyNativePagedOwnershipReleased(test, encoding, owner, err)
					return
				}
				if complete {
					test.Fatal("ownership cancellation/failure fixture completed before its target phase")
				}
				ledger.check(test, encoding, observation)
				ready = phase == "output" && len(chunk) != 0
			}
			test.Fatal("ownership cancellation/failure fixture exhausted its finite allowance")
		})
	}
}

func TestLegacyNativePagedOwnershipSourceCancellationAndFailureRelease(test *testing.T) {
	for _, phase := range []string{"singleton", "pages", "before-owner", "source-failure"} {
		test.Run(phase, func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(`{"envelope":{"cir_version":"2"},"events":[` + legacyNativePagedOwnershipRaw(false) + `]}`)}
			source := buffer.input()
			var observation *legacyNativePagedOwnershipObservation
			failure := errors.New("paged ownership source read failure")
			failed := false
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if phase == "source-failure" && observation != nil && observation.paged {
					failed = true
					return nil, failure
				}
				return buffer.Read(ctx, offset, length)
			}
			normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
			if err != nil {
				test.Fatal(err)
			}
			observation = &legacyNativePagedOwnershipObservation{
				Context: context.Background(), normalizer: normalizer,
				cancelSingle: phase == "singleton", cancelPages: phase == "pages", cancelReady: phase == "before-owner",
			}
			for range 256 {
				step, err := normalizer.Step(observation)
				if len(step.Events) != 0 || step.Complete {
					for _, event := range step.Events {
						event.Encoding.release()
					}
					test.Fatal("terminal source ownership fixture exposed a handed-off event")
				}
				if err == nil {
					continue
				}
				want := error(context.Canceled)
				if phase == "source-failure" {
					want = failure
				}
				if !errors.Is(err, want) || step.Reset || step.Envelope != nil || step.Count != 0 || observation.err != nil || !observation.singleton || observation.native == nil ||
					phase == "singleton" && observation.paged || phase != "singleton" && !observation.paged ||
					phase == "before-owner" && !observation.captured || phase == "source-failure" && !failed {
					test.Fatal("source ownership terminal phase or precedence changed", phase, err, observation.err)
				}
				legacyNativeEventsOwnershipCleared(test, observation.native)
				if normalizer.native != nil || !errors.Is(normalizer.terminal, want) {
					test.Fatal("terminal normalizer kept a live paged native driver", normalizer.terminal)
				}
				again, retryErr := normalizer.Step(context.Background())
				if !errors.Is(retryErr, want) || again.Complete || len(again.Events) != 0 {
					test.Fatal("terminal normalizer revived cleared page ownership", retryErr)
				}
				return
			}
			test.Fatal("source ownership terminal fixture exhausted its finite allowance")
		})
	}
}
