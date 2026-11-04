package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeBlocksReviewHeap(test *testing.T, values []ContentBlock) docRangeHeaderHeapEstimate {
	test.Helper()
	heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(values))
	if err != nil {
		test.Fatal(err)
	}
	return heap
}

func legacyNativeBlocksReviewCheckQuantum(test *testing.T, input *legacyNormalizationInput) {
	test.Helper()
	if input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.fields > 1024 || input.unbounded {
		test.Fatal("native block review exceeded a shared quantum", input.workBytes, input.loads, input.fields, input.unbounded)
	}
	for _, page := range input.pages {
		clear(page)
	}
}

func legacyNativeBlocksReviewStep(test *testing.T, list *legacyNativeBlockList, source LegacyByteSource, fields int) (bool, *legacyNormalizationInput, error) {
	test.Helper()
	ctx := context.Background()
	input := &legacyNormalizationInput{ctx: ctx, source: source, fields: fields}
	complete, err := list.step(ctx, input)
	legacyNativeBlocksReviewCheckQuantum(test, input)
	if input.failure != nil {
		return complete, input, input.failure
	}
	return complete, input, err
}

func legacyNativeBlocksReviewDrain(test *testing.T, list *legacyNativeBlockList, raw string) error {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := buffer.input()
	for range 256 {
		complete, _, err := legacyNativeBlocksReviewStep(test, list, source, 0)
		if err != nil || complete {
			return err
		}
	}
	test.Fatal("native block review exhausted its finite step allowance")
	return ErrIntegrity
}

func TestLegacyNativeBlocksReviewStandaloneDepthOracle(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		depth    int
		accepted bool
	}{
		{name: "native-maximum", depth: 9998, accepted: true},
		{name: "one-container-too-deep", depth: 9999},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := `[{"unknown":` + strings.Repeat("[", fixture.depth) + `0` + strings.Repeat("]", fixture.depth) + `}]`
			var expected []ContentBlock
			expectedErr := json.Unmarshal([]byte(raw), &expected)
			if (expectedErr == nil) != fixture.accepted {
				test.Fatal("standalone depth fixture changed its native boundary", expectedErr)
			}
			list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
			if err != nil {
				test.Fatal(err)
			}
			actualErr := legacyNativeBlocksReviewDrain(test, list, raw)
			if (actualErr == nil) != (expectedErr == nil) {
				test.Fatal("detached block helper disagrees with native JSON depth", actualErr, expectedErr)
			}
			if actualErr == nil && !reflect.DeepEqual(list.values(), expected) {
				test.Fatal("maximum-depth ignored value changed the decoded block", list.values(), expected)
			}
		})
	}
}

func TestLegacyNativeBlocksReviewWrappedDepthOracle(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		depth    int
		accepted bool
	}{
		{name: "native-maximum", depth: 9997, accepted: true},
		{name: "one-container-too-deep", depth: 9998},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := `{"blocks":[{"unknown":` + strings.Repeat("[", fixture.depth) + `0` + strings.Repeat("]", fixture.depth) + `}]}`
			var actual, expected struct {
				Blocks []ContentBlock `json:"blocks"`
			}
			expectedErr := json.Unmarshal([]byte(raw), &expected)
			if (expectedErr == nil) != fixture.accepted {
				test.Fatal("wrapped depth fixture changed its native boundary", expectedErr)
			}
			metadata := newLegacyNativeObjectFields(reflect.TypeOf(actual))
			native, err := newLegacyNativeObject(reflect.ValueOf(&actual), &metadata, 1, false)
			if err != nil {
				test.Fatal(err)
			}
			native.start(0, len(raw))
			buffer := &legacySourceProofBuffer{body: []byte(raw)}
			source := buffer.input()
			ctx := context.Background()
			var frame legacyJSONValueScanner
			var actualErr error
			stopped := false
			for range 256 {
				input := &legacyNormalizationInput{ctx: ctx, source: source}
				complete, err := native.step(ctx, input, &frame)
				legacyNativeBlocksReviewCheckQuantum(test, input)
				if input.failure != nil {
					test.Fatal("wrapped depth fixture lost authenticated source access", input.failure)
				}
				if err != nil || complete {
					actualErr, stopped = err, true
					break
				}
			}
			if !stopped {
				test.Fatal("wrapped depth fixture exhausted its finite step allowance")
			}
			if len(native.blocks) != 1 || native.blocks[0] == nil || native.blocks[0].parentDepth != 2 {
				test.Fatal("wrapped depth fixture did not delegate with both enclosing containers")
			}
			if (actualErr == nil) != (expectedErr == nil) {
				test.Fatal("wrapped block helper disagrees with native JSON depth", actualErr, expectedErr)
			}
			if actualErr == nil && !reflect.DeepEqual(actual, expected) {
				test.Fatal("maximum-depth wrapped value changed the decoded block", actual, expected)
			}
		})
	}
}

