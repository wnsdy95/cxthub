package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeAnyDenseArray(count int) string {
	tokens := [...]string{"0", "1", "-7", "0.1", "12.5", "-0", "true", "false"}
	var array strings.Builder
	array.WriteByte('[')
	for ordinal := range count {
		if ordinal > 0 {
			array.WriteByte(',')
		}
		array.WriteString(tokens[ordinal%len(tokens)])
	}
	array.WriteByte(']')
	return array.String()
}

func legacyNativeAnyDenseReadCost(test *testing.T, source *legacySourceProofBuffer, before int) (int, int, int) {
	test.Helper()
	loads, authenticatedBytes, eofProbes := 0, 0, 0
	for _, read := range source.reads[before:] {
		if read[0] == len(source.body) && (read[1] == 0 || read[1] == 1) {
			eofProbes++
			continue
		}
		if read[0] < 0 || read[0] >= len(source.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(source.body)-read[0]) {
			test.Fatal("dense container bypassed full authenticated source-part reads", read)
		}
		loads++
		authenticatedBytes += read[1]
	}
	if loads > 2 {
		test.Fatal("dense container exceeded two current-source loads in one step", loads)
	}
	return loads, authenticatedBytes, eofProbes
}

func legacyNativeAnyDenseCanonicalCheck(test *testing.T, event []byte) {
	test.Helper()
	raw := legacyNativeEventDocument("2", string(event))
	want, err := legacyNormalizationOracle(raw)
	if err != nil {
		test.Fatal("dense fixture lost its original typed canonical acceptance", err)
	}
	normalizer, source := legacyNativeScalarReviewStart(test, raw)
	test.Logf("public constructor_current_source_loads=%d constructor_position=%d", len(source.reads), normalizer.position)
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	var bodies [][]byte
	var authenticatedBytes int64
	maximumLoads, maximumSourceBytes, maximumAdvance, eofProbes := 0, 0, 0, 0
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		position, reads := normalizer.position, len(source.reads)
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, source)
		if err != nil {
			test.Fatal("public normalizer rejected an accepted dense container", stepIndex, err)
		}
		loads, sourceBytes, probes := legacyNativeAnyDenseReadCost(test, source, reads)
		advance := normalizer.position - position
		if advance < 0 || advance > legacyNormalizationFrameQuantum {
			test.Fatal("dense container exceeded the original per-step framing quantum", stepIndex, advance)
		}
		authenticatedBytes += int64(sourceBytes)
		eofProbes += probes
		maximumLoads, maximumSourceBytes = max(maximumLoads, loads), max(maximumSourceBytes, sourceBytes)
		maximumAdvance = max(maximumAdvance, advance)
		if stepIndex == 0 {
			legacyNativeEventZero(test, step)
			if normalizer.position != 1 || loads != 1 || sourceBytes != min(ConversationManifestChunkBytes, len(raw)) {
				test.Fatal("first dense step stopped being a single bounded root-open read", normalizer.position, loads, sourceBytes)
			}
			test.Logf("public first_step_current_source_loads=%d first_step_authenticated_bytes=%d first_step_progress=%d first_step_events=%d first_step_complete=%t", loads, sourceBytes, advance, len(step.Events), step.Complete)
		}
		if step.Reset {
			bodies = nil
		}
		for _, emitted := range step.Events {
			body, err := legacyNormalizationTestBytes(context.Background(), emitted)
			if err != nil {
				test.Fatal("dense event encoding retained a canceled step context", err)
			}
			bodies = append(bodies, body)
		}
		if !step.Complete {
			continue
		}
		if len(bodies) != 1 || step.Count != len(bodies) || !json.Valid(step.Envelope) || eofProbes == 0 {
			test.Fatal("dense normalization lost its event, count, envelope, or authenticated EOF fence", len(bodies), step.Count, eofProbes)
		}
		actual := AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(bodies, []byte(","))})
		if !bytes.Equal(actual, want) {
			test.Fatal("dense container changed original typed canonical bytes", len(actual), len(want))
		}
		test.Logf("public steps=%d total_authenticated_bytes=%d source_bytes=%d source_authentication_amplification=%.2fx max_step_current_source_loads=%d max_step_authenticated_bytes=%d max_step_progress=%d eof_probes=%d", stepIndex+1, authenticatedBytes, len(raw), float64(authenticatedBytes)/float64(len(raw)), maximumLoads, maximumSourceBytes, maximumAdvance, eofProbes)
		if authenticatedBytes > 8*int64(len(raw)) {
			test.Log("remaining performance issue: dense-container source reauthentication exceeds 8x; the separate giant-string fixture guards remain unchanged")
		}
		return
	}
	test.Fatal("dense normalization exhausted the original finite per-part step allowance")
}

