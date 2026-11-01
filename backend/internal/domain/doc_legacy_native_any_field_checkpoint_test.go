package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const legacyNativeAnyFieldW84Directory = "CXTHUB_W84_NATIVE_ANY_FIELD_DIRECTORY"
const legacyNativeAnyFieldW84Budget int64 = 64 << 20
const legacyNativeAnyFieldW84SourceSteps = 4096
const legacyNativeAnyFieldW84CheckpointSteps = 65536
const legacyNativeAnyFieldW84Unfinished = "R0 complete; R1 in progress; R2-R5 incomplete. Initial nonempty untracked Go maps/arrays and external deep Input/Output aliases remain immediate mandatory follow-up work, not successful empty checkpoints. Replacement, complete actual stage/sequence/pinning/encoder/four-spool/sort/proof/worker integration and all original full, performance, default-switch and operational gates remain unfinished. These finite private-v3 cuts do not establish transparent old/new running-binary compatibility."

type legacyNativeAnyFieldW84Fixture struct {
	name  string
	group string
	raw   string
	field string
	cut   string
}

func legacyNativeAnyFieldW84Fixtures() []legacyNativeAnyFieldW84Fixture {
	width := LegacyNormalizationPartBytes
	longText := strings.Repeat("a", 2*width) + `\n\uD83D\uDE42\u0000한`
	pad := func(prefix, ending, suffix string) string {
		return prefix + strings.Repeat(" ", width-len(prefix)-len(ending)) + ending + suffix
	}
	caches := `{"input":{"keep":-0,"nested":[{"old":true}]},"output":[-0,{"visible":"old"}],`
	return []legacyNativeAnyFieldW84Fixture{
		{name: "input-partial-root-deep-giant-key", group: "input", field: "input", cut: "deep-key", raw: `{"input":{"accepted":-0,"nested":[{"` + strings.Repeat("k", 2*width) + `":[false,null,5e-324]}]},"seq":7,"output":null}`},
		{name: "input-duplicate-reuses-exact-native-root", group: "input", field: "input", cut: "duplicate", raw: pad(`{"input":{"keep":-0,"replace":{"old":true}},"INPUT":{`, `"replace"`, `:{"new":[false,null]},"replace":null,"added":5e-324},"seq":7}`)},
		{name: "input-deep-array-growth-copy-migration", group: "input", field: "input", cut: "migration", raw: `{"input":{"array":[` + strings.Repeat("null,", 4200) + `{"tail":[-0,true]}]},"seq":7}`},
		{name: "output-active-independent-prior-negative-zero", group: "output", field: "output", cut: "scalar", raw: `{"output":{"nested":[{"text":"` + longText + `","bits":[-0,9007199254740993]}]},"seq":7}`},
		{name: "output-active-separate-visible-native-owner", group: "output", field: "output", cut: "visible", raw: pad(`{"output":{"previous":[-0,"visible"]},"output":[{`, `"fresh"`, `:[null,true]}],"seq":7}`)},
		{name: "inactive-any-caches-during-other-root-scalar", group: "output", cut: "cached-root", raw: caches + `"id":"` + longText + `","input":{"replace":[-0]},"output":{"after":false},"seq":7}`},
		{name: "inactive-any-caches-during-real-nested-child", group: "cache", cut: "cached-nested", raw: caches + `"locked":{"provider":"codex","blob":"` + longText + `"},"input":{"last":true},"output":null,"seq":7}`},
		{name: "inactive-any-caches-during-real-block-child", group: "cache", cut: "cached-block", raw: caches + `"blocks":[{"type":"text","text":"` + longText + `"}],"input":null,"output":"retired","seq":7}`},
		{name: "null-and-scalar-retire-both-native-caches", group: "cache", cut: "retired", raw: caches + `"input":null,"output":false,"id":"` + longText + `","input":{"fresh":-0},"output":[null,-0],"seq":7}`},
		{name: "outer-giant-number-preserves-negative-zero", group: "float", field: "output", cut: "number", raw: `{"output":-0.` + strings.Repeat("0", 2*width) + `0,"seq":7,"id":"after-negative-zero"}`},
		{name: "outer-giant-number-future-full-error", group: "float", field: "output", cut: "number", raw: `{"output":1` + strings.Repeat("0", 2*width) + `,"output":-0,"seq":7,"id":"after-error"}`},
		{name: "outer-giant-number-partial-full-diagnostic", group: "float", field: "output", cut: "diagnostic", raw: `{"output":1` + strings.Repeat("0", 2*width) + `,"output":-0,"seq":7,"id":"after-diagnostic"}`},
	}
}

func legacyNativeAnyFieldW84Initial() *wire {
	value := legacyNativeNestedW83Initial(legacyNativeNestedW83Fixture{})
	value.Output = math.Copysign(0, -1)
	return value
}

func legacyNativeAnyFieldW84Parser(native *legacyNativeObject, name string) *legacyNativeAny {
	index := native.metadata.match(name)
	if index < 0 || index >= len(native.containers) {
		return nil
	}
	return native.containers[index]
}

