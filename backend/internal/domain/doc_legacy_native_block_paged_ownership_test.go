package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeBlockPagedOwnershipSize struct {
	backing int64
	payload int64
	excess  int64
	visible int64
}

type legacyNativeBlockPagedOwnershipProbe struct {
	context.Context
	test        *testing.T
	normalizer  *LegacyNormalizationDecoder
	native      *legacyNormalizationNativeEvent
	capacities  l04LegacyCanonicalCapacityObservation
	borrowed    map[*legacyNativeBlockList]int64
	blocks      *legacyNativeBlockValues
	replacement *legacyNativeEventValues
	event       CIREvent
	fields      []docRangeHeaderHeapEstimate
	nativeBytes int64
	metadata    int64
	cancelAt    string
	captured    bool
	inline      bool
	paged       bool
	hidden      bool
	active      bool
	changed     bool
}

func (probe *legacyNativeBlockPagedOwnershipProbe) text(text string) (int64, int64) {
	probe.test.Helper()
	excess := probe.capacities.textExcess(probe.test, text)
	return legacyCanonicalReplacementOwnershipAllocation(int64(len(text)), 1) + excess, excess
}

func (probe *legacyNativeBlockPagedOwnershipProbe) values(values *legacyNativeBlockValues) legacyNativeBlockPagedOwnershipSize {
	probe.test.Helper()
	var size legacyNativeBlockPagedOwnershipSize
	if values == nil {
		return size
	}
	allocation := legacyCanonicalReplacementOwnershipAllocation
	size.backing = allocation(1, int64(unsafe.Sizeof(*values))) + legacyCanonicalIndependentPageHeap(probe.test, &values.records).bytes
	if values.count < 0 || values.count > values.records.length() || values.length() != values.count || values.at(-1) != nil || values.at(values.count) != nil {
		probe.test.Fatal("block owner exposes a nonlogical slot or has an invalid highwater")
	}
	for position := 0; position < values.records.length(); position++ {
		record := values.records.at(position)
		if record == nil || position < values.count && values.at(position) != record || position >= values.count && values.at(position) != nil {
			probe.test.Fatal("logical block access copied a slot or exposed the hidden tail", position)
		}
		model := reflect.ValueOf(record.block)
		for index, field := range legacyContentBlockNativeFields.fields {
			payload, excess := probe.text(model.Field(field.index).String())
			if record.heaps[index].err != nil || record.heaps[index].bytes != payload || record.excess[index] != excess {
				probe.test.Fatal("block field ledger differs from actual text and observed builder capacity", position, field.name, record.heaps[index], payload, record.excess[index], excess)
			}
			size.payload += payload
			size.excess += excess
			if position < values.count {
				size.visible += excess
			}
		}
	}
	if actual := values.backing(); actual.err != nil || actual.bytes != size.backing {
		probe.test.Fatal("block backing differs from the actual struct, inline allocation, nodes, and full pages", actual, size.backing)
	}
	if values.payload.err != nil || values.payload.bytes != size.payload {
		probe.test.Fatal("block payload omitted hidden field allocations or charged them twice", values.payload, size.payload)
	}
	if actual := values.heap(); actual.err != nil || actual.bytes != size.backing+size.payload {
		probe.test.Fatal("block heap differs from the independent backing and highwater payload", actual, size)
	}
	return size
}

func legacyNativeBlockPagedOwnershipMetadata(object *legacyNativeObject) int64 {
	if object == nil {
		return 0
	}
	allocation := legacyCanonicalReplacementOwnershipAllocation
	owned := allocation(1, int64(unsafe.Sizeof(*object))) + allocation(int64(cap(object.key)+cap(object.raw)), 1)
	if object.mutation != nil {
		owned += allocation(1, int64(unsafe.Sizeof(*object.mutation)))
	}
	model := reflect.ValueOf(object).Elem()
	for _, name := range []string{"heaps", "backingExcess", "lists", "blocks", "events", "nested", "containers"} {
		field := model.FieldByName(name)
		owned += allocation(int64(field.Cap()), int64(field.Type().Elem().Size()))
	}
	owned += legacyNativeEventsOwnershipScalar(object.scalar)
	for _, list := range object.blocks {
		if list != nil {
			owned += allocation(1, int64(unsafe.Sizeof(*list)))
			if list.ownedFrame && list.frame != nil {
				owned += allocation(1, int64(unsafe.Sizeof(*list.frame)))
			}
			owned += legacyNativeBlockPagedOwnershipMetadata(list.object)
		}
	}
	for _, list := range object.events {
		if list != nil {
			owned += allocation(1, int64(unsafe.Sizeof(*list))) + legacyNativeBlockPagedOwnershipMetadata(list.object)
		}
	}
	return owned
}

