package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

type docToolTreeFixture struct {
	raw       []byte
	discovery docToolFieldDiscoveryFixture
	source    DocToolTreeSource
	objects   map[ContentHash][]byte
	states    []DocToolTreeScanState
	steps     []DocToolTreeScanStep
	nodes     []DocToolTreeNode
	readBytes int
	readCalls int
}

func newDocToolTreeFixture(test testing.TB, body string) *docToolTreeFixture {
	test.Helper()
	raw := []byte(body)
	discovery, fields := requireDocToolFields(test, raw, DocFieldDiscoveryStepBytes)
	source, err := NewDocToolTreeSource(discovery.source, discovery.state, DocRangeEntry{DocEventIndex: DocEventIndex{Hash: HashContent(raw), Length: len(raw)}, Kind: fields.Kind})
	if err != nil {
		test.Fatal(err)
	}
	evidence, err := fields.Encode(discovery.source)
	if err != nil {
		test.Fatal(err)
	}
	encoded, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	decoded, err := DecodeDocToolTreeSource(HashContent(encoded), encoded)
	if err != nil || decoded != source || source.Evidence != HashContent(evidence) {
		test.Fatal("source round trip or evidence binding", err)
	}
	return &docToolTreeFixture{raw: raw, discovery: discovery, source: source, objects: map[ContentHash][]byte{source.Evidence: evidence}}
}

func (fixture *docToolTreeFixture) load(_ context.Context, hash ContentHash) ([]byte, error) {
	raw, ok := fixture.objects[hash]
	if !ok {
		return nil, ErrIntegrity
	}
	return bytes.Clone(raw), nil
}

func (fixture *docToolTreeFixture) read(_ context.Context, from, length int) ([]byte, error) {
	if from < fixture.source.Root.From || length <= 0 || from+length > fixture.source.Root.To {
		return nil, ErrIntegrity
	}
	return bytes.Clone(fixture.raw[from : from+length]), nil
}

func (fixture *docToolTreeFixture) scan(test testing.TB) error {
	test.Helper()
	state, err := NewDocToolTreeScanState(fixture.source)
	if err != nil {
		return err
	}
	for !state.Done() {
		if len(fixture.steps) > len(fixture.raw)+1 {
			test.Fatal("scanner made no progress")
		}
		fixture.states = append(fixture.states, state)
		before, err := state.Encode(fixture.source)
		if err != nil {
			return err
		}
		readBytes := 0
		step, err := StepDocToolTreeScan(context.Background(), fixture.source, state, func(ctx context.Context, from, length int) ([]byte, error) {
			if from < state.Next {
				test.Fatal("rescanned an ancestor", from, state.Next)
			}
			readBytes += length
			fixture.readBytes += length
			fixture.readCalls++
			return fixture.read(ctx, from, length)
		}, fixture.load)
		if err != nil {
			assertDocToolTreeEmptyStep(test, step)
			return err
		}
		after, err := state.Encode(fixture.source)
		if err != nil || !bytes.Equal(before, after) || step.Prior() != HashContent(before) || step.Source() != fixture.source {
			test.Fatal("predecessor mutated or source proof lost", err)
		}
		if len(step.Nodes()) > 16 || len(step.Objects()) > 16 || len(step.Objects()) > DocToolTreeStepObjects || readBytes != step.ReadBytes() || readBytes > 64<<10 {
			test.Fatal("step budget exceeded", len(step.Nodes()), len(step.Objects()), readBytes)
		}
		objectBytes := 0
		for _, object := range step.Objects() {
			objectBytes += len(object.Bytes)
			if HashContent(object.Bytes) != object.Hash || bytes.Contains(object.Bytes, []byte("PRIVATE_BODY_MARKER")) {
				test.Fatal("invalid or body-bearing frame")
			}
			fixture.objects[object.Hash] = bytes.Clone(object.Bytes)
		}
		if objectBytes > 128<<10 {
			test.Fatal("object byte budget exceeded")
		}
		for _, node := range step.Nodes() {
			encoded, err := node.Encode(fixture.source)
			if err != nil {
				test.Fatal("invalid emitted node", node, err)
			}
			decoded, err := DecodeDocToolTreeNode(fixture.source, HashContent(encoded), encoded)
			if err != nil || !reflect.DeepEqual(node, decoded) {
				test.Fatal("node round trip", err)
			}
		}
		fixture.nodes = append(fixture.nodes, step.Nodes()...)
		hash, checkpoint := step.Checkpoint()
		if len(checkpoint) > 64<<10 || bytes.Contains(checkpoint, []byte("PRIVATE_BODY_MARKER")) {
			test.Fatal("unbounded or body-bearing checkpoint")
		}
		state, err = DecodeDocToolTreeScanState(fixture.source, hash, checkpoint)
		if err != nil || !reflect.DeepEqual(state, step.State()) {
			test.Fatal("serialized restart", err)
		}
		if state.Next < fixture.states[len(fixture.states)-1].Next || state.NodeCount != len(fixture.nodes) || ValidateDocToolTreeCheckpoint(context.Background(), fixture.source, state, fixture.load) != nil {
			test.Fatal("invalid checkpoint progress", state.Next, state.NodeCount)
		}
		fixture.steps = append(fixture.steps, step)
	}
	fixture.states = append(fixture.states, state)
	finished, err := FinishDocToolTreeScan(context.Background(), fixture.source, state, fixture.load)
	if err != nil || !reflect.DeepEqual(finished, state) || !finished.Done() || finished.NodeCount != len(fixture.nodes) {
		test.Fatal("finish", err)
	}
	assertDocToolTreeTopology(test, fixture)
	return nil
}

