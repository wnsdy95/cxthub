package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

const legacyNativeBlockListW81Directory = "CXTHUB_W81_NATIVE_BLOCK_LIST_DIRECTORY"
const legacyNativeBlockListW81Budget int64 = 64 << 20
const legacyNativeBlockListW81SourceSteps = 4096
const legacyNativeBlockListW81CheckpointSteps = 65536

type legacyNativeBlockListW81Fixture struct {
	name      string
	raw       string
	prior     []ContentBlock
	before    []string
	cut       string
	depth     int
	paged     bool
	inherited bool
}

func legacyNativeBlockListW81Prior(length, capacity int) []ContentBlock {
	values := make([]ContentBlock, capacity)
	for index := range values {
		values[index] = ContentBlock{Type: fmt.Sprintf("old-type-%d", index), Text: fmt.Sprintf("old-text-%d", index)}
	}
	return values[:length]
}

func legacyNativeBlockListW81Clone(values []ContentBlock) []ContentBlock {
	if values == nil {
		return nil
	}
	result := make([]ContentBlock, cap(values))
	copy(result, values[:cap(values)])
	return result[:len(values)]
}

func legacyNativeBlockListW81Fixtures() []legacyNativeBlockListW81Fixture {
	width := LegacyNormalizationPartBytes
	keyPrefix := `{"blocks":[{"type":"text",`
	migrationPrefix := `[{"type":"text",`
	duplicate := `[{"text":"new-head"}]`
	return []legacyNativeBlockListW81Fixture{
		{name: "ordinary-partial-seed", raw: `[null,{"text":"updated"},{}]`, prior: legacyNativeBlockListW81Prior(1, 4), cut: "seed"},
		{name: "ordinary-growth-copy", raw: `[` + strings.Repeat(`{},`, 600) + `{}]`, prior: legacyNativeBlockListW81Prior(600, 600), cut: "copy"},
		{name: "ordinary-hidden-duplicate-null-scan", raw: `[` + strings.Repeat(" ", width-3-len(duplicate)) + `null,{"type":null,"text":null},{}]`, prior: legacyNativeBlockListW81Prior(2, 4), before: []string{duplicate}, cut: "null-scan"},
		{name: "ordinary-whole-null", raw: `null`, prior: legacyNativeBlockListW81Prior(1, 3), cut: "null-list"},
		{name: "depth-10000-native-null-child-handoff", raw: `[null]`, prior: legacyNativeBlockListW81Prior(1, 3), cut: "handoff", depth: 10000},
		{name: "inherited-native-child-key", raw: keyPrefix + strings.Repeat(" ", width-len(keyPrefix)-3) + `"text":"later"}],"id":"outside-list"}`, cut: "key", inherited: true},
		{name: "child-ignored-nested-object-key-frame", raw: `[{"ignored":{"` + strings.Repeat("k", 2*width) + `":0},"type":"text","text":"later"}]`, cut: "ignored-key"},
		{name: "child-unicode-scalar-prefix", raw: `[{"type":"text","text":"` + strings.Repeat("a", 2*width) + "\\n\\uD83D\\uDE42\\u0000\ud55c\"}]", cut: "scalar"},
		{name: "child-expanded-scalar-migration", raw: migrationPrefix + strings.Repeat(" ", width-len(migrationPrefix)) + `"text":"` + strings.Repeat("\xff", 2*width) + "\\uD83D\\uDE42\ud55c\"}]", cut: "migration"},
		{name: "paged-hidden-reused-tail", raw: `[null,{"type":null,"text":"new-tail"},{}]`, before: []string{legacyNativeBlockPagedSemanticsSeed(2*legacyCanonicalPageValues + 3), `[{"text":"short-head"}]`}, cut: "after-one", paged: true},
		{name: "paged-growth-second-page-child-type-error", raw: `[` + strings.Repeat(`{},`, 2*legacyCanonicalPageValues+2) + `{"text":false}]`, before: []string{legacyNativeBlockPagedSemanticsSeed(legacyCanonicalInlineValues)}, cut: "page-growth", paged: true},
		{name: "paged-empty-present-data", raw: `[]`, cut: "first", paged: true},
	}
}

