package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func legacyNativeOutputReviewString(test *testing.T, prefix string) string {
	test.Helper()
	const payloadBytes = 5*ConversationManifestChunkBytes + 73
	var scalar strings.Builder
	for _, boundary := range []struct {
		offset int
		token  string
		split  int
	}{
		{offset: LegacyCanonicalEncodingBytes, token: `\u0061`, split: 3},
		{offset: ConversationManifestChunkBytes, token: `\uD83D\uDE42`, split: 8},
		{offset: 2 * ConversationManifestChunkBytes, token: "🙂", split: 2},
		{offset: 2*ConversationManifestChunkBytes + LegacyCanonicalEncodingBytes, token: "\xed\xa0\x80", split: 2},
		{offset: 3 * ConversationManifestChunkBytes, token: `\uD800x`, split: 4},
		{offset: 4 * ConversationManifestChunkBytes, token: "\xf0\x9f", split: 1},
		{offset: 5 * ConversationManifestChunkBytes, token: `\uD800\uD801\uDC00`, split: 8},
	} {
		padding := boundary.offset - len(prefix) - 1 - scalar.Len() - boundary.split
		if padding < 0 {
			test.Fatal("output boundary fixture overlaps an earlier token", boundary.offset)
		}
		scalar.WriteString(strings.Repeat("x", padding))
		scalar.WriteString(boundary.token)
	}
	scalar.WriteString(`\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uDC00`)
	scalar.WriteString("한<&>\xff\xc0\x80")
	const finalSurrogate = `\uD800`
	padding := payloadBytes - scalar.Len() - len(finalSurrogate)
	if padding < 0 {
		test.Fatal("output boundary fixture exceeded its five-part payload", scalar.Len())
	}
	scalar.WriteString(strings.Repeat("x", padding))
	scalar.WriteString(finalSurrogate)
	return `"` + scalar.String() + `"`
}

func TestLegacyNativeOutputReviewGiantStringsMatchTypedOracle(test *testing.T) {
	for _, key := range []string{`"output"`, `"OUTPUT"`, `"outp\u0075t"`} {
		test.Run(key, func(test *testing.T) {
			prefix := `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","seq":7,"call_id":"call",` + key + `:`
			value := legacyNativeOutputReviewString(test, prefix)
			if len(value)-2 != 5*ConversationManifestChunkBytes+73 {
				test.Fatal("output fixture lost its exact giant string length", len(value)-2)
			}
			var native string
			if err := json.Unmarshal([]byte(value), &native); err != nil {
				test.Fatal("output string lost native Unicode acceptance", err)
			}
			raw := []byte(prefix + value + `},{"kind":"turn","seq":1,"id":"first"},{"kind":"tool_result","seq":7,"call_id":"tie","output":"tie-last"}]}`)
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, true)
			var normalized CIRDocument
			if err := json.Unmarshal(got, &normalized); err != nil || len(normalized.Events) != 3 {
				test.Fatal("output normalization lost its surrounding events", err, len(normalized.Events))
			}
			if normalized.Events[0].ID != "first" || normalized.Events[1].Output != native || normalized.Events[2].Output != "tie-last" {
				test.Fatal("output string changed native bytes or stable event ordering")
			}
		})
	}
}

