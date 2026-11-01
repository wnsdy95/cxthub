package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestDocFindFieldsExactSummarySpan(testContext *testing.T) {
	for _, test := range []struct {
		name   string
		prefix string
		value  string
		suffix string
	}{
		{
			name:   "identical value inside locked and unknown objects",
			prefix: `{"kind":"reasoning","locked":{"blob":"same summary","redacted_summary":"same summary"},"unknown":[{"nested":{"redacted_summary":"same summary"}}],"redacted_summary":`,
			value:  `"same summary"`,
			suffix: `}`,
		},
		{
			name:   "escaped key and value",
			prefix: `{"k\u0069nd":"reas\u006fning","locked":{"redacted_summary":"visible\n\"quoted\""},"redacted\u005fsummary" : `,
			value:  `"visible\n\"quoted\""`,
			suffix: `,"unknown":{"redacted_summary":"later nested"}}`,
		},
		{
			name:   "duplicate top level takes last escaped key",
			prefix: `{"kind":"message","kind":"reasoning","redacted_summary":"first","redacted\u005fsummary":`,
			value:  `"last"`,
			suffix: `,"locked":{"blob":"last"}}`,
		},
		{
			name:   "empty value remains a locator",
			prefix: `{"kind":"reasoning","redacted_summary":`,
			value:  `""`,
			suffix: `}`,
		},
		{
			name:   "decoded presentation whitespace remains a locator",
			prefix: `{"kind":"reasoning","redacted_summary":`,
			value:  `" \t\n\r\f\u000b\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"`,
			suffix: `}`,
		},
		{
			name:   "raw unicode and BOM whitespace remains a locator",
			prefix: `{"kind":"reasoning","redacted_summary":`,
			value:  "\"\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000\ufeff\"",
			suffix: `}`,
		},
		{
			name:   "nonpresentation whitespace",
			prefix: `{"kind":"reasoning","redacted_summary":`,
			value:  `"\u0085\u200b\ud83d\ude42"`,
			suffix: `}`,
		},
		{
			name:   "leading and trailing JSON whitespace",
			prefix: " \n\t\r{\n\t\"redacted_summary\":  ",
			value:  "\"\ud55c\uae00 visible\"",
			suffix: " \r,\"kind\":\"reasoning\"}\n \t",
		},
		{
			name:   "null followed by string",
			prefix: `{"kind":"reasoning","redacted_summary":null,"redacted_summary":`,
			value:  `"last"`,
			suffix: `}`,
		},
	} {
		testContext.Run(test.name, func(testContext *testing.T) {
			raw := []byte(test.prefix + test.value + test.suffix)
			before := bytes.Clone(raw)
			eventHash := HashContent(raw)
			fields, err := BuildDocFindFields(eventHash, raw)
			if err != nil {
				testContext.Fatal(err)
			}
			want := DocFindFields{
				Version: 1, EventHash: eventHash, EventBytes: len(raw),
				Summary: &DocFindSpan{From: len(test.prefix), To: len(test.prefix) + len(test.value)},
			}
			if !reflect.DeepEqual(fields, want) || fields.Validate(eventHash, len(raw)) != nil {
				testContext.Fatalf("unexpected fields: got %+v want %+v", fields, want)
			}
			if !bytes.Equal(raw[fields.Summary.From:fields.Summary.To], []byte(test.value)) || !bytes.Equal(raw, before) {
				testContext.Fatal("projection changed source bytes or located the wrong value")
			}
			encoded, err := fields.Encode()
			if err != nil || len(encoded) > MaxDocFindFieldsBytes {
				testContext.Fatal("bounded encoding failed", err)
			}
			decoded, err := DecodeDocFindFields(HashContent(encoded), eventHash, len(raw), encoded)
			if err != nil || !reflect.DeepEqual(decoded, fields) {
				testContext.Fatal("projection did not round trip", decoded, err)
			}
		})
	}
}