func legacyNativeBlockListW81Stop(list *legacyNativeBlockList, cut string) bool {
	if list == nil {
		return false
	}
	switch cut {
	case "seed":
		return !list.initialized && list.seeded > 0 && list.seeded < cap(list.decoded)
	case "copy":
		return list.previous != nil && list.copied > 0 && list.copied < len(list.previous)
	case "null-scan":
		return list.phase == legacyNativeBlocksScan && list.literal > 0 && list.literal < len("null")
	case "null-list":
		return list.phase == legacyNativeBlocksNull && list.literal == 2
	case "first":
		return list.phase == legacyNativeBlocksFirst
	case "after-one":
		return list.phase == legacyNativeBlocksAfter && list.count == 1
	case "page-growth":
		return list.phase == legacyNativeBlocksAfter && list.count == legacyCanonicalPageValues+1
	case "handoff":
		return list.phase == legacyNativeBlocksApply && list.object != nil && list.object.phase == legacyNativeObjectStart && list.frame == nil
	}
	child := list.object
	if child == nil || list.phase != legacyNativeBlocksApply {
		return false
	}
	switch cut {
	case "key":
		return child.phase == legacyNativeObjectKey && child.keyStarted && len(child.key) > 0 && child.position == LegacyNormalizationPartBytes
	case "ignored-key":
		return child.phase == legacyNativeObjectValue && child.valueStarted && child.field < 0 && child.position == LegacyNormalizationPartBytes && list.frame != nil && list.frame.key && !list.frame.complete
	case "scalar":
		return child.scalar != nil && child.scalar.position == LegacyNormalizationPartBytes && child.scalar.text != nil && child.scalar.text.Len() > 0
	case "migration":
		return child.scalar != nil && child.scalar.previous != nil
	default:
		return false
	}
}

func legacyNativeBlockListW81Suffix(list *legacyNativeBlockList, width int) int {
	position := list.position
	if list.object != nil {
		return legacyNativeObjectW79Suffix(list.object, width)
	}
	if list.phase == legacyNativeBlocksScan {
		position = min(position, list.childStart)
	}
	return position / width * width
}

type legacyNativeBlockListW81Array struct {
	Nil      bool           `json:"nil"`
	Length   int            `json:"length"`
	Capacity int            `json:"capacity"`
	Values   []ContentBlock `json:"values"`
}

func legacyNativeBlockListW81ArrayOf(values []ContentBlock) legacyNativeBlockListW81Array {
	state := legacyNativeBlockListW81Array{Nil: values == nil, Length: len(values), Capacity: cap(values)}
	if values != nil {
		state.Values = make([]ContentBlock, cap(values))
		copy(state.Values, values[:cap(values)])
	}
	return state
}

type legacyNativeBlockListW81State struct {
	Control  []int                         `json:"control"`
	Flags    []bool                        `json:"flags"`
	Decoded  legacyNativeBlockListW81Array `json:"decoded"`
	Previous legacyNativeBlockListW81Array `json:"previous"`
	Data     []ContentBlock                `json:"data"`
	Block    ContentBlock                  `json:"block"`
	Frame    []byte                        `json:"frame"`
	Child    *legacyNativeObjectW79State   `json:"child"`
	Aliases  []bool                        `json:"aliases"`
}

func legacyNativeBlockListW81TextAlias(left, right string) bool {
	return left != "" && right != "" && unsafe.StringData(left) == unsafe.StringData(right)
}

