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

const legacyNativeEventW82Directory = "CXTHUB_W82_NATIVE_EVENT_DIRECTORY"
const legacyNativeEventW82Budget int64 = 64 << 20
const legacyNativeEventW82SourceSteps = 4096
const legacyNativeEventW82CheckpointSteps = 65536
const legacyNativeEventW82Blocked = "Unfinished: nonstring Input/Output dynamic containers and maps; Replacement continuation; native container/event caches; atomic stage, incremental encoder, four spools, sort, proof, worker, full and performance gates. This outer Event component does not complete the full goal."

type legacyNativeEventW82Fixture struct {
	name      string
	group     string
	raw       string
	cut       string
	prior     []ContentBlock
	paged     bool
	boolAlias bool
}

func legacyNativeEventW82Fixtures() []legacyNativeEventW82Fixture {
	width := LegacyNormalizationPartBytes
	keyPrefix := `{"blocks":[{"type":"text",`
	longText := strings.Repeat("a", 2*width) + "\\n\\uD83D\\uDE42\\u0000\ud55c"
	pageSeed := legacyNativeBlockPagedSemanticsSeed(2*legacyCanonicalPageValues + 3)
	fixtures := []legacyNativeEventW82Fixture{
		{name: "pre-block-dispatch-hidden-backing", group: "dispatch", raw: `{"blocks":[null,null,null,null],"id":"after-dispatch"`, cut: "dispatch", prior: legacyNativeBlockListW81Prior(0, 4)},
		{name: "shared-native-child-key", group: "dispatch", raw: keyPrefix + strings.Repeat(" ", width-len(keyPrefix)-3) + `"text":"after-key"}],"id":"after-child"`, cut: "key"},
		{name: "child-scalar-independent-destination-after-growth", group: "dispatch", raw: `{"blocks":[{},{},{},{},{"type":"text","text":"` + longText + `"}],"id":"after-scalar"`, cut: "independent-scalar", prior: legacyNativeBlockListW81Prior(2, 4)},
		{name: "shared-child-ignored-deep-key", group: "dispatch", raw: `{"blocks":[{"ignored":{"inside":[{"` + strings.Repeat("k", 2*width) + `":0}]},"type":"text","text":"after-deep-key"}],"id":"after-ignored"`, cut: "ignored-key"},
		{name: "duplicate-hidden-decoded-alias", group: "ownership", raw: `{"blocks":[{"text":"short-head"}],"id":"between","blocks":[null,{},null,{}],"id":"after-hidden"`, cut: "duplicate", prior: legacyNativeBlockListW81Prior(2, 4)},
		{name: "growth-copy-previous-destination-alias", group: "ownership", raw: `{"blocks":[` + strings.Repeat("null,", 600) + `null],"id":"after-copy"`, cut: "copy", prior: legacyNativeBlockListW81Prior(2, 600)},
		{name: "paged-duplicate-hidden-highwater", group: "ownership", raw: `{"blocks":` + pageSeed + `,"blocks":[{"text":"short-head"}],"id":"between","blocks":[null,{"type":null,"text":null},{}],"id":"after-paged"`, cut: "paged-hidden", paged: true},
		{name: "paged-empty-present-cache", group: "ownership", raw: `{"blocks":[],"id":"after-empty"`, cut: "empty", paged: true},
		{name: "inactive-block-cache-then-id-scalar", group: "cache", raw: `{"blocks":[{"text":"fresh"}],"id":"` + longText + `","blocks":[null,null,null,null],"id":null`, cut: "cached-id", prior: legacyNativeBlockListW81Prior(2, 4)},
		{name: "inactive-paged-cache-then-output-scalar", group: "cache", raw: `{"blocks":` + pageSeed + `,"blocks":[{"text":"short-head"}],"output":"` + longText + `","blocks":[null,null],"output":null`, cut: "cached-output", paged: true},
		{name: "bool-null-retention-presence-and-pointer-alias", group: "primitive", raw: `{"compact_summary":null,"agent_message":null,"replacement_complete":null,"agent_author":"","agent_recipient":"","seq":null,"is_error":false,"id":"after-bools","is_error":true,"cross_replayable":false,"output":null,"seq":-17}`, cut: "bool-alias", boolAlias: true},
		{name: "pointer-null-then-present-false-and-true", group: "primitive", raw: `{"is_error":null,"cross_replayable":null,"is_error":false,"cross_replayable":true,"compact_summary":false,"agent_message":false,"replacement_complete":false,"status":"after-pointers","output":null}`, cut: "pointer-null"},
	}
	for index := range fixtures {
		if fixtures[index].group != "primitive" {
			fixtures[index].raw += `,"seq":7,"role":"user","agent_message":false,"agent_author":"","agent_recipient":""}`
		}
	}
	return fixtures
}

