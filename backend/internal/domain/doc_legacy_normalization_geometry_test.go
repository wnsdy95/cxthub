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

type legacyNormalizationGeometryResult struct {
	canonical []byte
	resets    int
	emitted   int
	discarded int
	count     int
}

func legacyNormalizationGeometrySource(raw []byte, partBytes int) (LegacyByteSource, *legacySourceProofBuffer) {
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	source := LegacyByteSource{Bytes: len(raw), PartBytes: partBytes, Read: input.Read}
	if partBytes == 0 {
		partBytes = ConversationManifestChunkBytes
	}
	for offset := 0; offset < len(raw); offset += partBytes {
		body := input.body[offset:min(offset+partBytes, len(raw))]
		source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(len(body))})
	}
	return source, input
}

func legacyNormalizationGeometryStart(test *testing.T, source LegacyByteSource, input *legacySourceProofBuffer) *LegacyNormalizationDecoder {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
	cancel()
	if err != nil || normalizer == nil || len(input.reads) != 0 {
		test.Fatal("geometry constructor failed or eagerly read source bytes", err, len(input.reads))
	}
	return normalizer
}

func legacyNormalizationGeometryDrain(test *testing.T, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, partBytes int) (legacyNormalizationGeometryResult, error) {
	test.Helper()
	if partBytes == 0 {
		partBytes = ConversationManifestChunkBytes
	}
	var result legacyNormalizationGeometryResult
	var events []struct {
		sequence int
		body     []byte
	}
	for stepIndex := 0; stepIndex < 4096; stepIndex++ {
		before, position := len(input.reads), normalizer.position
		ctx, cancel := context.WithCancel(context.Background())
		step, err := normalizer.Step(ctx)
		cancel()
		loads := 0
		for _, read := range input.reads[before:] {
			if read[0] == len(input.body) && (read[1] == 0 || read[1] == 1) {
				continue
			}
			if read[0] < 0 || read[0] >= len(input.body) || read[0]%partBytes != 0 ||
				read[1] != min(partBytes, len(input.body)-read[0]) {
				test.Fatal("public normalization bypassed its source leaf geometry", read, partBytes)
			}
			loads++
		}
		if loads > 2 || len(step.Events) > LegacyNormalizationStepEvents ||
			normalizer.position < position || normalizer.position-position > ConversationManifestChunkBytes {
			test.Fatal("source geometry changed the two-load, event, or logical-work guard", loads, len(step.Events), normalizer.position-position)
		}
		if err != nil {
			legacyNormalizationFrameZero(test, step)
			return result, err
		}
		if step.Reset {
			result.resets++
			result.discarded += len(events)
			events = nil
		}
		for _, event := range step.Events {
			if event.Encoding == nil {
				test.Fatal("public normalization emitted an event without an encoding")
			}
			before := len(input.reads)
			body, _ := collectLegacyCanonicalEncoding(test, event.Encoding)
			if len(input.reads) != before {
				test.Fatal("owned canonical encoding reread the source")
			}
			events = append(events, struct {
				sequence int
				body     []byte
			}{sequence: event.Sequence, body: body})
			result.emitted++
		}
		if !step.Complete {
			if step.Envelope != nil || step.Count != 0 {
				test.Fatal("unfinished geometry normalization exposed a final header")
			}
			continue
		}
		if len(events) != step.Count || !json.Valid(step.Envelope) {
			test.Fatal("geometry normalization lost reset state, events, or its envelope", len(events), step.Count)
		}
		covered := make([]bool, (len(input.body)+partBytes-1)/partBytes)
		var zeroEOF, oneEOF bool
		for _, read := range input.reads {
			if read[0] == len(input.body) {
				zeroEOF = zeroEOF || read[1] == 0
				oneEOF = oneEOF || read[1] == 1
				continue
			}
			covered[read[0]/partBytes] = true
		}
		for ordinal, present := range covered {
			if !present {
				test.Fatal("normalization completed without reading a source leaf", ordinal)
			}
		}
		if !zeroEOF || !oneEOF {
			test.Fatal("normalization completed without both original source EOF probes", zeroEOF, oneEOF)
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].sequence < events[right].sequence })
		bodies := make([][]byte, len(events))
		for ordinal, event := range events {
			bodies[ordinal] = event.body
		}
		result.canonical = AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(bodies, []byte(","))})
		result.count = step.Count
		return result, nil
	}
	test.Fatal("geometry normalization exhausted its finite fixture progress allowance")
	return result, ErrIntegrity
}

