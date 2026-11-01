package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDocFieldSchemaEventCanonicalCompatibility(test *testing.T) {
	for _, scenario := range []struct {
		name string
		body string
		want []ContentHash
	}{
		{
			name: "message",
			body: `{"kind":"message","blocks":false,"blocks":[{"type":"text","text":"한글🙂"}],"output":"visible","redacted_summary":null}`,
			want: []ContentHash{
				"sha256:7ee2b42eb1bff7ce50db526f717070a76fe089b0094eea49f92c687acfb74369",
				"sha256:88b0b733eabf0936775341483a112f756c8679468d37b8fb3fe8398c3e158cc4",
				"sha256:23448ace97815a433f98dceb7eb68d3d916456f5f635b090cf035967a790d105",
			},
		},
		{
			name: "reasoning",
			body: `{"redacted_summary":false,"redacted\u005fsummary":"last","kind":"reasoning","opaque":{"kind":"message","blocks":[]},"blocks":null}`,
			want: []ContentHash{
				"sha256:c8a22cc8a9c2373057ecef78778cfffbd322cdc86806c2483098e173fcc0dc4a",
				"sha256:e7d45d9c554e54da2a8d177135cebcc74faa5ccc458e5d36152eec3dd7f16300",
				"sha256:2eca04ef8106b22fe04cd78ad5a9e73ded08be9180e0347778f5d0a0f9e9e6e2",
				"sha256:246b302c76f1c4c3d4fcceb0ef976ae011b42433c96ba6e5e248b2825099ea3f",
			},
		},
		{
			name: "tool_result",
			body: `{"kind":"message","out\u0070ut":"visible","output":{},"k\u0069nd":"tool_result","output":" \t\u202f\ufeff","opaque":[true,false,null,-1.25e+10]}`,
			want: []ContentHash{
				"sha256:4d1ca588d0abc0555c3e5daa62e1f8833354133df61c3136b6cdd2b1eeb36b22",
				"sha256:6d1ceaee0ad2f0664711cf6de0af8af56d74452104c9a74470134eb45570d8cf",
				"sha256:32e3f4258c95b68439e2a30c59058920b22894b2fadeb0f06ff2a8229147b0ae",
				"sha256:8ca25eddf89ae175f1114c7ee42b142f1daea1d7b0ae7355059d6296618f35d9",
			},
		},
		{
			name: "unknown_kind",
			body: `{"kind":"tool_result","kind":"unknown","output":null,"redacted_summary":42,"blocks":{},"opaque-key-longer-than-seventeen":"private"}`,
			want: []ContentHash{
				"sha256:1e97d18b89c93db132f276f65f498b27616a476ab9710db6ab7c7bb8797499c3",
				"sha256:8230acc886ec431959ce86af10bb44745fc043effbe4170b357b1cba087a2573",
				"sha256:95c974701f6819f37ad3a562d9c458be0f45657a28e66ba68c25c92198305d16",
				"sha256:7aa51923aa329732d75cd5445bd6dd1b2af732df4b3019f35b1b1b2cffc6a7d0",
			},
		},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			raw := []byte(scenario.body)
			state, err := NewDocFieldDiscoveryState(HashContent(raw), len(raw))
			if err != nil {
				test.Fatal(err)
			}
			var actual []ContentHash
			for {
				encoded, err := state.Encode()
				if err != nil {
					test.Fatal(err)
				}
				actual = append(actual, HashContent(encoded))
				if state.Done {
					break
				}
				step, err := AdvanceDocFieldDiscovery(context.Background(), state, raw[state.Next:min(state.Next+64, len(raw))])
				if err != nil {
					test.Fatal(err)
				}
				state, err = step.State()
				if err != nil {
					test.Fatal(err)
				}
			}
			if !reflect.DeepEqual(actual, scenario.want) {
				test.Fatalf("canonical event checkpoints changed: got %v want %v", actual, scenario.want)
			}
		})
	}
}