func (probe *legacyNativeBlockPagedOwnershipProbe) list(object *legacyNativeObject, index int, list *legacyNativeBlockList) {
	probe.test.Helper()
	if !object.pagedEvents || !list.paged || list.decoded != nil || list.previous != nil || list.payloads != nil || list.previousHeaps != nil ||
		list.excess != nil || list.previousExtra != nil || list.owned || list.previousOwned || list.ownedFrame {
		probe.test.Fatal("public block owner retained typed arrays or owns its borrowed scanner")
	}
	if list.data != nil {
		probe.inline = probe.inline || list.data.records.inline != nil
		probe.paged = probe.paged || list.data.records.root != nil && list.data.records.depth > 0
	}
	if child := list.object; child != nil && child.scalar == nil && child.phase != legacyNativeObjectAfter && child.phase != legacyNativeObjectDone {
		return
	}
	size := probe.values(list.data)
	allocation := legacyCanonicalReplacementOwnershipAllocation
	wantField := allocation(1, int64(unsafe.Sizeof([]ContentBlock(nil)))) + size.backing + size.payload
	if list.estimate.err != nil || list.estimate.bytes != wantField || list.payload.err != nil || list.payload.bytes != size.payload ||
		list.totalExcess != size.excess || list.visibleExcess != size.visible {
		probe.test.Fatal("block-list field, highwater, or visible excess differs from actual allocations", list.estimate, wantField, list.payload, size)
	}
	committed := list.phase == legacyNativeBlocksDone && !object.blockActive
	startingDuplicate := list.phase == legacyNativeBlocksStart && object.blockActive && object.heaps[index].bytes != 0
	if committed || startingDuplicate {
		if object.heaps[index].err != nil || object.heaps[index].bytes != wantField || committed && object.backingExcess[index] != size.visible {
			probe.test.Fatal("committed blocks field lost its header, page owner, or visible builder excess", object.heaps[index], wantField)
		}
		probe.borrowed[list] = wantField
	}
	wantBorrowed := probe.borrowed[list]
	if list.borrowed.err != nil || list.borrowed.bytes != wantBorrowed {
		probe.test.Fatal("borrowed block charge differs from the independently measured last committed field", list.borrowed, wantBorrowed)
	}
	wantRetained := allocation(1, int64(unsafe.Sizeof(*list))) + max(int64(0), wantField-wantBorrowed)
	if child := list.object; child != nil {
		if child.metadata != &legacyContentBlockNativeFields || child.number != nil || child.typedNumber != nil || child.typeError != nil ||
			child.lists != nil || child.blocks != nil || child.events != nil || child.nested != nil || child.containers != nil {
			probe.test.Fatal("scoped active block entered an unexpected native helper")
		}
		wantRetained += legacyNativeBlockPagedOwnershipMetadata(child)
		prior := list.data.records.at(list.count)
		if prior == nil {
			probe.test.Fatal("active block lost the original borrowed slot")
		}
		before, after := reflect.ValueOf(prior.block), reflect.ValueOf(list.block)
		for fieldIndex, field := range legacyContentBlockNativeFields.fields {
			text := after.Field(field.index).String()
			payload, excess := probe.text(text)
			want := allocation(1, int64(unsafe.Sizeof(text))) + payload
			if child.heaps[fieldIndex].err != nil || child.heaps[fieldIndex].bytes != want || child.backingExcess[fieldIndex] != excess {
				probe.test.Fatal("active block field differs from its actual string allocation", field.name, child.heaps[fieldIndex], want)
			}
			if unsafe.StringData(before.Field(field.index).String()) != unsafe.StringData(text) {
				wantRetained += payload
				probe.changed = probe.changed || wantBorrowed > 0 && len(text) > LegacyCanonicalEncodingBytes
			}
		}
		probe.active = probe.active || wantBorrowed > 0 && child.scalar != nil
	}
	var actual docRangeHeaderHeapEstimate
	list.retained(&actual)
	if actual.err != nil || actual.bytes != wantRetained {
		probe.test.Fatal("active block retained ledger duplicated borrowed storage or omitted changed payload/builders", actual, wantRetained)
	}
	probe.hidden = probe.hidden || size.excess > size.visible
}

