package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeBlocksTestHeap(test *testing.T, values []ContentBlock) docRangeHeaderHeapEstimate {
	test.Helper()
	estimate, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(values))
	if err != nil {
		test.Fatal(err)
	}
	return estimate
}

func legacyNativeBlocksTestStep(test *testing.T, list *legacyNativeBlockList, source LegacyByteSource) (bool, error) {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	input := &legacyNormalizationInput{ctx: ctx, source: source}
	complete, err := list.step(ctx, input)
	cancel()
	if input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
		input.fields > LegacyNormalizationStepEvents*len(legacyContentBlockNativeFields.fields)+1 {
		test.Fatal("block list exceeded its shared work, authenticated read, or action budget", input.workBytes, input.loads, input.fields)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, err
}

func legacyNativeBlocksTestScratch(list *legacyNativeBlockList) docRangeHeaderHeapEstimate {
	var estimate docRangeHeaderHeapEstimate
	estimate.allocation(1, int64(unsafe.Sizeof(*list)))
	if list.ownedFrame && list.frame != nil {
		estimate.allocation(1, int64(unsafe.Sizeof(*list.frame)))
	}
	estimate.allocation(int64(cap(list.payloads)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
	estimate.allocation(int64(cap(list.previousHeaps)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
	estimate.allocation(int64(cap(list.excess)), int64(unsafe.Sizeof(int64(0))))
	estimate.allocation(int64(cap(list.previousExtra)), int64(unsafe.Sizeof(int64(0))))
	return estimate
}

func legacyNativeBlocksTestDrain(test *testing.T, list *legacyNativeBlockList, priorHeap docRangeHeaderHeapEstimate, raw string) int {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+64*parts+len(raw)/LegacyNormalizationStepEvents; stepIndex++ {
		complete, err := legacyNativeBlocksTestStep(test, list, source)
		if err != nil {
			test.Fatal("block list rejected a typed fixture", err, stepIndex, list.phase, list.position)
		}
		retained := priorHeap
		if complete {
			retained = list.heap()
		}
		list.retained(&retained)
		if retained.err != nil || !complete && retained.bytes < priorHeap.bytes || retained.bytes < list.heap().bytes {
			test.Fatal("block list omitted prior payload, hidden values, or temporary backing", retained, list.heap())
		}
		if complete {
			minimum := legacyNativeBlocksTestHeap(test, list.values())
			if list.heap().err != nil || list.heap().bytes < minimum.bytes {
				test.Fatal("block list omitted string capacity or hidden values", list.heap(), minimum)
			}
			var scratch docRangeHeaderHeapEstimate
			list.retained(&scratch)
			if expected := legacyNativeBlocksTestScratch(list); scratch != expected || list.owned || list.previousOwned ||
				list.added.bytes != 0 || list.object != nil || list.block != (ContentBlock{}) {
				test.Fatal("completed block list did not transfer destination ownership", scratch, expected)
			}
			var visible docRangeHeaderHeapEstimate
			fields := len(legacyContentBlockNativeFields.fields)
			for position, block := range list.values() {
				value := reflect.ValueOf(block)
				for index, field := range legacyContentBlockNativeFields.fields {
					var minimum docRangeHeaderHeapEstimate
					minimum.allocation(int64(value.Field(field.index).Len()), 1)
					payload := list.payloads[position*fields+index]
					visible.include(docRangeHeaderHeapEstimate{bytes: payload.bytes - minimum.bytes})
				}
			}
			if excess, err := list.backingExcess(); err != nil || visible.err != nil || excess != visible.bytes {
				test.Fatal("visible backing excess omitted capacity or included hidden slots", excess, visible, err)
			}
			return stepIndex + 1
		}
	}
	test.Fatal("block list exhausted its bounded progress allowance")
	return 0
}

func TestLegacyNativeBlockListDuplicateNullTailsMatchJSONUnmarshal(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		backing []ContentBlock
		length  int
		arrays  []string
	}{
		{
			name: "hidden-tail-survives-shorter-duplicates",
			backing: []ContentBlock{
				{Type: "old-first", Text: "first"}, {Type: "old-second", Text: "second"},
				{Type: "hidden-third", Text: "third"}, {Type: "hidden-fourth", Text: "fourth"},
			},
			length: 2,
			arrays: []string{
				`[{"type":"fresh","text":"changed","text":null}]`,
				`[null,null,null,null]`,
				`[{"TEXT":"latest","TyPe":null},{}]`,
				`[{},null,{"\u0074ext":"revealed"},null]`,
			},
		},
		{
			name:    "zero-length-live-backing",
			backing: []ContentBlock{{Type: "first", Text: "hidden-first"}, {Type: "second", Text: "hidden-second"}},
			arrays:  []string{`[null,{}]`, `[{"text":"fresh"}]`, `[null,null,null,null,null]`, `[{}]`, `[null,null,null,null,null]`},
		},
		{
			name:    "empty-array-replaces-backing",
			backing: []ContentBlock{{Type: "old-first", Text: "old"}, {Type: "old-tail", Text: "hidden"}},
			length:  1,
			arrays:  []string{`[]`, `[null,{}]`, `[]`, `null`, `[]`},
		},
		{
			name:    "whole-null-replaces-backing",
			backing: []ContentBlock{{Type: "old-first", Text: "old"}, {Type: "old-tail", Text: "hidden"}},
			length:  1,
			arrays:  []string{`null`, `[null,{}]`, `[{"type":"fresh"},null,{"text":"tail"}]`, `[null]`, `[null,null,null]`},
		},
		{
			name:   "nil-empty-and-object-merge",
			arrays: []string{`null`, `[]`, `[null]`, `[]`, `[{"type":"","text":""},null]`, `[{"type":"text","type":null}]`, `null`},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var got, want []ContentBlock
			if fixture.backing != nil {
				got = append([]ContentBlock(nil), fixture.backing...)
				want = append([]ContentBlock(nil), fixture.backing...)
				got, want = got[:fixture.length], want[:fixture.length]
			}
			heap := legacyNativeBlocksTestHeap(test, got)
			var list *legacyNativeBlockList
			for _, raw := range fixture.arrays {
				prior := got
				if err := json.Unmarshal([]byte(raw), &want); err != nil {
					test.Fatal(err)
				}
				if list == nil {
					var err error
					list, err = newLegacyNativeBlockList(got, heap, 0, len(raw))
					if err != nil {
						test.Fatal(err)
					}
				} else if err := list.start(0, len(raw)); err != nil {
					test.Fatal(err)
				}
				legacyNativeBlocksTestDrain(test, list, heap, raw)
				got, heap = list.values(), list.heap()
				if !reflect.DeepEqual(got, want) {
					test.Fatal("duplicate block array changed native field or null semantics", raw, got, want)
				}
				if len(got) > 0 && len(got) <= cap(prior) && &got[0] != &prior[:cap(prior)][0] {
					test.Fatal("block list discarded reusable hidden backing", raw)
				}
				if raw == `[]` && (got == nil || cap(got) != 0) {
					test.Fatal("empty block array kept old backing or became null")
				}
			}
		})
	}
}

func TestLegacyNativeBlockListGiantFieldsAndHiddenCapacities(test *testing.T) {
	var text strings.Builder
	text.WriteString(strings.Repeat("x", LegacyCanonicalEncodingBytes-5))
	text.WriteString(`\uD83D\uDE42\uD800x\uDC00\"\\\/\b\f\n\r\t\u0000`)
	text.WriteString(strings.Repeat("y", 2*ConversationManifestChunkBytes))
	text.WriteString("\ud55c🙂\xff\xc0\x80\xed\xa0\x80\xf0\x9f")
	text.WriteString(`\uD800`)
	raw := `[{"type":"head"},{"type":"` + strings.Repeat("t", ConversationManifestChunkBytes+73) + `","text":"` + text.String() + `","TEXT":null},null]`
	got := []ContentBlock{{Type: "old", Text: "head"}, {Type: "kept", Text: "replaced"}, {Type: "hidden", Text: "tail"}, {Type: "still", Text: "hidden"}}[:1]
	want := append([]ContentBlock(nil), got[:cap(got)]...)
	want = want[:len(got)]
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		test.Fatal(err)
	}
	heap := legacyNativeBlocksTestHeap(test, got)
	list, err := newLegacyNativeBlockList(got, heap, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	if steps := legacyNativeBlocksTestDrain(test, list, heap, raw); steps <= 1 || !reflect.DeepEqual(list.values(), want) {
		test.Fatal("giant block fields changed Unicode, typed decoding, or hidden tails", steps)
	}
	fields := len(legacyContentBlockNativeFields.fields)
	textField, found := reflect.TypeOf(ContentBlock{}).FieldByName("Text")
	if !found {
		test.Fatal("ContentBlock no longer has its native Text field")
	}
	priorPayloads := append([]docRangeHeaderHeapEstimate(nil), list.payloads[fields:2*fields]...)
	for _, next := range []string{`[{"text":"short"}]`, `[null,null,null,null]`, `[{}, {"type":null,"text":null}]`, `[null,{"text":""},null,null]`} {
		heap = list.heap()
		if err := list.start(0, len(next)); err != nil {
			test.Fatal(err)
		}
		legacyNativeBlocksTestDrain(test, list, heap, next)
		if err := json.Unmarshal([]byte(next), &want); err != nil || !reflect.DeepEqual(list.values(), want) {
			test.Fatal("hidden giant block fields changed across duplicate arrays", err)
		}
		for index, field := range legacyContentBlockNativeFields.fields {
			if next == `[null,{"text":""},null,null]` && field.index == textField.Index[0] {
				if list.payloads[fields+index].bytes != 0 {
					test.Fatal("empty overwrite kept a hidden giant allocation")
				}
			} else if list.payloads[fields+index] != priorPayloads[index] {
				test.Fatal("null or omitted block field lost its actual allocation capacity")
			}
		}
	}
}

func TestLegacyNativeBlockListBackingExcessIncludesOnlyVisibleSlots(test *testing.T) {
	raw := `[{"type":"head"},{"type":"` + strings.Repeat("t", 2*LegacyCanonicalEncodingBytes+37) +
		`","text":"` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `"},null,{"text":"` +
		strings.Repeat("y", LegacyCanonicalEncodingBytes+17) + `"}]`
	list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := list.backingExcess(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("incomplete block list exposed a final visible capacity total", err)
	}
	legacyNativeBlocksTestDrain(test, list, list.heap(), raw)
	initial, err := list.backingExcess()
	if err != nil || initial <= 0 {
		test.Fatal("giant block builders did not retain measurable capacity provenance", initial, err)
	}
	fields := len(legacyContentBlockNativeFields.fields)
	textField, found := reflect.TypeOf(ContentBlock{}).FieldByName("Text")
	if !found {
		test.Fatal("ContentBlock no longer has its native Text field")
	}
	textExcess := int64(0)
	for index, field := range legacyContentBlockNativeFields.fields {
		if field.index == textField.Index[0] {
			textExcess = list.excess[fields+index]
		}
	}
	if textExcess <= 0 {
		test.Fatal("giant Text builder lost its per-field capacity excess")
	}
	for _, fixture := range []struct {
		raw  string
		want int64
	}{
		{raw: `[{}]`, want: 0},
		{raw: `[null,null,null,null]`, want: initial},
		{raw: `[null,{"type":"small","text":null}]`, want: textExcess},
		{raw: `[null,{"text":""}]`, want: 0},
		{raw: `[]`, want: 0},
		{raw: `[null,null,null,null]`, want: 0},
		{raw: `null`, want: 0},
	} {
		heap := list.heap()
		if err := list.start(0, len(fixture.raw)); err != nil {
			test.Fatal(err)
		}
		legacyNativeBlocksTestDrain(test, list, heap, fixture.raw)
		if excess, err := list.backingExcess(); err != nil || excess != fixture.want {
			test.Fatal("visible block capacity provenance changed across null, shorter, or empty arrays", fixture.raw, excess, fixture.want, err)
		}
	}
}

func TestLegacyNativeBlockListPrefixUsesActualChildSpan(test *testing.T) {
	for _, array := range []string{
		`null`, `[]`,
		`[{"unknown":{"type":false,"text":[1,{"nested":"},]"}]},"type":"text","\u0074ext":"first","TEXT":"last","text":null}]`,
		`[{"` + strings.Repeat("unknown", 32) + `":{"type":false},"text":"kept"},null]`,
		`[{"text":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+37) + `},]"}]`,
	} {
		prefix := `{"blocks":`
		raw := prefix + array + `,"after":{"text":false}}`
		list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, len(prefix), len(raw))
		if err != nil {
			test.Fatal(err)
		}
		list.prefix = true
		legacyNativeBlocksTestDrain(test, list, list.heap(), raw)
		var want []ContentBlock
		if err := json.Unmarshal([]byte(array), &want); err != nil || !reflect.DeepEqual(list.values(), want) || list.position != len(prefix)+len(array) {
			test.Fatal("prefix block list consumed its parent suffix or used the parent as its child span", err, list.position)
		}
	}
}