func TestDocFindFieldsAbsentAndNullSummary(testContext *testing.T) {
	for _, body := range []string{
		`{"kind":"reasoning"}`,
		`{"kind":"reasoning","redacted_summary":null}`,
		`{"kind":"reasoning","locked":{"redacted_summary":"nested"},"unknown":{"redacted_summary":"nested"}}`,
		`{"kind":"reasoning","redacted_summary_extra":"not the field","redacted_summary\u0000":"also not the field"}`,
		`{"kind":"reasoning","redacted_summary":"first","redacted\u005fsummary":null}`,
	} {
		raw := []byte(body)
		eventHash := HashContent(raw)
		fields, err := BuildDocFindFields(eventHash, raw)
		if err != nil || fields.Summary != nil || fields.Validate(eventHash, len(raw)) != nil {
			testContext.Fatal("absent/null summary produced a locator", fields, err)
		}
		encoded, err := fields.Encode()
		if err != nil || !bytes.Contains(encoded, []byte(`"summary":null`)) {
			testContext.Fatal("nil summary did not encode explicitly", string(encoded), err)
		}
		decoded, err := DecodeDocFindFields(HashContent(encoded), eventHash, len(raw), encoded)
		if err != nil || !reflect.DeepEqual(decoded, fields) {
			testContext.Fatal("nil summary did not round trip", decoded, err)
		}
	}
}

func TestDocFindFieldsExactMessageBlocksSpan(testContext *testing.T) {
	for _, fixture := range []struct {
		name   string
		prefix string
		value  string
		suffix string
	}{
		{
			name:   "agent message locked and unknown identical arrays",
			prefix: `{"kind":"message","agent_message":true,"locked":{"blob":"opaque secret","blocks":[{"type":"text","text":"public message"}]},"unknown":{"blocks":[{"type":"text","text":"public message"}]},"blocks":`,
			value:  `[{"type":"text","text":"public message"}]`,
			suffix: `,"redacted_summary":{"ignored":"wrong type"}}`,
		},
		{
			name:   "identical empty array first inside locked blob",
			prefix: `{"kind":"message","agent_message":true,"locked":{"blob":"[]"},"blocks":`,
			value:  `[]`,
			suffix: `}`,
		},
		{
			name:   "escaped keys and values",
			prefix: `{"k\u0069nd":"mess\u0061ge","bl\u006fcks" : `,
			value:  "[{\"te\\u0078t\":\"\ud55c\uae00\\n\\\"quoted\\\"\",\"type\":\"text\"}]",
			suffix: `,"locked":{"blocks":["later nested"]}}`,
		},
		{
			name:   "invalid earlier blocks overwritten by escaped array key",
			prefix: `{"kind":"reasoning","blocks":false,"kind":"message","redacted_summary":false,"bl\u006fcks":`,
			value:  `[{"type":"text","text":"last"}]`,
			suffix: `}`,
		},
		{
			name:   "leading trailing and field whitespace",
			prefix: " \n\t{\"blocks\": \n ",
			value:  `[ {"type":"text","text":"public"} ]`,
			suffix: " \n,\"kind\":\"message\"}\r\n ",
		},
		{
			name:   "large locked blob small public blocks",
			prefix: `{"kind":"message","agent_message":true,"locked":{"blob":"` + strings.Repeat("opaque secret ", 32<<10) + `"},"blocks":`,
			value:  `[{"type":"text","text":"public message"}]`,
			suffix: `}`,
		},
		{
			name:   "large public blocks remain a span",
			prefix: `{"kind":"message","agent_message":true,"locked":{"blob":"opaque secret"},"blocks":`,
			value:  `[{"type":"text","text":"` + strings.Repeat("public message ", 32<<10) + `"}]`,
			suffix: `}`,
		},
	} {
		testContext.Run(fixture.name, func(testContext *testing.T) {
			raw := []byte(fixture.prefix + fixture.value + fixture.suffix)
			eventHash := HashContent(raw)
			fields, err := BuildDocFindFields(eventHash, raw)
			want := &DocFindSpan{From: len(fixture.prefix), To: len(fixture.prefix) + len(fixture.value)}
			if err != nil || fields.Summary != nil || !reflect.DeepEqual(fields.Blocks, want) || fields.Validate(eventHash, len(raw)) != nil {
				testContext.Fatal("message did not select exact public blocks", fields, want, err)
			}
			if !bytes.Equal(raw[fields.Blocks.From:fields.Blocks.To], []byte(fixture.value)) || HashContent(raw) != eventHash {
				testContext.Fatal("message blocks locator changed source or located nested bytes")
			}
			encoded, err := fields.Encode()
			if err != nil || !bytes.Contains(encoded, []byte(`"summary":null`)) || bytes.Contains(encoded, []byte("opaque secret")) || bytes.Contains(encoded, []byte("public message")) {
				testContext.Fatal("projection retained a public or locked source body", string(encoded), err)
			}
			decoded, err := DecodeDocFindFields(HashContent(encoded), eventHash, len(raw), encoded)
			if err != nil || !reflect.DeepEqual(decoded, fields) {
				testContext.Fatal("message projection did not round trip", decoded, err)
			}
		})
	}
}