func TestLegacyNativeBlocksReviewPrefixChildTransitionReauthenticatesSource(test *testing.T) {
	raw := `[{"type":"text","text":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}]`
	for _, fixture := range []struct {
		name   string
		offset int
	}{
		{name: "already-decoded-member", offset: strings.Index(raw, `"type":"text"`) + len(`"type":"`)},
		{name: "pending-member", offset: strings.Index(raw, `"text":"`) + len(`"text":"`)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
			if err != nil {
				test.Fatal(err)
			}
			buffer := &legacySourceProofBuffer{body: []byte(raw)}
			source := buffer.input()
			limit := LegacyNormalizationStepEvents * len(legacyContentBlockNativeFields.fields)
			complete, input, err := legacyNativeBlocksReviewStep(test, list, source, limit-1)
			if err != nil || complete || input.fields != limit || input.loads != 1 || input.workBytes >= LegacyCanonicalEncodingBytes {
				test.Fatal("prefix child did not yield at its first decoded member", complete, err, input.fields, input.loads, input.workBytes)
			}
			if list.object == nil || !list.object.prefix || list.object.parentDepth != list.parentDepth+1 ||
				list.position != list.childStart || list.object.position <= list.childStart ||
				list.block != (ContentBlock{Type: "text"}) || list.count != 0 {
				test.Fatal("source transition fixture did not retain an unapplied prefix child")
			}
			position, count, block := list.object.position, list.count, list.block
			reads := len(buffer.reads)
			if fixture.offset < 0 || fixture.offset >= ConversationManifestChunkBytes || fixture.offset >= len(buffer.body) {
				test.Fatal("source transition mutation escaped the authenticated first part", fixture.offset)
			}
			buffer.body[fixture.offset] = 'y'
			complete, input, err = legacyNativeBlocksReviewStep(test, list, source, 0)
			if complete || !errors.Is(err, ErrPreparedDocRangeObject) || !errors.Is(input.failure, ErrPreparedDocRangeObject) {
				test.Fatal("prefix child trusted a previous quantum's source authentication", complete, err, input.failure)
			}
			if len(buffer.reads) != reads+1 || buffer.reads[reads][0] != 0 || buffer.reads[reads][1] != ConversationManifestChunkBytes {
				test.Fatal("resumed prefix child did not re-read its current authenticated part", buffer.reads)
			}
			if input.workBytes != 0 || input.fields != 0 || list.object == nil || list.object.position != position ||
				list.count != count || list.block != block {
				test.Fatal("failed reauthentication advanced child decoding or transferred block ownership")
			}
		})
	}
}

