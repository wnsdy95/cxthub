package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

var legacyCanonicalBlocksOwnershipConstructors = []struct {
	name string
	new  func(context.Context, any) (*LegacyCanonicalEncoding, error)
}{
	{name: "eager", new: newLegacyCanonicalEncoding},
	{name: "pending", new: newLegacyCanonicalEncodingPending},
}

func assertLegacyCanonicalBlocksOwnershipReleased(test testing.TB, encoding *LegacyCanonicalEncoding, terminal error) {
	test.Helper()
	assertLegacyCanonicalNumberPendingReleased(test, encoding, terminal)
	if encoding.order != (legacyCanonicalKeyOrder{}) {
		test.Fatal("terminal typed blocks retained key ordering state")
	}
}

func TestLegacyCanonicalBlocksOwnershipLexicalMarshalParity(test *testing.T) {
	escaped := "\"\\/\b\f\n\r\t\x00\x01\x1f<&>\u2028\u2029한국🙂\ufffd"
	invalid := "\xff\x80\xc0\xaf\xed\xa0\x80\xf0\x9f\x92"
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{name: "zero-block", value: ContentBlock{}},
		{name: "both-fields-emitted", value: ContentBlock{Type: "not-an-enum"}},
		{name: "escaped-fields", value: ContentBlock{Type: escaped, Text: escaped}},
		{name: "invalid-utf8-fields", value: ContentBlock{Type: invalid + escaped, Text: escaped + invalid}},
		{name: "nil-array", value: []ContentBlock(nil)},
		{name: "empty-array", value: []ContentBlock{}},
		{name: "mixed-array", value: []ContentBlock{{}, {Type: escaped, Text: invalid}, {Type: invalid, Text: escaped}}},
		{name: "field-boundaries", value: ContentBlock{
			Type: strings.Repeat("t", LegacyCanonicalEncodingBytes-3) + "🙂" + invalid + escaped,
			Text: strings.Repeat("x", 2*LegacyCanonicalEncodingBytes-3) + "🙂" + escaped + invalid,
		}},
		{name: "escaped-multiple-chunks", value: []ContentBlock{{
			Type: strings.Repeat(escaped+invalid, 2048),
			Text: strings.Repeat(invalid+escaped, 4096),
		}, {Type: "text", Text: "after the long block"}}},
	} {
		for _, constructor := range legacyCanonicalBlocksOwnershipConstructors {
			test.Run(fixture.name+"/"+constructor.name, func(test *testing.T) {
				want, err := json.Marshal(legacyCanonicalBlocksProjectionForTest(fixture.value))
				if err != nil {
					test.Fatal(err)
				}
				before, err := json.Marshal(fixture.value)
				if err != nil {
					test.Fatal(err)
				}
				encoding, err := constructor.new(context.Background(), fixture.value)
				if err != nil {
					test.Fatal("typed blocks were rejected", err)
				}
				got, _ := collectLegacyCanonicalEncoding(test, encoding)
				if !bytes.Equal(got, want) {
					test.Fatal("typed blocks differ from the old map-shaped lexical bytes", len(got), len(want))
				}
				assertLegacyCanonicalBlocksOwnershipReleased(test, encoding, nil)
				after, err := json.Marshal(fixture.value)
				if err != nil || !bytes.Equal(before, after) {
					test.Fatal("typed block encoding mutated its caller-owned input", err)
				}
			})
		}
	}
}

