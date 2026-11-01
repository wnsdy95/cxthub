package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
)

type legacyCanonicalBlocksPublicResult struct {
	canonical        []byte
	events           int
	maxEncodingSteps int
}

func legacyCanonicalBlocksPublicDrain(test *testing.T, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer) (legacyCanonicalBlocksPublicResult, error) {
	test.Helper()
	var result legacyCanonicalBlocksPublicResult
	var events []struct {
		sequence int
		body     []byte
	}
	for stepIndex := 0; stepIndex < 4096; stepIndex++ {
		position := normalizer.position
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if advanced := normalizer.position - position; advanced < 0 || advanced > 512<<10 {
			test.Fatal("public blocks exceeded the framing quantum", advanced)
		}
		if err != nil {
			return result, err
		}
		if step.Reset {
			events = nil
			result.events = 0
		}
		for _, event := range step.Events {
			if event.Encoding == nil {
				test.Fatal("public blocks emitted an event without an encoding")
			}
			reads := len(input.reads)
			body, steps := collectLegacyCanonicalEncoding(test, event.Encoding)
			if len(input.reads) != reads {
				test.Fatal("owned public encoding reread its source")
			}
			chunk, complete, err := event.Encoding.Next(context.Background())
			if err != nil || !complete || len(chunk) != 0 {
				test.Fatal("completed public encoding changed on retry", complete, err)
			}
			result.maxEncodingSteps = max(result.maxEncodingSteps, steps)
			events = append(events, struct {
				sequence int
				body     []byte
			}{sequence: event.Sequence, body: body})
			result.events++
		}
		if !step.Complete {
			if step.Envelope != nil || step.Count != 0 {
				test.Fatal("unfinished public blocks exposed a completed header")
			}
			continue
		}
		if result.events != step.Count || !json.Valid(step.Envelope) {
			test.Fatal("public blocks lost events or the final envelope", result.events, step.Count)
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].sequence < events[right].sequence })
		bodies := make([][]byte, len(events))
		for ordinal, event := range events {
			bodies[ordinal] = event.body
		}
		result.canonical = AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(bodies, []byte(","))})
		reads := len(input.reads)
		header := bytes.Clone(step.Envelope)
		step.Envelope[0] ^= 1
		repeated, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil || !repeated.Complete || repeated.Count != step.Count || repeated.Reset ||
			len(repeated.Events) != 0 || !bytes.Equal(repeated.Envelope, header) || len(input.reads) != reads {
			test.Fatal("completed public blocks changed or reread source on retry", err)
		}
		return result, nil
	}
	test.Fatal("public blocks exhausted their fixed progress allowance")
	return result, ErrIntegrity
}

func legacyCanonicalBlocksPublicSourceComplete(test *testing.T, input *legacySourceProofBuffer) {
	test.Helper()
	covered := make([]bool, (len(input.body)+ConversationManifestChunkBytes-1)/ConversationManifestChunkBytes)
	var zeroEOF, oneEOF bool
	for _, read := range input.reads {
		if read[0] == len(input.body) {
			zeroEOF = zeroEOF || read[1] == 0
			oneEOF = oneEOF || read[1] == 1
			continue
		}
		covered[read[0]/ConversationManifestChunkBytes] = true
	}
	for ordinal, present := range covered {
		if !present {
			test.Fatal("public blocks completed without authenticating every source part", ordinal)
		}
	}
	if !zeroEOF || !oneEOF {
		test.Fatal("public blocks completed without both source EOF probes", zeroEOF, oneEOF)
	}
}

func legacyCanonicalBlocksPublicDocument(test *testing.T, count int, nested bool) []byte {
	test.Helper()
	blocks := make([]ContentBlock, count)
	for ordinal := range blocks {
		blocks[ordinal] = ContentBlock{Type: "text", Text: fmt.Sprintf("block-%04d: 한🙂<&>\n\"\\\u2028\u2029", ordinal)}
	}
	message := CIREvent{
		Kind: EventMessage, Seq: 7, ID: "large-message", Role: RoleAssistant, Blocks: blocks,
		AgentMessage: true, AgentAuthor: "worker", AgentRecipient: "parent",
		Locked:           &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "after-blocks<&>"},
		ProviderMetadata: &ProviderMetadata{TurnID: "metadata-after-blocks"},
	}
	first := message
	if nested {
		first = CIREvent{Kind: EventCompaction, Seq: 7, ReplacementComplete: true, Replacement: []CIREvent{
			message,
			{Kind: EventToolCall, Seq: -2, CallID: "before-blocks", ToolName: "test", Input: legacyKeyOrderNestedFixture(19)},
			{Kind: EventCompaction, Seq: 7, ReplacementComplete: true, Replacement: []CIREvent{
				{Kind: EventMessage, Seq: 4, ID: "later", Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "nested-tail"}}},
				{Kind: EventMessage, Seq: -1, ID: "tie-first", Role: RoleUser},
				{Kind: EventToolResult, Seq: -1, CallID: "tie-second", Output: legacyKeyOrderNestedFixture(11)},
			}},
		}}
	}
	document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2}, Events: []CIREvent{
		first,
		{Kind: EventToolResult, Seq: -3, CallID: "following-event", Output: legacyKeyOrderNestedFixture(23),
			ProviderMetadata: &ProviderMetadata{TurnID: "following-metadata"}},
	}}
	raw, err := json.Marshal(document)
	if err != nil {
		test.Fatal(err)
	}
	return append(raw, bytes.Repeat([]byte(" \t\r\n"), ConversationManifestChunkBytes/2+1)...)
}