func TestLegacyNativeBlockListMigrationBuffersRemainCharged(test *testing.T) {
	const count = 10000
	raw := `[` + strings.Repeat(`{"type":"value","text":"body"},`, count) + `null]`
	list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	migrating := false
	for stepIndex := 0; stepIndex < 2048; stepIndex++ {
		complete, err := legacyNativeBlocksTestStep(test, list, source)
		if err != nil {
			test.Fatal(err)
		}
		if list.previousOwned && list.copied < len(list.previous) {
			migrating = true
			expected := legacyNativeBlocksTestScratch(list)
			expected.include(list.added)
			expected.include(legacyNativeBlocksBacking(cap(list.decoded)))
			expected.include(legacyNativeBlocksBacking(cap(list.previous)))
			var retained docRangeHeaderHeapEstimate
			list.retained(&retained)
			if retained != expected || list.object != nil {
				test.Fatal("slice migration omitted a live buffer or retained a slice-element decoder", retained, expected)
			}
		}
		if complete {
			var want []ContentBlock
			if err := json.Unmarshal([]byte(raw), &want); err != nil || !reflect.DeepEqual(list.values(), want) || !migrating {
				test.Fatal("wide block list lost values or did not expose incremental migration", err, migrating)
			}
			return
		}
	}
	test.Fatal("wide block list exhausted its bounded action allowance")
}