func legacyNativeAnyFieldW84Stop(bundle *legacyNativeObjectRestored, fixture legacyNativeAnyFieldW84Fixture) bool {
	native := bundle.Object
	width := LegacyNormalizationPartBytes
	inputParser := legacyNativeAnyFieldW84Parser(native, "input")
	outputParser := legacyNativeAnyFieldW84Parser(native, "output")
	switch fixture.cut {
	case "cached-root", "retired":
		if native.containerActive || native.field != native.metadata.match("id") || native.scalar == nil || native.scalar.text == nil || native.scalar.text.Len() == 0 || native.scalar.position != width {
			return false
		}
		if fixture.cut == "retired" {
			value := native.destination.Interface().(*wire)
			return native.containers != nil && inputParser == nil && outputParser == nil && value.Input == nil && value.Output == false
		}
		return inputParser != nil && inputParser.finished && outputParser != nil && outputParser.finished
	case "cached-nested", "cached-block":
		if native.containerActive || inputParser == nil || !inputParser.finished || outputParser == nil || !outputParser.finished {
			return false
		}
		if fixture.cut == "cached-nested" {
			child := legacyNativeNestedW83Child(native, "locked")
			return native.nestedActive && child != nil && child.scalar != nil && child.scalar.text != nil && child.scalar.text.Len() > 0 && child.scalar.position == width
		}
		list := legacyNativeEventW82List(native)
		return native.blockActive && list != nil && legacyNativeBlockListW81Stop(list, "scalar")
	case "number", "diagnostic":
		if native.field != native.metadata.match("output") || native.number == nil || native.number.parser == nil {
			return false
		}
		number := native.number
		if fixture.cut == "number" {
			return !number.converted && number.parser.position == width && number.parser.authority != 0
		}
		return number.converted && number.rejected && !number.complete && number.errorText != nil && number.errorText.position == width && number.errorText.text != nil && number.errorText.text.Len() > len("number ")
	}
	parser := legacyNativeAnyFieldW84Parser(native, fixture.field)
	if !native.containerActive || parser == nil || native.field != native.metadata.match(fixture.field) {
		return false
	}
	switch fixture.cut {
	case "deep-key":
		return parser.depth == 3 && parser.keyToken && parser.scalar != nil && parser.scalar.position == width && parser.scalar.text != nil && parser.scalar.text.Len() > 0
	case "duplicate":
		return native.valueStart > strings.Index(fixture.raw, `,"INPUT":`) && parser.rootMap != nil && parser.pending && parser.keyToken && parser.position == width
	case "migration":
		return parser.frame != nil && parser.frame.array != nil && parser.frame.array.previous != nil && parser.frame.array.copied > 0 && parser.frame.array.copied < len(parser.frame.array.previous)
	case "scalar":
		return !parser.keyToken && parser.scalar != nil && parser.scalar.position == width && parser.scalar.text != nil && parser.scalar.text.Len() > 0 && parser.visible.object == nil && parser.visible.array == nil
	case "visible":
		return parser.pending && parser.keyToken && parser.position == width && parser.visible.object != nil && parser.rootArray != nil
	}
	return false
}

func legacyNativeAnyFieldW84Suffix(native *legacyNativeObject, width int) int {
	if native.containerActive {
		if native.field < 0 || native.field >= len(native.containers) || native.containers[native.field] == nil {
			return -1
		}
		parser := native.containers[native.field]
		position := legacyNativeAnyW78SemanticSuffix(parser, width)
		if parser.number != nil && parser.number.errorText != nil {
			position = min(position, parser.number.errorText.position/width*width)
		}
		return position
	}
	if native.number != nil {
		position := min(native.position, native.number.parser.position)
		if native.number.errorText != nil {
			position = min(position, native.number.errorText.position)
		}
		return position / width * width
	}
	return legacyNativeNestedW83Suffix(native, width)
}

type legacyNativeAnyFieldW84State struct {
	Control             []int                            `json:"control"`
	Flags               []bool                           `json:"flags"`
	Key                 []byte                           `json:"key"`
	Scanner             []byte                           `json:"scanner"`
	Values              []string                         `json:"values"`
	DestinationPresence eventFieldPresence               `json:"destination_presence"`
	Initialized         []bool                           `json:"initialized"`
	Scalar              []string                         `json:"scalar"`
	Number              []string                         `json:"number"`
	Containers          []*legacyNativeAnyW78StateRecord `json:"containers"`
	Bindings            []uint8                          `json:"bindings"`
	Visible             []string                         `json:"visible"`
	Nested              []*legacyNativeObjectW79State    `json:"nested"`
	Blocks              legacyNativeBlockListW81Array    `json:"blocks"`
	Block               *legacyNativeBlockListW81State   `json:"block"`
	BlockBinding        uint8                            `json:"block_binding"`
	Error               legacyNativeObjectW79Error       `json:"error"`
}

func legacyNativeAnyFieldW84OwnerAliases(test *testing.T, owner legacyNativeAnyValueOwner, visited *int) {
	test.Helper()
	*visited += 1
	if *visited > 8192 || owner.validate() != nil {
		test.Fatal("actual dynamic owner graph is invalid or exceeds the finite fixture schedule")
	}
	if owner.object != nil {
		for key, row := range owner.object.ownership {
			if err := row.owner.matches(owner.object.values()[key]); err != nil {
				test.Fatal("deep map value lost its exact native child owner", err)
			}
			if row.owner.object != nil || row.owner.array != nil {
				legacyNativeAnyFieldW84OwnerAliases(test, row.owner, visited)
			}
		}
	}
	if owner.array != nil {
		for index := 0; index < owner.array.children.length(); index++ {
			child := owner.array.children.at(index)
			if child == nil || index >= len(owner.array.values()) || child.owner.matches(owner.array.values()[index]) != nil {
				test.Fatal("deep array value lost its exact native child owner")
			}
			if child.owner.object != nil || child.owner.array != nil {
				legacyNativeAnyFieldW84OwnerAliases(test, child.owner, visited)
			}
		}
	}
}

func legacyNativeAnyFieldW84NumberState(test *testing.T, number *legacyNativeNumberValue, source LegacyByteSource) []string {
	test.Helper()
	if number == nil {
		return nil
	}
	if number.parser == nil || number.parser.authority == 0 || number.parser.authority != source.authority || number.parser.sourceBytes != source.Bytes || number.diagnostics != legacyNativeNumberDiagnosticsFull {
		test.Fatal("actual float continuation lost the current source grant, geometry, or Full diagnostics")
	}
	state, err := legacyNativeCheckpointNumberCapture(number, source.Bytes)
	if err != nil {
		test.Fatal("actual float state is not independently observable", err)
	}
	body, err := json.Marshal(state)
	if err != nil {
		test.Fatal(err)
	}
	observed := []string{string(body)}
	if number.errorText != nil {
		observed = append(observed, legacyNativeAnyW78ScalarState(test, number.errorText)...)
	}
	return observed
}

