package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyCanonicalBlocksHandoffReviewInitial() string {
	return `[{"type":"text","text":"visible"},{"type":"` + strings.Repeat("t", 2*LegacyCanonicalEncodingBytes+37) +
		`","text":"` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `"}]`
}

func legacyCanonicalBlocksHandoffReviewPagedMinimum(test *testing.T, values *legacyNativeBlockValues) docRangeHeaderHeapEstimate {
	test.Helper()
	var heap docRangeHeaderHeapEstimate
	if values == nil {
		return heap
	}
	heap.allocation(1, int64(unsafe.Sizeof(*values)))
	heap.include(legacyCanonicalIndependentPageHeap(test, &values.records))
	for ordinal := 0; ordinal < values.records.length(); ordinal++ {
		record := values.records.at(ordinal)
		if record == nil {
			test.Fatal("native block handoff lost a visible or hidden page slot", ordinal)
		}
		heap.allocation(int64(len(record.block.Type)), 1)
		heap.allocation(int64(len(record.block.Text)), 1)
	}
	if heap.err != nil {
		test.Fatal(heap.err)
	}
	return heap
}

func legacyCanonicalBlocksHandoffReviewLedger(test *testing.T, list *legacyNativeBlockList) (int64, int64) {
	test.Helper()
	if list == nil || list.phase != legacyNativeBlocksDone {
		test.Fatal("handoff ledger has no completed native block owner")
	}
	if list.paged {
		minimum := legacyCanonicalBlocksHandoffReviewPagedMinimum(test, list.data)
		minimum.allocation(1, int64(unsafe.Sizeof([]ContentBlock(nil))))
		var visible, total int64
		if list.data != nil {
			for ordinal := 0; ordinal < list.data.records.length(); ordinal++ {
				for _, excess := range list.data.records.at(ordinal).excess {
					if excess < 0 {
						test.Fatal("paged handoff ledger contains negative builder slack", ordinal, excess)
					}
					total += excess
					if ordinal < list.data.length() {
						visible += excess
					}
				}
			}
		}
		heap := list.heap()
		actual, err := list.backingExcess()
		if err != nil || actual != visible || list.visibleExcess != visible || list.totalExcess != total ||
			minimum.err != nil || heap.err != nil || heap.bytes-minimum.bytes != total {
			test.Fatal("paged visible, highwater, and source heap ledgers disagree", actual, visible, total, heap, minimum, err)
		}
		return visible, total
	}
	fields := len(legacyContentBlockNativeFields.fields)
	if len(list.excess) != cap(list.values())*fields {
		test.Fatal("handoff ledger does not cover every backing slot")
	}
	var visible, total int64
	for index, excess := range list.excess {
		if excess < 0 {
			test.Fatal("handoff ledger contains negative builder slack", index, excess)
		}
		total += excess
		if index < len(list.values())*fields {
			visible += excess
		}
	}
	minimum := legacyNativeBlocksTestHeap(test, list.values())
	heap := list.heap()
	actual, err := list.backingExcess()
	if err != nil || actual != visible || list.visibleExcess != visible || list.totalExcess != total ||
		heap.err != nil || heap.bytes-minimum.bytes != total {
		test.Fatal("native visible, full-capacity, and original heap ledgers disagree", actual, visible, total, heap, minimum, err)
	}
	return visible, total
}

func legacyCanonicalBlocksHandoffReviewNative(test *testing.T, arrays ...string) *legacyNativeObject {
	test.Helper()
	var decoded struct {
		Blocks []ContentBlock `json:"blocks"`
	}
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(decoded))
	native, err := newLegacyNativeObject(reflect.ValueOf(&decoded), &metadata, 1, false)
	if err != nil {
		test.Fatal(err)
	}
	raw := `{"blocks":` + strings.Join(arrays, `,"blocks":`) + `}`
	native.start(0, len(raw))
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	ctx := context.Background()
	var frame legacyJSONValueScanner
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		complete, err := native.step(ctx, input, &frame)
		legacyNativeBlocksReviewCheckQuantum(test, input)
		if err != nil || input.failure != nil {
			test.Fatal("scoped native handoff fixture failed", stepIndex, err, input.failure)
		}
		if complete {
			if len(native.blocks) != 1 || native.blocks[0] == nil {
				test.Fatal("scoped handoff fixture lost its native block owner")
			}
			return native
		}
	}
	test.Fatal("scoped handoff fixture exhausted its finite per-part allowance")
	return nil
}

