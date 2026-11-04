package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
)

const legacyNormalizationFrameQuantum = 512 << 10

type legacyNormalizationFrameResult struct {
	canonical []byte
	resets    int
	advances  int
}

func legacyNormalizationFrameOracle(raw []byte) ([]byte, VerifiedSessionDoc, error) {
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, VerifiedSessionDoc{}, ErrIntegrity
	}
	canonical, err := CanonicalBytes(document)
	if err != nil {
		return nil, VerifiedSessionDoc{}, ErrIntegrity
	}
	proof, err := VerifySessionDoc(SessionDoc{Hash: HashContent(canonical), CIR: document})
	if err != nil {
		return nil, VerifiedSessionDoc{}, ErrIntegrity
	}
	return canonical, proof, nil
}

func legacyNormalizationFrameStart(test *testing.T, raw []byte) (*LegacyNormalizationDecoder, *legacySourceProofBuffer) {
	test.Helper()
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), input.input())
	if err != nil || normalizer == nil || len(input.reads) != 0 {
		test.Fatal("constructor failed or loaded source payloads", err, len(input.reads))
	}
	return normalizer, input
}

func legacyNormalizationFrameZero(test *testing.T, step LegacyNormalizationStep) {
	test.Helper()
	if step.Reset || step.Events != nil || step.Complete || step.Envelope != nil || step.Count != 0 {
		test.Fatal("failed step exposed a nonzero result", step.Reset, len(step.Events), step.Complete, len(step.Envelope), step.Count)
	}
}

func legacyNormalizationFrameStep(test *testing.T, ctx context.Context, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, ignored bool) (LegacyNormalizationStep, error) {
	test.Helper()
	position, reads := normalizer.position, len(input.reads)
	step, err := normalizer.Step(ctx)
	if advanced := normalizer.position - position; advanced < 0 || advanced > legacyNormalizationFrameQuantum {
		test.Fatal("one step exceeded the framing cursor budget", position, normalizer.position, advanced)
	}
	if err != nil {
		legacyNormalizationFrameZero(test, step)
	}
	if len(step.Events) > LegacyNormalizationStepEvents {
		test.Fatal("step exceeded the existing event batch", len(step.Events))
	}
	if !ignored {
		return step, err
	}
	loads, previous := 0, -1
	for _, read := range input.reads[reads:] {
		if read[0] == len(input.body) && (read[1] == 0 || read[1] == 1) {
			continue
		}
		if read[0] < 0 || read[0] >= len(input.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(input.body)-read[0]) {
			test.Fatal("framing bypassed full authenticated source parts", read)
		}
		ordinal := read[0] / ConversationManifestChunkBytes
		if loads > 0 && ordinal != previous+1 {
			test.Fatal("step reloaded a part or left its adjacent two-page window", input.reads[reads:])
		}
		loads++
		previous = ordinal
	}
	if loads > 2 {
		test.Fatal("ignored input loaded more than two source payloads in one step", loads)
	}
	retained, retainedErr := normalizer.RetainedBytes()
	if retainedErr != nil || retained <= 0 || retained >= 64<<10 {
		test.Fatal("ignored framing retained source bodies, a long key, or an unknown value", retained, retainedErr)
	}
	var physical int64
	for _, read := range input.reads {
		physical += int64(read[1])
	}
	if physical > 8*int64(len(input.body)) {
		test.Fatal("framing exceeded the eightfold aggregate physical read bound", physical, len(input.body))
	}
	return step, err
}

func legacyNormalizationFrameDrain(test *testing.T, ctx context.Context, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, ignored bool) (legacyNormalizationFrameResult, error) {
	test.Helper()
	var result legacyNormalizationFrameResult
	type materializedEvent struct {
		Sequence int
		Bytes    []byte
	}
	var events []materializedEvent
	for stepIndex := 0; stepIndex < len(input.body)+16; stepIndex++ {
		position := normalizer.position
		stepCtx, cancel := context.WithCancel(ctx)
		step, err := legacyNormalizationFrameStep(test, stepCtx, normalizer, input, ignored)
		cancel()
		if err != nil {
			return result, err
		}
		if normalizer.position > position {
			result.advances++
		}
		if step.Reset {
			result.resets++
			events = nil
		}
		for _, event := range step.Events {
			body, err := legacyNormalizationTestBytes(ctx, event)
			if err != nil {
				return result, err
			}
			events = append(events, materializedEvent{Sequence: event.Sequence, Bytes: body})
		}
		if !step.Complete {
			continue
		}
		if len(events) != step.Count {
			test.Fatal("reset/event stream disagrees with final count", len(events), step.Count)
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].Sequence < events[right].Sequence })
		bodies := make([][]byte, len(events))
		for ordinal, event := range events {
			bodies[ordinal] = event.Bytes
		}
		result.canonical = AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(bodies, []byte(","))})
		return result, nil
	}
	test.Fatal("source-sized normalization progress limit exhausted")
	return result, ErrIntegrity
}