func (probe *legacyNativeBlockPagedOwnershipProbe) Err() error {
	if err := probe.Context.Err(); err != nil {
		return err
	}
	if probe.normalizer != nil {
		probe.native = probe.normalizer.nativeCache
	}
	if probe.native == nil {
		return nil
	}
	probe.capacities.observe(probe.native.object)
	var walk func(*legacyNativeObject)
	walk = func(object *legacyNativeObject) {
		if object == nil {
			return
		}
		for index, list := range object.blocks {
			if list != nil {
				probe.list(object, index, list)
			}
		}
		for _, list := range object.events {
			if list != nil {
				walk(list.object)
			}
		}
	}
	walk(probe.native.object)
	if probe.native.canonicalReady && !probe.captured {
		probe.captured = true
		probe.event = CIREvent(probe.native.decoded)
		probe.event.fieldPresence = probe.native.object.presence
		probe.fields = append([]docRangeHeaderHeapEstimate(nil), probe.native.object.heaps...)
		var err error
		probe.blocks, err = probe.native.object.pagedBlocks()
		if err != nil {
			probe.test.Fatal("ready public root could not transfer its block pages", err)
		}
		probe.replacement, err = probe.native.object.pagedReplacement()
		if err != nil {
			probe.test.Fatal(err)
		}
		probe.nativeBytes, err = probe.native.retained()
		if err != nil {
			probe.test.Fatal(err)
		}
		probe.metadata = legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*probe.native))) +
			legacyNativeBlockPagedOwnershipMetadata(probe.native.object)
	}
	if probe.cancelAt == "active" && probe.active || probe.cancelAt == "ready" && probe.captured {
		return context.Canceled
	}
	return nil
}

type legacyNativeBlockPagedOwnershipLedger struct {
	probe      *legacyNativeBlockPagedOwnershipProbe
	paged      *legacyNativePagedOwnershipLedger
	blocks     map[*legacyNativeBlockValues][]legacyNativeBlockPagedOwnershipSnapshot
	lengths    map[*legacyNativeBlockValues]int
	childEdges map[*legacyNativeEventValue]*legacyNativeBlockValues
}

type legacyNativeBlockPagedOwnershipSnapshot struct {
	record *legacyNativeBlockValue
	value  legacyNativeBlockValue
}

func (ledger *legacyNativeBlockPagedOwnershipLedger) blocksHeap(values *legacyNativeBlockValues) (int64, int64) {
	if values == nil {
		return 0, 0
	}
	test := ledger.probe.test
	test.Helper()
	if _, exists := ledger.blocks[values]; exists {
		test.Fatal("distinct root or replacement block owners alias one data allocation")
	}
	size := ledger.probe.values(values)
	ledger.lengths[values] = values.length()
	ledger.blocks[values] = make([]legacyNativeBlockPagedOwnershipSnapshot, values.records.length())
	for position := range ledger.blocks[values] {
		record := values.records.at(position)
		ledger.blocks[values][position] = legacyNativeBlockPagedOwnershipSnapshot{record: record, value: *record}
	}
	return size.backing + size.payload, size.excess
}