func advanceDocFieldSchemaBlock(state *DocFieldDiscoveryState, raw []byte) (int, error) {
	for index, character := range raw {
		if state.RootDone {
			return index, nil
		}
		for {
			consumed, err := state.accept(character)
			if err != nil {
				return index, err
			}
			if consumed {
				state.Next++
				break
			}
		}
		if err := state.validateSyntax(docFieldSchemaBlock); err != nil {
			return index + 1, err
		}
	}
	return len(raw), nil
}

func restartDocFieldSchemaBlock(test *testing.T, state DocFieldDiscoveryState) DocFieldDiscoveryState {
	test.Helper()
	raw, err := canonicalJSON(state)
	if err != nil || len(raw) > MaxDocFieldDiscoveryStateBytes {
		test.Fatal("block checkpoint is not bounded", err)
	}
	var restarted DocFieldDiscoveryState
	if err := json.Unmarshal(raw, &restarted); err != nil {
		test.Fatal(err)
	}
	if restarted.schema != docFieldSchemaEvent {
		test.Fatal("runtime schema was serialized")
	}
	restarted.schema = docFieldSchemaBlock
	if err := restarted.validateSyntax(docFieldSchemaBlock); err != nil {
		test.Fatal("block checkpoint lost lexical state", err)
	}
	reencoded, err := canonicalJSON(restarted)
	if err != nil || !bytes.Equal(raw, reencoded) {
		test.Fatal("block checkpoint changed after restart", err)
	}
	return restarted
}

func discoverDocFieldSchemaBlock(test *testing.T, body string, size int) (DocFieldDiscoveryState, []byte) {
	test.Helper()
	const prefix = `{"kind":"message","blocks":[`
	raw := []byte(prefix + body + `,{"type":"text","text":"unconsumed"}]}`)
	state := newDocBlockFieldParser(HashContent(raw), len(raw), len(prefix))
	if err := state.validateSyntax(docFieldSchemaBlock); err != nil {
		test.Fatal("new block parser is invalid", err)
	}
	for !state.RootDone {
		if state.Next >= len(raw) {
			test.Fatal("block root never finished")
		}
		if _, err := advanceDocFieldSchemaBlock(&state, raw[state.Next:min(state.Next+size, len(raw))]); err != nil {
			test.Fatalf("block failed at %d: %v", state.Next, err)
		}
		state = restartDocFieldSchemaBlock(test, state)
	}
	if state.Next != len(prefix)+len(body) || state.Done || state.Digest != "" || len(state.HashState) != 0 {
		test.Fatal("block parser consumed beyond its root or created event proof", state.Next)
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	if opening, err := decoder.Token(); err != nil || opening != json.Delim('{') {
		test.Fatal("invalid block oracle fixture", err)
	}
	want := map[string]*DocFindSpan{"type": nil, "name": nil, "text": nil}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			test.Fatal(err)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			test.Fatal(err)
		}
		member := key.(string)
		if _, known := want[member]; known {
			want[member] = nil
			if value[0] == '"' {
				end := len(prefix) + int(decoder.InputOffset())
				want[member] = &DocFindSpan{From: end - len(value), To: end}
			}
		}
	}
	actual := map[string]*DocFindSpan{"type": state.Blocks, "name": state.Output, "text": state.Summary}
	if !reflect.DeepEqual(actual, want) {
		test.Fatalf("block spans differ from root-only last-key oracle: got %+v want %+v", actual, want)
	}
	return state, raw
}

func assertDocFieldSchemaSpan(test *testing.T, raw []byte, span *DocFindSpan, token string) {
	test.Helper()
	if token == "" {
		if span != nil {
			test.Fatalf("unexpected span: %+v", span)
		}
		return
	}
	if span == nil || string(raw[span.From:span.To]) != token {
		test.Fatalf("span %+v does not select %s", span, token)
	}
}

