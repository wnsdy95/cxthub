package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestDecodeDocRangeMetadataPreservesIndexedIdentity(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		body     string
		expected DocRangeEntry
	}{
		{name: "turn", body: `{"kind":"turn","seq":41,"role":"user"}`, expected: DocRangeEntry{Kind: EventTurn}},
		{name: "seed message", body: `{"kind":"message","seq":41,"role":"assistant","compact_summary":true,"blocks":[{"type":"text","text":" \ufeff[cxt seed] Branch-switch context: details"}]}`, expected: DocRangeEntry{Kind: EventMessage, CompactSummary: true, SeedSummary: true}},
		{name: "tool call", body: `{"kind":"tool_call","seq":41,"tool_name":"Bash","input":{"command":"needle"}}`, expected: DocRangeEntry{Kind: EventToolCall, ToolName: "Bash"}},
		{name: "tool result", body: `{"kind":"tool_result","seq":41,"output":"needle"}`, expected: DocRangeEntry{Kind: EventToolResult}},
		{name: "opaque reasoning", body: `{"kind":"reasoning","seq":41,"redacted_summary":" \ufeff\t","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque-only needle"}}`, expected: DocRangeEntry{Kind: EventReasoning, EmptyReasoning: true}},
		{name: "visible reasoning", body: `{"kind":"reasoning","seq":41,"redacted_summary":"visible needle"}`, expected: DocRangeEntry{Kind: EventReasoning}},
		{name: "compaction", body: `{"kind":"compaction","seq":41,"replacement":[],"replacement_complete":true}`, expected: DocRangeEntry{Kind: EventCompaction}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.body)
			original := bytes.Clone(raw)
			location := DocEventIndex{Index: 7, Offset: 123, Length: len(raw), Hash: HashContent(raw), Seq: 41, Role: "indexed role", Text: "indexed projection"}
			fixture.expected.DocEventIndex = location
			actual, err := DecodeDocRangeMetadata(raw, location)
			if err != nil || actual != fixture.expected {
				test.Fatalf("metadata changed indexed identity or presentation: got %+v, want %+v, error %v", actual, fixture.expected, err)
			}
			encoded, err := json.Marshal(actual)
			if err != nil || bytes.Contains(encoded, []byte("opaque-only")) || !bytes.Equal(raw, original) {
				test.Fatalf("metadata retained opaque state or changed source bytes: %s, %v", encoded, err)
			}
		})
	}
}

func TestDecodeDocRangeMetadataRejectsInvalidCoordinatesAndBytes(test *testing.T) {
	valid := []byte(`{"kind":"reasoning","seq":41,"redacted_summary":"visible","locked":{"blob":"opaque"}}`)
	for _, fixture := range []struct {
		name   string
		body   []byte
		change func(*DocEventIndex)
	}{
		{name: "negative index", change: func(location *DocEventIndex) { location.Index = -1 }},
		{name: "negative offset", change: func(location *DocEventIndex) { location.Offset = -1 }},
		{name: "negative length", change: func(location *DocEventIndex) { location.Length = -1 }},
		{name: "zero length", change: func(location *DocEventIndex) { location.Length = 0 }},
		{name: "wrong length", change: func(location *DocEventIndex) { location.Length++ }},
		{name: "wrong hash", change: func(location *DocEventIndex) { location.Hash = HashContent([]byte("other event")) }},
		{name: "changed opaque bytes", body: bytes.Replace(valid, []byte("opaque"), []byte("altered"), 1), change: func(location *DocEventIndex) { location.Hash = HashContent(valid) }},
		{name: "malformed JSON", body: []byte(`{"kind":"reasoning","locked":{"blob":"\q"}}`)},
		{name: "wrong role type", body: []byte(`{"kind":"turn","role":false}`)},
		{name: "wrong compact type", body: []byte(`{"kind":"message","compact_summary":"true"}`)},
		{name: "wrong summary type", body: []byte(`{"kind":"reasoning","redacted_summary":false}`)},
		{name: "wrong blocks type", body: []byte(`{"kind":"message","blocks":"not an array"}`)},
		{name: "unknown kind", body: []byte(`{"kind":"unknown"}`)},
		{name: "missing kind", body: []byte(`{"seq":41}`)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := fixture.body
			if raw == nil {
				raw = valid
			}
			location := DocEventIndex{Index: 7, Offset: 123, Length: len(raw), Hash: HashContent(raw), Seq: 41}
			if fixture.change != nil {
				fixture.change(&location)
			}
			entry, err := DecodeDocRangeMetadata(raw, location)
			if !errors.Is(err, ErrIntegrity) || entry != (DocRangeEntry{}) {
				test.Fatalf("invalid event returned metadata: %+v, %v", entry, err)
			}
		})
	}
}

func TestDecodeDocRangeMetadataOpaqueLockedHasBoundedAllocations(test *testing.T) {
	for _, kind := range []EventKind{EventReasoning, EventMessage} {
		for _, size := range []int{1024, 4 << 20} {
			name := string(kind) + "/small"
			if size > 1024 {
				name = string(kind) + "/large"
			}
			test.Run(name, func(test *testing.T) {
				fields := `"redacted_summary":" \ufeff\t"`
				if kind == EventMessage {
					fields = `"role":"assistant","agent_message":true,"blocks":[{"type":"text","text":"ordinary visible text"}]`
				}
				raw := []byte(`{"kind":"` + string(kind) + `","seq":41,` + fields + `,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque-only needle [cxt seed] Branch-switch context: ` + strings.Repeat(`x\"\\`, size/5) + `"}}`)
				location := DocEventIndex{Index: 7, Offset: 123, Length: len(raw), Hash: HashContent(raw), Seq: 41}
				check := func() {
					entry, err := DecodeDocRangeMetadata(raw, location)
					if err != nil || entry.DocEventIndex != location || entry.Kind != kind || entry.SeedSummary || entry.EmptyReasoning != (kind == EventReasoning) {
						test.Fatalf("opaque state affected presentation or identity: %+v, %v", entry, err)
					}
				}
				check()
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				for range 3 {
					check()
				}
				runtime.ReadMemStats(&after)
				allocated := (after.TotalAlloc - before.TotalAlloc) / 3
				test.Logf("source=%d bytes, allocated=%d bytes/event", len(raw), allocated)
				if allocated > 64<<10 {
					test.Fatalf("metadata allocated in proportion to opaque state: %d bytes/event", allocated)
				}
			})
		}
	}
}