func assertDocToolTreeEmptyStep(test testing.TB, step DocToolTreeScanStep) {
	test.Helper()
	hash, raw := step.Checkpoint()
	if step.Prior() != "" || step.Source() != (DocToolTreeSource{}) || step.ReadBytes() != 0 || hash != "" || raw != nil || step.Nodes() != nil || step.Objects() != nil || !reflect.DeepEqual(step.State(), DocToolTreeScanState{}) {
		test.Fatal("failure leaked partial output")
	}
}

func assertDocToolTreeTopology(test testing.TB, fixture *docToolTreeFixture) {
	test.Helper()
	parents := make(map[int]DocToolTreeNode)
	children := make(map[int][]DocToolTreeNode)
	seen := make(map[int]bool)
	lastEnd := fixture.source.Root.From
	for index, node := range fixture.nodes {
		if seen[node.Value.From] || !json.Valid(fixture.raw[node.Value.From:node.Value.To]) || node.Value.To < lastEnd {
			test.Fatal("not unique valid postorder nodes", index, node)
		}
		seen[node.Value.From] = true
		lastEnd = node.Value.To
		parents[node.Value.From] = node
		if node.Parent != -1 {
			children[node.Parent] = append(children[node.Parent], node)
		}
	}
	for _, node := range fixture.nodes {
		if len(children[node.Value.From]) != node.Children {
			test.Fatal("lost direct children", node)
		}
		for ordinal, child := range children[node.Value.From] {
			if child.Ordinal != ordinal || child.Depth != node.Depth+1 || child.Value.From <= node.Value.From || child.Value.To >= node.Value.To || (child.Key != nil) != (node.Type == DocToolValueObject) {
				test.Fatal("invalid parent link or ordinal", node, child)
			}
		}
		if node.Parent >= 0 {
			if _, exists := parents[node.Parent]; !exists {
				test.Fatal("missing parent")
			}
		}
	}
	root := fixture.nodes[len(fixture.nodes)-1]
	if root.Value != fixture.source.Root || root.Parent != -1 || root.Key != nil || root.Depth != 0 || root.Ordinal != 0 {
		test.Fatal("invalid root", root)
	}
}

func TestDocToolTreeAllJSONTypes(test *testing.T) {
	for _, value := range []string{`{}`, `[]`, `""`, "\"\ud55c\uae00\\ud83d\\ude42\\ud800\\n\"", `0`, `-0`, `123.4e-20`, `1e99999`, `null`, `true`, `false`, `[{},[],"text",0,null,true,false]`} {
		test.Run(value, func(test *testing.T) {
			fixture := newDocToolTreeFixture(test, ` {"kind":"tool_result","output":`+value+`} `)
			if err := fixture.scan(test); err != nil {
				test.Fatal(err)
			}
			root := fixture.nodes[len(fixture.nodes)-1]
			if string(fixture.raw[root.Value.From:root.Value.To]) != value || root.Type != docToolTreeType(value[0]) {
				test.Fatal("incorrect root", root)
			}
		})
	}
	for _, value := range []string{`{}`, `null`, `{"file_path":"PRIVATE_BODY_MARKER","nested":{"input":null}}`} {
		fixture := newDocToolTreeFixture(test, `{"kind":"tool_call","output":"not the root","input":`+value+`}`)
		if err := fixture.scan(test); err != nil {
			test.Fatal(err)
		}
	}
}