func TestDocFieldSchemaBlockFragmentedFields(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		body      string
		kind      EventKind
		typeToken string
		nameToken string
		textToken string
		invalid   bool
	}{
		{
			name: "text",
			body: `{"type":"text","name":"speaker","text":"한글🙂"}`,
			kind: "text", typeToken: `"text"`, nameToken: `"speaker"`, textToken: `"한글🙂"`,
		},
		{
			name: "escaped_root_keys_and_kind",
			body: `{"t\u0079pe":"t\u0065xt","na\u006de":"speaker","te\u0078t":"\ud800\ud83d\ude42"}`,
			kind: "text", typeToken: `"t\u0065xt"`, nameToken: `"speaker"`, textToken: `"\ud800\ud83d\ude42"`,
		},
		{
			name: "last_duplicate_wins",
			body: `{"type":false,"name":{},"text":[],"type":"image","type":"text","name":"first","name":null,"name":"last","text":false,"text":"final"}`,
			kind: "text", typeToken: `"text"`, nameToken: `"last"`, textToken: `"final"`,
		},
		{
			name:    "last_duplicate_invalid",
			body:    `{"type":"text","type":42,"name":"first","name":true,"text":"first","text":{"text":"nested"}}`,
			invalid: true,
		},
		{
			name: "last_duplicate_null",
			body: `{"type":false,"type":"text","type":null,"name":{},"name":"first","name":null,"text":[],"text":"first","text":null}`,
		},
		{
			name: "identical_duplicates_ignore_later_nested_fields",
			body: `{"type":"text","name":"same","text":"same","type":"text","name":"same","text":"same","nested":{"type":"text","name":"same","text":"same"}}`,
			kind: "text", typeToken: `"text"`, nameToken: `"same"`, textToken: `"same"`,
		},
		{
			name:      "unknown_type_and_nested_opaque",
			body:      `{"type":"image","name":"asset","text":"caption","nested":{"type":"text","name":false,"text":null},"array":[{"type":true,"name":[],"text":{}}]}`,
			typeToken: `"image"`, nameToken: `"asset"`, textToken: `"caption"`,
		},
		{
			name:      "empty_type_and_name",
			body:      `{"type":"","name":"","text":""}`,
			typeToken: `""`, nameToken: `""`, textToken: `""`,
		},
		{
			name: "event_vocabulary_is_opaque",
			body: `{"kind":"tool_result","output":"ignored","redacted_summary":false,"blocks":[{"type":"text","name":false,"text":42}]}`,
		},
		{
			name:      "prefix_keys_and_kind_are_unknown",
			body:      `{"types":"text","names":false,"texts":false,"type":"textual","name":" \t\u202f\ufeff","text":null}`,
			typeToken: `"textual"`, nameToken: `" \t\u202f\ufeff"`,
		},
		{
			name: "opaque_lexical_states",
			body: `{"opaque":[null,true,false,-0,0.5,1e+99,-4.2E-9,1e99999,{"nested":["\"\\\/\b\f\n\r\t\u1234"]}],"type":"text"}`,
			kind: "text", typeToken: `"text"`,
		},
	} {
		for _, size := range []int{1, 2, 3, 7, 64, 1024} {
			test.Run(scenario.name+"/"+strconv.Itoa(size), func(test *testing.T) {
				state, raw := discoverDocFieldSchemaBlock(test, scenario.body, size)
				if state.Kind != scenario.kind || state.InvalidBlocks != scenario.invalid || state.OutputNonSpace != scenario.invalid || state.InvalidSummary != scenario.invalid {
					test.Fatalf("unexpected block fields: %+v", state)
				}
				assertDocFieldSchemaSpan(test, raw, state.Blocks, scenario.typeToken)
				assertDocFieldSchemaSpan(test, raw, state.Output, scenario.nameToken)
				assertDocFieldSchemaSpan(test, raw, state.Summary, scenario.textToken)
			})
		}
	}
}