func legacyNativeAnyFieldW84StateOf(test *testing.T, bundle *legacyNativeObjectRestored) legacyNativeAnyFieldW84State {
	test.Helper()
	native := bundle.Object
	value, typed := native.destination.Interface().(*wire)
	if !typed || native.metadata != &legacyEventNativeFields || !native.trackPresence || len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) || value.Replacement != nil || native.eventActive || len(native.events) != 0 || native.pagedEvents {
		test.Fatal("fixture lost actual Event metadata or silently entered an unfinished aggregate path")
	}
	if native.checkpointScanner != nil && *native.checkpointScanner != bundle.Scanner {
		test.Fatal("restored actual parent lost its single caller scanner")
	}
	scannerBytes, err := bundle.Scanner.checkpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	state := legacyNativeAnyFieldW84State{
		Control: []int{native.parentDepth, native.fieldLimit, native.position, native.end, native.valueStart, native.valueEnd, native.field, int(native.presence), int(native.phase), int(native.valueFirst), cap(native.key), cap(native.raw)},
		Flags:   []bool{native.trackPresence, native.deferTypeErrors, native.skipValue, native.prefix, native.keyStarted, native.keyOverflow, native.valueStarted, native.listActive, native.blockActive, native.nestedActive, native.containerActive, native.containers != nil, native.blocks != nil, native.nested != nil},
		Key:     bytes.Clone(native.key), Scanner: scannerBytes, DestinationPresence: value.fieldPresence,
		Blocks: legacyNativeBlockListW81ArrayOf(value.Blocks), Error: legacyNativeObjectW79ErrorState(native.typeError),
		Number: legacyNativeAnyFieldW84NumberState(test, native.number, bundle.Source),
	}
	for index, field := range native.metadata.fields {
		heap := native.heaps[index]
		if heap.err != nil || heap.bytes < 0 || native.backingExcess[index] < 0 {
			test.Fatal("actual outer field lost current ownership accounting", field.name)
		}
		state.Initialized = append(state.Initialized, heap.bytes != 0)
		stored := native.destination.Elem().Field(field.index)
		switch field.name {
		case "input", "output":
			state.Values = append(state.Values, field.name+":"+legacyNativeAnyW78Native(test, stored.Interface()))
		case "locked", "provider_metadata":
			state.Values = append(state.Values, field.name+fmt.Sprintf(":nil=%t", stored.IsNil()))
			if !stored.IsNil() {
				state.Values = append(state.Values, legacyNativeObjectW79Values(test, stored.Interface())...)
			}
		case "blocks", "replacement":
		default:
			switch stored.Kind() {
			case reflect.String:
				state.Values = append(state.Values, field.name+":"+stored.String())
			case reflect.Bool:
				state.Values = append(state.Values, fmt.Sprintf("%s:%t", field.name, stored.Bool()))
			case reflect.Int, reflect.Int64:
				state.Values = append(state.Values, fmt.Sprintf("%s:%d", field.name, stored.Int()))
			case reflect.Pointer:
				if stored.Type() != reflect.TypeFor[*bool]() {
					test.Fatal("unexpected actual Event pointer", field.name, stored.Type())
				}
				state.Values = append(state.Values, field.name+fmt.Sprintf(":nil=%t", stored.IsNil()))
				if !stored.IsNil() {
					state.Values = append(state.Values, fmt.Sprintf("%t", stored.Elem().Bool()))
				}
			default:
				test.Fatal("unexpected actual Event field in finite dynamic fixture", field.name, stored.Type())
			}
		}
	}
	if native.scalar != nil {
		state.Scalar = legacyNativeAnyW78ScalarState(test, native.scalar)
	}
	if native.containers != nil {
		if len(native.containers) != len(native.metadata.fields) {
			test.Fatal("actual any cache is not field-indexed")
		}
		state.Containers = make([]*legacyNativeAnyW78StateRecord, len(native.containers))
		state.Bindings = make([]uint8, len(native.containers))
		state.Visible = make([]string, len(native.containers))
	}
	for index, parser := range native.containers {
		if parser == nil {
			continue
		}
		field := native.metadata.fields[index]
		stored := native.destination.Elem().Field(field.index).Interface()
		active := native.containerActive && native.field == index
		var scanner legacyJSONValueScanner
		if active {
			if native.phase != legacyNativeObjectApply || native.valueFirst != '{' && native.valueFirst != '[' || native.position != native.valueStart || native.valueEnd != native.end || parser.from != native.valueStart || parser.end != native.valueEnd || parser.parentDepth != native.parentDepth || parser.fieldLimit != native.fieldActionLimit() {
				test.Fatal("actual active Any lost its parent range, depth, field limit, or Apply binding", field.name)
			}
			scanner = bundle.Scanner
		} else if !parser.finished {
			test.Fatal("inactive actual Any cache is not finished", field.name)
		}
		binding := uint8(0)
		if field.genericMap {
			if parser.rootMap != nil {
				if reflect.ValueOf(stored).Pointer() != reflect.ValueOf(parser.rootMap.values()).Pointer() {
					test.Fatal("Input destination is not the exact partial/reused native root map")
				}
				binding = 1
			}
		} else if !active {
			if parser.result.owner.matches(stored) != nil {
				test.Fatal("completed Output lost the actual result owner")
			}
			binding = 2
		} else if parser.visible.object != nil || parser.visible.array != nil {
			if parser.visible.matches(stored) != nil {
				test.Fatal("active Output lost its separate previous visible owner")
			}
			binding = 3
		}
		var root *legacyNativeAnyFrame
		for current := parser.frame; current != nil; current = current.parent {
			root = current
			owner, failure := legacyNativeAnyCheckpointContainer(current.object, current.array)
			if failure != nil {
				test.Fatal("actual frame owner cannot be observed", failure)
			}
			visited := 0
			legacyNativeAnyFieldW84OwnerAliases(test, owner.owner, &visited)
		}
		if root != nil && (root.object != parser.rootMap || root.array != parser.rootArray) {
			test.Fatal("actual root/frame alias split")
		}
		if parser.pending && parser.node.validateOwnership() != nil || parser.finished && parser.result.validateOwnership() != nil {
			test.Fatal("actual pending/result ownership is invalid")
		}
		if parser.pending {
			visited := 0
			legacyNativeAnyFieldW84OwnerAliases(test, parser.node.owner, &visited)
		}
		if parser.finished {
			visited := 0
			legacyNativeAnyFieldW84OwnerAliases(test, parser.result.owner, &visited)
		}
		visible, failure := legacyNativeAnyCheckpointOwnerValue(parser.visible)
		if failure != nil {
			test.Fatal("actual previous Output owner is invalid", failure)
		}
		visited := 0
		legacyNativeAnyFieldW84OwnerAliases(test, visible.owner, &visited)
		state.Visible[index] = legacyNativeAnyW78Native(test, visible.value)
		state.Bindings[index] = binding
		observed := legacyNativeAnyW78State(test, parser, &scanner)
		state.Containers[index] = &observed
		legacyNativeAnyFieldW84NumberState(test, parser.number, bundle.Source)
	}
	if native.containerActive && (native.field < 0 || native.field >= len(native.containers) || native.containers[native.field] == nil) {
		test.Fatal("actual active Any has no field cache")
	}
	if native.nested != nil {
		state.Nested = make([]*legacyNativeObjectW79State, len(native.nested))
	}
	for index, child := range native.nested {
		if child == nil {
			continue
		}
		stored := native.destination.Elem().Field(native.metadata.fields[index].index)
		var scanner legacyJSONValueScanner
		if native.nestedActive && native.field == index {
			scanner = bundle.Scanner
		}
		if stored.IsNil() || child.destination.Interface() != stored.Interface() || child.parentDepth != native.parentDepth+1 || child.trackPresence || child.checkpointScanner != nil && *child.checkpointScanner != scanner {
			test.Fatal("nested continuation lost its actual pointee/scanner/depth binding")
		}
		observed := legacyNativeObjectW79StateOf(test, child, &scanner)
		state.Nested[index] = &observed
	}
	if list := legacyNativeEventW82List(native); list != nil {
		if list.frame == nil || *list.frame != bundle.Scanner || list.parentDepth != native.parentDepth+1 {
			test.Fatal("block continuation lost the actual shared scanner")
		}
		observed := legacyNativeBlockListW81StateOf(test, list)
		state.Block = &observed
		if legacyNativeEventW82Binding(value.Blocks, list.decoded) {
			state.BlockBinding = 1
		} else if legacyNativeEventW82Binding(value.Blocks, list.previous) {
			state.BlockBinding = 2
		}
	}
	return state
}