func TestLegacyNativeBlockListRetainedTransfersOnlyIncrementalPayload(test *testing.T) {
	prior := []ContentBlock{{Type: strings.Repeat("t", 4*ConversationManifestChunkBytes), Text: strings.Repeat("p", 4*ConversationManifestChunkBytes)}}
	heap := legacyNativeBlocksTestHeap(test, prior)
	raw := `[{"type":null,"text":null}]`
	list, err := newLegacyNativeBlockList(prior, heap, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	var retained docRangeHeaderHeapEstimate
	list.retained(&retained)
	if retained != legacyNativeBlocksTestScratch(list) || retained.bytes >= heap.bytes {
		test.Fatal("new block list double-charged the caller's prior payload", retained, heap)
	}
	legacyNativeBlocksTestDrain(test, list, heap, raw)
	if list.heap() != heap {
		test.Fatal("null fields changed the caller's payload estimate", list.heap(), heap)
	}
	raw = `[{"type":null,"text":"` + strings.Repeat("n", 2*ConversationManifestChunkBytes+31) + `"}]`
	if err := list.start(0, len(raw)); err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	observed := false
	for stepIndex := 0; stepIndex < 128; stepIndex++ {
		complete, err := legacyNativeBlocksTestStep(test, list, source)
		if err != nil {
			test.Fatal(err)
		}
		if list.object != nil {
			observed = true
			if list.object.destination.Pointer() != reflect.ValueOf(&list.block).Pointer() {
				test.Fatal("child object retained a pointer into the migratable slice")
			}
			var scratch docRangeHeaderHeapEstimate
			list.retained(&scratch)
			if scratch.err != nil || scratch.bytes >= heap.bytes {
				test.Fatal("active child double-charged unchanged prior field payload", scratch, heap)
			}
		}
		if complete {
			if !observed || list.values()[0].Type != prior[0].Type || list.values()[0].Text != strings.Repeat("n", 2*ConversationManifestChunkBytes+31) {
				test.Fatal("incremental object decode lost an unchanged field or never yielded")
			}
			return
		}
	}
	test.Fatal("active child exhausted its bounded progress allowance")
}

func TestLegacyNativeBlockListRejectsInvalidTypedFieldsWithoutUnboundedDecode(test *testing.T) {
	for _, raw := range []string{
		`[false]`, `[[]]`, `["text"]`,
		`[{"type":7}]`, `[{"text":false}]`, `[{"TEXT":{}}]`, `[{"\u0074ext":[]}]`,
		`[{"text":"valid","text":false}]`,
		`[{"text":["` + strings.Repeat("x", 2*ConversationManifestChunkBytes) + `"]}]`,
		`[{"TYPE":{"ignored":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes) + `"}}]`,
	} {
		var want []ContentBlock
		wantErr := json.Unmarshal([]byte(raw), &want)
		if wantErr == nil {
			test.Fatal("invalid typed fixture became natively acceptable")
		}
		list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
		if err != nil {
			test.Fatal(err)
		}
		buffer := &legacySourceProofBuffer{body: []byte(raw)}
		source := legacyNormalizationPreparationInput(test, buffer, true)
		complete, err := legacyNativeBlocksTestStep(test, list, source)
		var actualTypeError, wantedTypeError *json.UnmarshalTypeError
		if complete || !errors.As(err, &actualTypeError) || !errors.As(wantErr, &wantedTypeError) ||
			actualTypeError.Value != wantedTypeError.Value || actualTypeError.Type != wantedTypeError.Type {
			test.Fatal("invalid typed block escaped the bounded scalar guard", complete, err)
		}
	}
}

func TestLegacyNativeBlockListPartHashesAreRecheckedAfterChildScan(test *testing.T) {
	raw := `[{"type":"text","text":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}]`
	list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	if complete, err := legacyNativeBlocksTestStep(test, list, source); err != nil || complete {
		test.Fatal("giant child did not yield during its authenticated scan", complete, err)
	}
	buffer.body[strings.Index(raw, strings.Repeat("x", 32))] = 'y'
	for stepIndex := 0; stepIndex < 32; stepIndex++ {
		complete, err := legacyNativeBlocksTestStep(test, list, source)
		if err != nil {
			if !errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("mutated authenticated part lost its source failure category", err)
			}
			return
		}
		if complete {
			test.Fatal("native block decoding trusted a part from the earlier child scan")
		}
	}
	test.Fatal("native block decoder did not revisit its mutated child source")
}

