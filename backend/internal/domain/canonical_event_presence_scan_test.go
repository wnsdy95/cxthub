package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type archivedCanonicalEventPresence struct{}

func (*archivedCanonicalEventPresence) UnmarshalJSON([]byte) error { return nil }

func archivedCanonicalEventDecode(raw []byte) (CIREvent, error) {
	type wire CIREvent
	var decoded wire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return CIREvent{}, err
	}
	var fields map[string]archivedCanonicalEventPresence
	if err := json.Unmarshal(raw, &fields); err != nil {
		return CIREvent{}, err
	}
	event := CIREvent(decoded)
	for name, bit := range map[string]eventFieldPresence{
		"provider_metadata": eventFieldProviderMetadata, "agent_message": eventFieldAgentMessage,
		"agent_author": eventFieldAgentAuthor, "agent_recipient": eventFieldAgentRecipient,
		"replacement": eventFieldReplacement, "replacement_complete": eventFieldReplacementComplete,
		"locked": eventFieldLocked,
	} {
		if _, present := fields[name]; present {
			event.fieldPresence |= bit
		}
	}
	return event, nil
}

func canonicalEventPresenceScanParity(test *testing.T, raw []byte) {
	test.Helper()
	before := bytes.Clone(raw)
	want, wantErr := archivedCanonicalEventDecode(raw)
	got, gotErr := decodeCanonicalEvent(raw)
	if fmt.Sprint(wantErr) != fmt.Sprint(gotErr) || !reflect.DeepEqual(want, got) || !bytes.Equal(raw, before) {
		test.Fatalf("event decoder changed values, errors, presence or input: want=%v got=%v", wantErr, gotErr)
	}
	if gotErr == nil {
		wantRaw, wantEncodeErr := canonicalDecodedEventJSON(want)
		gotRaw, gotEncodeErr := canonicalDecodedEventJSON(got)
		if fmt.Sprint(wantEncodeErr) != fmt.Sprint(gotEncodeErr) || !bytes.Equal(wantRaw, gotRaw) {
			test.Fatal("presence changed canonical encoding or union rejection")
		}
	}
}

func TestCanonicalEventPresenceScanArchivedParity(test *testing.T) {
	for _, candidate := range canonicalEventCases() {
		test.Run(candidate.name, func(test *testing.T) { canonicalEventPresenceScanParity(test, candidate.raw) })
	}
	for _, raw := range []string{
		`null`, ` null `, `{}`, `[]`, `true`, `0`, `"text"`, ``,
		`{"kind":"message","seq":1,"blocks":[{"type":"text","text":"fake \\\"agent_message\\\":false, }"}]}`,
		`{"kind":"message","seq":1,"unknown":{"agent_message":false,"locked":null}}`,
		`{"kind":"message","agent_message":true,"AGENT_MESSAGE":false,"agent_author":""}`,
		`{"kind":"message","AGENT_MESSAGE":true,"agent_message":null,"agent_author":""}`,
		`{"kind":"message","\u0061gent_message":false,"agent_message":true,"agent_message":null}`,
		`{"kind":"message","agent_message":true,"\u0061gent_author":"","\u0061gent_recipient":""}`,
		`{"kind":"compaction","replacement":[{"kind":"message","agent_message":false}],"replacement":null}`,
		" \n { \"kind\" : \"message\" , \"agent_message\" : false , \"blocks\" : [] } \t",
	} {
		canonicalEventPresenceScanParity(test, []byte(raw))
	}
}

func TestCanonicalEventPresenceScanOwnsDecodedValues(test *testing.T) {
	raw := []byte(`{"agent_author":"author","agent_message":true,"blocks":[{"text":"retained","type":"text"}],"kind":"message","role":"assistant","seq":1}`)
	event, err := decodeCanonicalEvent(raw)
	if err != nil {
		test.Fatal(err)
	}
	want, err := canonicalDecodedEventJSON(event)
	if err != nil {
		test.Fatal(err)
	}
	clear(raw)
	got, err := canonicalDecodedEventJSON(event)
	if err != nil || !bytes.Equal(got, want) || event.AgentAuthor != "author" || event.Blocks[0].Text != "retained" {
		test.Fatal("decoder retained mutable input backing", err)
	}
}

