package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestDocToolFieldDiscoveryLegacyBlockBytes(test *testing.T) {
	for _, scenario := range []struct {
		prefix string
		hash   ContentHash
	}{
		{`{"type":"te`, "sha256:84ba9fa6a7e4d35ea076a60ba17d042a63272bfaabcf0ce0b24f33852a9eea59"},
		{`{"type":"text","name":"sp`, "sha256:ef1387044db8c7ef5aea9f90ecfb18b501d17a6ce28b6e4065acc015b58af548"},
		{`{"type":"text","name":"speaker","text":"bo`, "sha256:d2db350813256a0426a6371800c08afb62fa775ad74b4b5ab8fa3938bd368dba"},
		{`{"type":"text","name":"speaker","text":"body"}`, "sha256:9c2af4ab275b418ffab309cacc043fbc6edfed84ca8dd7850a7677a9813625f5"},
	} {
		state := docFieldSchemaBlockPrefix(test, scenario.prefix)
		raw, err := canonicalJSON(state)
		if err != nil {
			test.Fatal(err)
		}
		if HashContent(raw) != scenario.hash {
			test.Fatal("legacy block bytes changed", scenario.prefix, HashContent(raw))
		}
	}
}

type docToolFieldDiscoveryFixture struct {
	source DocToolFieldDiscoverySource
	state  DocToolFieldDiscoveryState
	states []DocToolFieldDiscoveryState
}

func discoverDocToolFieldFixture(test testing.TB, raw []byte, size int) (docToolFieldDiscoveryFixture, error) {
	test.Helper()
	source, err := NewDocToolFieldDiscoverySource(HashContent(raw), len(raw))
	if err != nil {
		return docToolFieldDiscoveryFixture{}, err
	}
	encoded, err := source.Encode()
	if err != nil {
		return docToolFieldDiscoveryFixture{}, err
	}
	source, err = DecodeDocToolFieldDiscoverySource(HashContent(encoded), encoded)
	if err != nil {
		return docToolFieldDiscoveryFixture{}, err
	}
	state, err := NewDocToolFieldDiscoveryState(source)
	if err != nil {
		return docToolFieldDiscoveryFixture{}, err
	}
	fixture := docToolFieldDiscoveryFixture{source: source}
	for !state.Syntax.Done {
		fixture.states = append(fixture.states, state)
		before, err := state.Encode(source)
		if err != nil {
			return docToolFieldDiscoveryFixture{}, err
		}
		if fields, err := state.Fields(source); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocToolFields{}) {
			test.Fatal("unverified fields exposed", err)
		}
		step, err := AdvanceDocToolFieldDiscovery(context.Background(), source, state, raw[state.Syntax.Next:min(state.Syntax.Next+size, len(raw))])
		if err != nil {
			return docToolFieldDiscoveryFixture{}, err
		}
		after, err := state.Encode(source)
		if err != nil || !bytes.Equal(before, after) || step.From() != state.Syntax.Next || step.From() == 0 && step.Previous() != "" || step.From() > 0 && step.Previous() != HashContent(before) {
			test.Fatal("prior mutated or sealed predecessor lost", err)
		}
		hash, checkpoint := step.Checkpoint()
		if len(checkpoint) > 64<<10 || bytes.Contains(checkpoint, []byte("PRIVATE_BODY_MARKER")) || bytes.Contains(checkpoint, []byte(`"schema"`)) {
			test.Fatal("unbounded or body-bearing checkpoint", len(checkpoint))
		}
		decoded, err := step.State(source)
		if err != nil || decoded.Syntax.schema != docFieldSchemaTool {
			test.Fatal("sealed state did not rehydrate schema", err)
		}
		state, err = DecodeDocToolFieldDiscoveryState(source, hash, checkpoint)
		if err != nil || !reflect.DeepEqual(state, decoded) || state.Syntax.Next-step.From() != min(size, len(raw)-step.From()) {
			test.Fatal("checkpoint restart failed", err)
		}
		if !state.Syntax.Done && (!validDocFieldHashState(state.Syntax.HashState, state.Syntax.Next) || !bytes.Equal(state.Syntax.HashState[4+sha256.Size:len(state.Syntax.HashState)-8], make([]byte, sha256.BlockSize))) {
			test.Fatal("SHA checkpoint retained a private tail")
		}
		if _, err := DecodeDocFieldDiscoveryState(hash, checkpoint); !errors.Is(err, ErrIntegrity) {
			test.Fatal("legacy event decoder accepted tool wrapper")
		}
		checkpoint[0] ^= 1
		if _, err := step.State(source); err != nil {
			test.Fatal("checkpoint accessor exposed sealed bytes", err)
		}
	}
	fixture.state = state
	fixture.states = append(fixture.states, state)
	return fixture, nil
}