func legacyNativeBlockListW81StateOf(test *testing.T, list *legacyNativeBlockList) legacyNativeBlockListW81State {
	test.Helper()
	state := legacyNativeBlockListW81State{
		Control: []int{list.position, list.end, list.parentDepth, list.maximum, list.count, list.copied, list.childStart, list.literal, int(list.phase)},
		Flags:   []bool{list.prefix, list.paged, list.data != nil, list.frame != nil},
		Decoded: legacyNativeBlockListW81ArrayOf(list.decoded), Previous: legacyNativeBlockListW81ArrayOf(list.previous), Block: list.block,
	}
	var scanner legacyJSONValueScanner
	if list.frame != nil {
		scanner = *list.frame
		var err error
		state.Frame, err = list.frame.checkpoint(context.Background())
		if err != nil {
			test.Fatal("actual list scanner cannot be observed", err)
		}
	}
	if list.data != nil {
		if !list.data.records.valid() || list.data.count < 0 || list.data.count > list.data.records.length() {
			test.Fatal("actual paged list has invalid physical record history")
		}
		state.Control = append(state.Control, list.data.count, list.data.records.length())
		state.Data = make([]ContentBlock, list.data.records.length())
		for index := range state.Data {
			record := list.data.records.at(index)
			if record == nil || index < list.data.count && list.data.at(index) != record || index >= list.data.count && list.data.at(index) != nil {
				test.Fatal("paged list lost its record alias or exposed hidden tail", index)
			}
			state.Data[index] = record.block
		}
	}
	if list.previous != nil {
		for index := 0; index < list.copied; index++ {
			state.Aliases = append(state.Aliases,
				legacyNativeBlockListW81TextAlias(list.decoded[index].Type, list.previous[index].Type),
				legacyNativeBlockListW81TextAlias(list.decoded[index].Text, list.previous[index].Text))
		}
	}
	if child := list.object; child != nil {
		if child.metadata != &legacyContentBlockNativeFields || child.destination.Interface() != &list.block {
			test.Fatal("native child destination is not the actual list block")
		}
		childState := legacyNativeObjectW79StateOf(test, child, &scanner)
		state.Child = &childState
		record, err := list.blockRecord()
		if err != nil {
			test.Fatal("active child lost current prior record", err)
		}
		state.Aliases = append(state.Aliases,
			legacyNativeBlockListW81TextAlias(record.block.Type, list.block.Type),
			legacyNativeBlockListW81TextAlias(record.block.Text, list.block.Text))
	}
	return state
}

func legacyNativeBlockListW81Step(test *testing.T, current context.Context, list *legacyNativeBlockList, source LegacyByteSource, onePage bool) (bool, error) {
	test.Helper()
	input := &legacyNormalizationInput{ctx: current, source: source}
	if onePage {
		input.loads = 1
	}
	complete, failure := list.step(current, input)
	if input.failure != nil && !errors.Is(input.failure, context.Canceled) || input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.fields < 0 || input.fields > LegacyNormalizationStepEvents*len(legacyContentBlockNativeFields.fields)+1 {
		test.Fatal("actual block list escaped its shared step quantum", input.failure, input.workBytes, input.loads, input.fields)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, failure
}

func legacyNativeBlockListW81Continue(test *testing.T, list *legacyNativeBlockList, source LegacyByteSource) (int, error) {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeBlockListW81SourceSteps; attempt++ {
		current, cancel := context.WithCancel(context.Background())
		complete, failure := legacyNativeBlockListW81Step(test, current, list, source, false)
		cancel()
		if complete || failure != nil {
			if complete && (list.phase != legacyNativeBlocksDone || !list.prefix && list.position != list.end || list.object != nil || list.block != (ContentBlock{})) {
				test.Fatal("actual block list completed without handing off child state")
			}
			return attempt, failure
		}
	}
	test.Fatal("actual block list exceeded finite source step schedule")
	return 0, ErrIntegrity
}

func legacyNativeBlockListW81Drain(test *testing.T, work legacyNativeAnyW78Work, storage *legacyNativeAnyW78CAS) int {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeBlockListW81CheckpointSteps; attempt++ {
		phase, index := -1, -1
		switch value := work.(type) {
		case *legacyNativeBlockListCheckpoint:
			phase, index = int(value.phase), value.index
		case *legacyNativeBlockListCheckpointRestore:
			phase, index = int(value.phase), value.index
		}
		storage.quantum = 0
		current, cancel := context.WithCancel(context.Background())
		ready, failure := work.step(current)
		cancel()
		retained, heapErr := work.retainedBytes()
		if failure != nil || heapErr != nil || retained < 0 || retained > legacyNativeBlockListW81Budget || storage.quantum > ConversationManifestChunkBytes {
			test.Fatal("block checkpoint escaped bounded step admission", attempt, failure, retained, heapErr, storage.quantum, "phase", phase, "row", index)
		}
		if ready {
			return attempt
		}
	}
	test.Fatal("block checkpoint exceeded finite 65536-step schedule")
	return 0
}

func legacyNativeBlockListW81Inherited(test *testing.T, source LegacyByteSource, paged bool) *legacyNativeBlockList {
	test.Helper()
	value := &wire{}
	parent, err := newLegacyNativeObject(reflect.ValueOf(value), &legacyEventNativeFields, 1, false)
	if err != nil {
		test.Fatal(err)
	}
	parent.pagedEvents = paged
	parent.start(0, source.Bytes)
	index := parent.metadata.match("blocks")
	var scanner legacyJSONValueScanner
	stop := func() bool { return parent.blockActive && len(parent.blocks) > index && parent.blocks[index] != nil }
	for attempt := 0; attempt < legacyNativeBlockListW81SourceSteps && !stop(); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		current := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: stop}
		complete, failure := legacyNativeObjectW79Step(test, current, parent, &scanner, source, legacyNativeNumberDiagnosticsFull, true)
		cancel()
		if complete || failure != nil && !errors.Is(failure, context.Canceled) {
			test.Fatal("actual native parent failed before BlockList handoff", complete, failure)
		}
	}
	if !stop() || parent.blocks[index].frame != &scanner || parent.blocks[index].ownedFrame || !parent.blocks[index].prefix {
		test.Fatal("inherited fixture bypassed original native scanner/list dispatch")
	}
	return parent.blocks[index]
}