func TestLegacyCanonicalBlocksHandoffReviewPublicHiddenBackingAndReset(test *testing.T) {
	initial := legacyCanonicalBlocksHandoffReviewInitial()
	for _, fixture := range []struct {
		name  string
		tail  []string
		reset bool
	}{
		{name: "hidden-type-and-text"},
		{name: "empty-releases-hidden-owner", tail: []string{`[]`}, reset: true},
		{name: "null-releases-hidden-owner", tail: []string{`null`}, reset: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			arrays := append([]string{initial, `[{}]`}, fixture.tail...)
			expected, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(initial))
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeBlocksTestDrain(test, expected, legacyNativeBlocksTestHeap(test, nil), initial)
			originalHeap := expected.heap()
			fields := len(legacyContentBlockNativeFields.fields)
			for index, excess := range expected.excess[fields : 2*fields] {
				if excess <= 0 {
					test.Fatal("giant hidden Type or Text did not retain actual builder slack", index, excess)
				}
			}
			for _, array := range arrays[1:] {
				prior := expected.heap()
				if err := expected.start(0, len(array)); err != nil {
					test.Fatal(err)
				}
				legacyNativeBlocksTestDrain(test, expected, prior, array)
			}
			_, expectedExcess := legacyCanonicalBlocksHandoffReviewLedger(test, expected)
			if len(fixture.tail) == 0 && (expected.heap() != originalHeap || expectedExcess <= 0) {
				test.Fatal("short duplicate did not preserve the original native list heap and hidden slack")
			}
			if fixture.reset && expectedExcess != 0 {
				test.Fatal("whole-list reset retained prior hidden builder ownership", expectedExcess)
			}
			raw := legacyNativeEventDocument("2", `{"kind":"message","seq":7,"role":"user","blocks":`+
				strings.Join(arrays, `,"blocks":`)+`}`)
			normalizer, source := legacyNativeScalarReviewStart(test, raw)
			parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
			var owner *legacyNativeBlockList
			var eventBody []byte
			for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
				if normalizer.nativeCache != nil {
					native := normalizer.nativeCache.object
					index := native.metadata.match("blocks")
					if index < 0 {
						test.Fatal("native event metadata no longer identifies blocks")
					}
					if index < len(native.blocks) && native.blocks[index] != nil {
						if owner != nil && owner != native.blocks[index] {
							test.Fatal("duplicate fields replaced rather than reset the native block helper")
						}
						owner = native.blocks[index]
					}
				}
				step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, source)
				if err != nil {
					test.Fatal("public normalizer rejected the block handoff fixture", stepIndex, err)
				}
				if len(step.Events) != 0 {
					if eventBody != nil || len(step.Events) != 1 || step.Events[0].Sequence != 7 || step.Events[0].Encoding == nil || owner == nil {
						test.Fatal("public handoff was not captured before draining its encoder", len(step.Events))
					}
					encoding := step.Events[0].Encoding
					root, ok := encoding.root.(map[string]any)
					if !ok {
						test.Fatal("public canonical handoff replaced the event root")
					}
					if !owner.paged || owner.values() != nil || owner.previous != nil || owner.payloads != nil || owner.excess != nil ||
						owner.payload != expected.payload || encoding.nativeOwner == nil || encoding.nativeOwner.blocks != owner.data {
						test.Fatal("public projection changed typed payload ownership or retained a materialized block array")
					}
					var blocks []ContentBlock
					if owner.data != nil {
						if root["blocks"] != owner.data || owner.data.length() != len(expected.values()) || owner.data.records.length() > cap(expected.values()) {
							test.Fatal("public projection copied its actual block pages or changed logical/highwater bounds")
						}
						blocks = make([]ContentBlock, owner.data.length())
						for ordinal := 0; ordinal < owner.data.records.length(); ordinal++ {
							record := owner.data.records.at(ordinal)
							if record == nil || record.block != expected.values()[:cap(expected.values())][ordinal] {
								test.Fatal("public projection changed a visible or hidden native block value", ordinal)
							}
							if ordinal < len(blocks) {
								blocks[ordinal] = record.block
							}
						}
					} else {
						var typed bool
						blocks, typed = root["blocks"].([]ContentBlock)
						if !typed || len(blocks) != 0 || cap(blocks) != 0 || len(expected.values()) != 0 {
							test.Fatal("null block source changed its canonical empty sentinel")
						}
					}
					_, actualExcess := legacyCanonicalBlocksHandoffReviewLedger(test, owner)
					if actualExcess != expectedExcess {
						test.Fatal("public handoff changed full-capacity builder ownership", actualExcess, expectedExcess)
					}
					if (owner.data == nil || owner.data.records.length() == 0) && (owner.payloads != nil || owner.excess != nil || owner.previous != nil ||
						owner.previousHeaps != nil || owner.previousExtra != nil || owner.payload.bytes != 0 || owner.object != nil ||
						owner.block != (ContentBlock{})) {
						test.Fatal("whole-list reset retained an old hidden owner or migration ledger")
					}
					if normalizer.nativeCache == nil || normalizer.nativeCache.decoded.Blocks != nil || normalizer.nativeCache.object.blocks != nil {
						test.Fatal("pending encoder was observed before native ownership release")
					}
					visibleBlocks := make([]ContentBlock, len(blocks))
					copy(visibleBlocks, blocks)
					baselineRoot := make(map[string]any, len(root))
					for key, value := range root {
						baselineRoot[key] = value
					}
					baselineRoot["blocks"] = visibleBlocks
					baseline, err := newLegacyCanonicalEncodingPending(context.Background(), baselineRoot)
					if err != nil {
						test.Fatal("visible-only canonical baseline failed", err)
					}
					visibleHeap := legacyNativeBlocksTestHeap(test, visibleBlocks)
					if expected.heap().bytes < visibleHeap.bytes {
						test.Fatal("native full-capacity expectation is smaller than visible-only storage")
					}
					projectedBlocks := legacyCanonicalBlocksHandoffReviewPagedMinimum(test, owner.data)
					if owner.data == nil {
						projectedBlocks = visibleHeap
					}
					projectedBlocks.include(docRangeHeaderHeapEstimate{bytes: expectedExcess})
					wantHeap := baseline.rootHeap
					wantHeap.bytes -= visibleHeap.bytes
					wantHeap.include(projectedBlocks)
					if wantHeap.err != nil || encoding.nativeOwner == nil || encoding.preparation == nil ||
						encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 {
						test.Fatal("public native handoff visited children or lost its uninspected whole owner", encoding.rootHeap, wantHeap)
					}
					fresh, err := newLegacyCanonicalEncodingPending(context.Background(), root)
					if err != nil {
						test.Fatal("fresh full-capacity canonical baseline failed", err)
					}
					projectedWithSlack := fresh.rootHeap
					projectedWithSlack.include(docRangeHeaderHeapEstimate{bytes: expectedExcess})
					if projectedWithSlack.err != nil || projectedWithSlack != wantHeap {
						test.Fatal("independent projected ledgers lost or duplicated full-capacity native slack", projectedWithSlack, wantHeap, expectedExcess)
					}
					nativeEvent := CIREvent{Kind: EventMessage, Seq: 7, Role: RoleUser, Blocks: owner.values()}
					fieldHeaps := make([]docRangeHeaderHeapEstimate, len(legacyEventNativeFields.fields))
					sourceBlocks := legacyCanonicalBlocksHandoffReviewPagedMinimum(test, owner.data)
					sourceBlocks.allocation(1, int64(unsafe.Sizeof([]ContentBlock(nil))))
					sourceBlocks.include(docRangeHeaderHeapEstimate{bytes: expectedExcess})
					if sourceBlocks != owner.heap() {
						test.Fatal("source block owner differs from independent fixed backing plus typed payload/slack", sourceBlocks, owner.heap())
					}
					fieldHeaps[legacyEventNativeFields.match("blocks")] = sourceBlocks
					lower, upper := legacyCanonicalNativeOwnershipBounds(test, nativeEvent, fieldHeaps)
					extra := legacyCanonicalNativeOwnershipProjection(test, nativeEvent, root)
					legacyCanonicalNativeOwnershipLedger(test, encoding, lower, upper, extra)
					legacyCanonicalNativeOwnershipHeldEvent(test, encoding.nativeOwner, nativeEvent)
					if encoding.rootHeap.bytes < wantHeap.bytes {
						test.Fatal("whole-owner accounting is smaller than the independent projected-plus-slack ledger", encoding.rootHeap, wantHeap)
					}
					baseline.release()
					fresh.release()
					eventBody, _ = legacyCanonicalNativeOwnershipDrain(test, encoding, lower, upper, extra)
					if encoding.root != nil || encoding.rootHeap != (docRangeHeaderHeapEstimate{}) ||
						encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) ||
						encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) || encoding.nativeOwner != nil || encoding.preparation != nil {
						test.Fatal("completed encoder retained its hidden block owner")
					}
				}
				if step.Complete {
					if eventBody == nil || step.Count != 1 {
						test.Fatal("public block fixture completed without exactly one captured handoff", step.Count)
					}
					legacyNativeScalarReviewCompare(test, raw, AssembleCanonicalDoc(step.Envelope, [][]byte{eventBody}), nil, true)
					return
				}
			}
			test.Fatal("public block handoff exhausted its finite per-part allowance")
		})
	}
}