func TestCanonicalEventPresenceScanUnusualKeysAndNestedReset(test *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"kind":"reasoning","locKed":{"provider":"codex","scheme":"s","blob":"b"}}`),
		[]byte(`{"kind":"message","\ud800gent_message":false,"\udfff":null}`),
		[]byte(`{"kind":"message","\u0000agent_message":true,"agent_message\u0000":false}`),
		append(append([]byte(`{"kind":"reasoning","loc`), 0xff), []byte(`ked":null,"locked":null}`)...),
		append(append([]byte(`{"kind":"message","`), 0xc0, 0xaf), []byte(`agent_message":false}`)...),
	} {
		canonicalEventPresenceScanParity(test, raw)
	}
	raw := []byte(`{"kind":"compaction","replacement_complete":false,"replacement":[{"kind":"message","agent_message":false},{"kind":"reasoning","LOCKED":{"provider":"codex","scheme":"s","blob":"b"}},{"kind":"message","agent_message":true,"agent_author":"a","AGENT_AUTHOR":null},{"kind":"compaction","replacement":[{"kind":"message","agent_message":false}],"replacement":null}]}`)
	event, err := decodeCanonicalEvent(raw)
	if err != nil || event.fieldPresence != eventFieldReplacement|eventFieldReplacementComplete || len(event.Replacement) != 4 {
		test.Fatal("top-level replacement presence changed", err)
	}
	expected := []eventFieldPresence{eventFieldAgentMessage, 0, eventFieldAgentMessage | eventFieldAgentAuthor, eventFieldReplacement}
	for ordinal, presence := range expected {
		if event.Replacement[ordinal].fieldPresence != presence {
			test.Fatalf("nested exact presence changed at %d: got=%d want=%d", ordinal, event.Replacement[ordinal].fieldPresence, presence)
		}
	}
	if event.Replacement[1].Locked == nil || event.Replacement[2].AgentAuthor != "a" || event.Replacement[3].Replacement != nil {
		test.Fatal("folded typed field, null scalar merge or duplicate replacement reset changed")
	}
}

func FuzzCanonicalEventPresenceScanParity(fuzzer *testing.F) {
	for _, candidate := range canonicalEventCases() {
		if len(candidate.raw) <= 16<<10 {
			fuzzer.Add(candidate.raw)
		}
	}
	fuzzer.Add([]byte(` null `))
	fuzzer.Add([]byte(`{"unknown":{"locked":null},"agent_message":false}`))
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 16<<10 {
			test.Skip()
		}
		canonicalEventPresenceScanParity(test, raw)
	})
}

func BenchmarkCanonicalEventPresencePipeline(benchmark *testing.B) {
	for _, fixture := range []struct {
		name string
		raw  string
	}{
		{"message", `{"blocks":[{"text":"synthetic message 한🙂","type":"text"}],"kind":"message","role":"user","seq":17}`},
		{"agent", `{"agent_author":"author","agent_message":true,"agent_recipient":"all","blocks":[],"kind":"message","role":"assistant","seq":17}`},
		{"escaped", `{"\u0061gent_message":false,"blocks":[],"kind":"message","role":"assistant","seq":17}`},
		{"unknown", `{"blocks":[],"kind":"message","role":"user","seq":17,"unknown":"` + strings.Repeat("x", 256<<10) + `"}`},
	} {
		raw := []byte(fixture.raw)
		benchmark.Run(fixture.name+"/decode", func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for benchmark.Loop() {
				if _, err := decodeCanonicalEvent(raw); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
		if fixture.name != "message" && fixture.name != "agent" {
			continue
		}
		benchmark.Run(fixture.name+"/verify", func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for benchmark.Loop() {
				if _, err := verifyCanonicalEventBytes(context.Background(), CIRVersionV2, raw); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
	}
}