func TestDocToolTreeNestedDuplicateOpaqueValues(test *testing.T) {
	value := `{"kind":"opaque","text":["x",{"type":null,"name":true,"text":false}],"dup":1,"dup":{"n":-0.02e+3},"0":[]}`
	fixture := newDocToolTreeFixture(test, `{"output":"old","opaque":{"output":false},"kind":"tool_result","output":`+value+`}`)
	if err := fixture.scan(test); err != nil {
		test.Fatal(err)
	}
	want := []string{`"opaque"`, `"x"`, `null`, `true`, `false`, `{"type":null,"name":true,"text":false}`, `["x",{"type":null,"name":true,"text":false}]`, `1`, `-0.02e+3`, `{"n":-0.02e+3}`, `[]`, value}
	if len(fixture.nodes) != len(want) {
		test.Fatal("lost opaque or duplicate candidates", len(fixture.nodes))
	}
	var keys []string
	for index, node := range fixture.nodes {
		if string(fixture.raw[node.Value.From:node.Value.To]) != want[index] {
			test.Fatal("incorrect postorder span", index, node)
		}
		if node.Parent == fixture.source.Root.From {
			keys = append(keys, string(fixture.raw[node.Key.From:node.Key.To]))
		}
	}
	if !reflect.DeepEqual(keys, []string{`"kind"`, `"text"`, `"dup"`, `"dup"`, `"0"`}) {
		test.Fatal("key order or duplicate changed", keys)
	}
	for _, value := range []string{`{"type":true,"name":{},"text":[]}`, `{"a":0,"\u0061":1,"a":2}`} {
		fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+value+`}`)
		if err := fixture.scan(test); err != nil || len(fixture.nodes) != 4 {
			test.Fatal("schema fields selected or escaped duplicates lost", err)
		}
	}
}