func requireDocToolFields(test testing.TB, raw []byte, size int) (docToolFieldDiscoveryFixture, DocToolFields) {
	test.Helper()
	fixture, err := discoverDocToolFieldFixture(test, raw, size)
	if err != nil {
		test.Fatal(err)
	}
	fields, err := fixture.state.Fields(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := fields.Encode(fixture.source)
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocToolFields(fixture.source, HashContent(encoded), encoded)
	if err != nil || !reflect.DeepEqual(fields, decoded) {
		test.Fatal("completed fields round trip", err)
	}
	return fixture, fields
}

func TestDocToolFieldDiscoveryRootSpansAndDuplicates(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		body     string
		kind     string
		output   string
		input    string
		provider string
		tool     string
	}{
		{"call", `{"kind":"tool_call","input":{"file_path":"/src/main.go","command":"go test","opaque":{"tool_name":"private"}},"provider_tool_name":"functions.read_file","tool_name":"read","output":1}`, `"tool_call"`, "1", `{"file_path":"/src/main.go","command":"go test","opaque":{"tool_name":"private"}}`, `"functions.read_file"`, `"read"`},
		{"result", `{"kind":"tool_result","output":{"ok":true,"value":[null,1,"x"]}}`, `"tool_result"`, `{"ok":true,"value":[null,1,"x"]}`, "", "", ""},
		{"escaped", "{\"k\\u0069nd\":\"tool_\\u0063all\",\"in\\u0070ut\":{\"a\":\"\\ud800\"},\"provider\\u005ftool_name\":\"\ud55c\uae00\\u002eread\",\"tool\\u005fname\":\"\\ud83d\\ude42\",\"out\\u0070ut\":\"\\u0031\"}", `"tool_\u0063all"`, `"\u0031"`, `{"a":"\ud800"}`, "\"\ud55c\uae00\\u002eread\"", `"\ud83d\ude42"`},
		{"last-values", `{"kind":false,"kind":"tool_call","input":[],"input":{"first":0},"input":null,"provider_tool_name":{},"provider_tool_name":"first","provider_tool_name":null,"provider_tool_name":"last","tool_name":true,"tool_name":"first","tool_name":"last-tool","output":"first","output":[]}`, `"tool_call"`, `[]`, "null", `"last"`, `"last-tool"`},
		{"last-null-names", `{"kind":"tool_call","provider_tool_name":"first","provider_tool_name":null,"tool_name":"first","tool_name":null,"output":1,"output":null}`, `"tool_call"`, "null", "", "", ""},
		{"opaque", `{"opaque":{"kind":"tool_call","input":{},"provider_tool_name":"private","tool_name":"private","output":"private"},"list":[{"output":"private"}],"provider_tool_nameX":"private","provider_tool_name_extra":"private","Provider_tool_name":"private","provider_tool_names":"private","tool_nameX":"private","redacted_summary":"private","blocks":[],"kind":"tool_call"}`, `"tool_call"`, "", "", "", ""},
		{"kind-result-last", `{"kind":"tool_call","input":false,"provider_tool_name":{},"tool_name":[],"kind":"tool_result","output":0}`, `"tool_result"`, "0", "", "", ""},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for padding := 0; padding < 64; padding++ {
				raw := []byte(strings.Repeat(" ", padding) + scenario.body + "\r\n ")
				fixture, fields := requireDocToolFields(test, raw, 64)
				assertDocFieldSchemaSpan(test, raw, &fields.KindToken, scenario.kind)
				for _, field := range []struct {
					span  *DocFindSpan
					token string
				}{{fields.Output, scenario.output}, {fields.Input, scenario.input}, {fields.ProviderToolName, scenario.provider}, {fields.ToolName, scenario.tool}} {
					assertDocFieldSchemaSpan(test, raw, field.span, field.token)
				}
				before, _ := fixture.state.Encode(fixture.source)
				if fields.Input != nil {
					fields.Input.From++
				}
				if fields.Output != nil {
					fields.Output.From++
				}
				if fields.ProviderToolName != nil {
					fields.ProviderToolName.From++
				}
				if fields.ToolName != nil {
					fields.ToolName.From++
				}
				after, _ := fixture.state.Encode(fixture.source)
				if !bytes.Equal(before, after) {
					test.Fatal("fields exposed mutable parser spans")
				}
			}
		})
	}
}