func legacyNativeEventW82Initial(fixture legacyNativeEventW82Fixture) *wire {
	errorValue, replayValue := false, true
	value := &wire{
		Kind: EventMessage, ID: "prior-id", TS: "2026-10-29T00:00:00Z", Seq: 23, Role: RoleAssistant,
		Blocks: legacyNativeBlockListW81Clone(fixture.prior), CompactSummary: true, AgentMessage: true,
		AgentAuthor: "prior-author", AgentRecipient: "prior-recipient", ReplacementComplete: true,
		CallID: "prior-call", ToolName: "prior-tool", ProviderToolName: "prior-provider-tool", Status: "prior-status",
		Output: "prior-output", IsError: &errorValue, CrossReplayable: &replayValue, RedactedSummary: "prior-summary",
		fieldPresence: eventFieldLocked | eventFieldReplacement,
	}
	if fixture.boolAlias {
		value.CrossReplayable = value.IsError
	}
	return value
}

func legacyNativeEventW82List(native *legacyNativeObject) *legacyNativeBlockList {
	index := native.metadata.match("blocks")
	if index < 0 || index >= len(native.blocks) {
		return nil
	}
	return native.blocks[index]
}

func legacyNativeEventW82Binding(values, backing []ContentBlock) bool {
	return cap(values) > 0 && cap(backing) > 0 && unsafe.SliceData(values[:cap(values)]) == unsafe.SliceData(backing[:cap(backing)])
}

func legacyNativeEventW82Stop(native *legacyNativeObject, fixture legacyNativeEventW82Fixture) bool {
	list := legacyNativeEventW82List(native)
	switch fixture.cut {
	case "dispatch":
		return !native.blockActive && list == nil && native.phase == legacyNativeObjectApply && native.field == native.metadata.match("blocks") && native.valueStarted && native.valueFirst == '[' && native.position == native.valueStart && native.valueEnd == native.end
	case "copy":
		return native.blockActive && list != nil && list.previous != nil && list.copied > 0 && list.copied < len(list.previous)
	case "duplicate", "paged-hidden":
		return native.blockActive && list != nil && list.phase == legacyNativeBlocksAfter && list.count == 1 && native.valueStart > strings.LastIndex(fixture.raw, `,"blocks":`)
	case "empty":
		return !native.blockActive && list != nil && list.phase == legacyNativeBlocksDone && list.data != nil && list.data.length() == 0 && native.phase == legacyNativeObjectAfter
	case "cached-id", "cached-output":
		field := "id"
		if fixture.cut == "cached-output" {
			field = "output"
		}
		return list != nil && !native.blockActive && list.phase == legacyNativeBlocksDone && native.field == native.metadata.match(field) && native.scalar != nil && native.scalar.text != nil && native.scalar.text.Len() > 0 && native.scalar.position >= LegacyNormalizationPartBytes && native.scalar.position < native.scalar.end
	case "bool-alias":
		return native.phase == legacyNativeObjectAfter && native.field == native.metadata.match("is_error") && native.valueFirst == 'f'
	case "pointer-null":
		return native.phase == legacyNativeObjectAfter && native.field == native.metadata.match("cross_replayable") && native.valueFirst == 'n'
	}
	if !native.blockActive || list == nil {
		return false
	}
	cut := fixture.cut
	if cut == "independent-scalar" {
		value := native.destination.Interface().(*wire)
		if legacyNativeEventW82Binding(value.Blocks, list.decoded) || legacyNativeEventW82Binding(value.Blocks, list.previous) || list.previous != nil {
			return false
		}
		cut = "scalar"
	}
	return legacyNativeBlockListW81Stop(list, cut)
}

func legacyNativeEventW82Suffix(native *legacyNativeObject, width int) int {
	if native.blockActive {
		if list := legacyNativeEventW82List(native); list != nil {
			return legacyNativeBlockListW81Suffix(list, width)
		}
		return -1
	}
	return legacyNativeObjectW79Suffix(native, width)
}