func legacyNativeAnyFieldW84Step(test *testing.T, current context.Context, bundle *legacyNativeObjectRestored, onePage bool) (bool, error) {
	test.Helper()
	input := &legacyNormalizationInput{ctx: current, source: bundle.Source, numberDiagnostics: legacyNativeNumberDiagnosticsFull}
	if onePage {
		input.loads = 1
	}
	priorInput := legacyNativeAnyFieldW84Parser(bundle.Object, "input")
	priorOutput := legacyNativeAnyFieldW84Parser(bundle.Object, "output")
	var priorRoot *legacyNativeAnyMap
	if priorInput != nil {
		priorRoot = priorInput.rootMap
	}
	complete, failure := bundle.Object.step(current, input, &bundle.Scanner)
	maximumFields := max(bundle.Object.fieldActionLimit(), LegacyNormalizationStepEvents*len(legacyContentBlockNativeFields.fields)+1)
	if input.failure != nil && !errors.Is(input.failure, context.Canceled) || input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.fields < 0 || input.fields > maximumFields || input.values < 0 || input.values > legacyNativeAnyValueActions {
		test.Fatal("actual Event/Any escaped its shared finite source quantum", input.failure, input.workBytes, input.loads, input.fields, input.values)
	}
	if next := legacyNativeAnyFieldW84Parser(bundle.Object, "input"); priorInput != nil && next != nil && (next != priorInput || priorRoot != nil && next.rootMap != priorRoot) {
		test.Fatal("duplicate Input replaced rather than reused its actual parser/root")
	}
	if next := legacyNativeAnyFieldW84Parser(bundle.Object, "output"); priorOutput != nil && next != nil && next != priorOutput && !next.finished && (next.visible.object != priorOutput.result.owner.object || next.visible.array != priorOutput.result.owner.array) {
		test.Fatal("duplicate active Output discarded the previous native visible owner")
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, failure
}

func legacyNativeAnyFieldW84Continue(test *testing.T, bundle *legacyNativeObjectRestored) (int, error) {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeAnyFieldW84SourceSteps; attempt++ {
		current, cancel := context.WithCancel(context.Background())
		complete, failure := legacyNativeAnyFieldW84Step(test, current, bundle, false)
		cancel()
		if complete || failure != nil {
			if bundle.Object.phase != legacyNativeObjectDone || bundle.Object.position != bundle.Object.end || bundle.Object.containerActive || bundle.Object.nestedActive || bundle.Object.blockActive || bundle.Object.number != nil {
				test.Fatal("actual Event terminated without finishing its native suffix", failure)
			}
			return attempt, failure
		}
	}
	test.Fatal("actual Event/Any exceeded the finite 4096-step continuation schedule")
	return 0, ErrIntegrity
}

func legacyNativeAnyFieldW84Drain(test *testing.T, work legacyNativeAnyW78Work, storage *legacyNativeAnyW78CAS) int {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeAnyFieldW84CheckpointSteps; attempt++ {
		storage.quantum = 0
		current, cancel := context.WithCancel(context.Background())
		ready, failure := work.step(current)
		cancel()
		retained, heapErr := work.retainedBytes()
		if failure != nil || heapErr != nil || retained < 0 || retained > legacyNativeAnyFieldW84Budget || storage.quantum > ConversationManifestChunkBytes {
			test.Fatal("actual Any-field checkpoint escaped admission or its shared IO quantum", attempt, failure, retained, heapErr, storage.quantum)
		}
		if ready {
			return attempt
		}
	}
	test.Fatal("actual Any-field checkpoint exceeded its finite 65536-step schedule")
	return 0
}

func legacyNativeAnyFieldW84CaptureRoot(test *testing.T, bundle *legacyNativeObjectRestored, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) (legacyNativeCheckpointRef, int) {
	test.Helper()
	before := legacyNativeAnyFieldW84StateOf(test, bundle)
	quiescent := fmt.Sprintf("%#v|%#v", bundle.Object.heaps, bundle.Object.backingExcess)
	reads := 0
	source, captureSource := bundle.Source, bundle.Source
	captureSource.Read = func(current context.Context, offset, length int) ([]byte, error) {
		reads++
		return source.Read(current, offset, length)
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpoint(constructor, bundle.Object, &bundle.Scanner, request.Binding, captureSource, storage.io(), legacyNativeAnyFieldW84Budget)
	cancel()
	if err != nil {
		test.Fatal("actual Any-field root checkpoint construction", err)
	}
	defer work.close()
	steps := legacyNativeAnyFieldW84Drain(test, work, storage)
	root, err := work.take(context.Background())
	if err != nil || !root.valid() {
		test.Fatal("actual Any-field checkpoint failed to publish a real root", err)
	}
	legacyNativeAnyW78Released(test, work)
	if reads != 0 || storage.inputWrites != 0 || !reflect.DeepEqual(before, legacyNativeAnyFieldW84StateOf(test, bundle)) || quiescent != fmt.Sprintf("%#v|%#v", bundle.Object.heaps, bundle.Object.backingExcess) {
		test.Fatal("checkpoint replayed RAW, rewrote input CAS, or mutated exclusively quiescent actual state", reads, storage.inputWrites)
	}
	state, _ := legacyNativeNestedW83ReadControl(test, storage, root)
	schema, err := legacyNativeCheckpointModelSchema(&legacyEventNativeFields)
	if err != nil || state.Version != 3 || state.Model != legacyNativeCheckpointEventModel || state.Schema != schema || state.Binding != request.Binding || state.SourceBytes != request.SourceBytes || state.Presence != bundle.Object.presence || state.DestinationPresence != bundle.Object.destination.Interface().(*wire).fieldPresence || state.ContainerActive != bundle.Object.containerActive || state.HasContainerCache != (bundle.Object.containers != nil) || state.HasFloatNumber != (bundle.Object.number != nil) || (state.FloatNumber != nil) != state.HasFloatNumber || len(state.Fields) != len(bundle.Object.metadata.fields) || len(state.Fields) > 32 {
		test.Fatal("actual dynamic root lost private-v3/schema/source/presence/cache/number contracts", err)
	}
	for index, field := range bundle.Object.metadata.fields {
		if !field.genericMap && !field.interfaceString {
			continue
		}
		row := state.Fields[index]
		stored := bundle.Object.destination.Elem().Field(field.index)
		parser := legacyNativeAnyFieldW84Parser(bundle.Object, field.name)
		if row.Present != !stored.IsNil() || row.Initialized != (bundle.Object.heaps[index].bytes != 0) {
			test.Fatal("dynamic row conflated typed-nil/wire presence and actual heap initialization", field.name)
		}
		if parser == nil {
			if row.Any != nil && (row.Any.Binding != 0 || row.Any.Parser != (legacyNativeCheckpointRef{})) {
				test.Fatal("independent dynamic destination invented a real parser", field.name)
			}
			continue
		}
		if row.Any == nil || !row.Any.Parser.valid() || row.Any.Active != (bundle.Object.containerActive && bundle.Object.field == index) || row.Any.Binding != before.Bindings[index] || row.Any.Binding != 0 && row.Any.Destination != nil || row.Any.Binding == 0 && row.Any.Destination == nil {
			test.Fatal("actual dynamic cache/root/result/visible binding was omitted or replaced by a fake parser", field.name)
		}
		var child legacyNativeAnyCheckpointControl
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), row.Any.Parser, &child); err != nil || child.Binding != request.Binding || child.SourceBytes != request.SourceBytes || child.From != parser.from || child.End != parser.end || child.Position != parser.position || child.ParentDepth != parser.parentDepth || child.FieldLimit != parser.fieldLimit || child.Finished != parser.finished {
			test.Fatal("actual Any cache lost its original W78 control", field.name, err)
		}
		if bundle.Object.containerActive && bundle.Object.field == index {
			if child.Scanner != state.Scanner {
				test.Fatal("active Any did not checkpoint the actual shared parent scanner")
			}
		} else {
			var zero legacyJSONValueScanner
			body, err := zero.checkpoint(context.Background())
			if err != nil || child.Scanner.Hash != HashContent(body) || child.Scanner.Bytes != len(body) {
				test.Fatal("inactive Any cached another field's live scanner instead of zero framing", err)
			}
		}
	}
	return root, steps
}