func legacyNormalizationFrameCompare(test *testing.T, raw []byte, result legacyNormalizationFrameResult, err error) VerifiedSessionDoc {
	test.Helper()
	want, proof, wantErr := legacyNormalizationFrameOracle(raw)
	if (err == nil) != (wantErr == nil) || !bytes.Equal(result.canonical, want) {
		test.Fatal("framing changed original typed acceptance or canonical bytes", err, wantErr, len(result.canonical), len(want))
	}
	if err != nil && (!errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject)) {
		test.Fatal("well-authenticated invalid JSON changed rejection category", err)
	}
	if err == nil && (!proof.Valid() || HashContent(result.canonical) != proof.Hash() || !bytes.Equal(result.canonical, proof.Bytes())) {
		test.Fatal("normalized bytes disagree with the independent original document proof")
	}
	return proof
}

func TestLegacyNormalizationFrameGiantIgnoredTokens(test *testing.T) {
	long := strings.Repeat("x", 3*legacyNormalizationFrameQuantum+73)
	prefix := `{"ignored":"`
	escaped := strings.Repeat("x", legacyNormalizationFrameQuantum-len(prefix)-1) + `\u0061` +
		strings.Repeat("y", legacyNormalizationFrameQuantum-6) + `\"\\\/\b\f\n\r\t\uD83D\uDE42` + long
	member := `{"events":[true,false,null,-2.5e+4,"}]:,{"],"envelope":{"key":"\u0061"}}`
	nested := strings.Repeat(member+",", 3*legacyNormalizationFrameQuantum/len(member)+1) + member
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "string-and-split-escapes", raw: prefix + escaped + `"}`},
		{name: "number-with-unbounded-exponent", raw: `{"ignored":` + strings.Repeat("7", len(long)) + `e+999999}`},
		{name: "nested-ignored-object", raw: `{"ignored":{"events":[` + nested + `],"envelope":null}}`},
		{name: "long-key-known-prefix", raw: `{"events` + long + `":null}`},
		{name: "long-key-known-suffix", raw: `{"` + long + `\u0065vents":null}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			normalizer, input := legacyNormalizationFrameStart(test, raw)
			result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
			legacyNormalizationFrameCompare(test, raw, result, err)
			if err != nil || result.advances < 4 || result.resets != 0 {
				test.Fatal("giant ignored input was not incrementally ignored", err, result.advances, result.resets)
			}
		})
	}
}

func TestLegacyNormalizationFrameGiantWhitespaceAtEveryRootBoundary(test *testing.T) {
	space := strings.Repeat(" \t\r\n", 3*legacyNormalizationFrameQuantum/4+19)
	for _, fixture := range []struct {
		name   string
		before string
		after  string
	}{
		{name: "before-root", after: `{}`},
		{name: "before-first-key", before: `{`, after: `"ignored":null}`},
		{name: "before-next-key", before: `{"ignored":null,`, after: `"other":false}`},
		{name: "before-colon", before: `{"ignored"`, after: `:null}`},
		{name: "before-value", before: `{"ignored":`, after: `null}`},
		{name: "before-comma", before: `{"ignored":1`, after: `,"other":false}`},
		{name: "before-close", before: `{"ignored":null`, after: `}`},
		{name: "after-root", before: `{}`},
		{name: "before-null-root", after: `null`},
		{name: "after-null-root", before: `null`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.before + space + fixture.after)
			normalizer, input := legacyNormalizationFrameStart(test, raw)
			result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
			legacyNormalizationFrameCompare(test, raw, result, err)
			if err != nil || result.advances < 4 {
				test.Fatal("giant root whitespace was not incrementally consumed", err, result.advances)
			}
		})
	}
}

func TestLegacyNormalizationFrameRecognizedKeysAndResetParity(test *testing.T) {
	padding := `{"ignored":"` + strings.Repeat("x", 3*legacyNormalizationFrameQuantum+73) + `",`
	keyBoundary := strings.Repeat(" ", legacyNormalizationFrameQuantum-len(padding)%legacyNormalizationFrameQuantum-1)
	for _, fixture := range []struct {
		name string
		tail string
	}{
		{name: "escaped-casefold-long-s", tail: `"EnVeLoPe":{"cir_version":"2"},"\u0065vents":[{"kind":"turn","seq":7,"id":"stale"}],"EvEnT\u017f":null,"EVENTS":[{"kind":"turn","seq":2,"id":"first"},{"kind":"turn","seq":2,"id":"second"}]}`},
		{name: "literal-long-s-and-null-reset", tail: `"envelope":{"cir_version":"1"},"events":[{"kind":"unknown"}],"eventſ":null}`},
		{name: "fully-escaped-empty-reset", tail: `"events":[{"kind":"turn","seq":4}],"\u0065\u0076\u0065\u006e\u0074\u0073":[]}`},
		{name: "escaped-key-across-page-and-step", tail: keyBoundary + `"\u0065\u0076\u0065\u006e\u0074\u017f":[{"kind":"turn","seq":1}]}`},
		{name: "maximally-escaped-envelope", tail: `"\u0065\u006e\u0076\u0065\u006c\u006f\u0070\u0065":{"cir_version":"2"},"events":[]}`},
		{name: "duplicate-envelope-merge", tail: `"EnVeLoPe":{"cir_version":"2","source_model":"old"},"env\u0065lope":{"source_model":"new"},"ENVELOPE":null,"events":null}`},
		{name: "typed-error-not-erased-by-reset", tail: `"events":[{"kind":"turn","seq":"bad"}],"events":[]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(padding + fixture.tail)
			normalizer, input := legacyNormalizationFrameStart(test, raw)
			result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
			legacyNormalizationFrameCompare(test, raw, result, err)
			if err == nil && result.resets == 0 {
				test.Fatal("recognized events never emitted Reset")
			}
		})
	}
}