type legacyNativeEventW82State struct {
	Control             []int                          `json:"control"`
	Flags               []bool                         `json:"flags"`
	Key                 []byte                         `json:"key"`
	Scanner             []byte                         `json:"scanner"`
	Value               []byte                         `json:"value"`
	OutputType          string                         `json:"output_type"`
	DestinationPresence eventFieldPresence             `json:"destination_presence"`
	Destination         legacyNativeBlockListW81Array  `json:"destination"`
	Block               *legacyNativeBlockListW81State `json:"block"`
	BlockBinding        uint8                          `json:"block_binding"`
	BooleanAlias        bool                           `json:"boolean_alias"`
	Initialized         []bool                         `json:"initialized"`
	Scalar              []string                       `json:"scalar"`
	Error               legacyNativeObjectW79Error     `json:"error"`
}

func legacyNativeEventW82StateOf(test *testing.T, bundle *legacyNativeObjectRestored) legacyNativeEventW82State {
	test.Helper()
	native := bundle.Object
	value, typed := native.destination.Interface().(*wire)
	if !typed || native.metadata != &legacyEventNativeFields || native.metadata.model != reflect.TypeFor[wire]() || !native.trackPresence || len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) {
		test.Fatal("fixture does not retain actual wire/native Event metadata and heaps")
	}
	if value.Input != nil || value.Replacement != nil || value.Locked != nil || value.ProviderMetadata != nil || native.eventActive || native.nestedActive || native.containerActive || len(native.events) != 0 || len(native.nested) != 0 || len(native.containers) != 0 {
		test.Fatal("unfinished Event aggregate was populated or silently accepted")
	}
	scannerBytes, err := bundle.Scanner.checkpoint(context.Background())
	if err != nil {
		test.Fatal("actual shared Event scanner cannot be observed", err)
	}
	valueBytes, err := json.Marshal(value)
	if err != nil {
		test.Fatal("ordinary wire fields cannot be observed", err)
	}
	state := legacyNativeEventW82State{
		Control: []int{native.parentDepth, native.fieldLimit, native.position, native.end, native.valueStart, native.valueEnd, native.field, int(native.presence), int(native.phase), int(native.valueFirst), cap(native.key), cap(native.raw)},
		Flags:   []bool{native.trackPresence, native.pagedEvents, native.deferTypeErrors, native.skipValue, native.prefix, native.keyStarted, native.keyOverflow, native.valueStarted, native.listActive, native.blockActive, native.blocks != nil},
		Key:     bytes.Clone(native.key), Scanner: scannerBytes, Value: valueBytes, OutputType: fmt.Sprintf("%T", value.Output),
		DestinationPresence: value.fieldPresence, Destination: legacyNativeBlockListW81ArrayOf(value.Blocks),
		BooleanAlias: value.IsError != nil && value.IsError == value.CrossReplayable, Error: legacyNativeObjectW79ErrorState(native.typeError),
	}
	for index, heap := range native.heaps {
		if heap.err != nil || heap.bytes < 0 || native.backingExcess[index] < 0 {
			test.Fatal("actual Event field ownership is invalid", index, heap)
		}
		state.Initialized = append(state.Initialized, heap.bytes != 0)
	}
	if native.scalar != nil {
		state.Scalar = legacyNativeAnyW78ScalarState(test, native.scalar)
	}
	if list := legacyNativeEventW82List(native); list != nil {
		if list.frame == nil || *list.frame != bundle.Scanner || list.paged != native.pagedEvents || list.parentDepth != native.parentDepth+1 {
			test.Fatal("active or cached BlockList lost the actual shared scanner, mode or parent depth")
		}
		block := legacyNativeBlockListW81StateOf(test, list)
		state.Block = &block
		if legacyNativeEventW82Binding(value.Blocks, list.decoded) {
			state.BlockBinding = 1
		} else if legacyNativeEventW82Binding(value.Blocks, list.previous) {
			state.BlockBinding = 2
		} else if native.pagedEvents {
			state.BlockBinding = 3
		}
	}
	return state
}

func legacyNativeEventW82Quiescent(bundle *legacyNativeObjectRestored) string {
	native := bundle.Object
	shape := fmt.Sprintf("%#v|%#v", native.heaps, native.backingExcess)
	if list := legacyNativeEventW82List(native); list != nil {
		shape += fmt.Sprintf("|%#v|%#v|%#v|%#v|%#v|%#v|%#v|%#v|%d|%d|%d|%t|%t|%t|%t", list.borrowed, list.estimate, list.payload, list.added, list.payloads, list.previousHeaps, list.excess, list.previousExtra, list.seeded, list.visibleExcess, list.totalExcess, list.initialized, list.owned, list.previousOwned, list.ownedFrame)
	}
	return shape
}

