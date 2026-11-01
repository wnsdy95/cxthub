package domain

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func canonicalMessageEventRaw(test testing.TB, event CIREvent) []byte {
	test.Helper()
	raw, err := canonicalDecodedEventJSON(event)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func canonicalMessageEventPredecessor(ctx context.Context, version string, raw []byte) (int, error) {
	if ctx.Err() != nil {
		return verifyCanonicalEventBytes(ctx, version, raw)
	}
	projected, err := canonicalEventProjection(ctx, raw)
	if err != nil {
		return 0, err
	}
	return verifyCanonicalEventBytes(ctx, version, projected)
}

func TestCanonicalMessageEventRejectsNoncanonicalBoundaries(test *testing.T) {
	for _, raw := range []string{
		`{"blocks":[{"text": "hello","type":"text"}],"kind":"message","role":"user","seq":17}`,
		`{"blocks":[{"text":"hello","type":"text"}],"kind":"message","role": "user","seq":17}`,
		`{"blocks":[],"kind":"message","role": "user","seq":17}`,
		`{"blocks":[{"text":null,"type":"text"}],"kind":"message","role":"user","seq":17}`,
		`{"blocks":[],"kind":"message","role":"user","seq":-0}`,
		`{"blocks":[],"kind":"message","role":"user","seq":01}`,
		`{"blocks":[],"kind":"message","role":"user","seq":1e0}`,
		`{"blocks":[],"kind":"message","role":"user","seq":0,"unknown":true}`,
	} {
		want, expected := strictCanonicalEventReference(context.Background(), CIRVersionV2, []byte(raw))
		got, actual := verifyCanonicalEvent(context.Background(), CIRVersionV2, []byte(raw))
		if expected == nil || got != want || fmt.Sprint(actual) != fmt.Sprint(expected) {
			test.Errorf("strict shape accepted noncanonical bytes: %s got=%d/%v want=%d/%v", raw, got, actual, want, expected)
		}
	}
}

func TestCanonicalMessageEventFinalCancellationMatchesPredecessor(test *testing.T) {
	for _, block := range []bool{false, true} {
		event := CIREvent{Kind: EventMessage, Role: RoleUser, Seq: 17, Blocks: []ContentBlock{}}
		if block {
			event.Blocks = []ContentBlock{{Type: "text", Text: `literal \uD55C`}}
		}
		raw := canonicalMessageEventRaw(test, event)
		for _, verify := range []func(context.Context, string, []byte) (int, error){canonicalMessageEventPredecessor, verifyCanonicalEvent} {
			bound, cancel := context.WithCancel(context.Background())
			checks := 0
			ctx := canonicalAdmissionContext{Context: bound, check: func() {
				checks++
				if checks == 3 {
					cancel()
				}
			}}
			sequence, err := verify(ctx, CIRVersionV2, raw)
			cancel()
			if sequence != 0 || err != context.Canceled || checks != 3 {
				test.Errorf("final checkpoint changed: block=%t sequence=%d error=%v checks=%d", block, sequence, err, checks)
			}
		}
	}
}

func TestCanonicalMessageEventAcceptedShapeParity(test *testing.T) {
	minimum := -int(^uint(0)>>1) - 1
	maximum := int(^uint(0) >> 1)
	for _, version := range []string{"", CIRVersionV1, CIRVersionV2} {
		for _, role := range []Role{RoleUser, RoleAssistant, RoleSystem, RoleDeveloper} {
			for _, sequence := range []int{minimum, -17, -1, 0, 1, 17, maximum} {
				for _, text := range []string{"", "plain", `literal \uD55C`, "\ud55c🙂", "<>&", "\u2028\u2029", "\x00\b\f\n\r\t", "\"\\", `"}],"kind":"message","role":"user","seq":0}`} {
					for _, block := range []bool{false, true} {
						event := CIREvent{Kind: EventMessage, Role: role, Seq: sequence, Blocks: []ContentBlock{}}
						if block {
							event.Blocks = []ContentBlock{{Type: "text", Text: text}}
						}
						raw := canonicalMessageEventRaw(test, event)
						want, expected := strictCanonicalEventReference(context.Background(), version, raw)
						got, accepted := canonicalMessageEventSequence(version, raw)
						if expected != nil || !accepted || got != want {
							test.Fatalf("canonical common shape lost: version=%q seq=%d accepted=%t want=%d error=%v raw=%s", version, got, accepted, want, expected, raw)
						}
						got, actual := verifyCanonicalEvent(context.Background(), version, raw)
						if actual != nil || got != want {
							test.Fatal("shared verifier differs from strict reference", got, want, actual)
						}
					}
				}
			}
		}
	}
}

func TestCanonicalMessageEventSubsetDeclinesWithoutChangingFallback(test *testing.T) {
	for _, event := range []CIREvent{
		{Kind: EventMessage, Role: Role("custom"), Seq: 1},
		{Kind: EventMessage, Role: RoleUser, Seq: 2, ID: "id"},
		{Kind: EventMessage, Role: RoleUser, Seq: 3, CompactSummary: true},
		{Kind: EventMessage, Role: RoleAssistant, Seq: 4, AgentMessage: true, AgentAuthor: "worker"},
		{Kind: EventMessage, Role: RoleUser, Seq: 5, Blocks: []ContentBlock{{Type: "text", Text: "first"}, {Type: "text", Text: "second"}}},
		{Kind: EventToolCall, Seq: 6, ToolName: "test", Input: map[string]any{"kind": "message"}},
		{Kind: EventReasoning, Seq: 7, RedactedSummary: "reasoning"},
		{Kind: EventCompaction, Seq: 8, Replacement: []CIREvent{}, ReplacementComplete: true},
	} {
		raw := canonicalMessageEventRaw(test, event)
		if _, accepted := canonicalMessageEventSequence(CIRVersionV2, raw); accepted {
			test.Fatal("extended shape did not decline", string(raw))
		}
		for _, version := range []string{"", CIRVersionV1, CIRVersionV2, "99"} {
			for _, canceled := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				if canceled {
					cancel()
				}
				want, expected := strictCanonicalEventReference(ctx, version, raw)
				got, actual := verifyCanonicalEvent(ctx, version, raw)
				cancel()
				if got != want || fmt.Sprint(actual) != fmt.Sprint(expected) {
					test.Fatalf("fallback changed: version=%q canceled=%t got=%d/%v want=%d/%v", version, canceled, got, actual, want, expected)
				}
			}
		}
	}
}