func TestDocToolFieldDiscoveryOutputClassificationAndNull(test *testing.T) {
	for _, scenario := range []struct {
		token string
		kind  DocToolValueType
	}{
		{"1", DocToolValueNumber}, {"0", DocToolValueNumber}, {"9", DocToolValueNumber}, {"-0", DocToolValueNumber}, {"-12.5e+99999", DocToolValueNumber},
		{`{"output":1}`, DocToolValueObject}, {`[{},null,1]`, DocToolValueArray}, {`"\ud800\ud83d\ude42"`, DocToolValueString}, {"null", DocToolValueNull}, {"true", DocToolValueTrue}, {"false", DocToolValueFalse},
	} {
		for _, kind := range []EventKind{EventToolCall, EventToolResult} {
			test.Run(string(kind)+"/"+scenario.token, func(test *testing.T) {
				raw := []byte(`{"kind":"` + string(kind) + `","output": [] ,"output": ` + scenario.token + ` }`)
				_, fields := requireDocToolFields(test, raw, 64)
				assertDocFieldSchemaSpan(test, raw, fields.Output, scenario.token)
				if fields.OutputType != scenario.kind {
					test.Fatal("last output type lost", fields.OutputType)
				}
			})
		}
	}
	_, missing := requireDocToolFields(test, []byte(`{"kind":"tool_call"}`), 64)
	_, null := requireDocToolFields(test, []byte(`{"kind":"tool_call","output":null,"input":null,"provider_tool_name":null,"tool_name":null}`), 64)
	if missing.Output != nil || missing.OutputType != 0 || missing.Input != nil || missing.InputType != 0 || null.Output == nil || null.OutputType != DocToolValueNull || null.Input == nil || null.InputType != DocToolValueNull || null.ProviderToolName != nil || null.ToolName != nil {
		test.Fatal("null/missing or name fallback semantics lost")
	}
	_, object := requireDocToolFields(test, []byte(`{"kind":"tool_call","input":null,"input":{}}`), 64)
	if object.Input == nil || object.InputType != DocToolValueObject {
		test.Fatal("last input classification lost")
	}
}

func TestDocToolFieldDiscoveryRelevantTypesAndFinalKind(test *testing.T) {
	for _, name := range []string{"input", "provider_tool_name", "tool_name"} {
		for _, token := range []string{"1", "true", "[]", `"text"`, "{}"} {
			invalid := name == "input" && token != "{}" || name != "input" && token != `"text"`
			for _, kind := range []EventKind{EventToolCall, EventToolResult} {
				raw := []byte(`{"` + name + `":` + token + `,"kind":"` + string(kind) + `"}`)
				fixture, err := discoverDocToolFieldFixture(test, raw, 64)
				if err != nil {
					test.Fatal("valid JSON rejected before final kind", err)
				}
				fields, err := fixture.state.Fields(fixture.source)
				if invalid && kind == EventToolCall {
					if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocToolFields{}) {
						test.Fatal("invalid call types exposed fields", name, token, err)
					}
				} else if err != nil {
					test.Fatal("irrelevant result field rejected", name, token, err)
				}
			}
		}
	}
	for _, body := range []string{`{}`, `{"kind":null}`, `{"kind":42}`, `{"kind":"message"}`, `{"kind":"tool_result","kind":"unknown"}`, `{"kind":"tool_result","input":false,"kind":"tool_call"}`} {
		fixture, err := discoverDocToolFieldFixture(test, []byte(body), 64)
		if err != nil {
			test.Fatal(err)
		}
		if fields, err := fixture.state.Fields(fixture.source); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocToolFields{}) {
			test.Fatal("invalid completed tool fields accepted", body, err)
		}
	}
}