func (ledger *legacyNativeBlockPagedOwnershipLedger) eventsHeap(values *legacyNativeEventValues) (int64, int64) {
	if values == nil {
		return 0, 0
	}
	test := ledger.probe.test
	test.Helper()
	if ledger.paged.values[values] {
		test.Fatal("distinct replacement owners alias the same event pages")
	}
	ledger.paged.values[values] = true
	ledger.paged.storages[values] = values.records
	ledger.paged.lengths[values] = values.length()
	backing := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*values))) + legacyCanonicalIndependentPageHeap(test, &values.records).bytes
	ledger.paged.backings[values] = backing
	var payload, excess int64
	for position := 0; position < values.records.length(); position++ {
		record := values.records.at(position)
		if record == nil || record.event.Blocks != nil || record.event.Replacement != nil {
			test.Fatal("replacement child retained materialized block/event slices", position)
		}
		before := ledger.paged.source.sourceExcess
		owned := ledger.paged.source.source(test, reflect.ValueOf(record.event), false, 0)
		measured := ledger.paged.source.sourceExcess - before
		blockBytes, blockExcess := ledger.blocksHeap(record.blocks)
		childBytes, childExcess := ledger.eventsHeap(record.replacement)
		owned += blockBytes + childBytes
		measured += blockExcess + childExcess
		if record.heap.err != nil || record.heap.bytes != owned || record.excess != measured {
			test.Fatal("committed replacement child lost block pages, hidden fields, or actual builder slack", position, record.heap, owned, record.excess, measured)
		}
		payload += owned
		excess += measured
		ledger.childEdges[record] = record.blocks
		ledger.paged.snapshots[values] = append(ledger.paged.snapshots[values], legacyNativePagedOwnershipValue{
			record: record, event: legacyCanonicalReplacementOwnershipSnapshot(test, record.event),
			replacement: record.replacement, heap: record.heap, excess: record.excess,
		})
	}
	if values.payload.err != nil || values.payload.bytes != payload {
		test.Fatal("replacement payload omitted or duplicated owned block pages", values.payload, payload)
	}
	if actual := values.heap(); actual.err != nil || actual.bytes != backing+payload {
		test.Fatal("replacement heap differs from independently walked pages and child allocations", actual, backing+payload)
	}
	return backing + payload, excess
}

func legacyNativeBlockPagedOwnershipNewLedger(probe *legacyNativeBlockPagedOwnershipProbe, raw string) *legacyNativeBlockPagedOwnershipLedger {
	test := probe.test
	test.Helper()
	if !probe.captured || probe.capacities.err != nil || probe.event.Blocks != nil || probe.event.Replacement != nil {
		test.Fatal("fixture did not capture a public paged root without typed arrays", probe.capacities.err)
	}
	ledger := &legacyNativeBlockPagedOwnershipLedger{
		probe: probe, paged: legacyNativePagedOwnershipNewLedger(&probe.capacities),
		blocks: make(map[*legacyNativeBlockValues][]legacyNativeBlockPagedOwnershipSnapshot), lengths: make(map[*legacyNativeBlockValues]int),
		childEdges: make(map[*legacyNativeEventValue]*legacyNativeBlockValues),
	}
	ledger.paged.source = legacyCanonicalReplacementOwnershipNewLedger(test, `{}`, CIREvent{}, &probe.capacities)
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		test.Fatal(err)
	}
	var sourceBytes int64
	for index, field := range legacyEventNativeFields.fields {
		var want int64
		if _, present := members[field.name]; present {
			value := reflect.ValueOf(probe.event).Field(field.index)
			want = legacyCanonicalReplacementOwnershipAllocation(1, int64(value.Type().Size()))
			switch field.name {
			case "blocks":
				owned, _ := ledger.blocksHeap(probe.blocks)
				want += owned
			case "replacement":
				owned, _ := ledger.eventsHeap(probe.replacement)
				want += owned
			default:
				want = ledger.paged.source.source(test, value, true, 0)
			}
		}
		if probe.fields[index].err != nil || probe.fields[index].bytes != want {
			test.Fatal("root field heap differs from actual public source allocations", field.name, probe.fields[index], want)
		}
		sourceBytes += want
	}
	ledger.paged.sourceBytes, ledger.paged.source.sourceBytes = sourceBytes, sourceBytes
	if probe.nativeBytes != sourceBytes+probe.metadata {
		test.Fatal("ready native retained ledger duplicates committed borrowed blocks or loses parser metadata", probe.nativeBytes, sourceBytes+probe.metadata)
	}
	return ledger
}