func TestDocToolTreeSourceBindingAndCanonicalCodecs(test *testing.T) {
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_call","output":[1,2],"input":{"a":0}}`)
	entry := DocRangeEntry{DocEventIndex: DocEventIndex{Hash: fixture.source.EventHash, Length: fixture.source.EventBytes}, Kind: EventToolCall}
	for _, changed := range []DocRangeEntry{
		{DocEventIndex: DocEventIndex{Hash: HashContent([]byte("different")), Length: entry.Length}, Kind: entry.Kind},
		{DocEventIndex: DocEventIndex{Hash: entry.Hash, Length: entry.Length + 1}, Kind: entry.Kind},
		{DocEventIndex: entry.DocEventIndex, Kind: EventToolResult},
	} {
		if _, err := NewDocToolTreeSource(fixture.discovery.source, fixture.discovery.state, changed); !errors.Is(err, ErrIntegrity) {
			test.Fatal("entry mismatch accepted", err)
		}
	}
	if _, err := NewDocToolTreeSource(fixture.discovery.source, fixture.discovery.states[0], entry); !errors.Is(err, ErrIntegrity) {
		test.Fatal("unfinished discovery accepted", err)
	}
	for _, body := range []string{`{"kind":"tool_call","output":{}}`, `{"kind":"tool_result","input":{}}`} {
		raw := []byte(body)
		discovery, fields := requireDocToolFields(test, raw, 64)
		if _, err := NewDocToolTreeSource(discovery.source, discovery.state, DocRangeEntry{DocEventIndex: DocEventIndex{Hash: HashContent(raw), Length: len(raw)}, Kind: fields.Kind}); !errors.Is(err, ErrDocFindDisplayUnavailable) {
			test.Fatal("missing field did not remain unavailable", err)
		}
	}
	for _, mutate := range []func(*DocToolTreeSource){
		func(source *DocToolTreeSource) { source.Profile = DocToolFieldDiscoveryProfile },
		func(source *DocToolTreeSource) { source.EventHash = HashContent([]byte("wrong")) },
		func(source *DocToolTreeSource) { source.EventBytes++ },
		func(source *DocToolTreeSource) { source.Evidence = HashContent([]byte("wrong")) },
		func(source *DocToolTreeSource) { source.Root.From++ },
		func(source *DocToolTreeSource) { source.Type = DocToolValueArray },
	} {
		source := fixture.source
		mutate(&source)
		state, err := NewDocToolTreeScanState(source)
		if err == nil {
			if err := ValidateDocToolTreeCheckpoint(context.Background(), source, state, fixture.load); !errors.Is(err, ErrIntegrity) {
				test.Fatal("unbound source accepted", err)
			}
		}
	}
	if err := fixture.scan(test); err != nil {
		test.Fatal(err)
	}
	state := fixture.states[len(fixture.states)-1]
	stateRaw, _ := state.Encode(fixture.source)
	nodeRaw, _ := fixture.nodes[0].Encode(fixture.source)
	sourceRaw, _ := fixture.source.Encode()
	for _, noncanonical := range []func([]byte) []byte{
		func(raw []byte) []byte { return append([]byte(" "), raw...) },
		func(raw []byte) []byte {
			return append(append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"unexpected":1`)...), '}')
		},
	} {
		raw := noncanonical(stateRaw)
		if _, err := DecodeDocToolTreeScanState(fixture.source, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical state", err)
		}
		raw = noncanonical(nodeRaw)
		if _, err := DecodeDocToolTreeNode(fixture.source, HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical node", err)
		}
		raw = noncanonical(sourceRaw)
		if _, err := DecodeDocToolTreeSource(HashContent(raw), raw); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical source", err)
		}
	}
	if _, err := DecodeDocToolTreeScanState(fixture.source, HashContent([]byte("wrong")), stateRaw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("wrong state hash", err)
	}
	if _, err := DecodeDocToolTreeNode(fixture.source, HashContent([]byte("wrong")), nodeRaw); !errors.Is(err, ErrIntegrity) {
		test.Fatal("wrong node hash", err)
	}
}

func TestDocToolTreeDepthAndClosureBudgets(test *testing.T) {
	for _, depth := range []int{16, 32, 512, 513} {
		test.Run(fmt.Sprint(depth), func(test *testing.T) {
			fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+strings.Repeat("[", depth)+"0"+strings.Repeat("]", depth)+`}`)
			err := fixture.scan(test)
			if depth == 513 {
				if !errors.Is(err, ErrDocFindDisplayUnavailable) {
					test.Fatal("depth ceiling", err)
				}
				return
			}
			if err != nil || len(fixture.nodes) != depth+1 {
				test.Fatal("deep scan", err, len(fixture.nodes))
			}
		})
	}
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":[`+strings.Repeat(`0,`, 1000)+`0]}`)
	if err := fixture.scan(test); err != nil || len(fixture.nodes) != 1002 {
		test.Fatal("wide scan", err)
	}
}

func TestDocToolTreeSourceCostAndMetadataRetention(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		value string
	}{
		{"tiny-array", `[` + strings.Repeat(`0,`, 16384) + `0]`},
		{"tiny-object", `{"":0` + strings.Repeat(`,"":0`, 8192) + `}`},
		{"mixed-object", `{"dup":0` + strings.Repeat(`,"du\u0070":[null,{"a":-1e3},false],"dup":"x"`, 1024) + `}`},
		{"deep-tiny-array", strings.Repeat(`[`, 512) + strings.Repeat(`0,`, 4096) + `0` + strings.Repeat(`]`, 512)},
		{"long-string", `"` + strings.Repeat(`PRIVATE_BODY_MARKER`, 1<<17) + `"`},
		{"long-key", `{"` + strings.Repeat(`PRIVATE_BODY_MARKER`, 1<<17) + `":0}`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+scenario.value+`}`)
			if err := fixture.scan(test); err != nil {
				test.Fatal(err)
			}
			logical := fixture.source.Root.To - fixture.source.Root.From
			frameBytes := 0
			for hash, raw := range fixture.objects {
				if hash != fixture.source.Evidence {
					frameBytes += len(raw)
				}
			}
			test.Logf("logical=%d physical=%d amplification=%.3f range_reads=%d nodes=%d frames=%d frame_bytes=%d steps=%d", logical, fixture.readBytes, float64(fixture.readBytes)/float64(logical), fixture.readCalls, len(fixture.nodes), len(fixture.objects)-1, frameBytes, len(fixture.steps))
			if fixture.readBytes > 2*logical+128 {
				test.Fatal("excessive lookahead amplification", fixture.readBytes, logical)
			}
			containers := 0
			for _, node := range fixture.nodes {
				if node.Depth > 0 && (node.Type == DocToolValueObject || node.Type == DocToolValueArray) {
					containers++
				}
			}
			if len(fixture.objects)-1 != containers {
				test.Fatal("metadata retention is not one immutable frame per nested container")
			}
			if strings.HasPrefix(scenario.name, "long-") && fixture.readCalls > logical/4096+32 {
				test.Fatal("long lexeme did not reach full-size refills", fixture.readCalls, logical)
			}
		})
	}
}