func legacyNativeAnyFieldW84Capture(test *testing.T, fixture legacyNativeAnyFieldW84Fixture, storage *legacyNativeAnyW78CAS) (legacyNativeAnyW78Request, *legacyNativeObjectRestored) {
	test.Helper()
	if !json.Valid([]byte(fixture.raw)) {
		test.Fatal("actual Any-field process fixture is not valid JSON", fixture.name)
	}
	request, source := legacyNativeAnyW78FullSource(test, fixture.raw)
	request.Binding = HashContent([]byte("W84 actual Event Any field " + fixture.name))
	if len(request.Parts) == 0 || len(request.Parts) > 4 {
		test.Fatal("actual Any-field source exceeded the unchanged four-part bound", fixture.name)
	}
	value := legacyNativeAnyFieldW84Initial()
	native, err := newLegacyNativeObject(reflect.ValueOf(value), &legacyEventNativeFields, 1, true)
	if err != nil {
		test.Fatal(err)
	}
	for index, field := range native.metadata.fields {
		stored := native.destination.Elem().Field(field.index)
		if !stored.IsZero() {
			heap, err := legacyNativeValueHeap(context.Background(), stored)
			if err != nil {
				test.Fatal("actual initial Event heap", field.name, err)
			}
			native.heaps[index] = heap
		}
	}
	native.start(0, len(fixture.raw))
	native.fieldLimit, native.deferTypeErrors = 1, true
	bundle := &legacyNativeObjectRestored{Object: native, Source: source}
	storage.preload(test, request)
	var originalRoot *legacyNativeAnyMap
	for attempt := 0; attempt < legacyNativeAnyFieldW84SourceSteps && !legacyNativeAnyFieldW84Stop(bundle, fixture); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		current := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeAnyFieldW84Stop(bundle, fixture) }}
		complete, failure := legacyNativeAnyFieldW84Step(test, current, bundle, true)
		cancel()
		if parser := legacyNativeAnyFieldW84Parser(native, "input"); parser != nil && parser.rootMap != nil {
			if originalRoot == nil {
				originalRoot = parser.rootMap
			} else if fixture.cut == "duplicate" && parser.rootMap != originalRoot {
				test.Fatal("actual duplicate Input did not reuse its first native root map")
			}
		}
		if complete || failure != nil && !errors.Is(failure, context.Canceled) {
			test.Fatal("actual Event ended before its assigned process cut", fixture.name, native.position, failure)
		}
	}
	if !legacyNativeAnyFieldW84Stop(bundle, fixture) {
		test.Fatal("actual Event never reached the finite assigned native cut", fixture.name, native.position)
	}
	if native.presence == value.fieldPresence {
		test.Fatal("actual root presence was confused with untouched wire.fieldPresence")
	}
	request.Root, _ = legacyNativeAnyFieldW84CaptureRoot(test, bundle, request, storage)
	request.SuffixOffset = legacyNativeAnyFieldW84Suffix(native, request.PartBytes)
	if request.SuffixOffset < 0 || request.SuffixOffset > request.SourceBytes || request.SuffixOffset%request.PartBytes != 0 || fixture.cut != "migration" && request.SuffixOffset == 0 {
		test.Fatal("actual semantic suffix failed to exclude accepted RAW parts", fixture.name, request.SuffixOffset)
	}
	legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
	request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
	return request, bundle
}