func legacyNativeAnyDenseWorkCheck(test *testing.T, raw []byte, fieldName string, leafCount int) wire {
	test.Helper()
	var expected wire
	if err := json.Unmarshal(raw, &expected); err != nil {
		test.Fatal("dense fixture lost its original encoding/json wire acceptance", err)
	}
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal("dense native event constructor failed", err)
	}
	native.reset(0, len(raw))
	native.object.prefix = true
	buffer := &legacySourceProofBuffer{body: raw}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	if len(buffer.reads) != 0 {
		test.Fatal("dense native constructor eagerly read current-source bytes")
	}
	fieldIndex := native.object.metadata.match(fieldName)
	if fieldIndex < 0 {
		test.Fatal("dense fixture field is absent from native metadata", fieldName)
	}
	test.Logf("native constructor_current_source_loads=%d constructor_position=%d", len(buffer.reads), native.object.progressPosition())
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	fieldLimit := LegacyNormalizationStepEvents * len(native.object.metadata.fields)
	valueLimit := legacyNativeAnyValueActions
	var frame legacyJSONValueScanner
	var authenticatedBytes int64
	maximumWork, maximumFields, maximumValues, maximumLoads, maximumSourceBytes := 0, 0, 0, 0, 0
	totalFields, totalValues := 0, 0
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		position, reads := native.object.progressPosition(), len(buffer.reads)
		_, complete, err := native.step(ctx, input, &frame)
		cancel()
		if err != nil || input.failure != nil {
			test.Fatal("generic native parser rejected an accepted dense event", stepIndex, err, input.failure)
		}
		advance := native.object.progressPosition() - position
		if input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
			input.fields < 0 || input.fields > fieldLimit || input.values < 0 || input.values > valueLimit || advance < 0 || advance > input.workBytes {
			test.Fatal("dense generic parser exceeded shared byte, source-load, typed-field, value-action, or progress limits", stepIndex, input.workBytes, input.loads, input.fields, input.values, input.unbounded, advance)
		}
		loads, sourceBytes, probes := legacyNativeAnyDenseReadCost(test, buffer, reads)
		if loads != input.loads || probes != 0 {
			test.Fatal("dense child source work escaped its owner's shared input", loads, input.loads, probes)
		}
		authenticatedBytes += int64(sourceBytes)
		totalFields += input.fields
		totalValues += input.values
		maximumWork, maximumFields = max(maximumWork, input.workBytes), max(maximumFields, input.fields)
		maximumValues = max(maximumValues, input.values)
		maximumLoads, maximumSourceBytes = max(maximumLoads, loads), max(maximumSourceBytes, sourceBytes)
		if retained, retainedErr := native.retained(); retainedErr != nil || retained <= 0 {
			test.Fatal("dense generic parser lost retained-byte accounting", retained, retainedErr)
		}
		if stepIndex == 0 {
			test.Logf("native first_step_work_bytes=%d first_step_typed_fields=%d first_step_value_actions=%d first_step_current_source_loads=%d first_step_authenticated_bytes=%d first_step_progress=%d first_step_complete=%t", input.workBytes, input.fields, input.values, loads, sourceBytes, advance, complete)
		}
		for _, page := range input.pages {
			clear(page)
		}
		if !complete {
			continue
		}
		if fieldIndex >= len(native.object.containers) || native.object.containers[fieldIndex] == nil || !native.object.containers[fieldIndex].finished {
			test.Fatal("dense fixture bypassed the actual generic container parser", fieldName)
		}
		if !reflect.DeepEqual(native.decoded, expected) {
			test.Fatal("dense generic parser changed original native types, values, or duplicate-map semantics")
		}
		if totalValues < leafCount {
			test.Fatal("dense leaves bypassed cooperative per-value action accounting", totalValues, leafCount)
		}
		if leafCount > valueLimit && stepIndex == 0 {
			test.Fatal("large dense fixture did not yield across shared byte/value quanta", leafCount, valueLimit)
		}
		test.Logf("native steps=%d total_authenticated_bytes=%d source_bytes=%d source_authentication_amplification=%.2fx max_step_work_bytes=%d total_typed_fields=%d max_step_typed_fields=%d typed_field_limit=%d total_value_actions=%d max_step_value_actions=%d value_action_limit=%d max_step_current_source_loads=%d max_step_authenticated_bytes=%d", stepIndex+1, authenticatedBytes, len(raw), float64(authenticatedBytes)/float64(len(raw)), maximumWork, totalFields, maximumFields, fieldLimit, totalValues, maximumValues, valueLimit, maximumLoads, maximumSourceBytes)
		return native.decoded
	}
	test.Fatal("dense native parsing exhausted the original finite per-part step allowance")
	return wire{}
}