func TestDocToolTreeLongLexicalBoundaries(test *testing.T) {
	for _, suffix := range []string{`\u0061`, `\ud83d\ude42`, `\"`, `\\`, "\ud55c", "🙂"} {
		for _, object := range []bool{false, true} {
			prefix := `"` + strings.Repeat("x", DocToolTreeStepSourceBytes-2)
			value := prefix + suffix + `PRIVATE_BODY_MARKER"`
			if object {
				value = `{"` + strings.Repeat("k", DocToolTreeStepSourceBytes-3) + suffix + `PRIVATE_BODY_MARKER":` + value + `}`
			}
			fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+value+`}`)
			if err := fixture.scan(test); err != nil {
				test.Fatal("lexical boundary", suffix, object, err)
			}
		}
	}
	for _, length := range []int{DocToolTreeStepSourceBytes, DocToolTreeStepSourceBytes + 1} {
		fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+strings.Repeat("9", length)+`}`)
		if err := fixture.scan(test); err != nil || len(fixture.nodes) != 1 || fixture.nodes[0].Type != DocToolValueNumber {
			test.Fatal("root number EOF", err)
		}
	}
}

func TestDocToolTreeCheckpointTokenBoundaries(test *testing.T) {
	for _, token := range []string{`null`, `true`, `false`, `-12.5e+23`, `"\u0061"`, "\"\ud55c🙂\"", `"\ud83d\ude42"`} {
		for width := 1; width < len(token); width++ {
			value := `{"k":` + strings.Repeat(" ", DocToolTreeStepSourceBytes-5-width) + token + `}`
			fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+value+`}`)
			if err := fixture.scan(test); err != nil {
				test.Fatal("checkpoint inside token", token, width, err)
			}
			if len(fixture.steps) != 2 || fixture.states[1].Next != fixture.source.Root.From+DocToolTreeStepSourceBytes || fixture.states[1].Syntax.Token == 0 {
				test.Fatal("did not suspend inside token", token, width)
			}
		}
	}
	for _, padding := range []int{DocToolTreeStepSourceBytes - 3, DocToolTreeStepSourceBytes - 4} {
		value := `{"` + strings.Repeat("k", padding) + `":0}`
		fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+value+`}`)
		if err := fixture.scan(test); err != nil || fixture.states[1].Current.PendingKey == nil {
			test.Fatal("pending key checkpoint", padding, err)
		}
	}
}