func legacyNativeBlockListW81Raw(fixture legacyNativeBlockListW81Fixture) string {
	segments := append(append([]string(nil), fixture.before...), fixture.raw)
	raw := strings.Join(segments, " ")
	if fixture.depth > 0 {
		return strings.Repeat("[", fixture.depth-1) + raw + strings.Repeat("]", fixture.depth-1)
	}
	return raw
}

func legacyNativeBlockListW81Prepare(test *testing.T, fixture legacyNativeBlockListW81Fixture) (legacyNativeAnyW78Request, *legacyNativeBlockList, LegacyByteSource) {
	test.Helper()
	segments := append(append([]string(nil), fixture.before...), fixture.raw)
	raw := legacyNativeBlockListW81Raw(fixture)
	if fixture.depth > 0 && (len(fixture.before) != 0 || !json.Valid([]byte(raw))) {
		test.Fatal("depth handoff fixture is not an actual valid nested source")
	}
	request, source := legacyNativeAnyW78FullSource(test, raw)
	request.Binding = HashContent([]byte("W81 actual BlockList " + fixture.name))
	if len(request.Parts) > 4 {
		test.Fatal("block fixture exceeded finite four-source-part bound")
	}
	if fixture.inherited {
		return request, legacyNativeBlockListW81Inherited(test, source, fixture.paged), source
	}
	prior := legacyNativeBlockListW81Clone(fixture.prior)
	heap := legacyNativeBlocksTestHeap(test, prior)
	offset := max(0, fixture.depth-1)
	list, err := newLegacyNativeBlockList(prior, heap, offset, offset+len(segments[0]))
	if err != nil {
		test.Fatal("actual block constructor rejected valid prior", err)
	}
	list.paged = fixture.paged
	if fixture.depth > 0 {
		list.parentDepth = fixture.depth
	}
	for index, segment := range segments {
		if index != 0 {
			if err := list.start(offset, offset+len(segment)); err != nil {
				test.Fatal("actual list rejected duplicate restart", err)
			}
		}
		if index == len(segments)-1 {
			break
		}
		if _, failure := legacyNativeBlockListW81Continue(test, list, source); failure != nil {
			test.Fatal("actual duplicate history failed", failure)
		}
		offset += len(segment) + 1
	}
	return request, list, source
}

