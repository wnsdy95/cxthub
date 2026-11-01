package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unsafe"
)

func legacyCanonicalBlocksProjectionForTest(input any) any {
	switch value := input.(type) {
	case ContentBlock:
		return map[string]any{"text": value.Text, "type": value.Type}
	case []ContentBlock:
		if value == nil {
			return []map[string]any(nil)
		}
		blocks := make([]map[string]any, len(value))
		for index, block := range value {
			blocks[index] = legacyCanonicalBlocksProjectionForTest(block).(map[string]any)
		}
		return blocks
	case map[string]any:
		if value == nil {
			return map[string]any(nil)
		}
		object := make(map[string]any, len(value))
		for key, child := range value {
			object[key] = legacyCanonicalBlocksProjectionForTest(child)
		}
		return object
	case []any:
		if value == nil {
			return []any(nil)
		}
		values := make([]any, len(value))
		for index, child := range value {
			values[index] = legacyCanonicalBlocksProjectionForTest(child)
		}
		return values
	case []map[string]any:
		if value == nil {
			return []map[string]any(nil)
		}
		values := make([]map[string]any, len(value))
		for index, child := range value {
			values[index] = legacyCanonicalBlocksProjectionForTest(child).(map[string]any)
		}
		return values
	default:
		return input
	}
}

func assertLegacyCanonicalBlocksEncoding(test testing.TB, value any) {
	test.Helper()
	want, err := json.Marshal(legacyCanonicalBlocksProjectionForTest(value))
	if err != nil {
		test.Fatal(err)
	}
	before, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	for _, eager := range []bool{true, false} {
		encoding, err := newLegacyCanonicalEncodingWithNumbers(context.Background(), value, eager)
		if err != nil {
			test.Fatal(err)
		}
		if got := collectLegacyCanonicalNumberPending(test, encoding); !bytes.Equal(got, want) {
			test.Fatalf("typed block bytes differ: eager=%t got=%q want=%q", eager, got, want)
		}
	}
	after, err := json.Marshal(value)
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("typed block encoding changed its input", err)
	}
}

func TestLegacyCanonicalContentBlockParity(test *testing.T) {
	for _, candidate := range []struct {
		name  string
		value any
	}{
		{name: "zero", value: ContentBlock{}},
		{name: "text", value: ContentBlock{Type: "text", Text: "hello"}},
		{name: "escaping", value: ContentBlock{Type: "\"\\\x00<&>\u2028\xff", Text: "\b\f\n\r\t\x1f한국🙂\u2029\xff\x80"}},
		{name: "numeric-strings", value: ContentBlock{Type: "01", Text: "NaN"}},
		{name: "nil", value: []ContentBlock(nil)},
		{name: "empty", value: []ContentBlock{}},
		{name: "multiple", value: []ContentBlock{{Type: "text", Text: "first"}, {}, {Type: "other", Text: "last"}}},
		{name: "null-empty-siblings", value: []any{[]ContentBlock(nil), []ContentBlock{}, ContentBlock{}}},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			assertLegacyCanonicalBlocksEncoding(test, candidate.value)
		})
	}
}

func TestLegacyCanonicalContentBlockMapKeyIndexes(test *testing.T) {
	block := ContentBlock{Type: "text", Text: "typed"}
	blocks := []ContentBlock{block, {Type: "text", Text: "second"}}
	before := map[string]any{"z": map[string]any{"leaf": true}, "a": []map[string]any{{"z": 1, "a": nil}}}
	after := map[string]any{"z": []any{map[string]any{"z": false, "a": 2}}, "a": map[string]any{}}
	mixed := map[string]any{
		"a_before": before,
		"b_block":  block,
		"c_blocks": blocks,
		"d_nested": []any{before, block, blocks, after},
		"e_maps": []map[string]any{
			{"a_before": before, "b_blocks": blocks, "c_after": after},
			{"a_block": block, "b_after": after},
		},
		"f_null":  []ContentBlock(nil),
		"g_empty": []ContentBlock{},
		"h_block": block,
		"i_after": after,
	}
	assertLegacyCanonicalBlocksEncoding(test, []any{block, before, blocks, mixed, after, block})
	longKey := strings.Repeat("prefix", LegacyCanonicalEncodingBytes/6)
	assertLegacyCanonicalBlocksEncoding(test, map[string]any{
		longKey + "z": after,
		longKey + "a": blocks,
		longKey + "m": mixed,
	})
	backing := []any{blocks, map[string]any{"hidden": []ContentBlock{block}}}
	assertLegacyCanonicalBlocksEncoding(test, []any{backing[:1], after})
}