func TestCanonicalMessageEventBoundariesAndMalformedMutations(test *testing.T) {
	raw := canonicalMessageEventRaw(test, CIREvent{Kind: EventMessage, Role: RoleUser, Seq: 17, Blocks: []ContentBlock{{Type: "text", Text: `original \uD55C`}}})
	cases := [][]byte{bytes.Clone(raw)}
	for ordinal := range raw {
		cases = append(cases, append(bytes.Clone(raw[:ordinal]), raw[ordinal+1:]...))
		for _, replacement := range []byte{'"', '\\', ':', ',', ' ', 0xff} {
			changed := bytes.Clone(raw)
			changed[ordinal] = replacement
			cases = append(cases, changed)
		}
	}
	for _, number := range []string{"-0", "00", "+1", "1.0", "1e0", `"1"`, "null", strconv.Itoa(int(^uint(0)>>1)) + "0"} {
		cases = append(cases, bytes.Replace(raw, []byte(`"seq":17`), []byte(`"seq":`+number), 1))
	}
	for _, candidate := range cases {
		before := bytes.Clone(candidate)
		for _, version := range []string{"", CIRVersionV1, CIRVersionV2, "99"} {
			want, expected := strictCanonicalEventReference(context.Background(), version, candidate)
			got, actual := verifyCanonicalEvent(context.Background(), version, candidate)
			if got != want || fmt.Sprint(actual) != fmt.Sprint(expected) || !bytes.Equal(before, candidate) {
				test.Fatalf("literal mutation changed strict validation: version=%q got=%d/%v want=%d/%v raw=%q", version, got, actual, want, expected, candidate)
			}
		}
	}
	base := len(canonicalMessageEventRaw(test, CIREvent{Kind: EventMessage, Role: RoleUser, Seq: 0, Blocks: []ContentBlock{{Type: "text"}}}))
	for _, length := range []int{canonicalCheckBytes, canonicalCheckBytes + 1} {
		candidate := canonicalMessageEventRaw(test, CIREvent{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("x", length-base)}}})
		_, accepted := canonicalMessageEventSequence(CIRVersionV2, candidate)
		if accepted != (length <= canonicalCheckBytes) {
			test.Fatal("fast shape changed its byte bound", length, len(candidate), accepted)
		}
		want, expected := strictCanonicalEventReference(context.Background(), CIRVersionV2, candidate)
		got, actual := verifyCanonicalEvent(context.Background(), CIRVersionV2, candidate)
		if got != want || fmt.Sprint(actual) != fmt.Sprint(expected) {
			test.Fatal("projection boundary changed strict acceptance", got, want, actual, expected)
		}
	}
}