func TestDocFindFieldsSelectsOnlyFinalKindField(testContext *testing.T) {
	for _, body := range []string{
		`{"kind":"message"}`,
		`{"kind":"message","blocks":null}`,
		`{"kind":"message","blocks":false,"blocks":null,"redacted_summary":false}`,
		`{"kind":"message","blocks":[],"blocks":null,"redacted_summary":"ignored"}`,
		`{"kind":"message","unknown":{"blocks":[]},"locked":{"blocks":[]},"blocks_extra":[],"blocks\u0000":[]}`,
		`{"kind":"message","redacted_summary":{},"blocks":[]}`,
		`{"kind":"message","redacted_summary":0,"blocks":[]}`,
		`{"kind":"message","redacted_summary":[],"blocks":[]}`,
		`{"kind":"reasoning","blocks":false,"redacted_summary":"selected"}`,
		`{"kind":"message","blocks":{},"kind":"reasoning","redacted_summary":"selected"}`,
		`{"kind":"reasoning","redacted_summary":false,"kind":"message","blocks":[]}`,
	} {
		raw := []byte(body)
		fields, err := BuildDocFindFields(HashContent(raw), raw)
		if err != nil || fields.Validate(HashContent(raw), len(raw)) != nil || fields.Summary != nil && fields.Blocks != nil {
			testContext.Fatal("unselected field affected projection", body, fields, err)
		}
		if docRangeJSONTextEquals(docRangeJSONField(raw, "kind"), string(EventMessage)) && fields.Summary != nil {
			testContext.Fatal("message retained a reasoning summary", fields)
		}
		if docRangeJSONTextEquals(docRangeJSONField(raw, "kind"), string(EventReasoning)) && fields.Blocks != nil {
			testContext.Fatal("reasoning retained message blocks", fields)
		}
	}
}

func TestDocFindFieldsPreservesReasoningCanonicalBytes(testContext *testing.T) {
	for _, body := range []string{`{"kind":"reasoning"}`, `{"kind":"reasoning","redacted_summary":"visible"}`} {
		fields, err := BuildDocFindFields(HashContent([]byte(body)), []byte(body))
		if err != nil {
			testContext.Fatal(err)
		}
		legacy, err := canonicalJSON(struct {
			Version    int          `json:"version"`
			EventHash  ContentHash  `json:"event_hash"`
			EventBytes int          `json:"event_bytes"`
			Summary    *DocFindSpan `json:"summary"`
		}{Version: fields.Version, EventHash: fields.EventHash, EventBytes: fields.EventBytes, Summary: fields.Summary})
		if err != nil {
			testContext.Fatal(err)
		}
		encoded, err := fields.Encode()
		if err != nil || !bytes.Equal(encoded, legacy) {
			testContext.Fatal("blocks changed existing reasoning canonical bytes", string(encoded), string(legacy), err)
		}
		if decoded, err := DecodeDocFindFields(HashContent(legacy), fields.EventHash, fields.EventBytes, legacy); err != nil || !reflect.DeepEqual(decoded, fields) {
			testContext.Fatal("existing reasoning projection no longer decodes", decoded, err)
		}
	}
}