func TestDocToolFieldDiscovery64KLexicalSplits(test *testing.T) {
	for _, scenario := range []struct {
		head  string
		token string
		tail  string
	}{
		{`{"`, "provider_tool_name", `":"visible","kind":"tool_call","input":{}}`},
		{`{"`, `provider\u005ftool_name`, `":"visible","kind":"tool_call"}`},
		{`{"provider_tool_name":"`, "\ud55c\uae00\\ud800🙂\\u002evisible", `","kind":"tool_call"}`},
		{`{"kind":"`, "tool_call", `","provider_tool_name":"visible"}`},
		{`{"kind":"`, `tool_\u0072esult`, `","output":1}`},
		{`{"kind":"tool_result","output":`, "1", `}`},
	} {
		for split := 0; split <= len(scenario.token); split++ {
			test.Run(fmt.Sprintf("%s/%d", scenario.head+scenario.token, split), func(test *testing.T) {
				raw := []byte(strings.Repeat(" ", DocFieldDiscoveryStepBytes-len(scenario.head)-split) + scenario.head + scenario.token + scenario.tail)
				fixture, fields := requireDocToolFields(test, raw, DocFieldDiscoveryStepBytes)
				if len(fixture.states) != 3 || fixture.states[1].Syntax.Next != 64<<10 {
					test.Fatal("did not restart at 64KiB")
				}
				if fields.Kind == EventToolCall {
					if fields.ProviderToolName == nil {
						test.Fatal("split provider field was not recognized")
					}
				} else if fields.Output == nil || fields.OutputType != DocToolValueNumber || fields.Output.To-fields.Output.From != 1 {
					test.Fatal("split number output lost")
				}
				if scenario.head == `{"` && scenario.token == "provider_tool_name" && split == len(scenario.token) && fixture.states[1].Syntax.StringRunes != 18 {
					test.Fatal("tool matcher still capped at seventeen")
				}
			})
		}
	}
}

func TestDocToolFieldDiscoveryLargeOpaqueDepthAndLateCompletion(test *testing.T) {
	raw := []byte(`{"opaque":` + strings.Repeat("[", 9999) + `"` + strings.Repeat("PRIVATE_BODY_MARKER", 100000) + `"` + strings.Repeat("]", 9999) + `,"` + strings.Repeat("provider_tool_name", 7000) + `":"private","nested":{"kind":"tool_result","output":"private","provider_tool_name":"private"},"kind":"tool_call","provider_tool_name":"public","tool_name":"fallback","input":{"nested":{"input":false,"provider_tool_name":false}},"output":[1]}`)
	fixture, fields := requireDocToolFields(test, raw, 64<<10)
	if fields.Kind != EventToolCall || fields.OutputType != DocToolValueArray || fields.InputType != DocToolValueObject {
		test.Fatal("large field discovery lost root types")
	}
	assertDocFieldSchemaSpan(test, raw, fields.ProviderToolName, `"public"`)
	sawDepth := false
	for _, state := range fixture.states {
		sawDepth = sawDepth || len(state.Syntax.Stack) == 10000
	}
	if !sawDepth {
		test.Fatal("depth 10000 checkpoint was not exercised")
	}
	tooDeep := []byte(`{"opaque":` + strings.Repeat("[", 10000) + `0` + strings.Repeat("]", 10000) + `,"kind":"tool_result"}`)
	if _, err := discoverDocToolFieldFixture(test, tooDeep, 64<<10); !errors.Is(err, ErrIntegrity) {
		test.Fatal("depth limit widened", err)
	}
	late := []byte(`{"kind":"tool_result","output":1}` + strings.Repeat(" ", 200))
	lateFixture, _ := requireDocToolFields(test, late, 64)
	if !lateFixture.states[1].Syntax.RootDone || lateFixture.states[1].Syntax.Done {
		test.Fatal("did not cover closed root before hash completion")
	}
}