func TestDocToolTreeRejectsNoncanonicalShapes(test *testing.T) {
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":{"a":[`+strings.Repeat("[", 32)+`0`+strings.Repeat("]", 32)+`]}}`)
	if err := fixture.scan(test); err != nil {
		test.Fatal(err)
	}
	for _, mutate := range []func(*DocToolTreeScanState){
		func(state *DocToolTreeScanState) { state.Version++ },
		func(state *DocToolTreeScanState) { state.Source = HashContent([]byte("wrong")) },
		func(state *DocToolTreeScanState) { state.Next-- },
		func(state *DocToolTreeScanState) { state.NodeCount = -1 },
		func(state *DocToolTreeScanState) { state.NodeCount = fixture.source.Root.To - fixture.source.Root.From },
		func(state *DocToolTreeScanState) { state.Complete = true },
		func(state *DocToolTreeScanState) { state.Syntax.Stack[0] = docFieldArrayAfter },
		func(state *DocToolTreeScanState) {
			state.Syntax.Stack = append(state.Syntax.Stack, docFieldObjectFirst)
		},
		func(state *DocToolTreeScanState) { state.Syntax.HashState = []byte{1} },
		func(state *DocToolTreeScanState) { state.Syntax.Digest = fixture.source.EventHash },
		func(state *DocToolTreeScanState) { state.Syntax.Kind = EventToolCall },
		func(state *DocToolTreeScanState) { state.Syntax.Field = docFieldKind },
		func(state *DocToolTreeScanState) { state.Syntax.Output = &DocFindSpan{From: 1, To: 2} },
		func(state *DocToolTreeScanState) { state.Syntax.RootDone = true },
		func(state *DocToolTreeScanState) { state.Syntax.Done = true },
		func(state *DocToolTreeScanState) { state.Syntax.StringPurpose = 1 },
		func(state *DocToolTreeScanState) { state.Syntax.schema = docFieldSchemaTool },
		func(state *DocToolTreeScanState) { state.Current = nil },
		func(state *DocToolTreeScanState) { state.Current.ParentFrame = "invalid" },
		func(state *DocToolTreeScanState) { state.Current.Source = HashContent([]byte("wrong")) },
		func(state *DocToolTreeScanState) { state.Current.Node.Value.To = state.Next },
		func(state *DocToolTreeScanState) { state.Current.Node.Depth++ },
		func(state *DocToolTreeScanState) { state.Current.Node.Children++ },
		func(state *DocToolTreeScanState) { state.Current.KeyFrom = state.Next },
	} {
		state := fixture.steps[0].State()
		mutate(&state)
		if state.Validate(fixture.source) == nil {
			test.Fatal("invalid state accepted", state)
		}
	}
	var original DocToolTreeNode
	for _, node := range fixture.nodes {
		if node.Key != nil {
			original = node
			break
		}
	}
	for _, mutate := range []func(*DocToolTreeNode){
		func(node *DocToolTreeNode) { node.Parent = -1 },
		func(node *DocToolTreeNode) { node.Ordinal = -1 },
		func(node *DocToolTreeNode) { node.Depth = 0 },
		func(node *DocToolTreeNode) { node.Depth = 513 },
		func(node *DocToolTreeNode) { node.Type = DocToolValueString },
		func(node *DocToolTreeNode) { node.Children = math.MaxInt },
		func(node *DocToolTreeNode) { node.Value.To = fixture.source.Root.To },
		func(node *DocToolTreeNode) { node.Key = &DocFindSpan{From: math.MaxInt, To: math.MinInt + 2} },
		func(node *DocToolTreeNode) { node.Key = &DocFindSpan{From: node.Value.From, To: node.Value.From + 2} },
	} {
		node := original
		mutate(&node)
		if node.Validate(fixture.source) == nil {
			test.Fatal("invalid node accepted", node)
		}
	}
}

func TestDocToolTreeMalformedSelectedGrammar(test *testing.T) {
	for _, scenario := range [][2]string{
		{`[0,0]`, `[0,] `}, {`[0,0]`, `[0 0]`}, {`[0]`, `[0 `}, {`[0]`, `]0[`},
		{`{"a":0}`, `{"a" 0}`}, {`{"a":0}`, `{"a":]}`}, {`"ab"`, `"\q"`}, {`"ab"`, `"` + string([]byte{0xff, 'x'}) + `"`},
		{`"ab"`, `""xx`}, {`10`, `01`}, {`10`, `1e`}, {`10`, `1 `}, {`true`, `truX`}, {`null`, `nulx`}, {`false`, `falSe`}, {`[]`, `{}`},
	} {
		test.Run(scenario[1], func(test *testing.T) {
			fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+scenario[0]+`}`)
			if len(scenario[0]) != len(scenario[1]) {
				test.Fatal("bad malformed fixture")
			}
			copy(fixture.raw[fixture.source.Root.From:fixture.source.Root.To], scenario[1])
			if err := fixture.scan(test); !errors.Is(err, ErrIntegrity) {
				test.Fatal("malformed selected value accepted", err)
			}
		})
	}
}