func (ledger *legacyNativeBlockPagedOwnershipLedger) check(encoding *LegacyCanonicalEncoding) {
	test := ledger.probe.test
	test.Helper()
	owner := encoding.nativeOwner
	if owner == nil || owner.blocks != ledger.probe.blocks || owner.replacement != ledger.probe.replacement || owner.event.Blocks != nil ||
		owner.seal != nil || !reflect.DeepEqual(owner.event, ledger.probe.event) {
		test.Fatal("claimed one-use owner did not retain the exact root fields and block allocation")
	}
	for values, records := range ledger.blocks {
		if values.records.length() != len(records) || values.length() != ledger.lengths[values] {
			test.Fatal("canonical view changed the source block logical length or highwater")
		}
		for position, before := range records {
			after := values.records.at(position)
			if after != before.record || *after != before.value || unsafe.StringData(after.block.Type) != unsafe.StringData(before.value.block.Type) || unsafe.StringData(after.block.Text) != unsafe.StringData(before.value.block.Text) {
				test.Fatal("canonical preparation/output copied or mutated a visible or hidden source block", position)
			}
		}
	}
	for record, blocks := range ledger.childEdges {
		if record.blocks != blocks {
			test.Fatal("replacement child lost its transferred block owner")
		}
	}
	if owner.replacement != nil {
		ledger.paged.check(test, encoding, &legacyNativePagedOwnershipObservation{event: ledger.probe.event, replacement: ledger.probe.replacement})
	} else {
		root := encoding.root.(map[string]any)
		if owner.blocks != nil && root["blocks"] != owner.blocks {
			test.Fatal("root canonical view copied or replaced its actual block allocation")
		}
		if owner.blocks == nil {
			empty, ok := root["blocks"].([]ContentBlock)
			if !ok || len(empty) != 0 || cap(empty) != 0 {
				test.Fatal("null source block owner materialized a nonempty canonical sentinel")
			}
			ledger.paged.source.sourceSlices[legacyCanonicalReplacementOwnershipSliceID(reflect.ValueOf(empty))] = true
		}
		ledger.paged.source.check(test, encoding)
	}
}

type legacyNativeBlockPagedOwnershipEncodingContext struct {
	context.Context
	ledger   *legacyNativeBlockPagedOwnershipLedger
	encoding *LegacyCanonicalEncoding
	prepared map[*legacyNativeBlockValues]bool
	hidden   bool
	cancel   bool
}

func (observation *legacyNativeBlockPagedOwnershipEncodingContext) Err() error {
	if err := observation.Context.Err(); err != nil {
		return err
	}
	if preparation := observation.encoding.preparation; preparation != nil {
		for position := 0; position < preparation.frames.length(); position++ {
			frame := preparation.frames.at(position)
			if frame.kind == 'r' && frame.phase == 0 {
				wrapper := frame.value.(*legacyCanonicalReplacement)
				keys := observation.encoding.mapKeys.at(frame.mapPosition)
				for childPosition := 0; childPosition < keys.fields.length(); childPosition++ {
					record := wrapper.native.at(childPosition)
					view := keys.fields.at(childPosition).value.(map[string]any)
					blocks, emitted := view["blocks"]
					if emitted && blocks != record.blocks || !emitted && record.event.Kind == EventMessage && record.blocks != nil {
						observation.ledger.probe.test.Fatal("replacement canonical view copied, swapped, or omitted its child's actual block owner")
					}
				}
			}
			if frame.kind != 'd' {
				continue
			}
			values, ok := frame.value.(*legacyNativeBlockValues)
			records, owned := observation.ledger.blocks[values]
			if !ok || !owned || frame.length != values.length() || frame.count != len(records) {
				observation.ledger.probe.test.Fatal("block preparation substituted an unowned data structure or lost highwater")
			}
			if frame.position == 0 {
				backing := legacyCanonicalReplacementOwnershipAllocation(1, int64(unsafe.Sizeof(*values))) +
					legacyCanonicalIndependentPageHeap(observation.ledger.probe.test, &values.records).bytes
				if frame.heap.err != nil || frame.heap.bytes != backing {
					observation.ledger.probe.test.Fatal("block preparation lost backing or charged source payload a second time", frame.heap, backing)
				}
				observation.prepared[values] = true
				if observation.cancel {
					return context.Canceled
				}
			}
			observation.hidden = observation.hidden || frame.position > frame.length
		}
	}
	return nil
}