func TestLegacyCanonicalContentBlockFixedViewAndActions(test *testing.T) {
	blocks := make([]ContentBlock, 4096)
	for index := range blocks {
		blocks[index] = ContentBlock{Type: "text", Text: "small"}
	}
	encoding, err := newLegacyCanonicalEncoding(context.Background(), blocks)
	if err != nil {
		test.Fatal(err)
	}
	var frameStorage docRangeHeaderHeapEstimate
	frameStorage.allocation(1, int64(unsafe.Sizeof(legacyCanonicalEncodingFrame{})))
	if legacyCanonicalIndependentPageHeap(test, &encoding.frames) != frameStorage ||
		encoding.frames.single == nil || encoding.frames.inline != nil || encoding.frames.root != nil || encoding.frames.length() != 1 ||
		encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) || encoding.pendingKeysHeap.bytes != 0 {
		test.Fatal("fixed block views allocated map keys or miscounted fixed frame storage")
	}
	var result []byte
	var chunks, snapshots [][]byte
	var traversalStorage docRangeHeaderHeapEstimate
	traversalStorage.allocation(1, int64(unsafe.Sizeof([legacyCanonicalInlineValues]legacyCanonicalEncodingFrame{})))
	complete := false
	for step := 0; step < len(blocks) && !complete; step++ {
		chunk, done, err := encoding.Next(context.Background())
		if err != nil || len(chunk) == 0 || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			test.Fatal("typed block quantum failed or exceeded its byte limit", len(chunk), cap(chunk), err)
		}
		if step == 0 && (done || encoding.frames.at(0).position > legacyCanonicalEncodingActions) {
			test.Fatal("typed block structural work did not yield within the action budget")
		}
		if encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) || encoding.pendingKeysHeap.bytes != 0 || encoding.keyWork != 0 {
			test.Fatal("fixed block views entered dynamic key ordering")
		}
		if !done && (encoding.frames.length() > 3 || encoding.frames.single != nil || encoding.frames.inline == nil || encoding.frames.root != nil ||
			legacyCanonicalIndependentPageHeap(test, &encoding.frames) != traversalStorage) {
			test.Fatal("typed block traversal outgrew its prepared frame storage")
		}
		for position := 0; position < encoding.frames.length(); position++ {
			frame := encoding.frames.at(position)
			if frame.fields != (legacyCanonicalPages[legacyCanonicalKeyField]{}) {
				test.Fatal("fixed block view retained a dynamic key array")
			}
		}
		result = append(result, chunk...)
		chunks = append(chunks, chunk)
		snapshots = append(snapshots, bytes.Clone(chunk))
		complete = done
	}
	want, err := json.Marshal(legacyCanonicalBlocksProjectionForTest(blocks))
	if err != nil || !complete || !bytes.Equal(result, want) {
		test.Fatal("typed block action yields changed bytes or failed to terminate", err)
	}
	for index, chunk := range chunks {
		if !bytes.Equal(chunk, snapshots[index]) {
			test.Fatal("later typed block output overwrote an earlier chunk")
		}
	}
	chunks[0][0] ^= 0xff
	for index := 1; index < len(chunks); index++ {
		if !bytes.Equal(chunks[index], snapshots[index]) {
			test.Fatal("typed block output chunks share mutable storage")
		}
	}
	assertLegacyCanonicalNumberPendingReleased(test, encoding, nil)
}

func TestLegacyCanonicalContentBlockLargeStrings(test *testing.T) {
	for index, block := range []ContentBlock{
		{Type: "text", Text: strings.Repeat("a", 4*LegacyCanonicalEncodingBytes+17)},
		{Type: strings.Repeat("t", 4*LegacyCanonicalEncodingBytes+17), Text: "small"},
		{Type: strings.Repeat("<&>\u2028\xff", LegacyCanonicalEncodingBytes), Text: strings.Repeat("🙂\"\\\x00\u2029", LegacyCanonicalEncodingBytes)},
		{Type: "🙂" + strings.Repeat("b", LegacyCanonicalEncodingBytes), Text: strings.Repeat("a", LegacyCanonicalEncodingBytes-12)},
	} {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			assertLegacyCanonicalBlocksEncoding(test, []ContentBlock{block})
		})
	}
}