func legacyNativeAnyFieldW84Independent(test *testing.T, fixture legacyNativeAnyFieldW84Fixture, bundle *legacyNativeObjectRestored, failure error) {
	test.Helper()
	expected := legacyNativeAnyFieldW84Initial()
	expectedFailure := json.Unmarshal([]byte(fixture.raw), expected)
	actual := bundle.Object.destination.Interface().(*wire)
	if !reflect.DeepEqual(actual, expected) || legacyNativeAnyW78Native(test, actual.Input) != legacyNativeAnyW78Native(test, expected.Input) || legacyNativeAnyW78Native(test, actual.Output) != legacyNativeAnyW78Native(test, expected.Output) || (failure == nil) != (expectedFailure == nil) {
		test.Fatal("native wire values/null/duplicates/float bits differ from independent encoding/json", failure, expectedFailure)
	}
	if failure != nil {
		var actualTyped, expectedTyped *json.UnmarshalTypeError
		if !errors.As(failure, &actualTyped) || !errors.As(expectedFailure, &expectedTyped) || actualTyped.Value != expectedTyped.Value || actualTyped.Type != expectedTyped.Type || actualTyped.Struct != expectedTyped.Struct || actualTyped.Field != expectedTyped.Field || !strings.HasPrefix(actualTyped.Value, "number ") || len(actualTyped.Value) <= 2*LegacyNormalizationPartBytes {
			test.Fatal("outer float lost the complete original number diagnostic/type/field", failure, expectedFailure)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fixture.raw), &fields); err != nil || bundle.Object.presence != eventPresence(fields) || actual.fieldPresence != legacyNativeAnyFieldW84Initial().fieldPresence {
		test.Fatal("actual Event root/wire presence ceased to be separate", err)
	}
}

func legacyNativeAnyFieldW84Restore(test *testing.T, request legacyNativeAnyW78Request, source LegacyByteSource, storage *legacyNativeAnyW78CAS) (*legacyNativeObjectRestored, int) {
	test.Helper()
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpointRestore(constructor, request.Root, legacyNativeCheckpointEventModel, storage.io(), source, request.Binding, legacyNativeAnyFieldW84Budget)
	cancel()
	if err != nil {
		test.Fatal("fresh actual Any-field restore construction", err)
	}
	defer work.close()
	steps := legacyNativeAnyFieldW84Drain(test, work, storage)
	restored, err := work.take(context.Background())
	if err != nil || restored.Object == nil {
		test.Fatal("fresh restore did not return the actual wire/native Event", err)
	}
	legacyNativeAnyW78Released(test, work)
	if source.authority == 0 || restored.Source.authority != source.authority || restored.Source.Read == nil || restored.Source.Bytes != source.Bytes || restored.Source.PartBytes != source.PartBytes || !reflect.DeepEqual(restored.Source.Parts, source.Parts) || request.SuffixOffset != legacyNativeAnyFieldW84Suffix(restored.Object, request.PartBytes) || storage.inputWrites != 0 {
		test.Fatal("actual restored Event lost its current grant/Source/geometry/suffix")
	}
	for _, parser := range restored.Object.containers {
		if parser != nil {
			legacyNativeAnyW78HeapAndAliases(test, parser)
		}
	}
	legacyNativeAnyFieldW84StateOf(test, &restored)
	return &restored, steps
}

type legacyNativeAnyFieldW84Dependency struct {
	Bytes  int
	Prefix bool
}