func TestLegacyCanonicalBlocksOwnershipRetainsCapacityTailStrings(test *testing.T) {
	for _, length := range []int{0, 1} {
		for _, wrapped := range []bool{false, true} {
			for _, constructor := range legacyCanonicalBlocksOwnershipConstructors {
				test.Run(fmt.Sprintf("len-%d/wrapped-%t/%s", length, wrapped, constructor.name), func(test *testing.T) {
					backing := []ContentBlock{
						{Type: "text", Text: strings.Repeat("v", 2*LegacyCanonicalEncodingBytes+17)},
						{Type: strings.Repeat("hidden-type", 8192), Text: strings.Repeat("hidden-text", 16384)},
						{Type: "middle-type", Text: "middle-text"},
						{Type: strings.Repeat("last-type", 16384), Text: strings.Repeat("last-text", 8192)},
					}
					shortBacking := append([]ContentBlock(nil), backing...)
					shortBacking[1] = ContentBlock{Type: "t", Text: "x"}
					shortBacking[3] = ContentBlock{Type: "t", Text: "x"}
					shortBacking = shortBacking[:len(backing):len(backing)]
					var value any = backing[:length]
					var shortValue any = shortBacking[:length]
					if wrapped {
						value = []any{map[string]any{"blocks": value}, []map[string]any{{"after": true}}}
						shortValue = []any{map[string]any{"blocks": shortValue}, []map[string]any{{"after": true}}}
					}
					encoding, err := constructor.new(context.Background(), value)
					if err != nil {
						test.Fatal(err)
					}
					shortEncoding, err := constructor.new(context.Background(), shortValue)
					if err != nil {
						test.Fatal(err)
					}
					var payloadBytes, growth int64
					for index, block := range backing {
						payloadBytes += int64(len(block.Type) + len(block.Text))
						growth += int64(len(block.Type) + len(block.Text) - len(shortBacking[index].Type) - len(shortBacking[index].Text))
					}
					minimum := 2 * (payloadBytes + int64(cap(backing))*int64(unsafe.Sizeof(ContentBlock{})))
					retained, err := encoding.RetainedBytes()
					if err != nil || encoding.rootHeap.bytes < minimum || retained < encoding.rootHeap.bytes {
						test.Fatal("typed block capacity or hidden type/text storage is uncharged", retained, encoding.rootHeap.bytes, minimum, err)
					}
					shortRetained, err := shortEncoding.RetainedBytes()
					if err != nil || encoding.rootHeap.bytes-shortEncoding.rootHeap.bytes != 2*growth || retained-shortRetained != 2*growth {
						test.Fatal("capacity-tail strings were omitted or counted more than once", retained, shortRetained, 2*growth, err)
					}
					want, err := json.Marshal(legacyCanonicalBlocksProjectionForTest(value))
					if err != nil {
						test.Fatal(err)
					}
					var first, shortFirst []byte
					if length != 0 {
						var complete bool
						first, complete, err = encoding.Next(context.Background())
						if err != nil || complete || len(first) == 0 || len(first) > LegacyCanonicalEncodingBytes || cap(first) > LegacyCanonicalEncodingBytes {
							test.Fatal("visible typed block did not yield bounded output", len(first), complete, err)
						}
						shortFirst, complete, err = shortEncoding.Next(context.Background())
						if err != nil || complete || !bytes.Equal(first, shortFirst) {
							test.Fatal("hidden tail strings changed active traversal", complete, err)
						}
						active, err := encoding.RetainedBytes()
						if err != nil || active < retained || active-retained > 32<<10 {
							test.Fatal("active typed frames lost the root or charged its aliased strings again", retained, active, err)
						}
						shortActive, err := shortEncoding.RetainedBytes()
						if err != nil || active-shortActive != 2*growth || encoding.rootHeap.bytes-shortEncoding.rootHeap.bytes != 2*growth {
							test.Fatal("active typed frames lost capacity-tail string ownership", active, shortActive, 2*growth, err)
						}
					}
					for _, candidate := range []*LegacyCanonicalEncoding{encoding, shortEncoding} {
						prefix := first
						if candidate == shortEncoding {
							prefix = shortFirst
						}
						rest, _ := collectLegacyCanonicalEncoding(test, candidate)
						got := append(prefix, rest...)
						if !bytes.Equal(got, want) {
							test.Fatal("capacity-tail ownership changed visible block traversal")
						}
						assertLegacyCanonicalBlocksOwnershipReleased(test, candidate, nil)
					}
				})
			}
		}
	}
}

type legacyCanonicalBlocksOwnershipCancellationContext struct {
	context.Context
	cancelWhen func() bool
}