func TestLegacyCanonicalBlocksPublicLargeAndRecursiveProjection(test *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, count := range []int{0, 1, 4096} {
			test.Run(fmt.Sprintf("nested=%t/blocks=%d", nested, count), func(test *testing.T) {
				raw := legacyCanonicalBlocksPublicDocument(test, count, nested)
				want, proof, err := legacyNormalizationFrameOracle(raw)
				if err != nil || !proof.Valid() || HashContent(raw) == proof.Hash() {
					test.Fatal("public blocks fixture lost native canonical authority", err)
				}
				normalizer, input := legacyNativeScalarReviewStart(test, raw)
				result, err := legacyCanonicalBlocksPublicDrain(test, normalizer, input)
				if err != nil || result.events != 2 || !bytes.Equal(result.canonical, want) || HashContent(result.canonical) != proof.Hash() {
					test.Fatal("public blocks changed native canonical bytes or hash", result.events, len(result.canonical), len(want), err)
				}
				if count == 4096 && result.maxEncodingSteps <= 1 {
					test.Fatal("large public block encoding did not yield")
				}
				legacyCanonicalBlocksPublicSourceComplete(test, input)
			})
		}
	}
}

func TestLegacyCanonicalBlocksPublicNativeOverwriteParity(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		fields   string
		accepted bool
	}{
		{name: "nil", fields: `"blocks":null`, accepted: true},
		{name: "empty", fields: `"blocks":[]`, accepted: true},
		{name: "null-elements", fields: `"blocks":[null,{},null]`, accepted: true},
		{name: "folded-fields", fields: `"BLOCKS":[{"TYPE":"text","te\u0078t":"한🙂<&>\u2028\u2029\uD800","TEXT":null}]`, accepted: true},
		{name: "hidden-tail", fields: `"blocks":[{"type":"text","text":"head"},{"type":"text","text":"tail"}],"blocks":[{"text":"new"}],"blocKſ":[null,{}]`, accepted: true},
		{name: "empty-resets-backing", fields: `"blocks":[{"type":"text","text":"stale"}],"blocks":[],"blocks":[null,{}]`, accepted: true},
		{name: "null-resets-backing", fields: `"blocks":[{"type":"text","text":"stale"}],"blocks":null,"blocks":[null,{}]`, accepted: true},
		{name: "sticky-field-error", fields: `"blocks":[{"type":"text","text":7,"text":"repaired"}],"blocks":[]`},
		{name: "sticky-element-error", fields: `"blocks":[false],"blocks":null`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", `{"kind":"message","seq":4,"role":"user",`+fixture.fields+`,"provider_metadata":{"turn_id":"after-blocks"}},{"kind":"tool_result","seq":-1,"output":{"z":[{"b":2,"a":1}],"a":{"tail":true}}}`)
			want, wantErr := legacyNormalizationOracle(raw)
			if (wantErr == nil) != fixture.accepted {
				test.Fatal("public blocks fixture lost native acceptance", wantErr)
			}
			normalizer, input := legacyNativeScalarReviewStart(test, raw)
			result, err := legacyCanonicalBlocksPublicDrain(test, normalizer, input)
			if !errors.Is(err, wantErr) || (err == nil) != fixture.accepted || !bytes.Equal(result.canonical, want) {
				test.Fatal("public blocks changed native overwrite acceptance or bytes", err, wantErr)
			}
			if err == nil {
				if HashContent(result.canonical) != HashContent(want) {
					test.Fatal("public blocks changed the canonical hash")
				}
				legacyCanonicalBlocksPublicSourceComplete(test, input)
			} else if errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("authenticated native block error became a private source failure", err)
			}
		})
	}
}

func TestLegacyCanonicalBlocksPublicSourceFailureCannotComplete(test *testing.T) {
	raw := legacyCanonicalBlocksPublicDocument(test, 4096, false)
	for _, failure := range []string{"wrong-source-hash", "zero-eof-tail", "one-eof-tail"} {
		test.Run(failure, func(test *testing.T) {
			input := &legacySourceProofBuffer{body: raw}
			source := legacyNormalizationPreparationInput(test, input, true)
			if failure == "wrong-source-hash" {
				source.Parts[len(source.Parts)-1].Hash = HashContent([]byte("wrong final source part"))
			}
			readSource := source.Read
			triggered := false
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := readSource(ctx, offset, length)
				if failure == "wrong-source-hash" && offset == (len(source.Parts)-1)*ConversationManifestChunkBytes {
					triggered = true
				}
				if offset == len(raw) && (failure == "zero-eof-tail" && length == 0 || failure == "one-eof-tail" && length == 1) {
					triggered = true
					return []byte("!"), nil
				}
				return body, err
			}
			ctx, cancel := context.WithCancel(context.Background())
			normalizer, err := NewLegacyNormalizationDecoder(ctx, source)
			cancel()
			if err != nil || normalizer == nil || len(input.reads) != 0 {
				test.Fatal("public block failure constructor eagerly read source", err)
			}
			result, err := legacyCanonicalBlocksPublicDrain(test, normalizer, input)
			if !triggered || !errors.Is(err, ErrPreparedDocRangeObject) || result.canonical != nil || result.events != 2 {
				test.Fatal("late source failure completed public blocks or skipped provisional encodings", triggered, result.events, err)
			}
			reads := len(input.reads)
			repeated, repeatedErr := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
			legacyNativeEventZero(test, repeated)
			if !errors.Is(repeatedErr, ErrPreparedDocRangeObject) || len(input.reads) != reads {
				test.Fatal("failed public blocks resumed source reads or completed", repeatedErr)
			}
		})
	}
}