func TestDocFindFieldsBlocksValidationAndCanonicalMetadata(testContext *testing.T) {
	eventHash := HashContent([]byte("source"))
	for _, span := range []*DocFindSpan{
		{From: -1, To: 2}, {From: 40, To: 39}, {From: 40, To: 40}, {From: 40, To: 41},
		{From: 40, To: 101}, {From: 101, To: 103}, {From: math.MaxInt, To: math.MinInt},
	} {
		fields := DocFindFields{Version: 1, EventHash: eventHash, EventBytes: 100, Blocks: span}
		if !errors.Is(fields.Validate(eventHash, 100), ErrIntegrity) {
			testContext.Fatal("invalid blocks span validated", span)
		}
		if raw, err := fields.Encode(); raw != nil || !errors.Is(err, ErrIntegrity) {
			testContext.Fatal("invalid blocks span encoded", raw, err)
		}
	}
	fields := DocFindFields{Version: 1, EventHash: eventHash, EventBytes: 100, Blocks: &DocFindSpan{From: 40, To: 60}}
	valid, err := fields.Encode()
	if err != nil {
		testContext.Fatal(err)
	}
	for _, projection := range []string{
		strings.Replace(string(valid), `"blocks":`, `"blocks":null,"blocks":`, 1),
		strings.Replace(string(valid), `"blocks":{`, `"blocks":{"unknown":"body",`, 1),
		strings.Replace(string(valid), `"summary":null`, `"summary":{"from":40,"to":60}`, 1),
	} {
		if decoded, err := DecodeDocFindFields(HashContent([]byte(projection)), eventHash, 100, []byte(projection)); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocFindFields{}) {
			testContext.Fatal("invalid blocks metadata decoded", projection, decoded, err)
		}
	}
	fields.Summary = &DocFindSpan{From: 10, To: 20}
	if encoded, err := fields.Encode(); encoded != nil || !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("both public field types encoded", string(encoded), err)
	}
	fields.Summary, fields.Blocks = nil, nil
	valid, err = fields.Encode()
	if err != nil {
		testContext.Fatal(err)
	}
	projection := append([]byte(`{"blocks":null,`), valid[1:]...)
	if _, err := DecodeDocFindFields(HashContent(projection), eventHash, 100, projection); !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("noncanonical explicit nil blocks accepted", err)
	}
}

func TestDocRangeJSONFieldPositionSharesLastValueSemantics(testContext *testing.T) {
	const value = `"duplicate"`
	const prefix = `{"unknown":{"redacted_summary":"duplicate"},"redacted_summary":"old","locked":[{"blob":"duplicate"}],"redacted\u005fsummary" : `
	raw := []byte(prefix + value + `,"other":"duplicate"}`)
	got, from, to := docRangeJSONFieldPosition(raw, "redacted_summary")
	if from != len(prefix) || to != len(prefix)+len(value) || string(got) != value || !bytes.Equal(got, docRangeJSONField(raw, "redacted_summary")) {
		testContext.Fatal("field accessors disagree or returned nested/earlier bytes", string(got), from, to)
	}
	if &got[0] != &raw[from] {
		testContext.Fatal("field locator copied its value")
	}
	for _, body := range [][]byte{nil, []byte(`[]`), []byte(`{}`), []byte(`{"nested":{"redacted_summary":"duplicate"}}`)} {
		got, from, to := docRangeJSONFieldPosition(body, "redacted_summary")
		if got != nil || from != 0 || to != 0 || docRangeJSONField(body, "redacted_summary") != nil {
			testContext.Fatal("missing field yielded a value", string(body), string(got), from, to)
		}
	}
}