func (ctx legacyCanonicalBlocksOwnershipCancellationContext) Err() error {
	if ctx.cancelWhen() {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestLegacyCanonicalBlocksOwnershipCancellationIsTerminal(test *testing.T) {
	block := ContentBlock{
		Type: strings.Repeat("type", LegacyCanonicalEncodingBytes),
		Text: strings.Repeat("text", LegacyCanonicalEncodingBytes),
	}
	backing := []ContentBlock{block, {Type: "hidden-type", Text: strings.Repeat("tail", LegacyCanonicalEncodingBytes)}}
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{name: "direct", value: block},
		{name: "array", value: backing[:1]},
		{name: "nested", value: map[string]any{
			"a":      []map[string]any{{"before": true}},
			"blocks": backing[:1],
			"z":      []any{map[string]any{"after": true}},
		}},
	} {
		for _, constructor := range legacyCanonicalBlocksOwnershipConstructors {
			for _, stage := range []string{"before-first", "after-yield", "during-text", "during-type", "finalization"} {
				test.Run(fixture.name+"/"+constructor.name+"/"+stage, func(test *testing.T) {
					canceled, cancel := context.WithCancel(context.Background())
					cancel()
					if encoding, err := constructor.new(canceled, fixture.value); encoding != nil || !errors.Is(err, context.Canceled) {
						test.Fatal("typed block constructor ignored cancellation", err)
					}
					encoding, err := constructor.new(context.Background(), fixture.value)
					if err != nil {
						test.Fatal(err)
					}
					ctx := context.Context(canceled)
					if stage == "after-yield" {
						chunk, complete, err := encoding.Next(context.Background())
						if err != nil || complete || len(chunk) == 0 || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
							test.Fatal("typed block fixture did not yield bounded output before cancellation", len(chunk), complete, err)
						}
					} else if stage != "before-first" {
						ctx = legacyCanonicalBlocksOwnershipCancellationContext{Context: context.Background(), cancelWhen: func() bool {
							if stage == "finalization" {
								return encoding.frames.length() == 0
							}
							target := block.Text
							if stage == "during-type" {
								target = block.Type
							}
							for position := 0; position < encoding.frames.length(); position++ {
								frame := encoding.frames.at(position)
								if frame.kind == 's' && frame.text == target && frame.position > 0 {
									return true
								}
							}
							return false
						}}
					}
					for step := 0; step < 64; step++ {
						chunk, complete, err := encoding.Next(ctx)
						if err != nil {
							if !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 || cap(chunk) != 0 {
								test.Fatal("typed block cancellation exposed partial or successful output", len(chunk), complete, err)
							}
							assertLegacyCanonicalBlocksOwnershipReleased(test, encoding, context.Canceled)
							return
						}
						if complete || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
							test.Fatal("typed block traversal completed before its cancellation point", len(chunk), complete)
						}
					}
					test.Fatal("typed block cancellation exhausted its finite step allowance")
				})
			}
		}
	}
}