func legacyNativeEventW82Step(test *testing.T, current context.Context, bundle *legacyNativeObjectRestored, onePage, dispatch bool) (bool, error) {
	test.Helper()
	input := &legacyNormalizationInput{ctx: current, source: bundle.Source, numberDiagnostics: legacyNativeNumberDiagnosticsFull}
	if onePage {
		input.loads = 1
	}
	if dispatch {
		input.workBytes = ConversationManifestChunkBytes - 1
	}
	complete, failure := bundle.Object.step(current, input, &bundle.Scanner)
	maximumFields := max(bundle.Object.fieldActionLimit(), LegacyNormalizationStepEvents*len(legacyContentBlockNativeFields.fields)+1)
	if input.failure != nil && !errors.Is(input.failure, context.Canceled) || input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.fields < 0 || input.fields > maximumFields {
		test.Fatal("actual Event/block continuation escaped its shared quantum", input.failure, input.workBytes, input.loads, input.fields)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, failure
}

func legacyNativeEventW82Continue(test *testing.T, bundle *legacyNativeObjectRestored) (int, error) {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeEventW82SourceSteps; attempt++ {
		value := bundle.Object.destination.Interface().(*wire)
		errorPointer, replayPointer := value.IsError, value.CrossReplayable
		current, cancel := context.WithCancel(context.Background())
		complete, failure := legacyNativeEventW82Step(test, current, bundle, false, false)
		cancel()
		if errorPointer != nil && value.IsError != nil && errorPointer != value.IsError || replayPointer != nil && value.CrossReplayable != nil && replayPointer != value.CrossReplayable {
			test.Fatal("duplicate bool update replaced an existing nonnil destination pointer")
		}
		if list := legacyNativeEventW82List(bundle.Object); list != nil && (list.frame != &bundle.Scanner || list.ownedFrame) {
			test.Fatal("actual Event step did not bind the cached child to its caller scanner")
		}
		if complete || failure != nil {
			if complete && (bundle.Object.phase != legacyNativeObjectDone || bundle.Object.position != bundle.Object.end || bundle.Object.blockActive) {
				test.Fatal("actual Event completed with unfinished field or cursor")
			}
			return attempt, failure
		}
	}
	test.Fatal("actual Event exceeded finite 4096-step continuation schedule")
	return 0, ErrIntegrity
}

func legacyNativeEventW82Drain(test *testing.T, work legacyNativeAnyW78Work, storage *legacyNativeAnyW78CAS) int {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeEventW82CheckpointSteps; attempt++ {
		storage.quantum = 0
		current, cancel := context.WithCancel(context.Background())
		ready, failure := work.step(current)
		cancel()
		retained, heapErr := work.retainedBytes()
		if failure != nil || heapErr != nil || retained < 0 || retained > legacyNativeEventW82Budget || storage.quantum > ConversationManifestChunkBytes {
			test.Fatal("actual Event checkpoint escaped bounded admission", attempt, failure, retained, heapErr, storage.quantum)
		}
		if ready {
			return attempt
		}
	}
	test.Fatal("actual Event checkpoint exceeded finite 65536-step schedule")
	return 0
}

func legacyNativeEventW82Prepare(test *testing.T, fixture legacyNativeEventW82Fixture) (legacyNativeAnyW78Request, *legacyNativeObjectRestored) {
	test.Helper()
	if !json.Valid([]byte(fixture.raw)) {
		test.Fatal("Event process fixture is not valid independent JSON", fixture.name)
	}
	request, source := legacyNativeAnyW78FullSource(test, fixture.raw)
	request.Binding = HashContent([]byte("W82 actual Event " + fixture.name))
	if len(request.Parts) > 4 {
		test.Fatal("Event fixture exceeded finite four-source-part bound", fixture.name)
	}
	value := legacyNativeEventW82Initial(fixture)
	native, err := newLegacyNativeObject(reflect.ValueOf(value), &legacyEventNativeFields, 1, true)
	if err != nil {
		test.Fatal("actual wire Event constructor failed", err)
	}
	for index, field := range native.metadata.fields {
		stored := native.destination.Elem().Field(field.index)
		if !stored.IsZero() {
			heap, failure := legacyNativeValueHeap(context.Background(), stored)
			if failure != nil {
				test.Fatal("actual initial Event field heap failed", field.name, failure)
			}
			native.heaps[index] = heap
		}
	}
	native.start(0, len(fixture.raw))
	native.pagedEvents, native.fieldLimit, native.deferTypeErrors = fixture.paged, 1, true
	return request, &legacyNativeObjectRestored{Object: native, Source: source}
}

func legacyNativeEventW82CaptureRoot(test *testing.T, bundle *legacyNativeObjectRestored, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) (legacyNativeCheckpointRef, int) {
	test.Helper()
	before := legacyNativeEventW82StateOf(test, bundle)
	quiescent := legacyNativeEventW82Quiescent(bundle)
	reads := 0
	source := bundle.Source
	captureSource := source
	captureSource.Read = func(current context.Context, offset, length int) ([]byte, error) {
		reads++
		return source.Read(current, offset, length)
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpoint(constructor, bundle.Object, &bundle.Scanner, request.Binding, captureSource, storage.io(), legacyNativeEventW82Budget)
	cancel()
	if err != nil {
		test.Fatal("actual Event checkpoint construction", err)
	}
	defer work.close()
	steps := legacyNativeEventW82Drain(test, work, storage)
	root, err := work.take(context.Background())
	if err != nil || !root.valid() {
		test.Fatal("actual Event checkpoint did not publish a valid root", err)
	}
	legacyNativeAnyW78Released(test, work)
	if reads != 0 || storage.inputWrites != 0 || !reflect.DeepEqual(before, legacyNativeEventW82StateOf(test, bundle)) || quiescent != legacyNativeEventW82Quiescent(bundle) {
		test.Fatal("Event capture read RAW, rewrote input CAS or mutated quiescent accepted state", reads, storage.inputWrites)
	}
	filename, err := storage.objectPath(root.Hash)
	if err != nil {
		test.Fatal(err)
	}
	body, err := os.ReadFile(filename)
	var state legacyNativeObjectCheckpointControl
	if err != nil || len(body) != root.Bytes || HashContent(body) != root.Hash || json.Unmarshal(body, &state) != nil {
		test.Fatal("captured Event root bytes/hash/closed metadata are invalid", err)
	}
	schema, err := legacyNativeCheckpointModelSchema(&legacyEventNativeFields)
	if err != nil || state.Version != 3 || state.Model != legacyNativeCheckpointEventModel || state.Schema != schema || state.Binding != request.Binding || state.SourceBytes != request.SourceBytes || !state.TrackPresence || state.Presence != bundle.Object.presence || state.DestinationPresence != bundle.Object.destination.Interface().(*wire).fieldPresence || len(state.Fields) != len(legacyEventNativeFields.fields) || len(state.Fields) > 32 {
		test.Fatal("captured Event lost private Version3/model/schema/source or separate presence binding", err)
	}
	return root, steps
}

func legacyNativeEventW82Capture(test *testing.T, fixture legacyNativeEventW82Fixture, storage *legacyNativeAnyW78CAS) (legacyNativeAnyW78Request, *legacyNativeObjectRestored) {
	test.Helper()
	request, bundle := legacyNativeEventW82Prepare(test, fixture)
	storage.preload(test, request)
	for attempt := 0; attempt < legacyNativeEventW82SourceSteps && !legacyNativeEventW82Stop(bundle.Object, fixture); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		current := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeEventW82Stop(bundle.Object, fixture) }}
		complete, failure := legacyNativeEventW82Step(test, current, bundle, true, fixture.cut == "dispatch")
		cancel()
		if complete || failure != nil && !errors.Is(failure, context.Canceled) {
			test.Fatal("actual Event terminated before assigned process cut", fixture.name, bundle.Object.position, failure)
		}
	}
	if !legacyNativeEventW82Stop(bundle.Object, fixture) {
		test.Fatal("actual Event never reached its designated process cut", fixture.name, bundle.Object.position)
	}
	state := legacyNativeEventW82StateOf(test, bundle)
	if fixture.cut == "dispatch" && bundle.Scanner.complete || fixture.cut == "copy" && state.BlockBinding != 2 || fixture.cut == "duplicate" && state.BlockBinding != 1 || fixture.cut == "independent-scalar" && state.BlockBinding != 0 || fixture.cut == "paged-hidden" && (state.Block == nil || len(state.Block.Data) <= legacyCanonicalPageValues || state.BlockBinding != 3) || fixture.boolAlias && !state.BooleanAlias {
		test.Fatal("Event fixture missed its actual dispatch/hidden/previous/independent/paged/pointer-alias state", fixture.name)
	}
	if state.DestinationPresence == bundle.Object.presence {
		test.Fatal("fixture conflated destination wire.fieldPresence with current native.presence")
	}
	request.Root, _ = legacyNativeEventW82CaptureRoot(test, bundle, request, storage)
	request.SuffixOffset = legacyNativeEventW82Suffix(bundle.Object, request.PartBytes)
	if request.SuffixOffset < 0 || request.SuffixOffset > request.SourceBytes || request.SuffixOffset%request.PartBytes != 0 {
		test.Fatal("Event semantic suffix has invalid manifest geometry")
	}
	if fixture.cut == "key" || fixture.cut == "ignored-key" || fixture.cut == "independent-scalar" {
		if request.SuffixOffset <= bundle.Object.position/request.PartBytes*request.PartBytes {
			test.Fatal("active block/native child suffix stayed pinned to stale outer Event position")
		}
	}
	legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
	request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
	return request, bundle
}