func TestLegacyNativeBlockListPreCanceledStepDoesNotRead(test *testing.T) {
	raw := `[{"text":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes) + `"},null]`
	prior := []ContentBlock{{Type: "old", Text: "head"}, {Type: "hidden", Text: "tail"}}[:1]
	list, err := newLegacyNativeBlockList(prior, legacyNativeBlocksTestHeap(test, prior), 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	if complete, err := legacyNativeBlocksTestStep(test, list, source); err != nil || complete {
		test.Fatal("giant block did not yield", err)
	}
	var before docRangeHeaderHeapEstimate
	list.retained(&before)
	position, reads := list.position, len(buffer.reads)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := &legacyNormalizationInput{ctx: ctx, source: source}
	complete, err := list.step(ctx, input)
	var after docRangeHeaderHeapEstimate
	list.retained(&after)
	if complete || !errors.Is(err, context.Canceled) || len(buffer.reads) != reads || list.position != position || before != after {
		test.Fatal("pre-canceled block list changed retained state or read source", err, before, after)
	}
	for stepIndex := 0; stepIndex < 128; stepIndex++ {
		complete, err := legacyNativeBlocksTestStep(test, list, source)
		if err != nil {
			test.Fatal(err)
		}
		if complete {
			if list.values()[1] != (ContentBlock{Type: "hidden", Text: "tail"}) {
				test.Fatal("resumed block list discarded a hidden null element")
			}
			return
		}
	}
	test.Fatal("resumed block list exhausted its bounded progress allowance")
}

func TestLegacyNativeBlockListPartialSharedWorkBudget(test *testing.T) {
	prior := []ContentBlock{{Type: "old", Text: "head"}, {Type: "hidden", Text: "tail"}}[:1]
	raw := `[{"type":"new","text":"value"},null,{"text":"tail"}]`
	want := append([]ContentBlock(nil), prior[:cap(prior)]...)
	want = want[:len(prior)]
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		test.Fatal(err)
	}
	list, err := newLegacyNativeBlockList(prior, legacyNativeBlocksTestHeap(test, prior), 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	width := legacyNativeBlocksWidth()
	budgets := []int{1, width - 1, width, width + 1, 3 * width, canonicalCheckBytes}
	for stepIndex := 0; stepIndex < 128; stepIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source, workBytes: ConversationManifestChunkBytes - budgets[stepIndex%len(budgets)]}
		complete, err := list.step(ctx, input)
		cancel()
		if err != nil || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded {
			test.Fatal("nested block work exceeded the caller's remaining quantum", err, input.workBytes, input.loads)
		}
		for _, page := range input.pages {
			clear(page)
		}
		if complete {
			if !reflect.DeepEqual(list.values(), want) {
				test.Fatal("partial shared budgets changed hidden backing or migrated values", list.values(), want)
			}
			return
		}
	}
	test.Fatal("partial shared budgets prevented bounded progress")
}