func legacyNativeAnyFieldW84PinnedDependencies(test *testing.T, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) map[ContentHash]legacyNativeAnyFieldW84Dependency {
	test.Helper()
	allowed := make(map[ContentHash]legacyNativeAnyFieldW84Dependency)
	root, _ := legacyNativeNestedW83ReadControl(test, storage, request.Root)
	numbers := make([]legacyNativeCheckpointRef, 0, 3)
	if root.FloatNumber != nil {
		numbers = append(numbers, *root.FloatNumber)
	}
	for _, field := range root.Fields {
		if field.Any == nil || !field.Any.Parser.valid() {
			continue
		}
		var child legacyNativeAnyCheckpointControl
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), field.Any.Parser, &child); err != nil {
			test.Fatal("actual Any dependency metadata", err)
		}
		if child.Number.valid() {
			numbers = append(numbers, child.Number)
		}
	}
	for _, reference := range numbers {
		var number legacyNativeCheckpointNumberState
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), reference, &number); err != nil || number.validate() != nil || number.SourceBytes != request.SourceBytes {
			test.Fatal("actual number dependency metadata", err)
		}
		if number.SourceParts.Count == 0 {
			continue
		}
		if number.PartBytes != request.PartBytes || number.SourceParts.Count > uint64(len(request.Parts)) {
			test.Fatal("number dependency manifest escaped actual four-part geometry")
		}
		reader, err := newLegacyNativeCheckpointListReader[legacyNativeCheckpointNumberSourcePart](number.SourceParts, storage.io())
		if err != nil {
			test.Fatal(err)
		}
		count := 0
		for count <= len(request.Parts) {
			part, present, err := reader.next(context.Background())
			if err != nil {
				reader.close()
				test.Fatal(err)
			}
			if !present {
				break
			}
			if count == len(request.Parts) || part.Ordinal < 0 || part.Ordinal >= len(request.Parts) || request.Parts[part.Ordinal].Hash != part.Hash || request.Parts[part.Ordinal].Bytes != int64(part.Bytes) || part.Ordinal*request.PartBytes+part.Bytes > number.Parser.Position {
				reader.close()
				test.Fatal("number dependency is not an exactly declared accepted immutable source part", part)
			}
			allowed[part.Hash] = legacyNativeAnyFieldW84Dependency{Bytes: part.Bytes, Prefix: part.Ordinal*request.PartBytes < request.SuffixOffset}
			count++
		}
		reader.close()
		if uint64(count) != number.SourceParts.Count {
			test.Fatal("number dependency list count changed")
		}
	}
	return allowed
}

type legacyNativeAnyFieldW84Report struct {
	PID               int                                `json:"pid"`
	RestoreSteps      int                                `json:"restore_steps"`
	RecaptureSteps    int                                `json:"recapture_steps"`
	RerestoreSteps    int                                `json:"rerestore_steps"`
	ContinuationSteps int                                `json:"continuation_steps"`
	Retained          int64                              `json:"retained"`
	Initial           legacyNativeAnyFieldW84State       `json:"initial"`
	Recaptured        legacyNativeAnyFieldW84State       `json:"recaptured"`
	Final             legacyNativeAnyFieldW84State       `json:"final"`
	Error             legacyNativeObjectW79Error         `json:"error"`
	Reads             legacyNativeAnyW78Reads            `json:"reads"`
	Rereads           legacyNativeAnyW78Reads            `json:"rereads"`
	Dependencies      []legacyNativeAnyW78DependencyRead `json:"dependencies"`
}

func legacyNativeAnyFieldW84RunGroup(test *testing.T, group string) {
	test.Helper()
	fixtures := legacyNativeAnyFieldW84Fixtures()
	if len(fixtures) != 12 {
		test.Fatal("finite twelve-process actual Any-field schedule changed", len(fixtures))
	}
	cuts := 0
	for _, fixture := range fixtures {
		if fixture.group != group {
			continue
		}
		cuts++
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, original := legacyNativeAnyFieldW84Capture(test, fixture, storage)
			pinned := legacyNativeAnyFieldW84PinnedDependencies(test, request, storage)
			initial := legacyNativeAnyFieldW84StateOf(test, original)
			_, failure := legacyNativeAnyFieldW84Continue(test, original)
			final := legacyNativeAnyFieldW84StateOf(test, original)
			legacyNativeAnyFieldW84Independent(test, fixture, original, failure)
			legacyNativeAnyW78JSON(test, filepath.Join(storage.directory, "request.json"), request)
			current, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(current, os.Args[0], "-test.run=^TestLegacyNativeAnyFieldW84ChildHelper$", "-test.count=1", "-test.timeout=20s")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeAnyFieldW84Directory+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeAnyFieldW84Directory+"="+storage.directory)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("fresh actual Any-field child failed: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(storage.directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeAnyFieldW84Report
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			if report.PID <= 0 || report.PID == os.Getpid() || report.RestoreSteps <= 0 || report.RestoreSteps > legacyNativeAnyFieldW84CheckpointSteps || report.RecaptureSteps <= 0 || report.RecaptureSteps > legacyNativeAnyFieldW84CheckpointSteps || report.RerestoreSteps <= 0 || report.RerestoreSteps > legacyNativeAnyFieldW84CheckpointSteps || report.ContinuationSteps <= 0 || report.ContinuationSteps > legacyNativeAnyFieldW84SourceSteps || report.Retained <= 0 || report.Retained > legacyNativeAnyFieldW84Budget || report.Reads.Forbidden != 0 || report.Rereads.Forbidden != 0 {
				test.Fatal("missing bounded fresh-process actual Any restore/recapture/continuation", report.PID, report.RestoreSteps, report.RecaptureSteps, report.RerestoreSteps, report.ContinuationSteps, report.Retained)
			}
			if !reflect.DeepEqual(report.Initial, initial) || !reflect.DeepEqual(report.Recaptured, initial) || !reflect.DeepEqual(report.Final, final) || !reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)) {
				test.Fatalf("actual Any parent/child/owner/scanner/bits/error state diverged: initial=%t recaptured=%t final=%t error=%t", reflect.DeepEqual(report.Initial, initial), reflect.DeepEqual(report.Recaptured, initial), reflect.DeepEqual(report.Final, final), reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)))
			}
			if len(report.Reads.Ranges) != 0 || len(report.Rereads.Ranges) == 0 {
				test.Fatal("restore/recapture replayed RAW or native continuation consumed no suffix", report.Reads, report.Rereads)
			}
			for _, read := range report.Rereads.Ranges {
				if read[0] < request.SuffixOffset || read[0]%request.PartBytes != 0 || read[1] != min(request.PartBytes, request.SourceBytes-read[0]) {
					test.Fatal("fresh continuation reread original RAW prefix or a nonmanifest range", read)
				}
			}
			diagnosticReads := 0
			for _, dependency := range report.Dependencies {
				declared, present := pinned[dependency.Hash]
				if !present || declared.Bytes != dependency.Bytes || !dependency.Restoring && declared.Prefix && !dependency.Diagnostic {
					test.Fatal("actual number read an undeclared source dependency or semantically replayed accepted prefix", dependency)
				}
				if !dependency.Restoring && dependency.Diagnostic {
					diagnosticReads++
				}
				if fixture.cut == "diagnostic" && dependency.Hash == request.Parts[0].Hash {
					test.Fatal("already-emitted full diagnostic prefix was reread instead of restored scalar state")
				}
			}
			if fixture.name == "outer-giant-number-future-full-error" && diagnosticReads == 0 {
				test.Fatal("outer giant float did not resume its future Full diagnostic from declared immutable CAS")
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
			test.Logf("actual Any-field child PID=%d restore=%d recapture=%d rerestore=%d continue=%d suffix=%d", report.PID, report.RestoreSteps, report.RecaptureSteps, report.RerestoreSteps, report.ContinuationSteps, request.SuffixOffset)
		})
	}
	if cuts != 3 {
		test.Fatal("finite three-cut actual Any-field parent group schedule changed", group, cuts)
	}
	test.Log(legacyNativeAnyFieldW84Unfinished)
}