func FuzzCanonicalMessageEventParity(fuzzer *testing.F) {
	for _, text := range []string{"", "plain", `literal \uD55C`, "\ud55c🙂<>&\u2028\u2029\n\"\\"} {
		fuzzer.Add(canonicalMessageEventRaw(fuzzer, CIREvent{Kind: EventMessage, Role: RoleUser, Seq: 17, Blocks: []ContentBlock{{Type: "text", Text: text}}}))
	}
	fuzzer.Add([]byte(`{"blocks":[],"kind":"message","role": "user","seq":17}`))
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > canonicalCheckBytes {
			test.Skip()
		}
		for _, version := range []string{"", CIRVersionV1, CIRVersionV2, "99"} {
			want, expected := strictCanonicalEventReference(context.Background(), version, raw)
			got, actual := verifyCanonicalEvent(context.Background(), version, raw)
			if got != want || fmt.Sprint(actual) != fmt.Sprint(expected) {
				test.Fatalf("message verifier changed strict semantics: version=%q got=%d/%v want=%d/%v raw=%q", version, got, actual, want, expected, raw)
			}
		}
	})
}

func BenchmarkCanonicalMessageEventVerification(benchmark *testing.B) {
	benchmarkCanonicalMessageEventVerification(benchmark, false)
}

func BenchmarkCanonicalMessageEventVerificationReverse(benchmark *testing.B) {
	benchmarkCanonicalMessageEventVerification(benchmark, true)
}

func benchmarkCanonicalMessageEventVerification(benchmark *testing.B, reverse bool) {
	cases := []struct {
		name  string
		event CIREvent
	}{
		{"empty", CIREvent{Kind: EventMessage, Role: RoleUser, Seq: 17}},
		{"range-fixture", CIREvent{Kind: EventMessage, Role: RoleUser, Seq: 128000, Blocks: []ContentBlock{{Type: "text", Text: `source-origin-2026-10-24T00:00:00Z range event 128000 \uD55C`}}}},
		{"extended", CIREvent{Kind: EventMessage, Role: RoleAssistant, Seq: 17, AgentMessage: true, AgentAuthor: "worker", Blocks: []ContentBlock{{Type: "text", Text: "not a subset message"}}}},
	}
	for _, candidate := range cases {
		raw := canonicalMessageEventRaw(benchmark, candidate.event)
		implementations := []struct {
			name   string
			verify func(context.Context, string, []byte) (int, error)
		}{{"original", canonicalMessageEventPredecessor}, {"current", verifyCanonicalEvent}}
		if reverse {
			implementations[0], implementations[1] = implementations[1], implementations[0]
		}
		for _, implementation := range implementations {
			benchmark.Run(candidate.name+"/"+implementation.name, func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for range benchmark.N {
					sequence, err := implementation.verify(context.Background(), CIRVersionV2, raw)
					if err != nil || sequence != candidate.event.Seq {
						benchmark.Fatal(sequence, err)
					}
				}
			})
		}
	}
}