func TestLegacyCanonicalBlocksHandoffReviewOwnerIdentityAndVisibleContract(test *testing.T) {
	initial := legacyCanonicalBlocksHandoffReviewInitial()
	var native *legacyNativeObject
	for _, duplicate := range []string{`[{},{}]`, `[{}]`} {
		native = legacyCanonicalBlocksHandoffReviewNative(test, initial, duplicate)
		list := native.blocks[0]
		visible, total := legacyCanonicalBlocksHandoffReviewLedger(test, list)
		if total <= 0 || len(list.values()) == 2 && visible != total || len(list.values()) == 1 && visible != 0 {
			test.Fatal("visible-contract fixture lost its giant visible or hidden builder allocations", duplicate, visible, total)
		}
		if extra, err := native.backingExcessBytes(nil); err != nil || extra != visible {
			test.Fatal("unfiltered old backingExcessBytes included hidden builder slack", extra, visible, err)
		}
		for _, projection := range []struct {
			name   string
			fields map[string]any
			want   int64
		}{
			{name: "typed-owner", fields: map[string]any{"blocks": list.values()}, want: total},
			{name: "omitted", fields: map[string]any{}},
			{name: "map-projection", fields: map[string]any{"blocks": []map[string]any{{"type": "text", "text": "visible"}}}, want: visible},
		} {
			test.Run(duplicate+"/"+projection.name, func(test *testing.T) {
				extra, err := native.canonicalBackingExcessBytes(projection.fields)
				if err != nil || extra != projection.want {
					test.Fatal("canonical handoff charged hidden slack without an emitted typed owner", extra, projection.want, err)
				}
				wantVisible := visible
				if _, emitted := projection.fields["blocks"]; !emitted {
					wantVisible = 0
				}
				extra, err = native.backingExcessBytes(projection.fields)
				if err != nil || extra != wantVisible {
					test.Fatal("old backingExcessBytes no longer has its visible-only contract", extra, wantVisible, err)
				}
			})
		}
	}
	list := native.blocks[0]
	values := list.values()
	foreign := make([]ContentBlock, len(values), cap(values))
	copy(foreign[:cap(foreign)], values[:cap(values)])
	for _, fixture := range []struct {
		name   string
		values []ContentBlock
	}{
		{name: "equal-values-foreign-backing", values: foreign},
		{name: "same-backing-different-capacity", values: values[:len(values):len(values)]},
		{name: "same-backing-different-length", values: values[:cap(values)]},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			extra, err := native.canonicalBackingExcessBytes(map[string]any{"blocks": fixture.values})
			if extra != 0 || !errors.Is(err, ErrIntegrity) {
				test.Fatal("canonical handoff accepted mismatched positive-capacity ownership", extra, err)
			}
		})
	}
}