func legacyNativeEventW82Visible(test *testing.T, bundle *legacyNativeObjectRestored) wire {
	test.Helper()
	value := *bundle.Object.destination.Interface().(*wire)
	if bundle.Object.pagedEvents {
		list := legacyNativeEventW82List(bundle.Object)
		if list == nil || list.data == nil {
			value.Blocks = nil
		} else {
			value.Blocks = make([]ContentBlock, list.data.length())
			for index := range value.Blocks {
				record := list.data.at(index)
				if record == nil {
					test.Fatal("paged Event exposed missing visible record", index)
				}
				value.Blocks[index] = record.block
			}
		}
	}
	return value
}

func legacyNativeEventW82Independent(test *testing.T, fixture legacyNativeEventW82Fixture, bundle *legacyNativeObjectRestored, failure error) {
	test.Helper()
	expected := legacyNativeEventW82Initial(fixture)
	expectedError := json.Unmarshal([]byte(fixture.raw), expected)
	actual := legacyNativeEventW82Visible(test, bundle)
	if !reflect.DeepEqual(&actual, expected) || !reflect.DeepEqual(legacyNativeObjectW79ErrorState(failure), legacyNativeObjectW79ErrorState(expectedError)) {
		test.Fatal("actual Event visible fields/pointers/nulls differ from independent ordinary encoding/json", failure, expectedError)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fixture.raw), &fields); err != nil || bundle.Object.presence != eventPresence(fields) || actual.fieldPresence != legacyNativeEventW82Initial(fixture).fieldPresence {
		test.Fatal("native Event presence or untouched unexported wire presence differs from independent keys", err)
	}
	if !fixture.paged {
		return
	}
	ordinary := fixture
	ordinary.paged = false
	_, reference := legacyNativeEventW82Prepare(test, ordinary)
	_, referenceError := legacyNativeEventW82Continue(test, reference)
	if !reflect.DeepEqual(actual, legacyNativeEventW82Visible(test, reference)) || !reflect.DeepEqual(legacyNativeObjectW79ErrorState(failure), legacyNativeObjectW79ErrorState(referenceError)) {
		test.Fatal("paged Event visible continuation differs from original ordinary native algorithm")
	}
	list := legacyNativeEventW82List(bundle.Object)
	if list != nil && list.data != nil {
		values := reference.Object.destination.Interface().(*wire).Blocks
		if list.data.records.length() > cap(values) {
			test.Fatal("paged Event hidden history exceeds original ordinary native backing")
		}
		backing := values[:cap(values)]
		for index := 0; index < list.data.records.length(); index++ {
			if record := list.data.records.at(index); record == nil || record.block != backing[index] {
				test.Fatal("paged Event hidden record differs from original native duplicate history", index)
			}
		}
	}
}