func legacyNativeBlockListW81Capture(test *testing.T, fixture legacyNativeBlockListW81Fixture, storage *legacyNativeAnyW78CAS) (legacyNativeAnyW78Request, *legacyNativeBlockList, LegacyByteSource) {
	test.Helper()
	request, list, source := legacyNativeBlockListW81Prepare(test, fixture)
	storage.preload(test, request)
	for attempt := 0; attempt < legacyNativeBlockListW81SourceSteps && !legacyNativeBlockListW81Stop(list, fixture.cut); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		current := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeBlockListW81Stop(list, fixture.cut) }}
		complete, failure := legacyNativeBlockListW81Step(test, current, list, source, true)
		cancel()
		if complete || failure != nil && !errors.Is(failure, context.Canceled) {
			test.Fatal("actual list terminated before assigned cut", fixture.name, list.position, failure)
		}
	}
	if !legacyNativeBlockListW81Stop(list, fixture.cut) {
		test.Fatal("actual list never reached assigned cut", fixture.name, list.position)
	}
	before := legacyNativeBlockListW81StateOf(test, list)
	initialized, seeded, owned, previousOwned, ownedFrame := list.initialized, list.seeded, list.owned, list.previousOwned, list.ownedFrame
	reads := 0
	captureSource := source
	captureSource.Read = func(current context.Context, offset, length int) ([]byte, error) {
		reads++
		return source.Read(current, offset, length)
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeBlockListCheckpoint(constructor, list, request.Binding, captureSource, storage.io(), legacyNativeBlockListW81Budget)
	cancel()
	if err != nil {
		test.Fatal("actual BlockList checkpoint construction", err)
	}
	defer work.close()
	legacyNativeBlockListW81Drain(test, work, storage)
	request.Root, err = work.take(context.Background())
	if err != nil || !request.Root.valid() {
		test.Fatal("actual BlockList checkpoint did not publish root", err)
	}
	legacyNativeAnyW78Released(test, work)
	if reads != 0 || storage.inputWrites != 0 || !reflect.DeepEqual(before, legacyNativeBlockListW81StateOf(test, list)) || initialized != list.initialized || seeded != list.seeded || owned != list.owned || previousOwned != list.previousOwned || ownedFrame != list.ownedFrame {
		test.Fatal("capture read RAW or mutated quiescent list", reads, storage.inputWrites)
	}
	request.SuffixOffset = legacyNativeBlockListW81Suffix(list, request.PartBytes)
	if fixture.cut == "null-scan" && request.SuffixOffset >= list.position/request.PartBytes*request.PartBytes {
		test.Fatal("literal framing cursor was confused with semantic token start")
	}
	if fixture.cut == "ignored-key" && (list.frame == nil || !list.frame.key || request.SuffixOffset != list.object.position/request.PartBytes*request.PartBytes) {
		test.Fatal("ignored nested key did not preserve its scanner-only future frontier")
	}
	if fixture.depth > 0 && (list.object == nil || list.object.parentDepth != fixture.depth+1 || list.frame != nil || list.object.phase != legacyNativeObjectStart) {
		test.Fatal("deep null did not reach original non-nesting native child handoff")
	}
	if fixture.cut == "scalar" && request.SuffixOffset <= list.childStart/request.PartBytes*request.PartBytes {
		test.Fatal("active child scalar suffix was pinned to stale list position")
	}
	legacyNativeEnvelopeW80Immutable(test, storage, request, string(request.Suffix))
	request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
	return request, list, source
}

func legacyNativeBlockListW81Independent(test *testing.T, fixture legacyNativeBlockListW81Fixture, actual *legacyNativeBlockList, failure error) {
	test.Helper()
	if fixture.paged {
		ordinary := fixture
		ordinary.paged = false
		_, reference, source := legacyNativeBlockListW81Prepare(test, ordinary)
		_, expectedError := legacyNativeBlockListW81Continue(test, reference, source)
		expectedCount := len(reference.values())
		if expectedError != nil {
			expectedCount = reference.count
		}
		if !reflect.DeepEqual(legacyNativeObjectW79ErrorState(failure), legacyNativeObjectW79ErrorState(expectedError)) || (actual.data == nil) != (reference.values() == nil) || actual.data.length() != expectedCount {
			test.Fatal("paged continuation changed same-native logical values or error")
		}
		if actual.data != nil {
			backing := reference.values()[:cap(reference.values())]
			if actual.data.records.length() > len(backing) {
				test.Fatal("paged records exceed ordinary native highwater backing")
			}
			for index := 0; index < actual.data.records.length(); index++ {
				if actual.data.records.at(index).block != backing[index] {
					test.Fatal("paged materialized record differs from same native algorithm", index)
				}
			}
		}
		return
	}
	expected := legacyNativeBlockListW81Clone(fixture.prior)
	for _, segment := range fixture.before {
		if err := json.Unmarshal([]byte(segment), &expected); err != nil {
			test.Fatal("independent duplicate history invalid", err)
		}
	}
	var expectedError error
	if fixture.inherited {
		var event CIREvent
		expectedError = json.Unmarshal([]byte(fixture.raw), &event)
		expected = event.Blocks
	} else {
		expectedError = json.Unmarshal([]byte(fixture.raw), &expected)
	}
	if !reflect.DeepEqual(actual.values(), expected) || !reflect.DeepEqual(legacyNativeObjectW79ErrorState(failure), legacyNativeObjectW79ErrorState(expectedError)) {
		test.Fatal("ordinary continuation differs from independent encoding/json", failure, expectedError)
	}
}