func legacyNativeBlockPagedOwnershipFields(count int) string {
	blocks := make([]string, count)
	for position := range blocks {
		blocks[position] = fmt.Sprintf(`{"type":"text","text":"hidden-%d"}`, position)
	}
	blocks[0] = fmt.Sprintf(`{"type":%q,"text":%q}`, strings.Repeat("t", LegacyCanonicalEncodingBytes+31), strings.Repeat("o", 2*LegacyCanonicalEncodingBytes+37))
	blocks[count-1] = fmt.Sprintf(`{"type":"hidden","text":%q}`, strings.Repeat("h", LegacyCanonicalEncodingBytes+47))
	return `"blocks":[` + strings.Join(blocks, ",") + fmt.Sprintf(`],"blocks":[{"type":null,"text":%q}]`, strings.Repeat("v", 2*LegacyCanonicalEncodingBytes+53))
}

func legacyNativeBlockPagedOwnershipStart(test *testing.T, raw, cancelAt string) (*LegacyCanonicalEncoding, *legacyNativeBlockPagedOwnershipProbe) {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(`{"envelope":{"cir_version":"2"},"events":[` + raw + `]}` + strings.Repeat(" ", ConversationManifestChunkBytes))}
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	probe := &legacyNativeBlockPagedOwnershipProbe{
		Context: context.Background(), test: test, normalizer: normalizer, cancelAt: cancelAt,
		borrowed: make(map[*legacyNativeBlockList]int64),
	}
	for range 512 {
		step, err := normalizer.Step(probe)
		if cancelAt != "" && err != nil {
			if !errors.Is(err, context.Canceled) || len(step.Events) != 0 || step.Complete || !probe.active || normalizer.native != nil ||
				!errors.Is(normalizer.terminal, context.Canceled) || cancelAt == "ready" && !probe.captured {
				test.Fatal("source cancellation published or retained block ownership", cancelAt, err)
			}
			legacyNativeEventsOwnershipCleared(test, probe.native)
			again, retryErr := normalizer.Step(context.Background())
			if !errors.Is(retryErr, context.Canceled) || len(again.Events) != 0 || again.Complete {
				test.Fatal("terminal source revived released block ownership", retryErr)
			}
			return nil, probe
		}
		if err != nil || step.Complete || len(step.Events) > 1 {
			test.Fatal("public block ownership source failed before its sole handoff", err)
		}
		if len(step.Events) != 0 {
			encoding := step.Events[0].Encoding
			if cancelAt != "" || encoding == nil || !probe.captured || encoding.nativeOwner == nil || encoding.nativeOwner.seal != nil {
				test.Fatal("public source bypassed cancellation or its one-use block owner")
			}
			test.Cleanup(encoding.release)
			legacyNativeEventsOwnershipCleared(test, probe.native)
			return encoding, probe
		}
	}
	test.Fatal("public block ownership source exhausted its finite allowance")
	return nil, probe
}

func legacyNativeBlockPagedOwnershipReleased(test *testing.T, encoding *LegacyCanonicalEncoding, owner *legacyNativeCanonicalOwner, terminal error) {
	test.Helper()
	if owner.blocks != nil || owner.replacement != nil {
		test.Fatal("released sealed owner still reaches root or replacement block pages")
	}
	legacyCanonicalReplacementOwnershipReleased(test, encoding, owner, terminal)
}