func TestBuildDocFindFieldsRejectsInvalidSource(testContext *testing.T) {
	for _, body := range []string{
		"",
		`null`,
		`[]`,
		`[{"kind":"reasoning","redacted_summary":"nested"}]`,
		`{}`,
		`{"unknown":{"kind":"reasoning"},"redacted_summary":"nested kind"}`,
		`{"kind":"tool_call","redacted_summary":"not reasoning"}`,
		`{"kind":"Reasoning"}`,
		`{"kind":"reasoning","kind":"tool_result"}`,
		`{"kind":null}`,
		`{"kind":123}`,
		`{"kind":{}}`,
		`{"kind":"reasoning","redacted_summary":false}`,
		`{"kind":"reasoning","redacted_summary":[]}`,
		`{"kind":"reasoning","redacted_summary":{}}`,
		`{"kind":"reasoning","redacted_summary":1}`,
		`{"kind":"message","blocks":false}`,
		`{"kind":"message","blocks":{}}`,
		`{"kind":"message","blocks":"not an array"}`,
		`{"kind":"message","blocks":1}`,
		`{"kind":"message","blocks":[],"bl\u006fcks":false}`,
		`{"kind":"message","blocks":[{"text":"bad\q"}]}`,
		`{"kind":"message","blocks":[}`,
		`{"kind":"reasoning","redacted_summary":"bad\q"}`,
		`{"kind":"reasoning","locked":{"blob":"bad\u00x0"}}`,
		`{"kind":"reasoning","locked":{"blob":"truncated"}`,
		`{"kind":"reasoning",}`,
		`{"kind":"reasoning"} {}`,
		"\ufeff{\"kind\":\"reasoning\"}",
		"{\"kind\":\"reasoning\",\"locked\":\"invalid\xff\"}",
		"{\"kind\":\"reasoning\",\"redacted_summary\":\"invalid\xc0\xaf\"}",
		"{\"kind\":\"reasoning\",\"unknown\":\"surrogate\xed\xa0\x80\"}",
		"{\"kind\":\"reasoning\",\"unknown\":\"too high\xf4\x90\x80\x80\"}",
		"{\"kind\":\"message\",\"blocks\":[{\"text\":\"invalid\xff\"}]}",
		"{\"kind\":\"message\",\"blocks\":[],\"locked\":{\"blob\":\"invalid\xff\"}}",
	} {
		raw := []byte(body)
		fields, err := BuildDocFindFields(HashContent(raw), raw)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocFindFields{}) {
			testContext.Fatal("invalid source produced a projection", body, fields, err)
		}
	}
	raw := []byte(`{"kind":"reasoning","redacted_summary":"visible"}`)
	for _, eventHash := range []ContentHash{"", "sha256:invalid", HashContent([]byte("other source"))} {
		if fields, err := BuildDocFindFields(eventHash, raw); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocFindFields{}) {
			testContext.Fatal("incorrect source hash produced a projection", fields, err)
		}
	}
}

