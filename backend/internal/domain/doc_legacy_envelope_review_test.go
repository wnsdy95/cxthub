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

func legacyEnvelopeReviewStart(test *testing.T, raw []byte) (*LegacyNormalizationDecoder, *legacySourceProofBuffer) {
	test.Helper()
	input := &legacySourceProofBuffer{body: raw}
	constructorCtx, cancel := context.WithCancel(context.Background())
	normalizer, err := NewLegacyNormalizationDecoder(constructorCtx, legacyNormalizationPreparationInput(test, input, true))
	cancel()
	if err != nil || normalizer == nil || len(input.reads) != 0 {
		test.Fatal("envelope constructor failed or eagerly loaded payloads", err, len(input.reads))
	}
	return normalizer, input
}

func legacyEnvelopeReviewStep(test *testing.T, ctx context.Context, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, small bool) (LegacyNormalizationStep, int64, error) {
	test.Helper()
	before := len(input.reads)
	stepCtx, cancel := context.WithCancel(ctx)
	step, err := normalizer.Step(stepCtx)
	cancel()
	loads := 0
	for _, read := range input.reads[before:] {
		if read[0] == len(input.body) && (read[1] == 0 || read[1] == 1) {
			continue
		}
		if read[0] < 0 || read[0] >= len(input.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(input.body)-read[0]) {
			test.Fatal("envelope bypassed authenticated full-part source reads", read)
		}
		loads++
	}
	if loads > 2 {
		test.Fatal("envelope step atomically reread more than two source parts", loads)
	}
	var physical int64
	for _, read := range input.reads {
		physical += int64(read[1])
	}
	if physical > 8*int64(len(input.body))+2 {
		test.Fatal("envelope exceeded the existing eightfold physical read bound", physical, len(input.body))
	}
	retained, retainedErr := normalizer.RetainedBytes()
	if retainedErr != nil || retained <= 0 || retained > 16<<20 {
		test.Fatal("bounded envelope fixture lost accounting or exceeded the original parked heap budget", retained, retainedErr)
	}
	if small && retained >= 64<<10 {
		test.Fatal("small envelope retained a giant unknown member or source page", retained)
	}
	if len(step.Events) != 0 || step.Count != 0 || !step.Complete && step.Envelope != nil {
		test.Fatal("envelope-only fixture exposed events or a provisional header")
	}
	if err != nil {
		legacyNormalizationFrameZero(test, step)
	}
	return step, retained, err
}

func legacyEnvelopeReviewDrain(test *testing.T, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, small bool, observe func(LegacyNormalizationStep, int64, [][2]int)) ([]byte, int, error) {
	test.Helper()
	parts := (len(input.body) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		before := len(input.reads)
		step, retained, err := legacyEnvelopeReviewStep(test, context.Background(), normalizer, input, small)
		if err != nil {
			reads := len(input.reads)
			repeated, _, repeatedErr := legacyEnvelopeReviewStep(test, context.Background(), normalizer, input, small)
			legacyNormalizationFrameZero(test, repeated)
			if !errors.Is(repeatedErr, err) || len(input.reads) != reads {
				test.Fatal("rejected envelope failure was not sticky", err, repeatedErr)
			}
			return nil, stepIndex + 1, err
		}
		if observe != nil {
			observe(step, retained, input.reads[before:])
		}
		if step.Complete {
			if !json.Valid(step.Envelope) {
				test.Fatal("completed envelope is not valid JSON")
			}
			return AssembleCanonicalDoc(step.Envelope, nil), stepIndex + 1, nil
		}
	}
	test.Fatal("envelope exhausted its fixed per-part progress limit")
	return nil, 0, ErrIntegrity
}

func legacyEnvelopeReviewCompare(test *testing.T, raw, got []byte, err error, accepted bool) {
	test.Helper()
	want, wantErr := legacyNormalizationOracle(raw)
	if (wantErr == nil) != accepted {
		test.Fatal("envelope fixture lost its original typed acceptance", accepted, wantErr)
	}
	if !errors.Is(err, wantErr) || (err == nil) != accepted || !bytes.Equal(got, want) {
		test.Fatal("envelope changed native typed acceptance or canonical bytes", len(got), len(want), err, wantErr)
	}
	if err != nil && errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("authenticated envelope rejection became a private-source failure", err)
	}
}