func TestLegacyNativeAnyFieldCheckpointW84InputProcessContinuation(test *testing.T) {
	legacyNativeAnyFieldW84RunGroup(test, "input")
}

func TestLegacyNativeAnyFieldCheckpointW84OutputProcessContinuation(test *testing.T) {
	legacyNativeAnyFieldW84RunGroup(test, "output")
}

func TestLegacyNativeAnyFieldCheckpointW84CacheProcessContinuation(test *testing.T) {
	legacyNativeAnyFieldW84RunGroup(test, "cache")
}

func TestLegacyNativeAnyFieldCheckpointW84FloatProcessContinuation(test *testing.T) {
	legacyNativeAnyFieldW84RunGroup(test, "float")
}

func TestLegacyNativeAnyFieldW84ChildHelper(test *testing.T) {
	directory := os.Getenv(legacyNativeAnyFieldW84Directory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeAnyW78Request
	if err := json.Unmarshal(body, &request); err != nil || len(request.Parts) == 0 || len(request.Parts) > 4 || request.SourceBytes <= 0 || request.SourceBytes > 4*LegacyNormalizationPartBytes {
		test.Fatal("actual Any-field child escaped finite four-part geometry", err)
	}
	report := legacyNativeAnyFieldW84Report{PID: os.Getpid()}
	source := legacyNativeAnyW78Source(test, request, &report.Reads)
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request)
	restored, steps := legacyNativeAnyFieldW84Restore(test, request, source, storage)
	report.RestoreSteps, report.Initial = steps, legacyNativeAnyFieldW84StateOf(test, restored)
	root, steps := legacyNativeAnyFieldW84CaptureRoot(test, restored, request, storage)
	report.RecaptureSteps = steps
	if report.Reads.Forbidden != 0 || len(report.Reads.Ranges) != 0 {
		test.Fatal("actual Any restore or immediate pre-step recapture reread original RAW")
	}
	recaptured := request
	recaptured.Root = root
	fresh := legacyNativeAnyW78Source(test, recaptured, &report.Rereads)
	if fresh.authority == source.authority {
		test.Fatal("immediate recapture restore reused a stale rather than fresh caller grant")
	}
	continued, steps := legacyNativeAnyFieldW84Restore(test, recaptured, fresh, storage)
	report.RerestoreSteps, report.Recaptured = steps, legacyNativeAnyFieldW84StateOf(test, continued)
	if report.Rereads.Forbidden != 0 || len(report.Rereads.Ranges) != 0 || !reflect.DeepEqual(report.Initial, report.Recaptured) {
		test.Fatal("immediate pre-step recapture changed native semantics or replayed RAW")
	}
	retained, err := continued.Object.retained()
	if err != nil || retained <= 0 || retained > legacyNativeAnyFieldW84Budget {
		test.Fatal("restored actual Event/Any ownership escaped aggregate admission", retained, err)
	}
	report.Retained = retained
	storage.restoring = false
	storage.diagnostic = func() bool {
		number := continued.Object.number
		if number == nil && continued.Object.containerActive && continued.Object.field >= 0 && continued.Object.field < len(continued.Object.containers) && continued.Object.containers[continued.Object.field] != nil {
			number = continued.Object.containers[continued.Object.field].number
		}
		return number != nil && number.rejected && number.errorText != nil
	}
	var failure error
	report.ContinuationSteps, failure = legacyNativeAnyFieldW84Continue(test, continued)
	report.Final, report.Error = legacyNativeAnyFieldW84StateOf(test, continued), legacyNativeObjectW79ErrorState(failure)
	report.Dependencies = storage.dependencies
	if storage.inputWrites != 0 {
		test.Fatal("fresh actual Any-field child rewrote original immutable source CAS")
	}
	legacyNativeAnyW78JSON(test, filepath.Join(directory, "report.json"), report)
}