func legacyNativeAnyDenseLeavesCheck(test *testing.T, values []any, count int) {
	test.Helper()
	expected := [...]any{float64(0), float64(1), float64(-7), float64(0.1), float64(12.5), math.Copysign(0, -1), true, false}
	if len(values) != count {
		test.Fatal("dense array changed leaf count", len(values), count)
	}
	for ordinal, value := range values {
		switch wanted := expected[ordinal%len(expected)].(type) {
		case float64:
			actual, ok := value.(float64)
			if !ok || math.Float64bits(actual) != math.Float64bits(wanted) {
				test.Fatalf("dense leaf %d changed native float64 type or bits: got %T bits=%016x want=%016x", ordinal, value, math.Float64bits(actual), math.Float64bits(wanted))
			}
		case bool:
			actual, ok := value.(bool)
			if !ok || actual != wanted {
				test.Fatalf("dense leaf %d changed native bool type or value: got %T", ordinal, value)
			}
		}
	}
}

func TestLegacyNativeAnyDenseOutputArrays(test *testing.T) {
	for _, count := range []int{1000, 10000, 100000} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			event := []byte(`{"kind":"tool_result","seq":7,"call_id":"dense","output":` + legacyNativeAnyDenseArray(count) + `}`)
			legacyNativeAnyDenseCanonicalCheck(test, event)
			actual := legacyNativeAnyDenseWorkCheck(test, event, "output", count)
			values, ok := actual.Output.([]any)
			if !ok {
				test.Fatalf("dense output lost its native []any type: %T", actual.Output)
			}
			legacyNativeAnyDenseLeavesCheck(test, values, count)
		})
	}
}

func TestLegacyNativeAnyDenseInputMapDuplicates(test *testing.T) {
	const count = 10000
	event := []byte(`{"kind":"tool_call","seq":7,"call_id":"dense","input":{"keep":true,"dense":` + legacyNativeAnyDenseArray(count) + `,"replace":{"old":1},"same":0},"INP\u0055T":{"replace":{"new":2},"same":true,"same":false},"inpu\u0074":{}}`)
	legacyNativeAnyDenseCanonicalCheck(test, event)
	actual := legacyNativeAnyDenseWorkCheck(test, event, "input", count)
	if len(actual.Input) != 4 || actual.Input["keep"] != true || actual.Input["same"] != false ||
		!reflect.DeepEqual(actual.Input["replace"], map[string]any{"new": float64(2)}) {
		test.Fatal("dense typed input lost outer-map merge, inner-map replacement, or duplicate-key last-value semantics")
	}
	values, ok := actual.Input["dense"].([]any)
	if !ok {
		test.Fatalf("dense typed input lost its untouched native []any child: %T", actual.Input["dense"])
	}
	legacyNativeAnyDenseLeavesCheck(test, values, count)
}