func TestDocToolFieldDiscoveryCodecsAndRuntimeSchema(test *testing.T) {
	fixture, fields := requireDocToolFields(test, []byte(`{"kind":"tool_call","input":{},"output":1,"provider_tool_name":"provider","tool_name":"tool"}`), 64)
	source := fixture.source
	stateRaw, _ := fixture.state.Encode(source)
	sourceRaw, _ := source.Encode()
	fieldsRaw, _ := fields.Encode(source)
	for _, scenario := range []struct {
		name   string
		raw    []byte
		limit  int
		decode func(ContentHash, []byte) bool
	}{
		{"source", sourceRaw, MaxDocDisplaySourceBytes, func(hash ContentHash, raw []byte) bool {
			value, err := DecodeDocToolFieldDiscoverySource(hash, raw)
			return errors.Is(err, ErrIntegrity) && value == (DocToolFieldDiscoverySource{})
		}},
		{"state", stateRaw, MaxDocFieldDiscoveryStateBytes, func(hash ContentHash, raw []byte) bool {
			value, err := DecodeDocToolFieldDiscoveryState(source, hash, raw)
			return errors.Is(err, ErrIntegrity) && reflect.DeepEqual(value, DocToolFieldDiscoveryState{})
		}},
		{"fields", fieldsRaw, MaxDocFindFieldsBytes, func(hash ContentHash, raw []byte) bool {
			value, err := DecodeDocToolFields(source, hash, raw)
			return errors.Is(err, ErrIntegrity) && reflect.DeepEqual(value, DocToolFields{})
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for _, raw := range [][]byte{nil, append(bytes.Clone(scenario.raw), ' '), append([]byte(`{"unknown":0,`), scenario.raw[1:]...), append([]byte(`{"version":1,`), scenario.raw[1:]...), bytes.Replace(scenario.raw, []byte(`"version":1`), []byte(`"version":2`), 1), bytes.Repeat([]byte("x"), scenario.limit+1)} {
				if !scenario.decode(HashContent(raw), raw) {
					test.Fatal("noncanonical encoding accepted")
				}
			}
			if !scenario.decode(HashContent([]byte("another checkpoint")), scenario.raw) {
				test.Fatal("wrong expected hash accepted")
			}
		})
	}
	var plain DocToolFieldDiscoveryState
	if err := json.Unmarshal(stateRaw, &plain); err != nil || plain.Syntax.schema != docFieldSchemaEvent || plain.Validate(source) == nil {
		test.Fatal("wire syntax did not require source-wrapper rehydration", err)
	}
	restarted, err := DecodeDocToolFieldDiscoveryState(source, HashContent(stateRaw), stateRaw)
	if err != nil || restarted.Syntax.schema != docFieldSchemaTool {
		test.Fatal("wrapper did not restore tool schema", err)
	}
	other := source
	other.EventHash = HashContent([]byte("another event"))
	if _, err := DecodeDocToolFieldDiscoveryState(other, HashContent(stateRaw), stateRaw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("state accepted another source")
	}
	if _, err := DecodeDocToolFields(other, HashContent(fieldsRaw), fieldsRaw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("fields accepted another source")
	}
	if fields, err := fixture.state.Fields(other); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(fields, DocToolFields{}) {
		test.Fatal("source mismatch exposed fields")
	}
	if _, err := fixture.state.Syntax.Encode(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("tool syntax used event serializer")
	}
	if _, err := fixture.state.Syntax.DisplaySource(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("tool syntax exposed legacy display source")
	}
	if _, err := fixture.state.Syntax.FindFields(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("tool syntax exposed legacy fields")
	}
}

func TestDocToolFieldDiscoverySpanOverflowAndOverlap(test *testing.T) {
	fixture, fields := requireDocToolFields(test, []byte(`{"kind":"tool_call","output":1,"input":{},"provider_tool_name":"provider","tool_name":"tool"}`), 64)
	encoded, _ := fields.Encode(fixture.source)
	spans := func(value *DocToolFields) []*DocFindSpan {
		return []*DocFindSpan{&value.KindToken, value.Output, value.Input, value.ProviderToolName, value.ToolName}
	}
	for index := 0; index < 5; index++ {
		for _, span := range []DocFindSpan{{From: 1, To: math.MinInt}, {From: math.MaxInt, To: math.MinInt}, {From: -1, To: 2}, {From: 0, To: math.MaxInt}, {From: 2, To: 2}, {From: 3, To: 2}, {From: fixture.source.EventBytes, To: fixture.source.EventBytes}} {
			value, err := DecodeDocToolFields(fixture.source, HashContent(encoded), encoded)
			if err != nil {
				test.Fatal(err)
			}
			*spans(&value)[index] = span
			if value.Validate(fixture.source) == nil {
				test.Fatal("overflowing or empty root span accepted", index, span)
			}
			raw, _ := canonicalJSON(value)
			if decoded, err := DecodeDocToolFields(fixture.source, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(decoded, DocToolFields{}) {
				test.Fatal("invalid span survived codec")
			}
		}
		for other := index + 1; other < 5; other++ {
			value, _ := DecodeDocToolFields(fixture.source, HashContent(encoded), encoded)
			*spans(&value)[other] = *spans(&value)[index]
			if value.Validate(fixture.source) == nil {
				test.Fatal("overlapping distinct root values accepted", index, other)
			}
		}
	}
	for _, mutate := range []func(*DocToolFields){
		func(value *DocToolFields) { value.OutputType = 0 },
		func(value *DocToolFields) { value.OutputType = 255 },
		func(value *DocToolFields) { value.Output = nil },
		func(value *DocToolFields) { value.InputType = DocToolValueArray },
		func(value *DocToolFields) { value.InputType = 0 },
		func(value *DocToolFields) { value.Input = nil },
		func(value *DocToolFields) { value.Kind = EventToolResult },
	} {
		value, _ := DecodeDocToolFields(fixture.source, HashContent(encoded), encoded)
		mutate(&value)
		if value.Validate(fixture.source) == nil {
			test.Fatal("invalid completed type/span pair accepted")
		}
	}
}

type docToolFieldCancelContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *docToolFieldCancelContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func assertDocToolFieldAdvanceFailure(test testing.TB, ctx context.Context, source DocToolFieldDiscoverySource, state DocToolFieldDiscoveryState, raw []byte, want error) {
	test.Helper()
	before, err := state.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	step, err := AdvanceDocToolFieldDiscovery(ctx, source, state, raw)
	if !errors.Is(err, want) || !reflect.DeepEqual(step, DocToolFieldDiscoveryStep{}) {
		test.Fatal("failed advancement retained partial state", err, want)
	}
	after, err := state.Encode(source)
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("failed advancement mutated predecessor", err)
	}
}

