package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func legacyKeyOrderNestedFixture(count int) map[string]any {
	fields := make(map[string]any, count)
	for position := 0; position < count; position++ {
		name := fmt.Sprintf("key-%06d", count-position)
		fields[name] = []any{
			map[string]any{"identity": name, "nested": map[string]any{"value": position}},
			[]map[string]any{{"first": name}, nil, {"last": position}},
			[]any{nil, map[string]any{"tail": []any{map[string]any{"label": name}}}},
		}
	}
	return fields
}

func TestLegacyCanonicalKeyIntegrationSubtreeHandles(test *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		value := []any{
			legacyKeyOrderNestedFixture(17 + iteration%7),
			map[string]any{"after": []map[string]any{{"sentinel": "after every reordered subtree"}}},
		}
		before, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		assertLegacyCanonicalEncodingValue(test, value)
		after, err := json.Marshal(value)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("key ordering mutated caller-owned maps or arrays", iteration, err)
		}
	}
}

func TestLegacyCanonicalKeyIntegrationLongComparisonsYield(test *testing.T) {
	prefix := strings.Repeat("p", 3*LegacyCanonicalEncodingBytes)
	value := map[string]any{prefix + "z": 3, prefix + "a": 1, prefix + "m": 2}
	encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
	if err != nil {
		test.Fatal(err)
	}
	keys := encoding.mapKeys.at(0)
	if keys == nil || keys.sorted {
		test.Fatal("long-prefix ordering was completed in the constructor")
	}
	before, err := encoding.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	chunk, complete, err := encoding.Next(context.Background())
	if err != nil || complete || len(chunk) != 0 || cap(chunk) != 0 || encoding.frames.length() != 1 || encoding.keyWork == 0 {
		test.Fatal("long-prefix sorting did not yield before output", len(chunk), complete, err)
	}
	if after, err := encoding.RetainedBytes(); err != nil || after > before+4096 {
		test.Fatal("sort resumption retained another copy of key storage", before, after, err)
	}
	got, steps := collectLegacyCanonicalEncoding(test, encoding)
	want, err := json.Marshal(value)
	if err != nil || steps < 3 || !bytes.Equal(got, want) {
		test.Fatal("long-prefix continuation changed canonical bytes", steps, err)
	}
}

func TestLegacyCanonicalKeyIntegrationCancellationDiscardsSorting(test *testing.T) {
	encoding, err := newLegacyCanonicalEncoding(context.Background(), legacyKeyOrderNestedFixture(257))
	if err != nil {
		test.Fatal(err)
	}
	canceled := false
	ctx := legacyEventEncodingFinalizationContext{Context: context.Background(), completed: func() bool {
		if encoding.keyWork > 0 {
			canceled = true
		}
		return canceled
	}}
	chunk, complete, err := encoding.Next(ctx)
	if !errors.Is(err, context.Canceled) || complete || len(chunk) != 0 || encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) || encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) || encoding.root != nil {
		test.Fatal("sort cancellation exposed output or retained traversal", len(chunk), complete, err)
	}
	if _, _, err := encoding.Next(context.Background()); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled ordering resumed", err)
	}
}

func TestLegacyCanonicalKeyIntegrationPreparationKeepsPartialRecordPrivate(test *testing.T) {
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2}, Events: []CIREvent{
		{Kind: EventToolCall, Seq: 0, CallID: "synthetic", ToolName: "synthetic", Input: legacyKeyOrderNestedFixture(257)},
	}}
	raw, err := json.Marshal(document)
	if err != nil {
		test.Fatal(err)
	}
	canonical, original, err := legacyNormalizationPreparationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	zeroOutput := 0
	for quantum := 0; quantum < 1024 && work.phase == legacyNormalizationStage; quantum++ {
		before := work.files[0].size
		if complete, err := work.Step(context.Background()); err != nil || complete {
			test.Fatal("ordering step escaped its provisional stage", complete, err)
		}
		if work.phase == legacyNormalizationStage {
			if work.count != 0 || work.files[1].size != 0 {
				test.Fatal("pending key order earned a finalized event record")
			}
			if work.files[0].size == before {
				zeroOutput++
			}
		}
	}
	if work.phase != legacyNormalizationDecode || zeroOutput == 0 || work.count != 1 {
		test.Fatal("key ordering did not yield or finalize exactly once", zeroOutput, work.phase, work.count)
	}
	if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
		test.Fatal(err)
	}
	legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
}