func TestLegacyNativeOutputReviewDuplicateOverwritesMatchNativeAndCanonicalBytes(test *testing.T) {
	prefix := `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","call_id":"call",`
	suffix := `}]}`
	giant := legacyNativeOutputReviewString(test, prefix+`"output":`)
	var nativeGiant string
	if err := json.Unmarshal([]byte(giant), &nativeGiant); err != nil {
		test.Fatal("duplicate output fixture lost native acceptance", err)
	}
	nextGiant := strings.Replace(giant, "x", "y", 1)
	for _, fixture := range []struct {
		name       string
		raw        string
		wantNative any
		rejected   bool
	}{
		{name: "folded-short-overwrites-giant", raw: prefix + `"output":` + giant + `,"OUTPUT":"fresh\nvalue"` + suffix, wantNative: "fresh\nvalue"},
		{name: "escaped-giant-overwrites-short", raw: prefix + `"output":"old","outp\u0075t":` + giant + suffix, wantNative: nativeGiant},
		{name: "null-clears-interface-string", raw: prefix + `"output":` + giant + `,"OUTPUT":null` + suffix},
		{name: "giant-overwrites-null", raw: prefix + `"output":null,"OUTPUT":` + giant + suffix, wantNative: nativeGiant},
		{name: "empty-string-replaces-giant", raw: prefix + `"output":` + giant + `,"outp\u0075t":""` + suffix, wantNative: ""},
		{name: "false-replaces-interface-string", raw: prefix + `"output":` + giant + `,"OUTPUT":false` + suffix, wantNative: false},
		{name: "number-replaces-interface-string", raw: prefix + `"output":` + giant + `,"OUTPUT":2.5` + suffix, wantNative: float64(2.5)},
		{name: "giant-replaces-interface-number", raw: prefix + `"output":2.5,"OUTPUT":` + giant + suffix, wantNative: nativeGiant},
		{name: "second-giant-replaces-first", raw: prefix + `"output":` + giant + `,"OUTPUT":` + nextGiant + suffix, wantNative: strings.Replace(nativeGiant, "x", "y", 1)},
		{name: "whole-array-reset-discards-giant", raw: prefix + `"output":` + giant + `}],"EVENTS":null,"events":[{"kind":"tool_result","call_id":"fresh","output":"fresh"}]}`, wantNative: "fresh"},
		{name: "overflow-before-giant-and-array-reset-is-sticky", raw: prefix + `"output":1e999,"OUTPUT":` + giant + `}],"events":[]}`, rejected: true},
		{name: "overflow-after-giant-field-and-array-reset-is-sticky", raw: prefix + `"output":` + giant + `,"OUTPUT":1e999,"output":"fixed"}],"events":[]}`, rejected: true},
		{name: "late-invalid-unicode-escape", raw: prefix + `"output":` + giant[:len(giant)-1] + `\u12xz"` + suffix, rejected: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			var native struct {
				Events []wire `json:"events"`
			}
			nativeErr := json.Unmarshal(raw, &native)
			if !fixture.rejected && (nativeErr != nil || len(native.Events) != 1 || native.Events[0].Output != fixture.wantNative) {
				test.Fatal("duplicate output fixture changed native overwrite semantics", nativeErr, len(native.Events))
			}
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, !fixture.rejected)
			if fixture.rejected {
				return
			}
			wantOutput := fixture.wantNative
			if wantOutput == nil {
				wantOutput = ""
			}
			var normalized CIRDocument
			if err := json.Unmarshal(got, &normalized); err != nil || len(normalized.Events) != 1 || normalized.Events[0].Output != wantOutput {
				test.Fatal("duplicate output changed native-to-canonical value semantics", err, len(normalized.Events))
			}
		})
	}
}

func TestLegacyNativeOutputReviewPreCanceledStepResumesSelectedValue(test *testing.T) {
	prefix := `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","output":`
	raw := []byte(prefix + legacyNativeOutputReviewString(test, prefix) + `}]}`)
	normalizer, input := legacyNativeScalarReviewStart(test, raw)
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	selected := false
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil || step.Complete || len(step.Events) != 0 {
			test.Fatal("output did not expose resumable native work", err)
		}
		if native := normalizer.native; native != nil && native.object.phase == legacyNativeObjectApply && native.object.field >= 0 &&
			native.object.metadata.fields[native.object.field].name == "output" {
			selected = true
			break
		}
	}
	if !selected {
		test.Fatal("giant output never exposed its selected value")
	}
	before, retainedErr := normalizer.RetainedBytes()
	reads := len(input.reads)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	step, err := legacyNativeScalarReviewStep(test, ctx, normalizer, input)
	legacyNativeEventZero(test, step)
	after, afterErr := normalizer.RetainedBytes()
	if !errors.Is(err, context.Canceled) || len(input.reads) != reads || retainedErr != nil || afterErr != nil || before != after {
		test.Fatal("pre-canceled output changed reads or retained state", err, before, after, retainedErr, afterErr)
	}
	got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
	legacyNativeScalarReviewCompare(test, raw, got, err, true)
}