func TestDocToolFieldDiscoveryMalformedHashBudgetAndCancellation(test *testing.T) {
	for _, body := range []string{
		`[]`, `null`, `{"kind":"tool_call",}`, `{"kind":"tool_result","output":[1,]}`, `{"kind":"tool_call","output":01}`, `{"kind":"tool_call","output":1.}`, `{"kind":"tool_result","output":-}`, `{"kind":"tool_result","output":1e+}`, `{"kind":"tool_result","output":tru}`, `{"kind":"tool_result","output":falsex}`, `{"kind":"tool_call","provider_tool_name":"\q"}`, `{"kind":"tool_call","provider_tool_name":"\u12"}`, `{"kind":"tool_result"} {}`, `{"kind":"tool_result"}null`, `{"kind":"tool_call","input":{"x":1}`, "{\"kind\":\"tool_call\",\"opaque\":\"invalid\xff\"}", "{\"kind\":\"tool_call\",\"opaque\":\"invalid\xed\xa0\x80\"}",
	} {
		raw := []byte(body)
		source, _ := NewDocToolFieldDiscoverySource(HashContent(raw), len(raw))
		state, _ := NewDocToolFieldDiscoveryState(source)
		assertDocToolFieldAdvanceFailure(test, context.Background(), source, state, raw, ErrIntegrity)
	}
	raw := []byte(`{"kind":"tool_call","input":{"nested":[{"provider_tool_name":"` + strings.Repeat("PRIVATE_BODY_MARKER", 5000) + `"}]},"provider_tool_name":"public"}`)
	source, _ := NewDocToolFieldDiscoverySource(HashContent(raw), len(raw))
	state, _ := NewDocToolFieldDiscoveryState(source)
	for _, size := range []int{0, 63, 65, DocFieldDiscoveryStepBytes + 1} {
		assertDocToolFieldAdvanceFailure(test, context.Background(), source, state, raw[:size], ErrIntegrity)
	}
	baseline := &docToolFieldCancelContext{Context: context.Background(), cancelAt: math.MaxInt}
	step, err := AdvanceDocToolFieldDiscovery(baseline, source, state, raw[:64<<10])
	if err != nil {
		test.Fatal(err)
	}
	for cancelAt := 1; cancelAt <= baseline.checks; cancelAt++ {
		ctx := &docToolFieldCancelContext{Context: context.Background(), cancelAt: cancelAt}
		assertDocToolFieldAdvanceFailure(test, ctx, source, state, raw[:64<<10], context.Canceled)
	}
	resumed, err := step.State(source)
	if err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*DocToolFieldDiscoveryStep){
		func(value *DocToolFieldDiscoveryStep) { value.source = HashContent([]byte("another source")) },
		func(value *DocToolFieldDiscoveryStep) { value.from = -1 },
		func(value *DocToolFieldDiscoveryStep) { value.from = math.MaxInt },
		func(value *DocToolFieldDiscoveryStep) {
			value.previous = HashContent([]byte("unexpected initial predecessor"))
		},
		func(value *DocToolFieldDiscoveryStep) { value.hash = HashContent([]byte("corrupt checkpoint")) },
	} {
		corrupt := step
		mutate(&corrupt)
		if value, err := corrupt.State(source); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(value, DocToolFieldDiscoveryState{}) {
			test.Fatal("unsealed step accepted")
		}
	}
	tail := bytes.Clone(raw[resumed.Syntax.Next:])
	tail[len(tail)-1] = ']'
	assertDocToolFieldAdvanceFailure(test, context.Background(), source, resumed, tail, ErrIntegrity)
	valid := []byte(`{"kind":"tool_result","output":1}`)
	wrong, _ := NewDocToolFieldDiscoverySource(HashContent([]byte("different event")), len(valid))
	wrongState, _ := NewDocToolFieldDiscoveryState(wrong)
	assertDocToolFieldAdvanceFailure(test, context.Background(), wrong, wrongState, valid, ErrIntegrity)
	complete, _ := requireDocToolFields(test, valid, 64)
	assertDocToolFieldAdvanceFailure(test, context.Background(), complete.source, complete.state, []byte(" "), ErrIntegrity)
	if _, err := NewDocToolFieldDiscoverySource("invalid", 1); !errors.Is(err, ErrIntegrity) {
		test.Fatal("invalid source accepted")
	}
}