func TestLegacyCanonicalBlocksHandoffReviewZeroCapacityOwnership(test *testing.T) {
	for _, reset := range []string{`[]`, `null`} {
		test.Run(reset, func(test *testing.T) {
			native := legacyCanonicalBlocksHandoffReviewNative(test, reset)
			list := native.blocks[0]
			visible, total := legacyCanonicalBlocksHandoffReviewLedger(test, list)
			if cap(list.values()) != 0 || visible != 0 || total != 0 || (list.values() == nil) != (reset == `null`) {
				test.Fatal("zero-capacity native fixture changed empty/null semantics")
			}
			for _, fixture := range []struct {
				name     string
				values   []ContentBlock
				accepted bool
			}{
				{name: "nil-without-backing", accepted: true},
				{name: "empty-without-backing", values: []ContentBlock{}, accepted: true},
				{name: "forged-empty-positive-capacity", values: make([]ContentBlock, 0, 1)},
			} {
				test.Run(fixture.name, func(test *testing.T) {
					fields := map[string]any{"blocks": fixture.values}
					extra, err := native.canonicalBackingExcessBytes(fields)
					if extra != 0 || fixture.accepted && err != nil || !fixture.accepted && !errors.Is(err, ErrIntegrity) {
						test.Fatal("zero-capacity ownership confused nil/empty headers with forged backing", extra, err)
					}
					if extra, err := native.backingExcessBytes(fields); extra != 0 || err != nil {
						test.Fatal("zero-capacity guard changed old visible-only accounting", extra, err)
					}
				})
			}
		})
	}
}