func TestDocToolTreeSealedClones(test *testing.T) {
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":{"a":[`+strings.Repeat("[", 40)+`"text"`+strings.Repeat("]", 40)+`]}}`)
	if err := fixture.scan(test); err != nil {
		test.Fatal(err)
	}
	for _, step := range fixture.steps {
		hash, checkpoint := step.Checkpoint()
		state := step.State()
		state.Syntax.Stack[0] = 255
		if state.Current != nil {
			state.Current.Node.Children = -1
			if state.Current.Node.Key != nil {
				state.Current.Node.Key.From = -1
			}
		}
		for _, object := range step.Objects() {
			object.Bytes[0] ^= 1
		}
		for _, node := range step.Nodes() {
			if node.Key != nil {
				node.Key.From = -1
			}
		}
		checkpoint[0] ^= 1
		actualHash, actual := step.Checkpoint()
		decoded, err := DecodeDocToolTreeScanState(fixture.source, hash, actual)
		if err != nil || hash != actualHash || !reflect.DeepEqual(decoded, step.State()) {
			test.Fatal("state or checkpoint getter exposed sealed memory", err)
		}
		for _, node := range step.Nodes() {
			if node.Validate(fixture.source) != nil {
				test.Fatal("node getter exposed a key span")
			}
		}
		for _, object := range step.Objects() {
			if HashContent(object.Bytes) != object.Hash {
				test.Fatal("object getter exposed bytes")
			}
		}
	}
}

func TestDocToolTreeInlineScalarParentUnionAndClones(test *testing.T) {
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":{"outer":{"a":"`+strings.Repeat("PRIVATE_BODY_MARKER", 8192)+`"}}}`)
	if err := fixture.scan(test); err != nil {
		test.Fatal(err)
	}
	step := fixture.steps[0]
	original := step.State()
	if original.InlineParent == nil || original.Current == nil || original.Current.Node.Type != DocToolValueString || original.Current.ParentFrame != "" || original.InlineParent.Node.Key == nil || original.Current.Node.Key == nil || len(fixture.objects)-1 != 1 {
		test.Fatal("scalar parent was not inlined")
	}
	state := step.State()
	state.InlineParent.Node.Key.From = -1
	state.InlineParent.Node.Children = -1
	state.InlineParent.ParentFrame = "corrupt"
	state.Current.Node.Key.From = -1
	if !reflect.DeepEqual(step.State(), original) {
		test.Fatal("inline scalar parent getter exposed mutable memory")
	}
	for _, mutate := range []func(*DocToolTreeScanState){
		func(state *DocToolTreeScanState) { state.InlineParent = nil },
		func(state *DocToolTreeScanState) { state.Current.ParentFrame = original.InlineParent.ParentFrame },
		func(state *DocToolTreeScanState) { state.InlineParent.ParentFrame = "" },
		func(state *DocToolTreeScanState) { state.InlineParent.Version++ },
		func(state *DocToolTreeScanState) { state.InlineParent.Source = HashContent([]byte("wrong")) },
		func(state *DocToolTreeScanState) { state.InlineParent.Node.Children++ },
		func(state *DocToolTreeScanState) { state.InlineParent.Node.Depth++ },
		func(state *DocToolTreeScanState) { state.InlineParent.Node.Key = nil },
		func(state *DocToolTreeScanState) { state.InlineParent.Node.Type = DocToolValueArray },
		func(state *DocToolTreeScanState) { state.InlineParent.KeyFrom = state.InlineParent.Node.Value.From + 1 },
		func(state *DocToolTreeScanState) {
			state.InlineParent.PendingKey = cloneDocToolFieldSpan(state.Current.Node.Key)
		},
	} {
		state := step.State()
		mutate(&state)
		if state.Validate(fixture.source) == nil {
			test.Fatal("invalid scalar-parent union accepted", state)
		}
	}
	for _, step := range fixture.steps {
		state := step.State()
		if state.InlineParent == nil {
			state.InlineParent = cloneDocToolTreeFrame(original.InlineParent)
			if state.Validate(fixture.source) == nil {
				test.Fatal("extra inline parent accepted for container or completion")
			}
		}
	}
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if hash == original.InlineParent.ParentFrame {
			return []byte("corrupt"), nil
		}
		return fixture.load(ctx, hash)
	}
	failed, err := StepDocToolTreeScan(context.Background(), fixture.source, original, fixture.read, load)
	if !errors.Is(err, ErrIntegrity) {
		test.Fatal("inline parent's suspended ancestor not checked", err)
	}
	assertDocToolTreeEmptyStep(test, failed)
}