type legacyNativeBlockListW81Report struct {
	PID               int                                `json:"pid"`
	RestoreSteps      int                                `json:"restore_steps"`
	ContinuationSteps int                                `json:"continuation_steps"`
	Retained          int64                              `json:"retained"`
	Initial           legacyNativeBlockListW81State      `json:"initial"`
	Final             legacyNativeBlockListW81State      `json:"final"`
	Error             legacyNativeObjectW79Error         `json:"error"`
	Reads             legacyNativeAnyW78Reads            `json:"reads"`
	Dependencies      []legacyNativeAnyW78DependencyRead `json:"dependencies"`
}

func TestLegacyNativeBlockListCheckpointW81ProcessContinuation(test *testing.T) {
	fixtures := legacyNativeBlockListW81Fixtures()
	if len(fixtures) != 12 {
		test.Fatal("finite twelve-child BlockList schedule changed", len(fixtures))
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, list, source := legacyNativeBlockListW81Capture(test, fixture, storage)
			initial := legacyNativeBlockListW81StateOf(test, list)
			_, failure := legacyNativeBlockListW81Continue(test, list, source)
			final := legacyNativeBlockListW81StateOf(test, list)
			legacyNativeBlockListW81Independent(test, fixture, list, failure)
			legacyNativeAnyW78JSON(test, filepath.Join(storage.directory, "request.json"), request)
			current, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(current, os.Args[0], "-test.run=^TestLegacyNativeBlockListW81ChildHelper$", "-test.count=1", "-test.timeout=20s")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeBlockListW81Directory+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeBlockListW81Directory+"="+storage.directory)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("fresh BlockList child failed: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(storage.directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeBlockListW81Report
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			if report.PID <= 0 || report.PID == os.Getpid() || report.RestoreSteps <= 0 || report.RestoreSteps > legacyNativeBlockListW81CheckpointSteps || report.ContinuationSteps <= 0 || report.ContinuationSteps > legacyNativeBlockListW81SourceSteps || report.Retained <= 0 || report.Retained > legacyNativeBlockListW81Budget || report.Reads.Forbidden != 0 || len(report.Reads.Ranges) == 0 {
				test.Fatal("missing actual fresh-process suffix-only BlockList continuation", report.PID, report.RestoreSteps, report.ContinuationSteps, report.Reads)
			}
			if !reflect.DeepEqual(report.Initial, initial) || !reflect.DeepEqual(report.Final, final) || !reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)) {
				test.Fatalf("BlockList controls/content/hidden-tail/alias/scanner/error diverged: initial=%v final=%v error=%v", reflect.DeepEqual(report.Initial, initial), reflect.DeepEqual(report.Final, final), reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)))
			}
			for _, read := range report.Reads.Ranges {
				if read[0] < request.SuffixOffset || read[0]%request.PartBytes != 0 || read[1] != min(request.PartBytes, request.SourceBytes-read[0]) {
					test.Fatal("fresh BlockList child read consumed prefix or nonmanifest range", read)
				}
			}
			for _, dependency := range report.Dependencies {
				if !dependency.Restoring {
					test.Fatal("BlockList continued semantics through original CAS prefix", dependency)
				}
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, legacyNativeBlockListW81Raw(fixture))
		})
	}
	test.Log("Actual BlockList continuation only; full Event destination/presence, replacement aggregates, encoder/four spools/sort/proof, atomic stage/worker/current-authority/CAS/GC and full/performance gates remain unfinished.")
}

