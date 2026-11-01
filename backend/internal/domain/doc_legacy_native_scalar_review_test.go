package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

func legacyNativeScalarReviewStart(test *testing.T, raw []byte) (*LegacyNormalizationDecoder, *legacySourceProofBuffer) {
	test.Helper()
	input := &legacySourceProofBuffer{body: raw}
	ctx, cancel := context.WithCancel(context.Background())
	normalizer, err := NewLegacyNormalizationDecoder(ctx, legacyNormalizationPreparationInput(test, input, true))
	cancel()
	if err != nil || normalizer == nil || len(input.reads) != 0 {
		test.Fatal("scalar constructor failed or eagerly read source payloads", err, len(input.reads))
	}
	return normalizer, input
}

func legacyNativeScalarReviewStep(test *testing.T, ctx context.Context, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, wrappers ...func(context.Context) context.Context) (LegacyNormalizationStep, error) {
	test.Helper()
	before := len(input.reads)
	freshStep, cancel := context.WithCancel(ctx)
	selected := context.Context(freshStep)
	for _, wrap := range wrappers {
		selected = wrap(selected)
	}
	step, err := normalizer.Step(selected)
	cancel()
	loads := 0
	for _, read := range input.reads[before:] {
		if read[0] == len(input.body) && (read[1] == 0 || read[1] == 1) {
			continue
		}
		if read[0] < 0 || read[0] >= len(input.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(input.body)-read[0]) {
			test.Fatal("selected scalar bypassed authenticated full-part reads", read)
		}
		loads++
	}
	if loads > 2 {
		test.Fatal("selected scalar consumed more than two payload parts in one step", loads)
	}
	if len(step.Events) > LegacyNormalizationStepEvents {
		test.Fatal("selected scalar exceeded the existing event batch", len(step.Events))
	}
	if retained, retainedErr := normalizer.RetainedBytes(); retainedErr != nil || retained <= 0 {
		test.Fatal("selected scalar lost retained-byte accounting", retained, retainedErr)
	}
	if err != nil {
		legacyNativeEventZero(test, step)
	}
	return step, err
}

func legacyNativeScalarReviewDrain(test *testing.T, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer) ([]byte, error) {
	test.Helper()
	type materializedEvent struct {
		sequence int
		body     []byte
	}
	var events []materializedEvent
	parts := (len(input.body) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil {
			return nil, err
		}
		if step.Reset {
			events = nil
		}
		for _, event := range step.Events {
			body, err := legacyNormalizationTestBytes(context.Background(), event)
			if err != nil {
				test.Fatal("emitted scalar encoding retained a canceled step context", err)
			}
			events = append(events, materializedEvent{sequence: event.Sequence, body: body})
		}
		if !step.Complete {
			continue
		}
		if len(events) != step.Count || !json.Valid(step.Envelope) {
			test.Fatal("selected scalar event stream disagrees with final count or envelope", len(events), step.Count)
		}
		var physical int64
		for _, read := range input.reads {
			physical += int64(read[1])
		}
		if physical > 8*int64(len(input.body))+2 {
			test.Fatal("selected scalar exceeded the aggregate eightfold source read bound", physical, len(input.body))
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].sequence < events[right].sequence })
		bodies := make([][]byte, len(events))
		for ordinal, event := range events {
			bodies[ordinal] = event.body
		}
		return AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(bodies, []byte(","))}), nil
	}
	test.Fatal("selected scalar exhausted its fixed per-part progress limit")
	return nil, ErrIntegrity
}

func legacyNativeScalarReviewCompare(test *testing.T, raw, got []byte, err error, accepted bool) {
	test.Helper()
	want, wantErr := legacyNormalizationOracle(raw)
	if (wantErr == nil) != accepted {
		test.Fatal("scalar fixture lost its original typed acceptance", accepted, wantErr)
	}
	if !errors.Is(err, wantErr) || (err == nil) != accepted || !bytes.Equal(got, want) {
		test.Fatal("selected scalar changed typed acceptance or canonical output", len(got), len(want), err, wantErr)
	}
	if err != nil && errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("authenticated scalar rejection became a private-source failure", err)
	}
}