func TestDocFindFieldsValidateAndEncodeRejectInvalidMetadata(testContext *testing.T) {
	eventHash := HashContent([]byte("source"))
	valid := DocFindFields{Version: 1, EventHash: eventHash, EventBytes: 100, Summary: &DocFindSpan{From: 40, To: 60}}
	for _, mutate := range []func(*DocFindFields){
		func(fields *DocFindFields) { fields.Version = 0 },
		func(fields *DocFindFields) { fields.Version = 2 },
		func(fields *DocFindFields) { fields.EventHash = "" },
		func(fields *DocFindFields) { fields.EventHash = "sha256:invalid" },
		func(fields *DocFindFields) { fields.EventBytes = 0 },
		func(fields *DocFindFields) { fields.EventBytes = -1 },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: -1, To: 2} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 39} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 40} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 41} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 40, To: 101} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: 101, To: 103} },
		func(fields *DocFindFields) { fields.Summary = &DocFindSpan{From: math.MaxInt, To: math.MinInt} },
	} {
		fields := valid
		mutate(&fields)
		if !errors.Is(fields.Validate(fields.EventHash, fields.EventBytes), ErrIntegrity) {
			testContext.Fatal("invalid metadata validated", fields)
		}
		if raw, err := fields.Encode(); !errors.Is(err, ErrIntegrity) || raw != nil {
			testContext.Fatal("invalid metadata encoded", string(raw), err)
		}
	}
	for _, binding := range []struct {
		hash  ContentHash
		bytes int
	}{
		{hash: "", bytes: valid.EventBytes},
		{hash: "sha256:invalid", bytes: valid.EventBytes},
		{hash: HashContent([]byte("other source")), bytes: valid.EventBytes},
		{hash: eventHash, bytes: valid.EventBytes - 1},
		{hash: eventHash, bytes: valid.EventBytes + 1},
		{hash: eventHash, bytes: 0},
		{hash: eventHash, bytes: -1},
	} {
		if !errors.Is(valid.Validate(binding.hash, binding.bytes), ErrIntegrity) {
			testContext.Fatal("incorrect source binding validated", binding)
		}
	}
}