func legacyNormalizationGeometryParity(test *testing.T, raw []byte) legacyNormalizationGeometryResult {
	test.Helper()
	want, wantErr := legacyNormalizationOracle(raw)
	if wantErr != nil {
		test.Fatal("geometry fixture lost its original typed acceptance", wantErr)
	}
	var baseline, selected legacyNormalizationGeometryResult
	for _, partBytes := range []int{0, LegacyNormalizationPartBytes} {
		source, input := legacyNormalizationGeometrySource(raw, partBytes)
		normalizer := legacyNormalizationGeometryStart(test, source, input)
		result, err := legacyNormalizationGeometryDrain(test, normalizer, input, partBytes)
		if err != nil || !bytes.Equal(result.canonical, want) || HashContent(result.canonical) != HashContent(want) {
			test.Fatal("source geometry changed exact typed canonical bytes or SHA", partBytes, err, len(result.canonical), len(want))
		}
		if partBytes == 0 {
			baseline = result
		} else {
			selected = result
		}
	}
	if !bytes.Equal(selected.canonical, baseline.canonical) || HashContent(selected.canonical) != HashContent(baseline.canonical) ||
		selected.count != baseline.count {
		test.Fatal("64 KiB normalization diverged from the original default 512 KiB event/reset oracle")
	}
	return selected
}

func TestLegacyNormalizationGeometryStringEscapeAndUnicodeSeams(test *testing.T) {
	var raw strings.Builder
	raw.WriteString(`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":3,"role":"user","blocks":[{"type":"text","text":"`)
	for _, seam := range []struct {
		position int
		token    string
	}{
		{position: LegacyNormalizationPartBytes - 1, token: `\"\\\n`},
		{position: 2*LegacyNormalizationPartBytes - 3, token: `\uD83D\uDE42`},
		{position: 3*LegacyNormalizationPartBytes - 2, token: `🙂\u2028\u2029<&>`},
	} {
		if seam.position < raw.Len() {
			test.Fatal("live text fixture no longer fits its selected leaf seam")
		}
		raw.WriteString(strings.Repeat("text", (seam.position-raw.Len())/4))
		raw.WriteString(strings.Repeat("x", seam.position-raw.Len()))
		raw.WriteString(seam.token)
	}
	raw.WriteString(`"}]}]}`)
	result := legacyNormalizationGeometryParity(test, []byte(raw.String()))
	if result.count != 1 || result.emitted != 1 || result.resets != 1 {
		test.Fatal("string seam fixture stopped exercising a single live event", result.count, result.emitted, result.resets)
	}
}

func TestLegacyNormalizationGeometryFieldNameLargeChildAndNestedReplacement(test *testing.T) {
	const prefix = `{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","seq":8,"replacement":[{"kind":"message","seq":5,"role":"assistant","blocks":[{"type":"text","text":"`
	const closeText = `"}],`
	const field = `"agent_me\u0073sage"`
	split := strings.Index(field, `\u`) + 4
	text := strings.Repeat("m", LegacyNormalizationPartBytes-split-len(prefix)-len(closeText))
	raw := prefix + text + closeText + field + `:false},{"kind":"compaction","seq":-2,"replacement_complete":false,"replacement":[` +
		`{"kind":"tool_call","seq":6,"call_id":"large-child","tool_name":"inspect","input":{"keep":"` +
		strings.Repeat("child", (ConversationManifestChunkBytes+LegacyNormalizationPartBytes)/5+1) +
		`","replace":{"old":1}},"INP\u0055T":{"replace":{"new":[null,{},[]]}}},` +
		`{"kind":"turn","seq":-1,"id":"nested-tie-first","role":"user"},{"kind":"turn","seq":-1,"id":"nested-tie-second","role":"system"}]}],"replacement_complete":true},` +
		`{"kind":"tool_result","seq":-3,"call_id":"following-event","output":{"tail":true}}]}`
	if strings.Index(raw, field)+split != LegacyNormalizationPartBytes {
		test.Fatal("escaped typed field name no longer crosses the selected leaf")
	}
	result := legacyNormalizationGeometryParity(test, []byte(raw))
	for _, retained := range []string{`"agent_message":false`, `"replacement_complete":false`, `"replace":{"new":[null,{},[]]}`, `"nested-tie-first"`, `"nested-tie-second"`} {
		if !bytes.Contains(result.canonical, []byte(retained)) {
			test.Fatal("nested geometry fixture lost presence, replacement, or stable ordering evidence", retained)
		}
	}
	if result.count != 2 || bytes.Contains(result.canonical, []byte(`"old":1`)) {
		test.Fatal("large native child merge retained a replaced nested object", result.count)
	}
}