func TestDocFieldSchemaBlockInvalidFlags(test *testing.T) {
	for _, field := range []string{"type", "name", "text"} {
		for _, value := range []string{`null`, `""`, `" \t\u202f\ufeff"`, `"visible"`, `true`, `false`, `0`, `-1.2e+3`, `[]`, `{}`} {
			test.Run(field+"/"+value, func(test *testing.T) {
				body := `{"type":"text","name":"speaker","text":"body","` + field + `":` + value + `}`
				state, raw := discoverDocFieldSchemaBlock(test, body, 1)
				invalid := value[0] != '"' && value != "null"
				if state.InvalidBlocks != (field == "type" && invalid) || state.OutputNonSpace != (field == "name" && invalid) || state.InvalidSummary != (field == "text" && invalid) {
					test.Fatalf("invalid value flags differ: %+v", state)
				}
				kind := EventKind("text")
				typeToken, nameToken, textToken := `"text"`, `"speaker"`, `"body"`
				token := ""
				if value[0] == '"' {
					token = value
				}
				switch field {
				case "type":
					kind, typeToken = "", token
				case "name":
					nameToken = token
				case "text":
					textToken = token
				}
				if state.Kind != kind {
					test.Fatal("invalid type retained a known kind", state.Kind)
				}
				assertDocFieldSchemaSpan(test, raw, state.Blocks, typeToken)
				assertDocFieldSchemaSpan(test, raw, state.Output, nameToken)
				assertDocFieldSchemaSpan(test, raw, state.Summary, textToken)
			})
		}
	}
	for _, kind := range []string{"message", "reasoning", "tool_result", "image", "TEXT"} {
		state, raw := discoverDocFieldSchemaBlock(test, `{"type":"`+kind+`"}`, 1)
		if state.Kind != "" || state.InvalidBlocks {
			test.Fatal("unknown block type became a known kind or invalid value", state.Kind)
		}
		assertDocFieldSchemaSpan(test, raw, state.Blocks, `"`+kind+`"`)
	}
}

func TestDocFieldSchemaBlockBoundedCheckpoint(test *testing.T) {
	body := `{"type":"text","name":"private-name","text":"private-text","` + strings.Repeat("private-key-", 1000) + `":"` + strings.Repeat("private-opaque-", 1000) + `","nested":{"type":false,"name":false,"text":false}}`
	state, _ := discoverDocFieldSchemaBlock(test, body, 64)
	raw, err := canonicalJSON(state)
	if err != nil || bytes.Contains(raw, []byte("private-")) || len(raw) > 1024 {
		test.Fatal("block checkpoint retained source text or grew with opaque data", len(raw), err)
	}
}

func docFieldSchemaBlockPrefix(test *testing.T, prefix string) DocFieldDiscoveryState {
	test.Helper()
	state := newDocBlockFieldParser(HashContent([]byte("verified parent event")), len(prefix)+128, 7)
	if _, err := advanceDocFieldSchemaBlock(&state, []byte(prefix)); err != nil {
		test.Fatal("invalid lexical fixture", err)
	}
	return state
}