func TestDecodeDocFindFieldsRejectsNoncanonicalMetadata(testContext *testing.T) {
	source := []byte(`{"kind":"reasoning","redacted_summary":"visible"}`)
	fields, err := BuildDocFindFields(HashContent(source), source)
	if err != nil {
		testContext.Fatal(err)
	}
	canonical, err := fields.Encode()
	if err != nil {
		testContext.Fatal(err)
	}
	valid := string(canonical)
	for _, raw := range []string{
		"",
		`null`,
		`[]`,
		`{}`,
		valid + "\n",
		" " + valid,
		strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(valid, `"event_hash":`, `"event_hash":"ignored","event_hash":`, 1),
		strings.Replace(valid, `"summary":`, `"summary":null,"summary":`, 1),
		strings.Replace(valid, `"version":1`, `"version":1,"unknown":"source plaintext"`, 1),
		strings.Replace(valid, `"summary":{`, `"summary":{"unknown":"source plaintext",`, 1),
		strings.Replace(valid, `"from":`, `"from":0,"from":`, 1),
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"version":1`, `"version":null`, 1),
		strings.Replace(valid, `,"version":1`, ``, 1),
		strings.Replace(valid, `"summary":`, `"Summary":`, 1),
		strings.Replace(valid, `"summary":`, `"summ\u0061ry":`, 1),
		strings.Replace(valid, `"version":1`, `"version":1.0`, 1),
		strings.Replace(valid, `"version":1`, `"version":1e0`, 1),
		strings.Replace(valid, `"from":`, `"from":-`, 1),
		strings.Replace(valid, `"from":`, `"from":999999999999999999999999999999999`, 1),
		`{"version":1,` + valid[1:len(valid)-len(`,"version":1}`)] + `}`,
		valid[:len(valid)-1] + `,"body":"source plaintext"}`,
		valid + `{}`,
	} {
		projection := []byte(raw)
		decoded, err := DecodeDocFindFields(HashContent(projection), fields.EventHash, fields.EventBytes, projection)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocFindFields{}) {
			testContext.Fatal("invalid/noncanonical metadata decoded", raw, decoded, err)
		}
	}
	withoutSummary := fields
	withoutSummary.Summary = nil
	canonical, err = withoutSummary.Encode()
	if err != nil {
		testContext.Fatal(err)
	}
	projection := bytes.Replace(canonical, []byte(`,"summary":null`), nil, 1)
	if _, err := DecodeDocFindFields(HashContent(projection), fields.EventHash, fields.EventBytes, projection); !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("missing explicit summary accepted", err)
	}
	projection = bytes.Repeat([]byte(" "), MaxDocFindFieldsBytes+1)
	if _, err := DecodeDocFindFields(HashContent(projection), fields.EventHash, fields.EventBytes, projection); !errors.Is(err, ErrIntegrity) {
		testContext.Fatal("oversized metadata accepted", err)
	}
}

func TestDecodeDocFindFieldsChecksAllSourceBindings(testContext *testing.T) {
	source := []byte(`{"kind":"reasoning","redacted_summary":"visible"}`)
	fields, err := BuildDocFindFields(HashContent(source), source)
	if err != nil {
		testContext.Fatal(err)
	}
	raw, err := fields.Encode()
	if err != nil {
		testContext.Fatal(err)
	}
	projectionHash := HashContent(raw)
	for _, binding := range []struct {
		projection ContentHash
		event      ContentHash
		bytes      int
	}{
		{projection: "", event: fields.EventHash, bytes: fields.EventBytes},
		{projection: "sha256:invalid", event: fields.EventHash, bytes: fields.EventBytes},
		{projection: HashContent([]byte("other projection")), event: fields.EventHash, bytes: fields.EventBytes},
		{projection: projectionHash, event: HashContent([]byte("other event")), bytes: fields.EventBytes},
		{projection: projectionHash, event: "", bytes: fields.EventBytes},
		{projection: projectionHash, event: fields.EventHash, bytes: fields.EventBytes - 1},
		{projection: projectionHash, event: fields.EventHash, bytes: fields.EventBytes + 1},
		{projection: projectionHash, event: fields.EventHash, bytes: 0},
		{projection: projectionHash, event: fields.EventHash, bytes: -1},
	} {
		decoded, err := DecodeDocFindFields(binding.projection, binding.event, binding.bytes, raw)
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocFindFields{}) {
			testContext.Fatal("incorrect binding decoded", binding, decoded, err)
		}
	}
}

func TestDocFindFieldsLargeBodiesDoNotCopyOrEncodeSource(testContext *testing.T) {
	for _, test := range []struct {
		name    string
		locked  string
		summary string
	}{
		{name: "large locked body", locked: strings.Repeat("opaque encrypted blob ", 32<<10), summary: "visible summary plaintext"},
		{name: "large summary", locked: "opaque encrypted blob", summary: strings.Repeat(`\ufeff `, 64<<10) + "visible summary plaintext"},
	} {
		testContext.Run(test.name, func(testContext *testing.T) {
			prefix := `{"kind":"reasoning","locked":{"blob":"` + test.locked + `"},"redacted_summary":`
			value := `"` + test.summary + `"`
			raw := []byte(prefix + value + `}`)
			eventHash := HashContent(raw)
			fields, err := BuildDocFindFields(eventHash, raw)
			if err != nil || fields.Summary == nil || fields.Summary.From != len(prefix) || fields.Summary.To != len(prefix)+len(value) {
				testContext.Fatal("large source did not produce the exact summary span", fields, err)
			}
			encoded, err := fields.Encode()
			if err != nil || len(encoded) > MaxDocFindFieldsBytes || bytes.Contains(encoded, []byte("opaque encrypted blob")) || bytes.Contains(encoded, []byte("visible summary plaintext")) {
				testContext.Fatal("projection contained source text or exceeded its bound", string(encoded), err)
			}
			var metadata map[string]json.RawMessage
			if json.Unmarshal(encoded, &metadata) != nil || len(metadata) != 4 {
				testContext.Fatal("projection contained unexpected fields", string(encoded))
			}
			benchmark := testing.Benchmark(func(benchmark *testing.B) {
				benchmark.ReportAllocs()
				for range benchmark.N {
					if _, err := BuildDocFindFields(eventHash, raw); err != nil {
						benchmark.Fatal(err)
					}
				}
			})
			if allocated := benchmark.AllocedBytesPerOp(); allocated > 16<<10 {
				testContext.Fatalf("source-sized allocation: %d bytes per projection for %d source bytes", allocated, len(raw))
			}
			clear(raw)
			retained, err := fields.Encode()
			if err != nil || !bytes.Equal(retained, encoded) {
				testContext.Fatal("projection retained mutable source bytes", err)
			}
		})
	}
}