func TestLegacyNativeBlocksReviewSeedSharedActionBudget(test *testing.T) {
	const count = 2048
	raw := `null`
	prior := make([]ContentBlock, 0, count)
	list, err := newLegacyNativeBlockList(prior, legacyNativeBlocksReviewHeap(test, prior), 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := buffer.input()
	limit := LegacyNormalizationStepEvents * len(legacyContentBlockNativeFields.fields)
	if limit != 128 || limit > 1024 {
		test.Fatal("native block seed action fence changed", limit)
	}
	for _, fields := range []int{limit, 1024} {
		complete, input, err := legacyNativeBlocksReviewStep(test, list, source, fields)
		if err != nil || complete || list.seeded != 0 || list.position != 0 || list.payload.bytes != 0 ||
			input.fields != fields || input.workBytes != 0 || input.loads != 0 || len(buffer.reads) != 0 {
			test.Fatal("exhausted shared actions allowed seed progress or source decoding", complete, err, list.seeded, input.fields)
		}
	}
	complete, input, err := legacyNativeBlocksReviewStep(test, list, source, limit-1)
	if err != nil || complete || list.seeded != 1 || input.fields != limit || list.position != 0 || len(buffer.reads) != 0 {
		test.Fatal("seeding ignored the caller's one remaining shared action", complete, err, list.seeded, input.fields)
	}
	for range 64 {
		before := list.seeded
		complete, input, err := legacyNativeBlocksReviewStep(test, list, source, 0)
		if err != nil {
			test.Fatal(err)
		}
		actions := list.seeded - before
		if actions < 0 || actions > 1024 || actions > limit || input.fields < actions || input.fields > limit {
			test.Fatal("seed slots escaped the shared structural-action ledger", actions, input.fields, limit)
		}
		if list.seeded < count && (list.position != 0 || len(buffer.reads) != 0 || complete) {
			test.Fatal("source decoding began before the hidden seed ledger was complete", list.seeded, list.position)
		}
		if complete {
			if list.seeded != count || list.values() != nil || len(buffer.reads) == 0 {
				test.Fatal("bounded seed completion changed whole-list null semantics", list.seeded, list.values())
			}
			return
		}
	}
	test.Fatal("seeded block list exhausted its finite action allowance")
}

func TestLegacyNativeBlocksReviewHiddenBuilderCapacityOwnership(test *testing.T) {
	raw := `[{"type":"head"},{"type":"` + strings.Repeat("t", 2*LegacyCanonicalEncodingBytes+37) +
		`","text":"` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `"}]`
	list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	if err := legacyNativeBlocksReviewDrain(test, list, raw); err != nil {
		test.Fatal(err)
	}
	var expected []ContentBlock
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal(err)
	}
	initialHeap := list.heap()
	initialExcess, err := list.backingExcess()
	minimum := legacyNativeBlocksReviewHeap(test, list.values())
	if err != nil || initialExcess <= 0 || initialHeap.err != nil || initialHeap.bytes-minimum.bytes != initialExcess {
		test.Fatal("giant hidden-slot fixture did not retain measurable builder capacity", initialHeap, minimum, initialExcess, err)
	}
	fields := len(legacyContentBlockNativeFields.fields)
	payloads := append([]docRangeHeaderHeapEstimate(nil), list.payloads[fields:2*fields]...)
	excesses := append([]int64(nil), list.excess[fields:2*fields]...)
	for index, excess := range excesses {
		if excess <= 0 {
			test.Fatal("giant Type or Text field did not retain capacity excess", index, excess)
		}
	}
	for _, fixture := range []struct {
		raw     string
		visible bool
	}{
		{raw: `[{}]`},
		{raw: `[null,{"type":null,"text":null}]`, visible: true},
		{raw: `[null]`},
		{raw: `[null,null]`, visible: true},
		{raw: `[{"text":null}]`},
		{raw: `[{},{}]`, visible: true},
		{raw: `[{}]`},
	} {
		if err := list.start(0, len(fixture.raw)); err != nil {
			test.Fatal(err)
		}
		if err := legacyNativeBlocksReviewDrain(test, list, fixture.raw); err != nil {
			test.Fatal(err)
		}
		if err := json.Unmarshal([]byte(fixture.raw), &expected); err != nil || !reflect.DeepEqual(list.values(), expected) {
			test.Fatal("same-helper duplicate changed native hidden-tail semantics", fixture.raw, err)
		}
		visible := int64(0)
		if fixture.visible {
			visible = initialExcess
		}
		actualExcess, err := list.backingExcess()
		if err != nil || actualExcess != visible || list.heap() != initialHeap ||
			!reflect.DeepEqual(list.payloads[fields:2*fields], payloads) || !reflect.DeepEqual(list.excess[fields:2*fields], excesses) {
			test.Fatal("same-helper duplicate lost hidden capacity provenance", fixture.raw, list.heap(), actualExcess, visible, err)
		}
	}
	test.Run("foreign-aggregate-reseed-fails-closed", func(test *testing.T) {
		next := `[null,null]`
		foreign, err := newLegacyNativeBlockList(list.values(), list.heap(), 0, len(next))
		if err != nil {
			if !errors.Is(err, ErrIntegrity) {
				test.Fatal("unsupported aggregate-only seed lost its integrity category", err)
			}
			return
		}
		buffer := &legacySourceProofBuffer{body: []byte(next)}
		complete, input, err := legacyNativeBlocksReviewStep(test, foreign, buffer.input(), 0)
		if complete || !errors.Is(err, ErrIntegrity) || input.failure != nil || input.loads != 0 || len(buffer.reads) != 0 ||
			foreign.position != 0 || foreign.count != 0 {
			test.Fatal("unsupported aggregate-only capacity seed did not fail before decoding", complete, err, foreign.position, foreign.count)
		}
		if list.heap() != initialHeap || !reflect.DeepEqual(list.payloads[fields:2*fields], payloads) ||
			!reflect.DeepEqual(list.excess[fields:2*fields], excesses) || !reflect.DeepEqual(list.values(), expected) {
			test.Fatal("rejected foreign seed changed the original helper's capacity ownership")
		}
	})
	for _, reset := range []string{`[]`, `null`} {
		if err := list.start(0, len(reset)); err != nil {
			test.Fatal(err)
		}
		if err := legacyNativeBlocksReviewDrain(test, list, reset); err != nil {
			test.Fatal(err)
		}
		emptyHeap := legacyNativeBlocksReviewHeap(test, list.values())
		actualExcess, err := list.backingExcess()
		if err != nil || actualExcess != 0 || list.heap() != emptyHeap || cap(list.values()) != 0 ||
			list.payloads != nil || list.excess != nil || list.payload.bytes != 0 || list.previous != nil ||
			list.previousHeaps != nil || list.previousExtra != nil || list.object != nil || list.owned || list.previousOwned {
			test.Fatal("whole-list replacement retained hidden capacity or migration ownership", reset, list.heap(), actualExcess, err)
		}
		if (list.values() == nil) != (reset == `null`) {
			test.Fatal("whole-list replacement changed native nil versus empty semantics", reset)
		}
		next := `[null,null]`
		if err := list.start(0, len(next)); err != nil {
			test.Fatal(err)
		}
		if err := legacyNativeBlocksReviewDrain(test, list, next); err != nil {
			test.Fatal(err)
		}
		actualExcess, err = list.backingExcess()
		if err != nil || actualExcess != 0 || !reflect.DeepEqual(list.values(), []ContentBlock{{}, {}}) {
			test.Fatal("growth resurrected capacity or contents after whole-list replacement", reset, actualExcess, err)
		}
		if reset == `[]` {
			for _, next := range []string{raw, `[{}]`} {
				if err := list.start(0, len(next)); err != nil {
					test.Fatal(err)
				}
				if err := legacyNativeBlocksReviewDrain(test, list, next); err != nil {
					test.Fatal(err)
				}
			}
			minimum := legacyNativeBlocksReviewHeap(test, list.values())
			if list.heap().bytes <= minimum.bytes || !reflect.DeepEqual(list.excess[fields:2*fields], excesses) {
				test.Fatal("whole-null release fixture did not restore hidden builder capacity")
			}
		}
	}
}