func TestDocToolTreeCancellationIOAndCorruption(test *testing.T) {
	fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":[0,"`+strings.Repeat("x", 9000)+`"]}`)
	initial, _ := NewDocToolTreeScanState(fixture.source)
	initialRaw, _ := initial.Encode(fixture.source)
	failure := errors.New("tree load failed")
	for _, mode := range []string{"cancel-before", "cancel-raw", "cancel-object", "raw-error", "short-read", "object-error", "late-raw-error"} {
		test.Run(mode, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			if mode == "cancel-before" {
				cancel()
			}
			step, err := StepDocToolTreeScan(ctx, fixture.source, initial, func(ctx context.Context, from, length int) ([]byte, error) {
				reads++
				raw, err := fixture.read(ctx, from, length)
				switch mode {
				case "cancel-raw":
					cancel()
				case "raw-error":
					return nil, failure
				case "short-read":
					return raw[:len(raw)-1], nil
				case "late-raw-error":
					if reads == 2 {
						return nil, failure
					}
				}
				return raw, err
			}, func(ctx context.Context, hash ContentHash) ([]byte, error) {
				if mode == "cancel-object" {
					cancel()
				}
				if mode == "object-error" {
					return nil, failure
				}
				return fixture.load(ctx, hash)
			})
			if err == nil || strings.HasPrefix(mode, "cancel") && !errors.Is(err, context.Canceled) || strings.HasSuffix(mode, "error") && !errors.Is(err, failure) {
				test.Fatal("wrong failure", err)
			}
			assertDocToolTreeEmptyStep(test, step)
			after, err := initial.Encode(fixture.source)
			if err != nil || !bytes.Equal(initialRaw, after) {
				test.Fatal("failed step mutated predecessor")
			}
		})
	}
	deep := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+strings.Repeat("[", 40)+`0`+strings.Repeat("]", 40)+`}`)
	if err := deep.scan(test); err != nil {
		test.Fatal(err)
	}
	state := deep.states[1]
	for _, mode := range []string{"hash", "scope", "children", "depth", "unknown", "pending-key"} {
		test.Run(mode, func(test *testing.T) {
			encoded, _ := state.Encode(deep.source)
			changed, _ := DecodeDocToolTreeScanState(deep.source, HashContent(encoded), encoded)
			parentHash := changed.Current.ParentFrame
			raw := bytes.Clone(deep.objects[parentHash])
			if mode == "hash" {
				raw[0] ^= 1
			} else {
				var frame docToolTreeFrame
				if json.Unmarshal(raw, &frame) != nil {
					test.Fatal("frame fixture")
				}
				switch mode {
				case "scope":
					frame.Source = HashContent([]byte("other scope"))
				case "children":
					frame.Node.Children++
				case "depth":
					frame.Node.Depth--
				case "pending-key":
					frame.PendingKey = &DocFindSpan{From: frame.Node.Value.From + 1, To: frame.Node.Value.From + 3}
				}
				raw, _ = canonicalJSON(frame)
				if mode == "unknown" {
					raw = append(append(raw[:len(raw)-1], []byte(`,"extra":0`)...), '}')
				}
				parentHash = HashContent(raw)
				changed.Current.ParentFrame = parentHash
			}
			load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
				if hash == parentHash {
					return raw, nil
				}
				return deep.load(ctx, hash)
			}
			step, err := StepDocToolTreeScan(context.Background(), deep.source, changed, deep.read, load)
			if !errors.Is(err, ErrIntegrity) {
				test.Fatal("corrupt parent accepted", err)
			}
			assertDocToolTreeEmptyStep(test, step)
		})
	}
	if _, err := FinishDocToolTreeScan(context.Background(), deep.source, state, deep.load); !errors.Is(err, ErrIntegrity) {
		test.Fatal("unfinished scan finished", err)
	}
	state = deep.states[2]
	var parent docToolTreeFrame
	if json.Unmarshal(deep.objects[state.Current.ParentFrame], &parent) != nil || parent.ParentFrame == "" {
		test.Fatal("missing ancestor fixture")
	}
	load := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if hash == parent.ParentFrame {
			return []byte("corrupt ancestor"), nil
		}
		return deep.load(ctx, hash)
	}
	step, err := StepDocToolTreeScan(context.Background(), deep.source, state, deep.read, load)
	if !errors.Is(err, ErrIntegrity) {
		test.Fatal("late ancestor corruption accepted", err)
	}
	assertDocToolTreeEmptyStep(test, step)
}