func TestLegacyNativeBlockPagedOwnershipPublicBackingAndHandoff(test *testing.T) {
	for _, count := range []int{legacyCanonicalInlineValues - 1, legacyCanonicalPageValues + 6} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			raw := `{"kind":"message","role":"assistant",` + legacyNativeBlockPagedOwnershipFields(count) + `}`
			encoding, probe := legacyNativeBlockPagedOwnershipStart(test, raw, "")
			ledger := legacyNativeBlockPagedOwnershipNewLedger(probe, raw)
			owner := encoding.nativeOwner
			if !probe.inline || !probe.active || !probe.changed || !probe.hidden || probe.paged != (count > legacyCanonicalPageValues) ||
				owner.blocks.length() != 1 || owner.blocks.records.length() != count {
				test.Fatal("ownership fixture missed inline/tree, borrowed active payload, or hidden builder slack")
			}
			ctx := &legacyNativeBlockPagedOwnershipEncodingContext{
				Context: context.Background(), ledger: ledger, encoding: encoding, prepared: make(map[*legacyNativeBlockValues]bool),
			}
			for range 256 {
				ledger.check(encoding)
				_, complete, err := encoding.Next(ctx)
				if err != nil {
					test.Fatal(err)
				}
				if complete {
					if !ctx.prepared[probe.blocks] || !ctx.hidden {
						test.Fatal("canonical preparation skipped the actual backing or hidden highwater slots")
					}
					legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, nil)
					return
				}
			}
			test.Fatal("block ownership output exhausted its finite allowance")
		})
	}
}

func TestLegacyNativeBlockPagedOwnershipReplacementChildrenAndRelease(test *testing.T) {
	message := `{"kind":"message","role":"user",` + legacyNativeBlockPagedOwnershipFields(legacyCanonicalPageValues+6) + `}`
	omitted := `{"kind":"turn",` + legacyNativeBlockPagedOwnershipFields(legacyCanonicalInlineValues-1) + `}`
	nested := `{"kind":"compaction","replacement":[` + message + `]}`
	raw := `{"kind":"compaction",` + legacyNativeBlockPagedOwnershipFields(legacyCanonicalInlineValues-1) + `,"replacement":[` + message + `,` + omitted + `,` + nested + `]}`
	for _, phase := range []string{"complete", "preparation", "output", "release"} {
		test.Run(phase, func(test *testing.T) {
			encoding, probe := legacyNativeBlockPagedOwnershipStart(test, raw, "")
			ledger := legacyNativeBlockPagedOwnershipNewLedger(probe, raw)
			owner := encoding.nativeOwner
			ledger.check(encoding)
			if _, emitted := encoding.root.(map[string]any)["blocks"]; emitted || len(ledger.blocks) != 4 {
				test.Fatal("compaction revived omitted root blocks or lost child block owners")
			}
			if _, _, err := owner.claim(context.Background()); !errors.Is(err, ErrIntegrity) {
				test.Fatal("public block owner admitted a second claim", err)
			}
			if phase == "release" {
				encoding.release()
				encoding.release()
				owner.release()
				legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, nil)
				return
			}
			ctx := &legacyNativeBlockPagedOwnershipEncodingContext{
				Context: context.Background(), ledger: ledger, encoding: encoding, prepared: make(map[*legacyNativeBlockValues]bool), cancel: phase == "preparation",
			}
			for range 512 {
				chunk, complete, err := encoding.Next(ctx)
				if err != nil {
					if phase != "preparation" || !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 || len(ctx.prepared) == 0 {
						test.Fatal("preparation cancellation exposed or lost owned child blocks", err)
					}
					legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, context.Canceled)
					return
				}
				if complete {
					if phase != "complete" || len(ctx.prepared) != 2 || !ctx.hidden {
						test.Fatal("canonical views lost child blocks or prepared omitted union blocks", phase, len(ctx.prepared))
					}
					legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, nil)
					return
				}
				ledger.check(encoding)
				if phase == "output" && len(chunk) != 0 {
					canceled, cancel := context.WithCancel(context.Background())
					cancel()
					chunk, complete, err = encoding.Next(canceled)
					if !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 {
						test.Fatal("output cancellation exposed owned child blocks", err)
					}
					legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, context.Canceled)
					return
				}
			}
			test.Fatal("replacement block ownership exhausted its finite allowance")
		})
	}
}