func TestLegacyNativeOutputReviewFailuresStayAtomicAndKeepPrecedence(test *testing.T) {
	prefix := `{"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","output":`
	giant := legacyNativeOutputReviewString(test, prefix)
	for _, fixture := range []struct {
		name      string
		phase     string
		malformed bool
		corrupt   bool
		cancel    bool
	}{
		{name: "output-part-hash", phase: "output", corrupt: true},
		{name: "output-private-eof", phase: "output"},
		{name: "output-private-eof-cancel", phase: "output", cancel: true},
		{name: "output-part-hash-cancel", phase: "output", corrupt: true, cancel: true},
		{name: "rejected-private-eof", phase: "reject", malformed: true},
		{name: "rejected-private-eof-cancel", phase: "reject", malformed: true, cancel: true},
		{name: "end-zero-cancel", phase: "end-zero", cancel: true},
		{name: "end-one-cancel", phase: "end-one", cancel: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			value := giant
			if fixture.malformed {
				value = giant[:len(giant)-1] + `\u12xz"`
			}
			raw := []byte(prefix + value + `}]}`)
			input := &legacySourceProofBuffer{body: raw}
			source := legacyNormalizationPreparationInput(test, input, true)
			readSource := source.Read
			var normalizer *LegacyNormalizationDecoder
			var cancelStep context.CancelFunc
			triggered, failedAt := false, -1
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := readSource(ctx, offset, length)
				selected := false
				if normalizer != nil && normalizer.native != nil {
					native := normalizer.native.object
					selected = native.phase == legacyNativeObjectApply && native.field >= 0 && native.metadata.fields[native.field].name == "output"
				}
				matches := fixture.phase == "output" && selected && offset == ConversationManifestChunkBytes && length > 0 ||
					fixture.phase == "reject" && normalizer != nil && normalizer.phase == legacyNormalizationReject && offset == ConversationManifestChunkBytes && length > 0 ||
					fixture.phase == "end-zero" && offset == len(raw) && length == 0 ||
					fixture.phase == "end-one" && offset == len(raw) && length == 1
				if triggered || !matches {
					return body, err
				}
				triggered, failedAt = true, len(input.reads)
				if fixture.cancel {
					if cancelStep == nil {
						test.Fatal("output failure read escaped its active step")
					}
					cancelStep()
				}
				if fixture.corrupt {
					corrupt := bytes.Clone(body)
					corrupt[0] ^= 1
					return corrupt, err
				}
				return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
			}
			ctx, cancel := context.WithCancel(context.Background())
			normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
			cancel()
			if err != nil || normalizer == nil || len(input.reads) != 0 {
				test.Fatal("output failure fixture constructor read payloads", err, len(input.reads))
			}
			parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
			for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
				stepCtx, stop := context.WithCancel(context.Background())
				cancelStep = stop
				step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
				stop()
				if err == nil {
					if step.Complete {
						test.Fatal("output failure was swallowed", fixture.name, triggered)
					}
					continue
				}
				legacyNativeEventZero(test, step)
				want := error(ErrPreparedDocRangeObject)
				if fixture.cancel {
					want = context.Canceled
				}
				if !triggered || !errors.Is(err, want) || fixture.cancel && errors.Is(err, ErrPreparedDocRangeObject) || len(input.reads) != failedAt {
					test.Fatal("output failure lost precedence or continued reading", fixture.name, triggered, err, want)
				}
				reads := len(input.reads)
				repeated, repeatedErr := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
				legacyNativeEventZero(test, repeated)
				if !errors.Is(repeatedErr, want) || fixture.cancel && errors.Is(repeatedErr, ErrPreparedDocRangeObject) || len(input.reads) != reads {
					test.Fatal("output terminal failure was not sticky", repeatedErr, want)
				}
				return
			}
			test.Fatal("output failure fixture exhausted its fixed per-part progress limit", fixture.name)
		})
	}
}