func TestLegacyNativeScalarReviewGiantStringsMatchTypedOracle(test *testing.T) {
	for _, field := range []struct {
		name string
		key  string
	}{
		{name: "redacted_summary", key: `"redacted_\u0073ummary"`},
		{name: "id", key: `"ID"`},
		{name: "ts", key: `"tſ"`},
	} {
		test.Run(field.name, func(test *testing.T) {
			prefix := `{"envelope":{"cir_version":"2"},"events":[{"kind":"reasoning","seq":7,` + field.key + `:"`
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
			} {
				padding := boundary.offset - len(prefix) - scalar.Len() - boundary.split
				if padding < 0 {
					test.Fatal("scalar boundary fixture overlaps an earlier token", boundary.offset)
				}
				scalar.WriteString(strings.Repeat("x", padding))
				scalar.WriteString(boundary.token)
			}
			scalar.WriteString(`\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uD800x\uDC00\uD800\uD801\uDC00`)
			scalar.WriteString("\ud55c<&>\xff\xc0\x80\xf0\x9f")
			scalar.WriteString(`\uD800`)
			raw := []byte(prefix + scalar.String() + `"},{"kind":"turn","seq":1,"id":"first"},{"kind":"turn","seq":7,"id":"tie-last"}]}`)
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, true)
		})
	}
}