func TestLegacyNormalizationGeometryDuplicateEventsResetPresence(test *testing.T) {
	const prefix = `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","seq":9,"id":"discarded-first","role":"assistant","agent_message":false,"blocks":[{"type":"text","text":"`
	const closeText = `"}]}],`
	const field = `"event\u0073"`
	split := strings.Index(field, `\u`) + 3
	raw := prefix + strings.Repeat("r", LegacyNormalizationPartBytes-split-len(prefix)-len(closeText)) + closeText + field +
		`:null,"EVENTS":[{"kind":"message","seq":5,"id":"discarded-again","agent_message":false}],"events":[` +
		`{"kind":"turn","seq":2,"id":"retained-first","role":"user"},{"kind":"turn","seq":2,"id":"retained-second","role":"system"},` +
		`{"kind":"turn","seq":-1,"id":"retained-earliest","role":"assistant"}]}`
	if strings.Index(raw, field)+split != LegacyNormalizationPartBytes {
		test.Fatal("duplicate escaped events key no longer crosses the selected leaf")
	}
	result := legacyNormalizationGeometryParity(test, []byte(raw))
	if result.resets < 2 || result.emitted != 5 || result.discarded != 2 || result.count != 3 ||
		bytes.Contains(result.canonical, []byte("discarded")) || bytes.Contains(result.canonical, []byte(`"agent_message"`)) {
		test.Fatal("cross-leaf duplicate events leaked prior v2 presence or provisional events", result.resets, result.emitted, result.discarded, result.count)
	}
}

func TestLegacyNormalizationGeometryLateSourceFailureOutranksRejection(test *testing.T) {
	payload := strings.Repeat("s", 3*LegacyNormalizationPartBytes+31)
	for _, fixture := range []struct {
		name     string
		raw      string
		rejected bool
	}{
		{name: "provisional-events", raw: `{"events":[{"kind":"turn","seq":7,"role":"user"}],"envelope":{"cir_version":"2","source_model":"` + payload + `"}}`},
		{name: "rejected-envelope", raw: `{"envelope":42,"events":[{"kind":"message","blocks":[{"type":"text","text":"` + payload + `"}]}]}`, rejected: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_, oracleErr := legacyNormalizationOracle([]byte(fixture.raw))
			if (oracleErr != nil) != fixture.rejected {
				test.Fatal("late failure fixture changed original typed acceptance", oracleErr)
			}
			for _, failure := range []string{"leaf-hash", "late-io", "late-leaf-eof", "eof-probe"} {
				test.Run(failure, func(test *testing.T) {
					source, input := legacyNormalizationGeometrySource([]byte(fixture.raw), LegacyNormalizationPartBytes)
					last := (len(source.Parts) - 1) * LegacyNormalizationPartBytes
					if last == 0 {
						test.Fatal("late source failure fixture no longer spans multiple leaves")
					}
					if failure == "leaf-hash" {
						source.Parts[len(source.Parts)-1].Hash = HashContent([]byte("different final normalization leaf"))
					}
					ioFailure := errors.New("late normalization source IO failure")
					triggered := false
					source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
						body, err := input.Read(ctx, offset, length)
						if offset == last {
							switch failure {
							case "leaf-hash":
								triggered = true
							case "late-io":
								triggered = true
								return nil, ioFailure
							case "late-leaf-eof":
								triggered = true
								return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
							}
						}
						if failure == "eof-probe" && offset == len(input.body) && length == 1 {
							triggered = true
							return nil, errors.Join(ErrPreparedDocRangeObject, ioFailure, io.EOF)
						}
						return body, err
					}
					normalizer := legacyNormalizationGeometryStart(test, source, input)
					result, err := legacyNormalizationGeometryDrain(test, normalizer, input, LegacyNormalizationPartBytes)
					if !triggered || !errors.Is(err, ErrPreparedDocRangeObject) || result.canonical != nil {
						test.Fatal("late leaf failure became completion or permanent document rejection", triggered, err)
					}
					if (failure == "late-io" || failure == "eof-probe") && !errors.Is(err, ioFailure) {
						test.Fatal("late source IO identity was lost", err)
					}
					if (failure == "late-leaf-eof" || failure == "eof-probe") && !errors.Is(err, io.EOF) {
						test.Fatal("private EOF identity was swallowed", err)
					}
					if fixture.rejected && result.emitted != 0 || !fixture.rejected && result.emitted != 1 {
						test.Fatal("late failure did not exercise the intended provisional event state", result.emitted)
					}
					before := len(input.reads)
					repeated, repeatedErr := normalizer.Step(context.Background())
					legacyNormalizationFrameZero(test, repeated)
					if !errors.Is(repeatedErr, ErrPreparedDocRangeObject) || len(input.reads) != before {
						test.Fatal("failed geometry normalization resumed reads or authorized completion", repeatedErr)
					}
				})
			}
		})
	}
}