func TestDocFieldSchemaBlockCheckpointRejectsMalformedSyntax(test *testing.T) {
	valid := docFieldSchemaBlockPrefix(test, `{"type":"te`)
	for _, scenario := range []struct {
		name   string
		mutate func(*DocFieldDiscoveryState)
	}{
		{"version", func(state *DocFieldDiscoveryState) { state.Version = 0 }},
		{"empty_event", func(state *DocFieldDiscoveryState) { state.EventBytes = 0 }},
		{"oversized_event", func(state *DocFieldDiscoveryState) { state.EventBytes = MaxConversationDocumentBytes + 1 }},
		{"negative_next", func(state *DocFieldDiscoveryState) { state.Next = -1 }},
		{"next_after_event", func(state *DocFieldDiscoveryState) { state.Next = state.EventBytes + 1 }},
		{"stack_frame", func(state *DocFieldDiscoveryState) { state.Stack = []byte{255} }},
		{"root_array", func(state *DocFieldDiscoveryState) { state.Stack = []byte{docFieldArrayValue} }},
		{"ancestor_frame", func(state *DocFieldDiscoveryState) { state.Stack = []byte{docFieldObjectAfter, docFieldObjectValue} }},
		{"depth", func(state *DocFieldDiscoveryState) {
			state.Stack = bytes.Repeat([]byte{docFieldObjectValue}, docFieldDiscoveryDepth+1)
		}},
		{"not_started", func(state *DocFieldDiscoveryState) { state.Started = false }},
		{"root_done", func(state *DocFieldDiscoveryState) { state.RootDone = true }},
		{"token", func(state *DocFieldDiscoveryState) { state.Token = docFieldTokenFalse + 1 }},
		{"number", func(state *DocFieldDiscoveryState) { state.Number = 9 }},
		{"residual_number", func(state *DocFieldDiscoveryState) { state.Number = 1 }},
		{"negative_literal", func(state *DocFieldDiscoveryState) { state.Literal = -1 }},
		{"residual_literal", func(state *DocFieldDiscoveryState) { state.Literal = 1 }},
		{"escape", func(state *DocFieldDiscoveryState) { state.Escape = 3 }},
		{"hex_digits", func(state *DocFieldDiscoveryState) { state.HexDigits = 4 }},
		{"hex_value", func(state *DocFieldDiscoveryState) { state.HexValue = 0x10000 }},
		{"hex_overflow", func(state *DocFieldDiscoveryState) { state.Escape, state.HexDigits, state.HexValue = 2, 1, 16 }},
		{"utf8_need", func(state *DocFieldDiscoveryState) { state.UTF8Need = 4 }},
		{"utf8_value", func(state *DocFieldDiscoveryState) { state.UTF8Value = 0x110000 }},
		{"residual_utf8", func(state *DocFieldDiscoveryState) { state.UTF8Value = 1 }},
		{"utf8_minimum", func(state *DocFieldDiscoveryState) { state.UTF8Need, state.UTF8Minimum = 1, 0x81 }},
		{"unreachable_utf8", func(state *DocFieldDiscoveryState) { state.UTF8Need, state.UTF8Minimum, state.UTF8Value = 3, 0x80, 2 }},
		{"surrogate_utf8", func(state *DocFieldDiscoveryState) {
			state.UTF8Need, state.UTF8Minimum, state.UTF8Value = 1, 0x800, 0x360
		}},
		{"purpose", func(state *DocFieldDiscoveryState) { state.StringPurpose = 3 }},
		{"key_purpose_for_kind", func(state *DocFieldDiscoveryState) { state.StringPurpose = 1 }},
		{"event_key_mask", func(state *DocFieldDiscoveryState) { state.StringMask = 8 }},
		{"event_kind_mask", func(state *DocFieldDiscoveryState) { state.StringMask = 2 }},
		{"negative_runes", func(state *DocFieldDiscoveryState) { state.StringRunes = -1 }},
		{"runes", func(state *DocFieldDiscoveryState) { state.StringRunes = 18 }},
		{"event_field", func(state *DocFieldDiscoveryState) { state.Field = docFieldBlocks }},
		{"negative_value_from", func(state *DocFieldDiscoveryState) { state.ValueFrom = -1 }},
		{"value_from_after_next", func(state *DocFieldDiscoveryState) { state.ValueFrom = state.Next + 1 }},
		{"event_kind", func(state *DocFieldDiscoveryState) { state.Kind = EventMessage }},
		{"unknown_kind", func(state *DocFieldDiscoveryState) { state.Kind = "image" }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			malformed := valid
			scenario.mutate(&malformed)
			raw, err := canonicalJSON(malformed)
			if err != nil {
				test.Fatal(err)
			}
			var restarted DocFieldDiscoveryState
			if err := json.Unmarshal(raw, &restarted); err != nil {
				test.Fatal(err)
			}
			restarted.schema = docFieldSchemaBlock
			if err := restarted.validateSyntax(docFieldSchemaBlock); !errors.Is(err, ErrIntegrity) {
				test.Fatal("malformed block checkpoint accepted", err)
			}
		})
	}
	for _, member := range []string{"type", "name", "text"} {
		for _, span := range []DocFindSpan{
			{From: -1, To: 2},
			{From: 3, To: 2},
			{From: 0, To: 1},
			{From: valid.Next + 1, To: valid.Next + 3},
			{From: 0, To: valid.Next + 1},
			{From: 0, To: valid.EventBytes + 1},
			{From: math.MaxInt, To: -2},
		} {
			malformed := valid
			switch member {
			case "type":
				malformed.Blocks = &span
			case "name":
				malformed.Output = &span
			case "text":
				malformed.Summary = &span
			}
			if err := malformed.validateSyntax(docFieldSchemaBlock); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("out-of-bounds %s span accepted: %+v: %v", member, span, err)
			}
		}
	}
}