type legacyNativeEventW82Request struct {
	Source legacyNativeAnyW78Request `json:"source"`
}

type legacyNativeEventW82Report struct {
	PID               int                                `json:"pid"`
	RestoreSteps      int                                `json:"restore_steps"`
	RecaptureSteps    int                                `json:"recapture_steps"`
	RerestoreSteps    int                                `json:"rerestore_steps"`
	ContinuationSteps int                                `json:"continuation_steps"`
	Retained          int64                              `json:"retained"`
	Initial           legacyNativeEventW82State          `json:"initial"`
	Recaptured        legacyNativeEventW82State          `json:"recaptured"`
	Final             legacyNativeEventW82State          `json:"final"`
	Error             legacyNativeObjectW79Error         `json:"error"`
	Reads             legacyNativeAnyW78Reads            `json:"reads"`
	Rereads           legacyNativeAnyW78Reads            `json:"rereads"`
	Dependencies      []legacyNativeAnyW78DependencyRead `json:"dependencies"`
}

func legacyNativeEventW82RunGroup(test *testing.T, group string, expectedCuts int) {
	test.Helper()
	fixtures := legacyNativeEventW82Fixtures()
	if len(fixtures) != 12 {
		test.Fatal("finite twelve-child actual Event schedule changed", len(fixtures))
	}
	cuts := 0
	for _, fixture := range fixtures {
		if fixture.group != group {
			continue
		}
		cuts++
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, original := legacyNativeEventW82Capture(test, fixture, storage)
			initial := legacyNativeEventW82StateOf(test, original)
			_, failure := legacyNativeEventW82Continue(test, original)
			final := legacyNativeEventW82StateOf(test, original)
			legacyNativeEventW82Independent(test, fixture, original, failure)
			legacyNativeAnyW78JSON(test, filepath.Join(storage.directory, "request.json"), legacyNativeEventW82Request{Source: request})
			current, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(current, os.Args[0], "-test.run=^TestLegacyNativeEventW82ChildHelper$", "-test.count=1", "-test.timeout=20s")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeEventW82Directory+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeEventW82Directory+"="+storage.directory)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("fresh actual Event child failed: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(storage.directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeEventW82Report
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			if report.PID <= 0 || report.PID == os.Getpid() || report.RestoreSteps <= 0 || report.RestoreSteps > legacyNativeEventW82CheckpointSteps || report.RecaptureSteps <= 0 || report.RecaptureSteps > legacyNativeEventW82CheckpointSteps || report.RerestoreSteps <= 0 || report.RerestoreSteps > legacyNativeEventW82CheckpointSteps || report.ContinuationSteps <= 0 || report.ContinuationSteps > legacyNativeEventW82SourceSteps || report.Retained <= 0 || report.Retained > legacyNativeEventW82Budget || report.Reads.Forbidden != 0 || report.Rereads.Forbidden != 0 || len(report.Rereads.Ranges) == 0 {
				test.Fatal("missing bounded fresh-process Event restore/recapture/suffix-only continuation", report.PID, report.RestoreSteps, report.RecaptureSteps, report.RerestoreSteps, report.ContinuationSteps, report.Retained)
			}
			if !reflect.DeepEqual(report.Initial, initial) || !reflect.DeepEqual(report.Recaptured, initial) || !reflect.DeepEqual(report.Final, final) || !reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)) {
				test.Fatalf("Event parent/scanner/field/presence/alias/block/paged state diverged: initial=%v recaptured=%v final=%v error=%v", reflect.DeepEqual(report.Initial, initial), reflect.DeepEqual(report.Recaptured, initial), reflect.DeepEqual(report.Final, final), reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)))
			}
			if len(report.Reads.Ranges) != 0 {
				test.Fatal("Event restore or immediate pre-step recapture reread original RAW", report.Reads)
			}
			for _, read := range report.Rereads.Ranges {
				if read[0] < request.SuffixOffset || read[0]%request.PartBytes != 0 || read[1] != min(request.PartBytes, request.SourceBytes-read[0]) {
					test.Fatal("fresh Event continuation read consumed semantic prefix or nonmanifest range", read)
				}
			}
			for _, dependency := range report.Dependencies {
				if !dependency.Restoring {
					test.Fatal("Event continued semantics through original CAS prefix", dependency)
				}
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
			test.Logf("actual Event child PID=%d restore=%d recapture=%d rerestore=%d continue=%d suffix=%d", report.PID, report.RestoreSteps, report.RecaptureSteps, report.RerestoreSteps, report.ContinuationSteps, request.SuffixOffset)
		})
	}
	if cuts != expectedCuts {
		test.Fatal("finite Event parent group schedule changed", group, cuts, expectedCuts)
	}
	test.Log(legacyNativeEventW82Blocked)
}