func TestLegacyCanonicalContentBlockRetainedOwners(test *testing.T) {
	shared := strings.Repeat("s", 2*LegacyCanonicalEncodingBytes)
	block := ContentBlock{Type: shared, Text: shared}
	backing := make([]ContentBlock, 1, 4)
	backing[0] = block
	backing[:cap(backing)][3] = ContentBlock{Type: strings.Repeat("t", 8192), Text: strings.Repeat("h", 16384)}
	for index, input := range []any{block, []ContentBlock(nil), []ContentBlock{}, backing[:0], backing} {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			var expected docRangeHeaderHeapEstimate
			wantFrames := 1
			switch value := input.(type) {
			case ContentBlock:
				expected.allocation(1, int64(unsafe.Sizeof(value)))
				expected.allocation(int64(len(value.Text)), 1)
				expected.allocation(int64(len(value.Type)), 1)
				wantFrames = 2
			case []ContentBlock:
				expected.allocation(1, int64(unsafe.Sizeof(value)))
				expected.allocation(int64(cap(value)), int64(unsafe.Sizeof(ContentBlock{})))
				for _, owner := range value[:cap(value)] {
					expected.allocation(int64(len(owner.Text)), 1)
					expected.allocation(int64(len(owner.Type)), 1)
				}
				if len(value) != 0 {
					wantFrames = 3
				}
			}
			encoding, err := newLegacyCanonicalEncoding(context.Background(), input)
			if err != nil {
				test.Fatal(err)
			}
			var frameStorage docRangeHeaderHeapEstimate
			frameStorage.allocation(1, int64(unsafe.Sizeof(legacyCanonicalEncodingFrame{})))
			if encoding.rootHeap.bytes != expected.bytes ||
				encoding.frames.single == nil || encoding.frames.inline != nil || encoding.frames.root != nil || encoding.frames.length() != 1 ||
				legacyCanonicalIndependentPageHeap(test, &encoding.frames) != frameStorage {
				test.Fatal("block owners, backing capacity, or fixed frame storage were miscounted", encoding.rootHeap.bytes, expected.bytes)
			}
			before, err := encoding.RetainedBytes()
			wantRetained := expected
			wantRetained.allocation(1, int64(unsafe.Sizeof(*encoding)))
			wantRetained.include(frameStorage)
			if err != nil || wantRetained.err != nil || before != wantRetained.bytes {
				test.Fatal("typed block retention omitted its owners", before, expected.bytes, err)
			}
			traversalStorage := frameStorage
			if wantFrames > 1 {
				traversalStorage = docRangeHeaderHeapEstimate{}
				traversalStorage.allocation(1, int64(unsafe.Sizeof([legacyCanonicalInlineValues]legacyCanonicalEncodingFrame{})))
			}
			traversalRetained := expected
			traversalRetained.allocation(1, int64(unsafe.Sizeof(*encoding)))
			traversalRetained.include(traversalStorage)
			for step := 0; step < 32; step++ {
				_, complete, err := encoding.Next(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				if complete {
					assertLegacyCanonicalNumberPendingReleased(test, encoding, nil)
					assertLegacyCanonicalBlocksEncoding(test, input)
					return
				}
				retained, err := encoding.RetainedBytes()
				if err != nil || traversalRetained.err != nil || retained != traversalRetained.bytes || encoding.frames.length() > wantFrames ||
					legacyCanonicalIndependentPageHeap(test, &encoding.frames) != traversalStorage {
					test.Fatal("block frames recharged aliased strings or grew beyond their depth", retained, traversalRetained.bytes, err)
				}
			}
			test.Fatal("typed block retention fixture did not terminate")
		})
	}
}

func TestLegacyCanonicalContentBlockDepthAndCycles(test *testing.T) {
	for _, candidate := range []struct {
		name     string
		leaf     any
		wrappers int
	}{
		{name: "block", leaf: ContentBlock{}, wrappers: 9997},
		{name: "array", leaf: []ContentBlock{{}}, wrappers: 9996},
		{name: "hidden-backing", leaf: make([]ContentBlock, 0, 1), wrappers: 9996},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			value := candidate.leaf
			for depth := 0; depth < candidate.wrappers; depth++ {
				value = []any{value}
			}
			assertLegacyCanonicalBlocksEncoding(test, value)
			if encoding, err := newLegacyCanonicalEncoding(context.Background(), []any{value}); encoding != nil || !errors.Is(err, ErrValidation) {
				test.Fatal("typed block exceeded the canonical container depth", err)
			}
		})
	}
	cycleMap := map[string]any{"a_block": ContentBlock{}}
	cycleMap["z_self"] = cycleMap
	cycleSlice := []any{[]ContentBlock{{}}, nil}
	cycleSlice[1] = cycleSlice
	hiddenCycle := make([]any, 1, 2)
	hiddenCycle[0] = []ContentBlock{{}}
	hiddenCycle[:cap(hiddenCycle)][1] = hiddenCycle
	for _, value := range []any{cycleMap, cycleSlice, hiddenCycle} {
		if encoding, err := newLegacyCanonicalEncoding(context.Background(), value); encoding != nil || !errors.Is(err, ErrValidation) {
			test.Fatal("typed block siblings bypassed cyclic container rejection", err)
		}
	}
}