func TestDocToolFieldDiscoveryRejectsCorruptStateAndLegacyToolFields(test *testing.T) {
	raw := []byte(`{"provider_tool_name":"` + strings.Repeat("p", 200) + `","kind":"tool_call","input":{},"tool_name":"tool","output":1}`)
	fixture, _ := requireDocToolFields(test, raw, 64)
	stateRaw, _ := fixture.states[1].Encode(fixture.source)
	for _, mutate := range []func(*DocToolFieldDiscoveryState){
		func(value *DocToolFieldDiscoveryState) { value.Source = HashContent([]byte("another source")) },
		func(value *DocToolFieldDiscoveryState) { value.Version++ },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.Next = math.MaxInt },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.Stack[0] = 255 },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.HashState[4+sha256.Size] = 'x' },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.HashState[0] ^= 1 },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.Done = true },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.StringRunes = 20 },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.ToolOutputType = DocToolValueNull },
		func(value *DocToolFieldDiscoveryState) { value.Syntax.ToolInputType = DocToolValueObject },
	} {
		state, err := DecodeDocToolFieldDiscoveryState(fixture.source, HashContent(stateRaw), stateRaw)
		if err != nil {
			test.Fatal(err)
		}
		mutate(&state)
		encoded, _ := canonicalJSON(state)
		if value, err := DecodeDocToolFieldDiscoveryState(fixture.source, HashContent(encoded), encoded); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(value, DocToolFieldDiscoveryState{}) {
			test.Fatal("corrupt state decoded")
		}
		step, err := AdvanceDocToolFieldDiscovery(context.Background(), fixture.source, state, raw[64:128])
		if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(step, DocToolFieldDiscoveryStep{}) {
			test.Fatal("corrupt state advanced")
		}
	}
	for _, mutate := range []func(*DocFieldDiscoveryState){
		func(value *DocFieldDiscoveryState) { value.KindToken = &DocFindSpan{From: 0, To: 2} },
		func(value *DocFieldDiscoveryState) { value.ToolName = &DocFindSpan{From: 0, To: 2} },
		func(value *DocFieldDiscoveryState) { value.InvalidToolName = true },
		func(value *DocFieldDiscoveryState) { value.ToolOutputType = DocToolValueNumber },
		func(value *DocFieldDiscoveryState) { value.ToolInputType = DocToolValueNull },
	} {
		event, _ := NewDocFieldDiscoveryState(HashContent([]byte(`{}`)), 2)
		mutate(&event)
		if event.Validate() == nil {
			test.Fatal("event accepted tool-only checkpoint fields")
		}
		block := docFieldSchemaBlockPrefix(test, `{"type":"text"}`)
		mutate(&block)
		if block.validateSyntax(docFieldSchemaBlock) == nil {
			test.Fatal("block accepted tool-only checkpoint fields")
		}
	}
	if docFieldSchemaEvent.maxStringRunes() != 17 || docFieldSchemaBlock.maxStringRunes() != 17 || docFieldSchemaTool.maxStringRunes() != 19 {
		test.Fatal("legacy matcher limits changed")
	}
}