func TestLegacyNativeBlockPagedOwnershipResetAndSourceCancellation(test *testing.T) {
	for _, reset := range []string{"null", "[]"} {
		test.Run("reset-"+reset, func(test *testing.T) {
			raw := `{"kind":"message",` + legacyNativeBlockPagedOwnershipFields(legacyCanonicalPageValues+6) + `,"blocks":` + reset + `}`
			encoding, probe := legacyNativeBlockPagedOwnershipStart(test, raw, "")
			ledger := legacyNativeBlockPagedOwnershipNewLedger(probe, raw)
			ledger.check(encoding)
			if !probe.paged || reset == "null" && probe.blocks != nil || reset == "[]" && (probe.blocks == nil || probe.blocks.records != (legacyCanonicalPages[legacyNativeBlockValue]{})) {
				test.Fatal("whole-list reset retained the old page tree or changed null/empty ownership", reset)
			}
			owner := encoding.nativeOwner
			encoding.release()
			legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, nil)
		})
	}
	for _, phase := range []string{"active", "ready"} {
		test.Run("cancel-"+phase, func(test *testing.T) {
			raw := `{"kind":"message",` + legacyNativeBlockPagedOwnershipFields(legacyCanonicalPageValues+6) + `}`
			legacyNativeBlockPagedOwnershipStart(test, raw, phase)
		})
	}
}

func TestLegacyNativeBlockPagedOwnershipSealedClaimCurrentContext(test *testing.T) {
	raw := `{"kind":"message",` + legacyNativeBlockPagedOwnershipFields(legacyCanonicalInlineValues-1) + `}`
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	fixture.native.object.pagedEvents = true
	probe := &legacyNativeBlockPagedOwnershipProbe{
		Context: context.Background(), test: test, native: fixture.native, borrowed: make(map[*legacyNativeBlockList]int64),
	}
	complete := false
	for range 256 {
		_, ready, err := fixture.step(test, probe, 0)
		if err != nil {
			test.Fatal(err)
		}
		if ready {
			complete = true
			break
		}
	}
	if !complete || probe.Err() != nil {
		test.Fatal("sealed block fixture did not reach native readiness")
	}
	ledger := legacyNativeBlockPagedOwnershipNewLedger(probe, raw)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if owner, err := fixture.native.takeCanonicalOwner(canceled); owner != nil || !errors.Is(err, context.Canceled) || !fixture.native.canonicalReady {
		test.Fatal("canceled take consumed ready block ownership", err)
	}
	owner, err := fixture.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil || owner.seal != owner || owner.blocks != probe.blocks || owner.heap.bytes != ledger.paged.sourceBytes {
		test.Fatal("sealed owner lost root pages or independently measured source heap", err)
	}
	test.Cleanup(owner.release)
	legacyNativeEventsOwnershipCleared(test, fixture.native)
	copied := *owner
	if _, _, err := copied.claim(context.Background()); !errors.Is(err, ErrIntegrity) || owner.seal != owner {
		test.Fatal("copied sealed owner admitted a claim or consumed the original block allocation", err)
	}
	if encoding, err := newLegacyCanonicalEncodingNative(canceled, owner); encoding != nil || !errors.Is(err, context.Canceled) || owner.seal != owner || owner.blocks != probe.blocks {
		test.Fatal("canceled claim discarded or exposed sealed root blocks", err)
	}
	encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(encoding.release)
	ledger.check(encoding)
	encoding.release()
	legacyNativeBlockPagedOwnershipReleased(test, encoding, owner, nil)
}