func TestLegacyCanonicalContentBlockNumberRules(test *testing.T) {
	number := json.Number("1E+1000")
	blocks := []ContentBlock{{Type: "01", Text: "NaN"}}
	assertLegacyCanonicalBlocksEncoding(test, map[string]any{
		"a_blocks": blocks,
		"m_numbers": []any{
			json.Number(""), json.Number("-0"), &number, (*json.Number)(nil),
			int(9007199254740993), math.Copysign(0, -1), 1e-7, 1e21,
		},
		"z_block": ContentBlock{Type: "text", Text: "last"},
	})
	badNumber := json.Number("01")
	for _, invalid := range []any{badNumber, &badNumber, json.Number("+1"), math.NaN(), math.Inf(1)} {
		value := map[string]any{"a_blocks": blocks, "z_invalid": invalid}
		_, nativeErr := json.Marshal(legacyCanonicalBlocksProjectionForTest(value))
		encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
		if nativeErr == nil || encoding != nil || err == nil || err.Error() != nativeErr.Error() {
			test.Fatal("typed blocks changed eager native number errors", err, nativeErr)
		}
	}
	encoding, err := newLegacyCanonicalEncodingPending(context.Background(), map[string]any{"a_blocks": blocks, "z_invalid": badNumber})
	if err != nil {
		test.Fatal(err)
	}
	chunk, complete, err := encoding.Next(context.Background())
	if len(chunk) != 0 || complete || !errors.Is(err, ErrValidation) {
		test.Fatal("typed block bytes escaped before pending number validation", len(chunk), complete, err)
	}
	assertLegacyCanonicalNumberPendingReleased(test, encoding, err)
}

type legacyCanonicalBlocksInspectionContext struct {
	context.Context
	checks int
}

func (ctx *legacyCanonicalBlocksInspectionContext) Err() error {
	ctx.checks++
	if ctx.checks >= 8 {
		return context.Canceled
	}
	return ctx.Context.Err()
}

type legacyCanonicalBlocksProgressContext struct {
	context.Context
	encoding *LegacyCanonicalEncoding
	target   string
}

func (ctx legacyCanonicalBlocksProgressContext) Err() error {
	if ctx.encoding.frames.length() != 0 {
		frame := ctx.encoding.frames.at(ctx.encoding.frames.length() - 1)
		if frame.kind == 's' && frame.text == ctx.target && frame.position != 0 {
			return context.Canceled
		}
	}
	return ctx.Context.Err()
}

func TestLegacyCanonicalContentBlockCancellation(test *testing.T) {
	inspection := &legacyCanonicalBlocksInspectionContext{Context: context.Background()}
	if encoding, err := newLegacyCanonicalEncoding(inspection, make([]ContentBlock, 0, 128)); encoding != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("hidden typed block inspection skipped cancellation", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, stage := range []string{"canceled", "text", "type", "finalization"} {
		test.Run(stage, func(test *testing.T) {
			block := ContentBlock{Type: strings.Repeat("t", 4*LegacyCanonicalEncodingBytes), Text: strings.Repeat("x", 4*LegacyCanonicalEncodingBytes)}
			if stage == "finalization" {
				block = ContentBlock{}
			}
			encoding, err := newLegacyCanonicalEncoding(context.Background(), block)
			if err != nil {
				test.Fatal(err)
			}
			var ctx context.Context = canceled
			switch stage {
			case "text":
				ctx = legacyCanonicalBlocksProgressContext{Context: context.Background(), encoding: encoding, target: block.Text}
			case "type":
				ctx = legacyCanonicalBlocksProgressContext{Context: context.Background(), encoding: encoding, target: block.Type}
			case "finalization":
				ctx = legacyCanonicalFinalizationContext{Context: context.Background(), encoding: encoding}
			}
			for step := 0; step < 32; step++ {
				chunk, complete, err := encoding.Next(ctx)
				if err != nil {
					if len(chunk) != 0 || complete || !errors.Is(err, context.Canceled) {
						test.Fatal("typed block cancellation returned partial or completed output", len(chunk), complete, err)
					}
					assertLegacyCanonicalNumberPendingReleased(test, encoding, context.Canceled)
					return
				}
				if complete {
					test.Fatal("typed block encoding masked cancellation")
				}
			}
			test.Fatal("typed block fixture never observed cancellation")
		})
	}
}