func TestLegacyNativeBlockListSharedBudgetsAndInvalidState(test *testing.T) {
	raw := `[{}]`
	list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	if err := list.start(0, len(raw)); !errors.Is(err, ErrIntegrity) {
		test.Fatal("active list accepted a second span", err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	for _, input := range []*legacyNormalizationInput{
		{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes},
		{ctx: context.Background(), source: source, loads: 2},
		{ctx: context.Background(), source: source, fields: LegacyNormalizationStepEvents * len(legacyContentBlockNativeFields.fields)},
	} {
		position, reads := list.position, len(buffer.reads)
		complete, err := list.step(context.Background(), input)
		if err != nil || complete || list.position != position || len(buffer.reads) != reads || input.workBytes > ConversationManifestChunkBytes {
			test.Fatal("block list ignored an exhausted shared budget", complete, err, input.workBytes, input.loads)
		}
	}
	for _, fixture := range []struct {
		from int
		to   int
		heap docRangeHeaderHeapEstimate
	}{
		{from: -1, to: 1}, {from: 0, to: 0},
		{from: 0, to: 1, heap: docRangeHeaderHeapEstimate{bytes: -1}},
		{from: 0, to: 1, heap: docRangeHeaderHeapEstimate{bytes: 1}},
		{from: 0, to: 1, heap: docRangeHeaderHeapEstimate{err: context.Canceled}},
	} {
		if _, err := newLegacyNativeBlockList(nil, fixture.heap, fixture.from, fixture.to); err == nil {
			test.Fatal("block list accepted invalid source bounds or prior heap", fixture)
		}
	}
	if complete, err := list.step(context.Background(), &legacyNormalizationInput{ctx: context.Background(), source: source, unbounded: true}); complete || !errors.Is(err, ErrIntegrity) {
		test.Fatal("block list accepted an unbounded caller input", complete, err)
	}
}