func TestLegacyNativeScalarReviewDuplicateCasefoldAndNullOverwrites(test *testing.T) {
	giant := `"` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `\uD83D\uDE42\uD800"`
	for _, fixture := range []struct {
		name     string
		raw      string
		accepted bool
	}{
		{name: "summary-folded-null-keeps-giant", raw: `{"events":[{"kind":"reasoning","redacted_summary":` + giant + `,"redacted_ſummary":null,"REDACTED_SUMMARY":null}]}`, accepted: true},
		{name: "id-short-overwrite-then-escaped-null", raw: `{"events":[{"kind":"turn","id":` + giant + `,"ID":"fresh\nvalue","\u0069d":null}]}`, accepted: true},
		{name: "ts-folded-giant-overwrites-short-then-null", raw: `{"events":[{"kind":"turn","ts":"old","TS":` + giant + `,"tſ":null}]}`, accepted: true},
		{name: "empty-summary-releases-giant", raw: `{"events":[{"kind":"reasoning","redacted_summary":` + giant + `,"REDACTED_SUMMARY":"","redacted_\u0073ummary":null}]}`, accepted: true},
		{name: "whole-array-reset-discards-selected-scalar", raw: `{"events":[{"kind":"turn","id":` + giant + `}],"EVENTS":null,"events":[{"kind":"turn","id":"fresh"}]}`, accepted: true},
		{name: "native-type-error-survives-field-and-array-reset", raw: `{"events":[{"kind":"reasoning","redacted_summary":` + giant + `,"REDACTED_SUMMARY":false,"redacted_summary":"fixed"}],"events":[]}`},
		{name: "late-invalid-unicode-escape", raw: `{"events":[{"kind":"turn","id":"` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `\u12xz"}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
			legacyNativeScalarReviewCompare(test, raw, got, err, fixture.accepted)
		})
	}
}

func TestLegacyNativeScalarReviewPreCanceledStepResumesSelectedValue(test *testing.T) {
	raw := legacyNativeEventDocument("2", `{"kind":"turn","id":"`+strings.Repeat("x", 2*ConversationManifestChunkBytes+73)+`\uD83D\uDE42","ts":"after-scalar"}`)
	normalizer, input := legacyNativeScalarReviewStart(test, raw)
	maximumOffset, replayed := -1, false
	for stepIndex := 0; stepIndex < 64 && !replayed; stepIndex++ {
		before := len(input.reads)
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil || step.Complete || len(step.Events) != 0 {
			test.Fatal("selected scalar did not expose resumable semantic work", err)
		}
		for _, read := range input.reads[before:] {
			if read[0] < len(raw) && read[1] > 0 {
				replayed = replayed || read[0] < maximumOffset
				maximumOffset = max(maximumOffset, read[0])
			}
		}
	}
	if !replayed {
		test.Fatal("selected scalar never began its semantic source replay")
	}
	before, retainedErr := normalizer.RetainedBytes()
	reads := len(input.reads)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	step, err := legacyNativeScalarReviewStep(test, ctx, normalizer, input)
	legacyNativeEventZero(test, step)
	after, afterErr := normalizer.RetainedBytes()
	if !errors.Is(err, context.Canceled) || len(input.reads) != reads || retainedErr != nil || afterErr != nil || before != after {
		test.Fatal("pre-canceled selected scalar changed reads or retained state", err, before, after, retainedErr, afterErr)
	}
	got, err := legacyNativeScalarReviewDrain(test, normalizer, input)
	legacyNativeScalarReviewCompare(test, raw, got, err, true)
}

func TestLegacyNativeScalarReviewReadFailuresAndCancellationStayAtomic(test *testing.T) {
	raw := legacyNativeEventDocument("2", `{"kind":"reasoning","redacted_summary":"`+strings.Repeat("x", 2*ConversationManifestChunkBytes+73)+`"}`)
	for _, when := range []string{"native-part-hash", "native-private-eof", "native-read-cancel", "end-zero-cancel", "end-one-cancel"} {
		test.Run(when, func(test *testing.T) {
			input := &legacySourceProofBuffer{body: raw}
			source := legacyNormalizationPreparationInput(test, input, true)
			readSource := source.Read
			maximumOffset, failedAt := -1, -1
			replayed, triggered := false, false
			var cancelStep context.CancelFunc
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := readSource(ctx, offset, length)
				if offset < len(raw) && length > 0 {
					replayed = replayed || offset < maximumOffset
					maximumOffset = max(maximumOffset, offset)
				}
				nativeRead := replayed && offset == ConversationManifestChunkBytes && length > 0
				if !triggered && (strings.HasPrefix(when, "native-") && nativeRead ||
					when == "end-zero-cancel" && offset == len(raw) && length == 0 ||
					when == "end-one-cancel" && offset == len(raw) && length == 1) {
					triggered, failedAt = true, len(input.reads)
					if when == "native-part-hash" {
						corrupt := bytes.Clone(body)
						corrupt[0] ^= 1
						return corrupt, err
					}
					if strings.HasSuffix(when, "-cancel") {
						cancelStep()
					}
					return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
				}
				return body, err
			}
			ctx, cancel := context.WithCancel(context.Background())
			normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
			cancel()
			if err != nil || len(input.reads) != 0 {
				test.Fatal("failure fixture constructor read payloads", err)
			}
			for stepIndex := 0; stepIndex < 128; stepIndex++ {
				stepCtx, stop := context.WithCancel(context.Background())
				cancelStep = stop
				step, err := legacyNativeScalarReviewStep(test, stepCtx, normalizer, input)
				stop()
				if err == nil {
					if step.Complete {
						test.Fatal("selected scalar failure was swallowed", when, triggered)
					}
					continue
				}
				want := error(ErrPreparedDocRangeObject)
				if strings.HasSuffix(when, "-cancel") {
					want = context.Canceled
				}
				if !triggered || !errors.Is(err, want) || len(input.reads) != failedAt {
					test.Fatal("scalar failure lost precedence or continued reading", when, triggered, err, want)
				}
				reads := len(input.reads)
				repeated, repeatedErr := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
				legacyNativeEventZero(test, repeated)
				if !errors.Is(repeatedErr, want) || len(input.reads) != reads {
					test.Fatal("selected scalar terminal failure was not sticky", repeatedErr, want)
				}
				return
			}
			test.Fatal("selected scalar failure fixture exhausted its progress limit", when)
		})
	}
}

func TestLegacyNativeScalarReviewOversizedPendingEncodingRemainsCharged(test *testing.T) {
	const parkedHeapBytes = 16 << 20
	const payloadBytes = parkedHeapBytes + 73
	prefix := `{"events":[{"kind":"turn","id":"`
	suffix := `"}]}`
	raw := bytes.Repeat([]byte("x"), len(prefix)+payloadBytes+len(suffix))
	copy(raw, prefix)
	copy(raw[len(prefix)+payloadBytes:], suffix)
	normalizer, input := legacyNativeScalarReviewStart(test, raw)
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil || step.Complete {
			test.Fatal("oversized scalar failed before its pending encoding was observable", err)
		}
		if len(step.Events) == 0 {
			continue
		}
		if len(step.Events) != 1 || step.Events[0].Encoding == nil {
			test.Fatal("oversized scalar did not produce exactly one pending encoding", len(step.Events))
		}
		retained, err := step.Events[0].Encoding.RetainedBytes()
		if err != nil || retained < int64(payloadBytes) || retained <= parkedHeapBytes {
			test.Fatal("oversized scalar became falsely eligible for 16 MiB parked admission", retained, err)
		}
		test.Log("Accounting only: app-level permit retention, the 64 MiB spool cap, and peak RSS remain parent integration checks")
		return
	}
	test.Fatal("oversized scalar exhausted its fixed per-part progress limit")
}