func TestLegacyNormalizationFrameMalformedTokensAndOriginalDepth(test *testing.T) {
	long := strings.Repeat("x", 3*legacyNormalizationFrameQuantum+73)
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "escape", raw: `{"ignored":"` + long + `\q"}`},
		{name: "unicode-escape", raw: `{"ignored":"` + long + `\u12xz"}`},
		{name: "unfinished-escape", raw: `{"ignored":"` + long + `\`},
		{name: "long-key-escape", raw: `{"` + long + `\q":null}`},
		{name: "literal", raw: `{"ignored":tru` + strings.Repeat("e", len(long)) + `}`},
		{name: "leading-zero", raw: `{"ignored":0` + strings.Repeat("7", len(long)) + `}`},
		{name: "unfinished-number", raw: `{"ignored":1` + strings.Repeat("7", len(long)) + `e+}`},
		{name: "unfinished-fraction", raw: `{"ignored":1` + strings.Repeat("7", len(long)) + `.}`},
		{name: "mismatched-structure", raw: `{"ignored":[{"nested":"` + long + `"}}}`},
		{name: "nested-trailing-comma", raw: `{"ignored":{"nested":"` + long + `",}}`},
		{name: "root-trailing-comma", raw: `{"ignored":"` + long + `",}`},
		{name: "extra-root", raw: `{"ignored":"` + long + `"}true`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			normalizer, input := legacyNormalizationFrameStart(test, raw)
			result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
			legacyNormalizationFrameCompare(test, raw, result, err)
			if !errors.Is(err, ErrIntegrity) {
				test.Fatal("malformed giant input was accepted", err)
			}
		})
	}
	for _, depth := range []int{9998, 9999, 10000} {
		test.Run(fmt.Sprintf("root-depth-%d", depth+1), func(test *testing.T) {
			raw := []byte(`{"padding":"` + long + `","ignored":` + strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth) + `}`)
			normalizer, input := legacyNormalizationFrameStart(test, raw)
			result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
			legacyNormalizationFrameCompare(test, raw, result, err)
			if (err == nil) != (depth+1 <= 10000) {
				test.Fatal("original root-inclusive depth 10000 changed", depth+1, err)
			}
		})
	}
}

func TestLegacyNormalizationFrameFreshWindowAndOriginalSourceProof(test *testing.T) {
	raw := []byte(`{"ignored":"` + strings.Repeat("x", 3*legacyNormalizationFrameQuantum+73) + `"}`)
	test.Run("reauthenticate-part-after-yield", func(test *testing.T) {
		normalizer, input := legacyNormalizationFrameStart(test, raw)
		step, err := legacyNormalizationFrameStep(test, context.Background(), normalizer, input, true)
		if err != nil || step.Complete || normalizer.position <= 0 || normalizer.position >= ConversationManifestChunkBytes {
			test.Fatal("root opening did not yield inside its authenticated part", err, normalizer.position)
		}
		input.body[0] ^= 1
		before := len(input.reads)
		step, err = legacyNormalizationFrameStep(test, context.Background(), normalizer, input, true)
		legacyNormalizationFrameZero(test, step)
		if !errors.Is(err, ErrPreparedDocRangeObject) || len(input.reads) <= before || input.reads[before][0] != 0 {
			test.Fatal("a retained authenticated page hid private mutation behind the cursor", err)
		}
	})
	test.Run("borrowed-source-and-detached-descriptors", func(test *testing.T) {
		input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
		source := input.input()
		borrowed := make([]byte, ConversationManifestChunkBytes)
		source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
			body, err := input.Read(ctx, offset, length)
			if err != nil {
				return nil, err
			}
			copy(borrowed, body)
			return borrowed[:len(body)], nil
		}
		constructorCtx, cancel := context.WithCancel(context.Background())
		normalizer, err := NewLegacyNormalizationDecoder(constructorCtx, source)
		cancel()
		if err != nil || len(input.reads) != 0 {
			test.Fatal("constructor retained work or eagerly read a page", err)
		}
		source.Parts[0].Hash = HashContent([]byte("caller changed descriptor"))
		result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
		proof := legacyNormalizationFrameCompare(test, raw, result, err)
		if err != nil || HashContent(raw) == proof.Hash() {
			test.Fatal("fixture lost its noncanonical source or failed detached decoding", err)
		}
		for _, corrupt := range []bool{false, true} {
			normalized := &legacySourceProofBuffer{body: bytes.Clone(result.canonical)}
			var verifier CanonicalDocVerifier
			bridge, err := NewLegacyByteSourcePreparation(context.Background(), proof.Hash(), normalized.input(), &legacySourceProofBuffer{}, &verifier)
			if err != nil {
				test.Fatal(err)
			}
			pending, pendingErr := bridge.Result()
			if pending.Valid() || !errors.Is(pendingErr, ErrDocRangeIndexPending) {
				test.Fatal("framing completion bypassed the original source-proof producer", pendingErr)
			}
			if corrupt {
				normalized.body[len(normalized.body)-1] ^= 1
			}
			progress, err := legacySourceProofDrain(test, bridge)
			sealed, resultErr := bridge.Result()
			if corrupt {
				if !errors.Is(err, ErrPreparedDocRangeObject) || sealed.Valid() {
					test.Fatal("provisional normalized bytes bypassed original authentication", err)
				}
			} else if err != nil || resultErr != nil || progress != LegacyByteSourceVerified || !sealed.Valid() {
				test.Fatal("normalized bytes could not earn original source proof", progress, err, resultErr)
			}
		}
	})
}

func TestLegacyNormalizationFramePrivateCorruptionOutranksMalformed(test *testing.T) {
	long := strings.Repeat("x", 3*legacyNormalizationFrameQuantum+73)
	for _, malformed := range []string{
		`{"ignored":truX,"later":"` + long + `"}`,
		`{"ignored":"` + long + `\q","later":"` + strings.Repeat("x", 2*legacyNormalizationFrameQuantum) + `"}`,
	} {
		for _, failure := range []string{"payload-hash", "payload-private-eof", "private-end-probe"} {
			test.Run(fmt.Sprintf("%d-%s", len(malformed), failure), func(test *testing.T) {
				input := &legacySourceProofBuffer{body: []byte(malformed)}
				source := input.input()
				last := (len(input.body) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
				if failure == "payload-hash" {
					input.body[len(input.body)-2] ^= 1
				} else {
					source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
						body, err := input.Read(ctx, offset, length)
						if failure == "payload-private-eof" && offset == last ||
							failure == "private-end-probe" && offset == len(input.body) && length == 1 {
							return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
						}
						return body, err
					}
				}
				normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
				if err != nil {
					test.Fatal(err)
				}
				result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
				if !errors.Is(err, ErrPreparedDocRangeObject) || result.canonical != nil {
					test.Fatal("malformed rejection outranked later private-source failure", err)
				}
				reads := len(input.reads)
				step, repeatedErr := normalizer.Step(context.Background())
				legacyNormalizationFrameZero(test, step)
				if !errors.Is(repeatedErr, ErrPreparedDocRangeObject) || len(input.reads) != reads {
					test.Fatal("terminal private-source failure was not sticky", repeatedErr)
				}
			})
		}
	}
}

func TestLegacyNormalizationFrameCancellationIsAtomic(test *testing.T) {
	raw := []byte(`{"ignored":"` + strings.Repeat("x", 3*legacyNormalizationFrameQuantum+73) + `"}`)
	test.Run("pre-canceled-constructor-and-step", func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
		if normalizer, err := NewLegacyNormalizationDecoder(ctx, input.input()); normalizer != nil || !errors.Is(err, context.Canceled) || len(input.reads) != 0 {
			test.Fatal("pre-canceled construction touched its source", err)
		}
		normalizer, input := legacyNormalizationFrameStart(test, raw)
		if _, err := legacyNormalizationFrameStep(test, context.Background(), normalizer, input, true); err != nil {
			test.Fatal(err)
		}
		position, reads := normalizer.position, len(input.reads)
		retained, err := normalizer.RetainedBytes()
		if err != nil {
			test.Fatal(err)
		}
		step, err := normalizer.Step(ctx)
		legacyNormalizationFrameZero(test, step)
		after, retainedErr := normalizer.RetainedBytes()
		if !errors.Is(err, context.Canceled) || normalizer.position != position || len(input.reads) != reads || retainedErr != nil || after != retained {
			test.Fatal("pre-canceled step advanced, read, or retained state", err, retainedErr)
		}
		result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, true)
		legacyNormalizationFrameCompare(test, raw, result, err)
		if err != nil {
			test.Fatal("pre-cancellation poisoned resumable framing", err)
		}
	})
	for _, when := range []string{"first-payload", "second-payload", "end-zero", "end-one"} {
		test.Run(when, func(test *testing.T) {
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			source := input.input()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			loads, triggered := 0, false
			source.Read = func(readCtx context.Context, offset, length int) ([]byte, error) {
				body, err := input.Read(readCtx, offset, length)
				if offset < len(input.body) && length > 0 {
					loads++
				}
				if when == "first-payload" && loads == 1 || when == "second-payload" && loads == 2 ||
					when == "end-zero" && offset == len(input.body) && length == 0 ||
					when == "end-one" && offset == len(input.body) && length == 1 {
					triggered = true
					cancel()
					return body, errors.Join(ErrPreparedDocRangeObject, err)
				}
				return body, err
			}
			normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
			if err != nil {
				test.Fatal(err)
			}
			result, err := legacyNormalizationFrameDrain(test, ctx, normalizer, input, true)
			if !triggered || !errors.Is(err, context.Canceled) || result.canonical != nil {
				test.Fatal("in-flight cancellation lost atomic zero-step or error precedence", triggered, err)
			}
		})
	}
}

func TestLegacyNormalizationFrameKnownTypedReductionRemainsOpen(test *testing.T) {
	long := strings.Repeat("x", 3*legacyNormalizationFrameQuantum+73)
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{name: "event", raw: `{"envelope":{"cir_version":"2"},"events":[{"kind":"message","role":"user","seq":2,"blocks":[{"type":"text","text":"` + long + `"}]}]}`},
		{name: "envelope", raw: `{"envelope":{"cir_version":"2","source_model":"` + long + `","source_model":null},"events":[]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			test.Log("OPEN: known-value typed semantic re-read and final header canonicalization are not byte/read/retention bounded here")
			raw := []byte(fixture.raw)
			normalizer, input := legacyNormalizationFrameStart(test, raw)
			result, err := legacyNormalizationFrameDrain(test, context.Background(), normalizer, input, false)
			legacyNormalizationFrameCompare(test, raw, result, err)
			if err != nil || result.advances < 4 {
				test.Fatal("known raw framing did not yield before original typed reduction", err, result.advances)
			}
		})
	}
}