func TestLegacyNativeEventCheckpointW82DispatchProcessContinuation(test *testing.T) {
	legacyNativeEventW82RunGroup(test, "dispatch", 4)
}

func TestLegacyNativeEventCheckpointW82OwnershipProcessContinuation(test *testing.T) {
	legacyNativeEventW82RunGroup(test, "ownership", 4)
}

func TestLegacyNativeEventCheckpointW82CacheProcessContinuation(test *testing.T) {
	legacyNativeEventW82RunGroup(test, "cache", 2)
}

func TestLegacyNativeEventCheckpointW82PrimitiveProcessContinuation(test *testing.T) {
	legacyNativeEventW82RunGroup(test, "primitive", 2)
}

func legacyNativeEventW82Restore(test *testing.T, request legacyNativeAnyW78Request, source LegacyByteSource, storage *legacyNativeAnyW78CAS) (*legacyNativeObjectRestored, int) {
	test.Helper()
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpointRestore(constructor, request.Root, legacyNativeCheckpointEventModel, storage.io(), source, request.Binding, legacyNativeEventW82Budget)
	cancel()
	if err != nil {
		test.Fatal("fresh actual Event restore construction", err)
	}
	defer work.close()
	steps := legacyNativeEventW82Drain(test, work, storage)
	restored, err := work.take(context.Background())
	if err != nil || restored.Object == nil {
		test.Fatal("fresh child did not restore actual wire/native Event", err)
	}
	legacyNativeAnyW78Released(test, work)
	if restored.Source.authority == 0 || restored.Source.authority != source.authority || restored.Source.Read == nil || restored.Source.Bytes != source.Bytes || restored.Source.PartBytes != source.PartBytes || !reflect.DeepEqual(restored.Source.Parts, source.Parts) {
		test.Fatal("Event restore reused saved authority instead of fresh caller source")
	}
	if request.SuffixOffset != legacyNativeEventW82Suffix(restored.Object, request.PartBytes) || storage.inputWrites != 0 {
		test.Fatal("Event restore lost semantic child suffix or rewrote input CAS")
	}
	return &restored, steps
}