func legacyEnvelopeReviewString(test *testing.T, prefix string) string {
	test.Helper()
	var raw strings.Builder
	raw.WriteString(prefix)
	for _, boundary := range []struct {
		offset int
		token  string
		split  int
	}{
		{offset: LegacyCanonicalEncodingBytes, token: `\u0061`, split: 3},
		{offset: ConversationManifestChunkBytes, token: `\uD83D\uDE42`, split: 8},
		{offset: 2 * ConversationManifestChunkBytes, token: "🙂", split: 2},
	} {
		padding := boundary.offset - raw.Len() - boundary.split
		if padding < 0 {
			test.Fatal("envelope string boundary overlaps an earlier token", boundary.offset)
		}
		raw.WriteString(strings.Repeat("x", padding))
		raw.WriteString(boundary.token)
	}
	raw.WriteString(`\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uD800x\uDC00`)
	raw.WriteString("\ud55c<&>\xff\xc0\x80\xed\xa0\x80\xf0\x9f")
	raw.WriteByte('"')
	return raw.String()
}

func TestLegacyEnvelopeReviewUnknownMembersStayBoundedAndSyntaxChecked(test *testing.T) {
	long := strings.Repeat("7", 2*ConversationManifestChunkBytes+73)
	for _, fixture := range []struct {
		name     string
		raw      string
		accepted bool
	}{
		{name: "ignored-string-with-escaped-structure", raw: `{"envelope":{"cir_version":"2","unknown":"` + long + `\uD83D\uDE42\"},\"source_models\":[null]","source_model":"kept"}}`, accepted: true},
		{name: "ignored-object-number-is-not-float64", raw: `{"envelope":{"unknown":{"source_models":[` + long + `e+999999,{"envelope":null,"events":[true,false]}]},"cir_version":"2"}}`, accepted: true},
		{name: "late-malformed-ignored-string", raw: `{"envelope":{"cir_version":"2","unknown":"` + long + `\u12xz","source_model":"cannot-rescue"}}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			normalizer, input := legacyEnvelopeReviewStart(test, raw)
			got, steps, err := legacyEnvelopeReviewDrain(test, normalizer, input, true, nil)
			legacyEnvelopeReviewCompare(test, raw, got, err, fixture.accepted)
			if steps < 3 {
				test.Fatal("giant unknown envelope member did not yield", steps)
			}
		})
	}
}

func TestLegacyEnvelopeReviewGiantKnownStringsAndModelArraysMatchTypedOracle(test *testing.T) {
	model := `"` + strings.Repeat("m", 509) + `"`
	count := 2*ConversationManifestChunkBytes/(len(model)+1) + 1
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "escaped-source-model-string", raw: legacyEnvelopeReviewString(test, `{"envelope":{"cir_version":"2","\u0073ource_model":"`) + `,"SOURCE_MODEL":null}}`},
		{name: "folded-source-models-string-element", raw: legacyEnvelopeReviewString(test, `{"EnVeLoPe":{"CIR_VERſION":"2","ſource_modelſ":["`) + `,null,"last"]}}`},
		{name: "large-source-models-array", raw: `{"envelope":{"cir_version":"2","source_models":[` + strings.Repeat(model+",", count-1) + model + `,null]}}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			normalizer, input := legacyEnvelopeReviewStart(test, raw)
			got, steps, err := legacyEnvelopeReviewDrain(test, normalizer, input, false, nil)
			legacyEnvelopeReviewCompare(test, raw, got, err, true)
			if steps < 3 {
				test.Fatal("giant selected envelope value did not yield", steps)
			}
			before := len(input.reads)
			original := bytes.Clone(got)
			completed, _, err := legacyEnvelopeReviewStep(test, context.Background(), normalizer, input, false)
			if err != nil || !completed.Complete || !bytes.Equal(AssembleCanonicalDoc(completed.Envelope, nil), original) {
				test.Fatal("fresh context lost the completed envelope", err)
			}
			completed.Envelope[0] ^= 1
			repeated, _, err := legacyEnvelopeReviewStep(test, context.Background(), normalizer, input, false)
			if err != nil || !bytes.Equal(AssembleCanonicalDoc(repeated.Envelope, nil), original) || len(input.reads) != before {
				test.Fatal("completed envelope aliases caller storage or rereads its source", err)
			}
		})
	}
}

func TestLegacyEnvelopeReviewDuplicatesCasefoldNullAndNativeErrors(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		raw      string
		accepted bool
	}{
		{name: "root-null", raw: `null`, accepted: true},
		{name: "envelope-null", raw: `{"envelope":null}`, accepted: true},
		{name: "folded-duplicates-and-envelope-null-merge", raw: `{"EnVeLoPe":{"cir_version":"2","source_provider":"codex","source_model":"old","context_tokens":7},"env\u0065lope":{"CIR_VERſION":null,"ſOURCE_PROVIDER":null,"\u0073ource_model":"new","CONTEXT_TOKENS":null,"output_tokens":-2,"compaction_count":3},"ENVELOPE":null}`, accepted: true},
		{name: "scalar-null-keeps-value-array-null-clears", raw: `{"envelope":{"cir_version":"2","source_model":"old","source_models":["first","last"],"fidelity":"full"},"envelope":{"SOURCE_MODEL":null,"ſource_modelſ":null,"FIDELITY":null},"events":null}`, accepted: true},
		{name: "empty-array-discards-backing", raw: `{"envelope":{"source_models":["old","tail"],"SOURCE_MODELS":[],"source_models":[null,null]}}`, accepted: true},
		{name: "bad-version-type-not-rescued", raw: `{"envelope":{"cir_version":7},"envelope":{"cir_version":"2"}}`},
		{name: "bad-provider-alias-type-not-rescued", raw: `{"envelope":{"source_provider":false,"SOURCE_PROVIDER":"codex"},"envelope":null}`},
		{name: "bad-fidelity-alias-type-not-rescued", raw: `{"envelope":{"fidelity":{},"FIDELITY":"full"}}`},
		{name: "bad-string-type-not-rescued", raw: `{"envelope":{"source_model":{},"SOURCE_MODEL":"fixed"}}`},
		{name: "bad-model-array-type-not-rescued", raw: `{"envelope":{"source_models":"wrong","ſource_modelſ":[]}}`},
		{name: "bad-model-element-not-rescued", raw: `{"envelope":{"source_models":["first",7]},"ENVELOPE":{"SOURCE_MODELS":["fixed"]},"envelope":null}`},
		{name: "fractional-native-int-not-rescued", raw: `{"envelope":{"context_tokens":1.5,"CONTEXT_TOKENS":7}}`},
		{name: "overflow-native-int-not-rescued", raw: `{"envelope":{"output_tokens":18446744073709551616,"OUTPUT_TOKENS":0}}`},
		{name: "bad-envelope-type-not-rescued", raw: `{"envelope":[],"ENVELOPE":{"cir_version":"2"}}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			normalizer, input := legacyEnvelopeReviewStart(test, raw)
			got, _, err := legacyEnvelopeReviewDrain(test, normalizer, input, true, nil)
			legacyEnvelopeReviewCompare(test, raw, got, err, fixture.accepted)
		})
	}
}

func TestLegacyEnvelopeReviewHiddenModelBackingSurvivesShorterThenLongerNullArrays(test *testing.T) {
	tail := strings.Repeat("hidden-model", LegacyCanonicalEncodingBytes/len("hidden-model")+1)
	quotedTail, err := json.Marshal(tail)
	if err != nil {
		test.Fatal(err)
	}
	for _, release := range []bool{false, true} {
		name := "shortened-array-retains-hidden-tail"
		if release {
			name = "null-array-releases-hidden-tail"
		}
		test.Run(name, func(test *testing.T) {
			prefix := `{"envelope":{"cir_version":"2","source_models":["first",` + string(quotedTail) + `,"last"]},"envelope":{"SOURCE_MODELS":["changed"]},"ENVELOPE":null,`
			if release {
				prefix += `"envelope":{"ſource_modelſ":null},`
			}
			prefix += `"review_padding":"`
			paddingEnd := len(prefix) + 2*ConversationManifestChunkBytes
			raw := []byte(prefix + strings.Repeat("x", 2*ConversationManifestChunkBytes) + `","envelope":{"source_models":[null,null,null]}}`)
			var original CIRDocument
			if err := json.Unmarshal(raw, &original); err != nil || len(original.Envelope.SourceModels) != 3 {
				test.Fatal("hidden-backing fixture lost its original typed array", err)
			}
			if !release && (original.Envelope.SourceModels[0] != "changed" || original.Envelope.SourceModels[1] != tail || original.Envelope.SourceModels[2] != "last") {
				test.Fatal("native oracle no longer exposes the hidden backing tail")
			}
			if release && strings.Join(original.Envelope.SourceModels, "") != "" {
				test.Fatal("native null-array oracle retained released elements")
			}
			normalizer, input := legacyEnvelopeReviewStart(test, raw)
			observed := false
			got, _, err := legacyEnvelopeReviewDrain(test, normalizer, input, false, func(step LegacyNormalizationStep, retained int64, reads [][2]int) {
				for _, read := range reads {
					if step.Complete || read[0] < len(prefix) || read[0]+read[1] > paddingEnd || read[1] == 0 {
						continue
					}
					if !release && retained < int64(len(tail)) || release && retained >= 64<<10 {
						test.Fatal("hidden SourceModels backing was dropped or null failed to release it", release, retained, len(tail))
					}
					observed = true
				}
			})
			legacyEnvelopeReviewCompare(test, raw, got, err, true)
			if !observed {
				test.Fatal("no public step exposed the shortened-array accounting checkpoint")
			}
		})
	}
}

func TestLegacyEnvelopeReviewPreCanceledObservationResumesWithFreshStepContexts(test *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"2","source_models":["` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `","last"]}}`)
	normalizer, input := legacyEnvelopeReviewStart(test, raw)
	step, before, err := legacyEnvelopeReviewStep(test, context.Background(), normalizer, input, false)
	if err != nil || step.Complete {
		test.Fatal("large envelope did not leave resumable work", err)
	}
	reads := len(input.reads)
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	step, after, err := legacyEnvelopeReviewStep(test, canceledCtx, normalizer, input, false)
	legacyNormalizationFrameZero(test, step)
	if !errors.Is(err, context.Canceled) || len(input.reads) != reads || before != after {
		test.Fatal("pre-canceled envelope observation mutated state or touched source", err, before, after)
	}
	got, _, err := legacyEnvelopeReviewDrain(test, normalizer, input, false, nil)
	legacyEnvelopeReviewCompare(test, raw, got, err, true)
}

func TestLegacyEnvelopeReviewSourceFailuresAndInFlightCancellationAreStickyAndZero(test *testing.T) {
	for _, when := range []string{"part-hash", "part-private-eof", "part-short", "part-cancel", "end-zero-private-eof", "end-one-private-eof", "end-zero-cancel", "end-one-cancel", "bad-type-late-hash"} {
		test.Run(when, func(test *testing.T) {
			prefix := `{"envelope":{"cir_version":"2",`
			if when == "bad-type-late-hash" {
				prefix += `"source_model":false,`
			}
			raw := []byte(prefix + `"source_model":"` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `"}}`)
			input := &legacySourceProofBuffer{body: raw}
			source := legacyNormalizationPreparationInput(test, input, true)
			readSource := source.Read
			triggered, failedAt := false, -1
			var cancelStep context.CancelFunc
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := readSource(ctx, offset, length)
				payload := (strings.HasPrefix(when, "part-") || when == "bad-type-late-hash") && offset == ConversationManifestChunkBytes && length > 0
				zero := strings.HasPrefix(when, "end-zero-") && offset == len(raw) && length == 0
				one := strings.HasPrefix(when, "end-one-") && offset == len(raw) && length == 1
				if triggered || !payload && !zero && !one {
					return body, err
				}
				triggered, failedAt = true, len(input.reads)
				if when == "part-hash" || when == "bad-type-late-hash" {
					corrupt := bytes.Clone(body)
					corrupt[0] ^= 1
					return corrupt, err
				}
				if when == "part-short" {
					return body[:len(body)-1], nil
				}
				if strings.HasSuffix(when, "-cancel") {
					cancelStep()
				}
				return nil, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
			}
			constructorCtx, cancel := context.WithCancel(context.Background())
			normalizer, err := NewLegacyNormalizationDecoder(constructorCtx, source)
			cancel()
			if err != nil || normalizer == nil || len(input.reads) != 0 {
				test.Fatal("failure fixture constructor touched payloads", err)
			}
			for stepIndex := 0; stepIndex < 128; stepIndex++ {
				stepCtx, stop := context.WithCancel(context.Background())
				cancelStep = stop
				step, retained, err := legacyEnvelopeReviewStep(test, stepCtx, normalizer, input, false)
				stop()
				if err == nil {
					if step.Complete {
						test.Fatal("envelope source failure was swallowed", when, triggered)
					}
					continue
				}
				want := error(ErrPreparedDocRangeObject)
				if strings.HasSuffix(when, "-cancel") {
					want = context.Canceled
				}
				legacyNormalizationFrameZero(test, step)
				if !triggered || !errors.Is(err, want) || len(input.reads) != failedAt || retained >= 64<<10 {
					test.Fatal("envelope failure lost precedence, continued reading, or retained partial values", when, triggered, err, want, retained)
				}
				reads := len(input.reads)
				repeated, _, repeatedErr := legacyEnvelopeReviewStep(test, context.Background(), normalizer, input, true)
				legacyNormalizationFrameZero(test, repeated)
				if !errors.Is(repeatedErr, want) || len(input.reads) != reads {
					test.Fatal("envelope terminal failure did not remain sticky", repeatedErr, want)
				}
				return
			}
			test.Fatal("envelope failure exhausted its fixed progress limit", when)
		})
	}
}