func TestDocFieldSchemaBlockCheckpointStringPurposes(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		prefix  string
		purpose byte
		mask    byte
	}{
		{"root_key", `{"`, 1, 7},
		{"root_kind", `{"type":"`, 2, 1},
		{"root_name", `{"name":"`, 0, 0},
		{"root_text", `{"text":"`, 0, 0},
		{"nested_key", `{"opaque":{"`, 0, 0},
		{"nested_kind", `{"opaque":{"type":"`, 0, 0},
		{"array_kind", `{"opaque":[{"type":"`, 0, 0},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			state := docFieldSchemaBlockPrefix(test, scenario.prefix)
			state = restartDocFieldSchemaBlock(test, state)
			if state.StringPurpose != scenario.purpose || state.StringMask != scenario.mask {
				test.Fatal("incorrect schema-specific string purpose or mask", state.StringPurpose, state.StringMask)
			}
			state.StringPurpose = (scenario.purpose + 1) % 3
			if err := state.validateSyntax(docFieldSchemaBlock); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid string purpose accepted", err)
			}
		})
	}
}

func TestDocFieldSchemaBlockRejectsMalformedGrammar(test *testing.T) {
	for _, body := range []string{
		`[]`, `null`, `{"type":"text",}`, `{"opaque":[1,]}`,
		`{"name":01}`, `{"text":1.}`, `{"text":1e+}`, `{"type":tru}`,
		`{"name":"\q"}`, `{"text":"\u12"}`, `{"opaque":[{]}}`,
		"{\"text\":\"invalid\xff\"}",
		"{\"opaque\":\"invalid\xed\xa0\x80\"}",
		"{\"opaque\":\"invalid\xf4\x90\x80\x80\"}",
	} {
		state := newDocBlockFieldParser(HashContent([]byte(body)), len(body), 0)
		if _, err := advanceDocFieldSchemaBlock(&state, []byte(body)); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("invalid block grammar accepted: %q: %v", body, err)
		}
	}
}

func TestDocFieldSchemaSyntaxAndEventProofStaySeparate(test *testing.T) {
	state := docFieldSchemaBlockPrefix(test, `{"type":"text","name":"speaker","text":"body"}`)
	state.EventBytes = state.Next
	state = restartDocFieldSchemaBlock(test, state)
	if state.Done || !state.RootDone || state.validateSyntax(docFieldSchemaBlock) != nil {
		test.Fatal("block grammar required a completed event proof")
	}
	state.EventHash, state.HashState, state.Digest, state.Done = "parent-verifies-hash", []byte("parent-verifies-sha"), "parent-verifies-digest", true
	if err := state.validateSyntax(docFieldSchemaBlock); err != nil {
		test.Fatal("lexical validator inspected the event proof", err)
	}
	for _, schema := range []docFieldSchema{docFieldSchemaEvent, docFieldSchema(2)} {
		if err := state.validateSyntax(schema); !errors.Is(err, ErrIntegrity) {
			test.Fatal("wrong runtime schema accepted", schema, err)
		}
	}
	raw := []byte(`{}`)
	event, err := NewDocFieldDiscoveryState(HashContent(raw), len(raw))
	if err != nil || event.schema != docFieldSchemaEvent || event.Validate() != nil {
		test.Fatal("normal constructor no longer defaults to event grammar", err)
	}
	event.schema = docFieldSchemaBlock
	if !errors.Is(event.Validate(), ErrIntegrity) {
		test.Fatal("normal validation accepted a block runtime schema")
	}
	if _, err := event.Encode(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("normal checkpoint accepted a block runtime schema", err)
	}
	if _, err := AdvanceDocFieldDiscovery(context.Background(), event, raw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("normal advancement accepted a block runtime schema", err)
	}
	if _, err := state.DisplaySource(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("block grammar exposed normal display evidence", err)
	}
	if _, err := state.FindFields(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("block grammar exposed normal field evidence", err)
	}
}