func TestLegacyNativeEventW82ChildHelper(test *testing.T) {
	directory := os.Getenv(legacyNativeEventW82Directory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeEventW82Request
	if err := json.Unmarshal(body, &request); err != nil || len(request.Source.Parts) == 0 || len(request.Source.Parts) > 4 || request.Source.SourceBytes > 4*LegacyNormalizationPartBytes {
		test.Fatal("child Event request exceeds finite four-part source fixture", err)
	}
	report := legacyNativeEventW82Report{PID: os.Getpid()}
	source := legacyNativeAnyW78Source(test, request.Source, &report.Reads)
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request.Source)
	restored, steps := legacyNativeEventW82Restore(test, request.Source, source, storage)
	report.RestoreSteps = steps
	report.Initial = legacyNativeEventW82StateOf(test, restored)
	root, steps := legacyNativeEventW82CaptureRoot(test, restored, request.Source, storage)
	report.RecaptureSteps = steps
	if report.Reads.Forbidden != 0 || len(report.Reads.Ranges) != 0 {
		test.Fatal("actual Event restore or immediate before-first-step recapture replayed RAW")
	}
	recaptured := request.Source
	recaptured.Root = root
	fresh := legacyNativeAnyW78Source(test, recaptured, &report.Rereads)
	if fresh.authority == source.authority {
		test.Fatal("second Event restore did not receive a fresh caller grant")
	}
	continued, steps := legacyNativeEventW82Restore(test, recaptured, fresh, storage)
	report.RerestoreSteps = steps
	report.Recaptured = legacyNativeEventW82StateOf(test, continued)
	if len(report.Rereads.Ranges) != 0 || report.Rereads.Forbidden != 0 || !reflect.DeepEqual(report.Initial, report.Recaptured) {
		test.Fatal("immediate pre-step recapture lost the real saved shared frame or reread RAW")
	}
	retained, err := continued.Object.retained()
	if err != nil || retained <= 0 || retained > legacyNativeEventW82Budget {
		test.Fatal("restored Event/native child current storage was not admitted", retained, err)
	}
	report.Retained = retained
	storage.restoring = false
	var failure error
	report.ContinuationSteps, failure = legacyNativeEventW82Continue(test, continued)
	report.Final = legacyNativeEventW82StateOf(test, continued)
	report.Error = legacyNativeObjectW79ErrorState(failure)
	report.Dependencies = storage.dependencies
	if storage.inputWrites != 0 {
		test.Fatal("fresh Event child rewrote immutable original input CAS")
	}
	legacyNativeAnyW78JSON(test, filepath.Join(directory, "report.json"), report)
}