func TestLegacyNativeBlockListCheckpointW81DepthBoundary(test *testing.T) {
	for _, edge := range []struct {
		depth int
		raw   string
	}{
		{depth: 9999, raw: `[{}]`},
		{depth: 9999, raw: `[null]`},
		{depth: 10000, raw: `[null]`},
	} {
		test.Run(fmt.Sprintf("actual-child-depth-%d-%s", edge.depth+1, edge.raw), func(test *testing.T) {
			fixture := legacyNativeBlockListW81Fixture{name: fmt.Sprintf("depth-%d-%s", edge.depth, edge.raw), raw: edge.raw, prior: legacyNativeBlockListW81Prior(1, 2), cut: "handoff", depth: edge.depth}
			storage := legacyNativeAnyW78Store(test)
			request, list, source := legacyNativeBlockListW81Capture(test, fixture, storage)
			initial := legacyNativeBlockListW81StateOf(test, list)
			_, failure := legacyNativeBlockListW81Continue(test, list, source)
			if failure != nil {
				test.Fatal("valid depth-boundary native child did not continue", failure)
			}
			legacyNativeBlockListW81Independent(test, fixture, list, failure)
			final := legacyNativeBlockListW81StateOf(test, list)
			var reads legacyNativeAnyW78Reads
			fresh := legacyNativeAnyW78Source(test, request, &reads)
			work, err := newLegacyNativeBlockListCheckpointRestore(context.Background(), request.Root, storage.io(), fresh, request.Binding, legacyNativeBlockListW81Budget)
			if err != nil {
				test.Fatal("valid depth-boundary BlockList restore rejected", err)
			}
			defer work.close()
			legacyNativeBlockListW81Drain(test, work, storage)
			restored, err := work.take(context.Background())
			if err != nil || restored.List == nil || len(reads.Ranges) != 0 || restored.Source.authority != fresh.authority || !reflect.DeepEqual(initial, legacyNativeBlockListW81StateOf(test, restored.List)) {
				test.Fatal("depth-boundary restore changed native child/frame/aliases or read RAW", err)
			}
			legacyNativeAnyW78Released(test, work)
			_, failure = legacyNativeBlockListW81Continue(test, restored.List, restored.Source)
			if failure != nil || reads.Forbidden != 0 || !reflect.DeepEqual(final, legacyNativeBlockListW81StateOf(test, restored.List)) {
				test.Fatal("depth-boundary restored native child diverged", failure)
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, legacyNativeBlockListW81Raw(fixture))
		})
	}
}

func TestLegacyNativeBlockListW81ChildHelper(test *testing.T) {
	directory := os.Getenv(legacyNativeBlockListW81Directory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeAnyW78Request
	if err := json.Unmarshal(body, &request); err != nil || len(request.Parts) > 4 || request.SourceBytes > 4*LegacyNormalizationPartBytes {
		test.Fatal("child BlockList request exceeds finite four-part fixture", err)
	}
	report := legacyNativeBlockListW81Report{PID: os.Getpid()}
	source := legacyNativeAnyW78Source(test, request, &report.Reads)
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request)
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeBlockListCheckpointRestore(constructor, request.Root, storage.io(), source, request.Binding, legacyNativeBlockListW81Budget)
	cancel()
	if err != nil {
		test.Fatal("fresh actual BlockList restore construction", err)
	}
	defer work.close()
	report.RestoreSteps = legacyNativeBlockListW81Drain(test, work, storage)
	restored, err := work.take(context.Background())
	if err != nil || restored.List == nil {
		test.Fatal("fresh child did not restore actual native BlockList", err)
	}
	legacyNativeAnyW78Released(test, work)
	if restored.Source.authority == 0 || restored.Source.authority != source.authority || restored.Source.Read == nil || restored.Source.Bytes != source.Bytes || restored.Source.PartBytes != source.PartBytes || !reflect.DeepEqual(restored.Source.Parts, source.Parts) {
		test.Fatal("BlockList restore reused saved authority instead of fresh caller source")
	}
	if request.SuffixOffset != legacyNativeBlockListW81Suffix(restored.List, request.PartBytes) || report.Reads.Forbidden != 0 || len(report.Reads.Ranges) != 0 || storage.inputWrites != 0 {
		test.Fatal("BlockList restore replayed RAW or lost semantic future floor")
	}
	var retained docRangeHeaderHeapEstimate
	restored.List.retained(&retained)
	if retained.err != nil || retained.bytes <= 0 || retained.bytes > legacyNativeBlockListW81Budget || restored.List.frame != nil && !restored.List.ownedFrame || !restored.List.initialized {
		test.Fatal("restored list/frame storage not currently admitted", retained)
	}
	report.Retained = retained.bytes
	report.Initial = legacyNativeBlockListW81StateOf(test, restored.List)
	storage.restoring = false
	var failure error
	report.ContinuationSteps, failure = legacyNativeBlockListW81Continue(test, restored.List, restored.Source)
	report.Final = legacyNativeBlockListW81StateOf(test, restored.List)
	report.Error = legacyNativeObjectW79ErrorState(failure)
	report.Dependencies = storage.dependencies
	if storage.inputWrites != 0 {
		test.Fatal("fresh BlockList child rewrote original input CAS")
	}
	legacyNativeAnyW78JSON(test, filepath.Join(directory, "report.json"), report)
}