func TestLegacyCanonicalBlocksOwnershipSurroundingKeyCursors(test *testing.T) {
	blocks := []ContentBlock{{Type: "text", Text: strings.Repeat("<&>🙂\xff", 16384)}, {Type: "tail", Text: "last visible block"}}
	backing := append(append([]ContentBlock(nil), blocks...), ContentBlock{Type: "hidden", Text: "not serialized"})
	for _, depth := range []int{0, 1, 64, 65} {
		fields := map[string]any{
			"a-before":      []any{map[string]any{"z": false, "a": true}, []map[string]any{{"nested": map[string]any{"before": 1}}}},
			"b-blocks":      backing[:len(blocks)],
			"c-nil":         []ContentBlock(nil),
			"d-empty":       []ContentBlock{},
			"e-direct":      ContentBlock{Type: "direct", Text: "between arrays"},
			"f-siblings":    []any{map[string]any{"before": true}, blocks, map[string]any{"after": []map[string]any{{"z": 2, "a": 1}}}},
			"g-map-array":   []map[string]any{{"before": 1}, {"blocks": blocks, "after": map[string]any{"identity": "sibling"}}},
			"h-hidden-only": backing[:0],
			"i-after":       []any{blocks, []any{map[string]any{"after": blocks}}},
			"j-sentinel":    map[string]any{"z": "last", "a": []any{ContentBlock{}, map[string]any{"sentinel": true}}},
			"z-final":       []map[string]any{{"identity": "after every typed subtree"}},
		}
		var value any = fields
		for level := 0; level < depth; level++ {
			value = []any{map[string]any{"a": []ContentBlock{}, "m": value, "z": []map[string]any{{"level": level}}}}
		}
		for _, constructor := range legacyCanonicalBlocksOwnershipConstructors {
			test.Run(fmt.Sprintf("depth-%d/%s", depth, constructor.name), func(test *testing.T) {
				before, err := json.Marshal(value)
				if err != nil {
					test.Fatal(err)
				}
				want, err := json.Marshal(legacyCanonicalBlocksProjectionForTest(value))
				if err != nil {
					test.Fatal(err)
				}
				encoding, err := constructor.new(context.Background(), value)
				if err != nil {
					test.Fatal(err)
				}
				got, steps := collectLegacyCanonicalEncoding(test, encoding)
				if steps <= 1 || !bytes.Equal(got, want) {
					test.Fatal("typed subtrees disturbed surrounding map handles, sibling arrays, or resumable depth", steps, len(got), len(want))
				}
				assertLegacyCanonicalBlocksOwnershipReleased(test, encoding, nil)
				after, err := json.Marshal(value)
				if err != nil || !bytes.Equal(before, after) || backing[len(blocks)] != (ContentBlock{Type: "hidden", Text: "not serialized"}) {
					test.Fatal("typed subtree traversal mutated caller-owned maps or block capacity", err)
				}
			})
		}
	}
}

func TestLegacyCanonicalBlocksOwnershipContainerDepthParity(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		value    any
		wrappers int
	}{
		{name: "direct", value: ContentBlock{Type: "text", Text: "deep"}, wrappers: 9997},
		{name: "array", value: []ContentBlock{{Type: "text", Text: "deep"}}, wrappers: 9996},
		{name: "empty-array", value: []ContentBlock{}, wrappers: 9997},
		{name: "nil-array", value: []ContentBlock(nil), wrappers: 9998},
	} {
		for _, extra := range []int{0, 1} {
			value := fixture.value
			oldShape := legacyCanonicalBlocksProjectionForTest(value)
			for depth := 0; depth < fixture.wrappers+extra; depth++ {
				value = []any{value}
				oldShape = []any{oldShape}
			}
			for _, constructor := range legacyCanonicalBlocksOwnershipConstructors {
				test.Run(fmt.Sprintf("%s/extra-%d/%s", fixture.name, extra, constructor.name), func(test *testing.T) {
					reference, referenceErr := constructor.new(context.Background(), oldShape)
					encoding, err := constructor.new(context.Background(), value)
					if extra != 0 {
						if reference != nil || !errors.Is(referenceErr, ErrValidation) || encoding != nil || !errors.Is(err, ErrValidation) {
							test.Fatal("typed blocks changed the old container depth rejection", err, referenceErr)
						}
						return
					}
					if err != nil || referenceErr != nil {
						test.Fatal("typed blocks lost the old maximum accepted container depth", err, referenceErr)
					}
					want, err := json.Marshal(oldShape)
					if err != nil {
						test.Fatal(err)
					}
					for _, candidate := range []*LegacyCanonicalEncoding{encoding, reference} {
						got, _ := collectLegacyCanonicalEncoding(test, candidate)
						if !bytes.Equal(got, want) {
							test.Fatal("deep typed blocks changed lexical traversal", len(got), len(want))
						}
						assertLegacyCanonicalBlocksOwnershipReleased(test, candidate, nil)
					}
				})
			}
		}
	}
}
