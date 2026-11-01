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
)

const legacyNativeNestedW83Directory = "CXTHUB_W83_NATIVE_NESTED_DIRECTORY"
const legacyNativeNestedW83Budget int64 = 64 << 20
const legacyNativeNestedW83SourceSteps = 4096
const legacyNativeNestedW83CheckpointSteps = 65536
const legacyNativeNestedW83Unfinished = "Finite private-v3 Event Locked/ProviderMetadata continuation only. Dynamic Input/Output containers, Replacement, complete native aggregate caches, the actual atomic stage/sequence and pinning, incremental encoder, four spools, sorting, proof, worker, and all original full-model/hash/quantum/performance gates remain open. This does not finish the full goal or prove transparent old/new running-binary compatibility."

type legacyNativeNestedW83Fixture struct {
	name   string
	group  string
	raw    string
	field  string
	cut    string
	seeded bool
}

func legacyNativeNestedW83Fixtures() []legacyNativeNestedW83Fixture {
	width := LegacyNormalizationPartBytes
	longText := strings.Repeat("a", 2*width) + `\n\uD83D\uDE42\u0000한`
	validNumber := `"0.` + strings.Repeat("0", 2*width) + `1"`
	invalidNumber := `"0.` + strings.Repeat("0", 2*width) + `x"`
	keyPrefix := `{"locked":{"provider":"codex",`
	blockPrefix := `{"locked":{"blob":"cached"},"provider_metadata":{"turn_id":"cached","create_time":17},"blocks":[{"type":"text",`
	return []legacyNativeNestedW83Fixture{
		{name: "pre-dispatch-independent-initial-pointers", group: "dispatch", raw: `{"locked":{"blob":"merged","scheme":null},"provider_metadata":{"turn_id":"merged","create_time":null},"seq":7}`, field: "locked", cut: "dispatch", seeded: true},
		{name: "active-nested-shared-partial-key", group: "dispatch", raw: keyPrefix + strings.Repeat(" ", width-len(keyPrefix)-3) + `"blob":"after-key"},"id":"after-child"}`, field: "locked", cut: "key"},
		{name: "active-ignored-deep-key", group: "dispatch", raw: `{"provider_metadata":{"ignored":{"inside":[{"` + strings.Repeat("k", 2*width) + `":0}]},"turn_id":"after-ignored","create_time":null},"seq":7}`, field: "provider_metadata", cut: "ignored-key"},
		{name: "giant-locked-blob-before-duplicate-merge", group: "values", raw: `{"locked":{"provider":"codex","blob":"` + longText + `","scheme":"encrypted_content"},"LOCKED":{"blob":null,"scheme":"signature"},"seq":7}`, field: "locked", cut: "scalar"},
		{name: "giant-turn-id-with-number-null-and-duplicates", group: "values", raw: `{"provider_metadata":{"create_time":17,"turn_id":"` + longText + `"},"PROVIDER_METADATA":{"turn_id":null,"create_time":null},"provider_metadata":{"create_time":"12.50e+03"},"seq":7}`, field: "provider_metadata", cut: "scalar", seeded: true},
		{name: "typed-number-private-target-after-null", group: "values", raw: `{"provider_metadata":{"create_time":null,"create_time":` + validNumber + `,"turn_id":"after-number"},"seq":7}`, field: "provider_metadata", cut: "number", seeded: true},
		{name: "inactive-caches-then-different-root-scalar", group: "cache", raw: `{"locked":{"blob":"first"},"LOCKED":{"blob":null,"scheme":"signature"},"provider_metadata":{"create_time":"17.00e+02"},"id":"` + longText + `","locked":{"blob":"last"},"seq":7}`, field: "locked", cut: "cached-scalar", seeded: true},
		{name: "null-clears-both-destinations-and-real-caches", group: "cache", raw: `{"locked":{"blob":"discarded"},"provider_metadata":{"turn_id":"discarded","create_time":17},"locked":null,"provider_metadata":null,"locked":{"blob":"fresh"},"provider_metadata":{"turn_id":"fresh","create_time":null},"seq":7}`, field: "provider_metadata", cut: "null", seeded: true},
		{name: "block-child-shared-key-with-inactive-nested-caches", group: "cache", raw: blockPrefix + strings.Repeat(" ", width-len(blockPrefix)-3) + `"text":"after-key"}],"locked":{"scheme":"signature"},"seq":7}`, field: "locked", cut: "block-key", seeded: true},
		{name: "typed-number-full-diagnostic-in-progress", group: "errors", raw: `{"id":"before-error","provider_metadata":{"turn_id":"before-number","create_time":` + invalidNumber + `}}`, field: "provider_metadata", cut: "diagnostic", seeded: true},
		{name: "active-done-remembers-child-type-error", group: "errors", raw: `{"id":"before-error","provider_metadata":{"create_time":false,"turn_id":"after-error"}}`, field: "provider_metadata", cut: "done-error", seeded: true},
		{name: "active-done-canceled-before-parent-handoff", group: "errors", raw: `{"locked":{"provider":"codex","scheme":"signature","blob":"complete-child"},"id":"after-handoff","seq":7}`, field: "locked", cut: "done-clean"},
	}
}

func legacyNativeNestedW83Initial(fixture legacyNativeNestedW83Fixture) *wire {
	errorValue, replayValue := false, true
	value := &wire{
		Kind: EventMessage, ID: "prior-id", TS: "2026-10-29T00:00:00Z", Seq: 23, Role: RoleAssistant,
		CompactSummary: true, AgentMessage: true, AgentAuthor: "prior-author", AgentRecipient: "prior-recipient",
		ReplacementComplete: true, CallID: "prior-call", ToolName: "prior-tool", ProviderToolName: "prior-provider-tool",
		Status: "prior-status", Output: "prior-output", IsError: &errorValue, CrossReplayable: &replayValue,
		RedactedSummary: "prior-summary", fieldPresence: eventFieldLocked | eventFieldReplacement,
	}
	if fixture.seeded {
		number := json.Number("123.50e+02")
		value.Locked = &LockedBlob{Provider: ProviderCodex, Scheme: "initial-scheme", Blob: "initial-blob"}
		value.ProviderMetadata = &ProviderMetadata{TurnID: "initial-turn", CreateTime: &number}
	}
	return value
}

func legacyNativeNestedW83Child(native *legacyNativeObject, name string) *legacyNativeObject {
	field := native.metadata.match(name)
	if field < 0 || field >= len(native.nested) {
		return nil
	}
	return native.nested[field]
}

func legacyNativeNestedW83Stop(bundle *legacyNativeObjectRestored, fixture legacyNativeNestedW83Fixture) bool {
	native := bundle.Object
	child := legacyNativeNestedW83Child(native, fixture.field)
	switch fixture.cut {
	case "dispatch":
		return !native.nestedActive && child == nil && native.phase == legacyNativeObjectApply && native.field == native.metadata.match(fixture.field) && native.valueStarted && native.valueFirst == '{' && native.position == native.valueStart && native.valueEnd == native.end
	case "cached-scalar":
		return !native.nestedActive && child != nil && child.phase == legacyNativeObjectDone && legacyNativeNestedW83Child(native, "provider_metadata") != nil && native.field == native.metadata.match("id") && native.scalar != nil && native.scalar.text != nil && native.scalar.text.Len() > 0 && native.scalar.position == LegacyNormalizationPartBytes
	case "null":
		value := native.destination.Interface().(*wire)
		return !native.nestedActive && child == nil && legacyNativeNestedW83Child(native, "locked") == nil && value.Locked == nil && value.ProviderMetadata == nil && native.phase == legacyNativeObjectAfter && native.field == native.metadata.match(fixture.field) && native.valueFirst == 'n'
	case "block-key":
		return !native.nestedActive && child != nil && native.blockActive && legacyNativeBlockListW81Stop(legacyNativeEventW82List(native), "key")
	}
	if !native.nestedActive || child == nil || native.field != native.metadata.match(fixture.field) {
		return false
	}
	switch fixture.cut {
	case "key":
		return child.phase == legacyNativeObjectKey && child.keyStarted && len(child.key) > 0 && child.position == LegacyNormalizationPartBytes && !bundle.Scanner.complete
	case "ignored-key":
		return child.phase == legacyNativeObjectValue && child.valueStarted && child.field < 0 && child.position == LegacyNormalizationPartBytes && bundle.Scanner.key && !bundle.Scanner.complete
	case "scalar":
		return child.scalar != nil && child.scalar.position == LegacyNormalizationPartBytes && child.scalar.text != nil && child.scalar.text.Len() > 0
	case "number":
		return child.typedNumber != nil && child.typedNumber.newTarget && !child.typedNumber.decoded && child.typedNumber.scalar != nil && child.typedNumber.scalar.position == LegacyNormalizationPartBytes
	case "diagnostic":
		return child.typedNumber != nil && child.typedNumber.rejected && !child.typedNumber.complete && child.typedNumber.errorText != nil && child.typedNumber.errorText.position >= LegacyNormalizationPartBytes && child.typedNumber.errorText.position < child.typedNumber.end
	case "done-error":
		return child.phase == legacyNativeObjectDone && child.typeError != nil
	case "done-clean":
		return child.phase == legacyNativeObjectDone && child.typeError == nil
	}
	return false
}

func legacyNativeNestedW83Suffix(native *legacyNativeObject, width int) int {
	if native.nestedActive {
		if native.field < 0 || native.field >= len(native.nested) || native.nested[native.field] == nil {
			return -1
		}
		child := native.nested[native.field]
		position := legacyNativeObjectW79Suffix(child, width)
		if number := child.typedNumber; number != nil && number.errorText != nil {
			position = min(position, number.errorText.position/width*width)
		}
		return position
	}
	if native.blockActive {
		if list := legacyNativeEventW82List(native); list != nil {
			return legacyNativeBlockListW81Suffix(list, width)
		}
		return -1
	}
	return legacyNativeObjectW79Suffix(native, width)
}

type legacyNativeNestedW83State struct {
	Control             []int                          `json:"control"`
	Flags               []bool                         `json:"flags"`
	Key                 []byte                         `json:"key"`
	Scanner             []byte                         `json:"scanner"`
	Value               []byte                         `json:"value"`
	Locked              []string                       `json:"locked"`
	Provider            []string                       `json:"provider"`
	OutputType          string                         `json:"output_type"`
	DestinationPresence eventFieldPresence             `json:"destination_presence"`
	Initialized         []bool                         `json:"initialized"`
	Scalar              []string                       `json:"scalar"`
	Children            []*legacyNativeObjectW79State  `json:"children"`
	Aliases             []bool                         `json:"aliases"`
	Block               *legacyNativeBlockListW81State `json:"block"`
	BlockBinding        uint8                          `json:"block_binding"`
	Error               legacyNativeObjectW79Error     `json:"error"`
}

func legacyNativeNestedW83StateOf(test *testing.T, bundle *legacyNativeObjectRestored) legacyNativeNestedW83State {
	test.Helper()
	native := bundle.Object
	value, typed := native.destination.Interface().(*wire)
	if !typed || native.metadata != &legacyEventNativeFields || native.metadata.model != reflect.TypeFor[wire]() || !native.trackPresence || len(native.heaps) != len(native.metadata.fields) || len(native.backingExcess) != len(native.metadata.fields) {
		test.Fatal("nested process fixture lost the actual full wire/native Event metadata")
	}
	if value.Input != nil || value.Replacement != nil || native.eventActive || native.containerActive || len(native.events) != 0 || len(native.containers) != 0 || native.pagedEvents {
		test.Fatal("nested process fixture silently populated an unfinished aggregate path")
	}
	if native.checkpointScanner != nil && *native.checkpointScanner != bundle.Scanner {
		test.Fatal("restored parent scanner differs from the actual shared caller frame")
	}
	scannerBytes, err := bundle.Scanner.checkpoint(context.Background())
	if err != nil {
		test.Fatal("actual shared nested scanner cannot be observed", err)
	}
	ordinary := *value
	ordinary.Locked, ordinary.ProviderMetadata = nil, nil
	valueBytes, err := json.Marshal(&ordinary)
	if err != nil {
		test.Fatal("actual wire values cannot be observed", err)
	}
	state := legacyNativeNestedW83State{
		Control: []int{native.parentDepth, native.fieldLimit, native.position, native.end, native.valueStart, native.valueEnd, native.field, int(native.presence), int(native.phase), int(native.valueFirst), cap(native.key), cap(native.raw)},
		Flags:   []bool{native.trackPresence, native.pagedEvents, native.deferTypeErrors, native.skipValue, native.prefix, native.keyStarted, native.keyOverflow, native.valueStarted, native.listActive, native.blockActive, native.nestedActive, native.blocks != nil, native.nested != nil},
		Key:     bytes.Clone(native.key), Scanner: scannerBytes, Value: valueBytes, OutputType: fmt.Sprintf("%T", value.Output),
		DestinationPresence: value.fieldPresence, Error: legacyNativeObjectW79ErrorState(native.typeError),
	}
	if value.Locked != nil {
		state.Locked = legacyNativeObjectW79Values(test, value.Locked)
	}
	if value.ProviderMetadata != nil {
		state.Provider = legacyNativeObjectW79Values(test, value.ProviderMetadata)
	}
	for index, heap := range native.heaps {
		if heap.err != nil || heap.bytes < 0 || native.backingExcess[index] < 0 {
			test.Fatal("actual parent ownership is invalid", index, heap)
		}
		state.Initialized = append(state.Initialized, heap.bytes != 0)
	}
	if native.scalar != nil {
		state.Scalar = legacyNativeAnyW78ScalarState(test, native.scalar)
	}
	if native.nested != nil {
		if len(native.nested) != len(native.metadata.fields) {
			test.Fatal("actual nested cache is not field-indexed")
		}
		state.Children = make([]*legacyNativeObjectW79State, len(native.nested))
		state.Aliases = make([]bool, len(native.nested))
	}
	for index, child := range native.nested {
		if child == nil {
			continue
		}
		field := native.metadata.fields[index]
		destination := native.destination.Elem().Field(field.index)
		metadata := legacyNativeNestedMetadata(destination.Type())
		if !field.nestedObject || metadata == nil || child.metadata != metadata || destination.IsNil() || child.destination.Interface() != destination.Interface() || child.trackPresence || child.parentDepth != native.parentDepth+1 {
			test.Fatal("nested child lost actual metadata, depth, separate presence, or exact pointee alias", field.name)
		}
		active := native.nestedActive && native.field == index
		var childScanner legacyJSONValueScanner
		if active {
			if native.phase != legacyNativeObjectApply || native.valueFirst != '{' || native.position != native.valueStart || native.valueEnd != native.end || !child.prefix || child.end != native.valueEnd || child.position < native.valueStart || child.position > child.end {
				test.Fatal("active nested child lost its actual prefix/range/parent Apply contract", field.name)
			}
			childScanner = bundle.Scanner
		} else if child.phase != legacyNativeObjectDone {
			test.Fatal("inactive nested cache is not actually Done", field.name)
		}
		if child.checkpointScanner != nil && *child.checkpointScanner != childScanner {
			test.Fatal("nested framing is not the active shared frame or inactive owned zero scanner", field.name)
		}
		if number := child.typedNumber; number != nil && (number.authority != bundle.Source.authority || number.authority == 0 || number.sourceBytes != bundle.Source.Bytes) {
			test.Fatal("nested typed number retained serialized or stale caller authority")
		}
		for member, heap := range child.heaps {
			if heap.err != nil || heap.bytes < 0 || child.backingExcess[member] < 0 {
				test.Fatal("actual child ownership is invalid", field.name, member)
			}
		}
		observed := legacyNativeObjectW79StateOf(test, child, &childScanner)
		state.Children[index], state.Aliases[index] = &observed, true
	}
	if native.nestedActive && (native.field < 0 || native.field >= len(native.nested) || native.nested[native.field] == nil) {
		test.Fatal("active nested continuation has no actual child")
	}
	if list := legacyNativeEventW82List(native); list != nil {
		if list.frame == nil || *list.frame != bundle.Scanner || list.parentDepth != native.parentDepth+1 || list.paged != native.pagedEvents {
			test.Fatal("block and nested caches lost the single caller scanner")
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

func legacyNativeNestedW83Quiescent(bundle *legacyNativeObjectRestored) string {
	native := bundle.Object
	shape := fmt.Sprintf("%#v|%#v", native.heaps, native.backingExcess)
	for _, child := range native.nested {
		if child != nil {
			shape += fmt.Sprintf("|%#v|%#v", child.heaps, child.backingExcess)
		}
	}
	return shape + "|" + legacyNativeEventW82Quiescent(bundle)
}

func legacyNativeNestedW83Drain(test *testing.T, work legacyNativeAnyW78Work, storage *legacyNativeAnyW78CAS) int {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeNestedW83CheckpointSteps; attempt++ {
		storage.quantum = 0
		current, cancel := context.WithCancel(context.Background())
		ready, failure := work.step(current)
		cancel()
		retained, heapErr := work.retainedBytes()
		if failure != nil || heapErr != nil || retained < 0 || retained > legacyNativeNestedW83Budget || storage.quantum > ConversationManifestChunkBytes {
			test.Fatal("nested checkpoint escaped retained admission or shared IO quantum", attempt, failure, retained, heapErr, storage.quantum)
		}
		if ready {
			return attempt
		}
	}
	test.Fatal("nested checkpoint exceeded its finite 65536-step schedule")
	return 0
}

func legacyNativeNestedW83Prepare(test *testing.T, fixture legacyNativeNestedW83Fixture) (legacyNativeAnyW78Request, *legacyNativeObjectRestored) {
	test.Helper()
	if !json.Valid([]byte(fixture.raw)) {
		test.Fatal("nested process fixture is not independent valid JSON", fixture.name)
	}
	request, source := legacyNativeAnyW78FullSource(test, fixture.raw)
	request.Binding = HashContent([]byte("W83 actual nested Event " + fixture.name))
	if len(request.Parts) == 0 || len(request.Parts) > 4 {
		test.Fatal("nested process fixture escaped its four-part source bound")
	}
	native, err := newLegacyNativeObject(reflect.ValueOf(legacyNativeNestedW83Initial(fixture)), &legacyEventNativeFields, 1, true)
	if err != nil {
		test.Fatal("actual wire Event constructor failed", err)
	}
	for index, field := range native.metadata.fields {
		value := native.destination.Elem().Field(field.index)
		if !value.IsZero() {
			heap, err := legacyNativeValueHeap(context.Background(), value)
			if err != nil {
				test.Fatal("actual seeded Event heap failed", field.name, err)
			}
			native.heaps[index] = heap
		}
	}
	native.start(0, len(fixture.raw))
	native.fieldLimit, native.deferTypeErrors = 1, true
	return request, &legacyNativeObjectRestored{Object: native, Source: source}
}

func legacyNativeNestedW83Continue(test *testing.T, bundle *legacyNativeObjectRestored) (int, error) {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeNestedW83SourceSteps; attempt++ {
		value := bundle.Object.destination.Interface().(*wire)
		locked, provider := value.Locked, value.ProviderMetadata
		var number *json.Number
		if provider != nil {
			number = provider.CreateTime
		}
		current, cancel := context.WithCancel(context.Background())
		complete, failure := legacyNativeEventW82Step(test, current, bundle, false, false)
		cancel()
		if locked != nil && value.Locked != nil && locked != value.Locked || provider != nil && value.ProviderMetadata != nil && provider != value.ProviderMetadata || provider != nil && provider == value.ProviderMetadata && number != nil && provider.CreateTime != nil && number != provider.CreateTime {
			test.Fatal("nonnull duplicate nested/number update replaced its actual destination pointer")
		}
		if list := legacyNativeEventW82List(bundle.Object); list != nil && (list.frame != &bundle.Scanner || list.ownedFrame) {
			test.Fatal("block continuation did not bind the actual caller scanner")
		}
		if complete || failure != nil {
			if complete && (bundle.Object.phase != legacyNativeObjectDone || bundle.Object.position != bundle.Object.end || bundle.Object.nestedActive || bundle.Object.blockActive) {
				test.Fatal("nested Event completed with unfinished real parent/child state")
			}
			for _, child := range bundle.Object.nested {
				if child != nil && child.phase == legacyNativeObjectDone && child.typeError != nil && failure != child.typeError {
					test.Fatal("active Done child lost its remembered error identity or became empty success")
				}
				if child != nil && child.typedNumber != nil && child.typedNumber.complete && failure != child.typedNumber.semantic {
					test.Fatal("nested full diagnostic lost its exact remembered error identity")
				}
			}
			return attempt, failure
		}
	}
	test.Fatal("nested Event exceeded its finite 4096-step continuation schedule")
	return 0, ErrIntegrity
}

func legacyNativeNestedW83ReadControl(test *testing.T, storage *legacyNativeAnyW78CAS, root legacyNativeCheckpointRef) (legacyNativeObjectCheckpointControl, []byte) {
	test.Helper()
	filename, err := storage.objectPath(root.Hash)
	if err != nil {
		test.Fatal(err)
	}
	body, err := os.ReadFile(filename)
	var state legacyNativeObjectCheckpointControl
	if err != nil || !root.valid() || len(body) != root.Bytes || len(body) > LegacyCanonicalEncodingBytes || HashContent(body) != root.Hash || json.Unmarshal(body, &state) != nil {
		test.Fatal("private-v3 object root lost its exact bounded bytes, hash, or DTO", err)
	}
	return state, body
}

func legacyNativeNestedW83CaptureRoot(test *testing.T, bundle *legacyNativeObjectRestored, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) (legacyNativeCheckpointRef, int) {
	test.Helper()
	before, quiescent := legacyNativeNestedW83StateOf(test, bundle), legacyNativeNestedW83Quiescent(bundle)
	reads := 0
	source, captureSource := bundle.Source, bundle.Source
	captureSource.Read = func(current context.Context, offset, length int) ([]byte, error) {
		reads++
		return source.Read(current, offset, length)
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpoint(constructor, bundle.Object, &bundle.Scanner, request.Binding, captureSource, storage.io(), legacyNativeNestedW83Budget)
	cancel()
	if err != nil {
		test.Fatal("actual nested Event checkpoint construction", err)
	}
	defer work.close()
	steps := legacyNativeNestedW83Drain(test, work, storage)
	root, err := work.take(context.Background())
	if err != nil || !root.valid() {
		test.Fatal("actual nested Event checkpoint did not publish a root", err)
	}
	legacyNativeAnyW78Released(test, work)
	if reads != 0 || storage.inputWrites != 0 || !reflect.DeepEqual(before, legacyNativeNestedW83StateOf(test, bundle)) || quiescent != legacyNativeNestedW83Quiescent(bundle) {
		test.Fatal("capture replayed RAW, rewrote source CAS, or mutated exclusively quiescent actual state", reads, storage.inputWrites)
	}
	state, _ := legacyNativeNestedW83ReadControl(test, storage, root)
	schema, err := legacyNativeCheckpointModelSchema(&legacyEventNativeFields)
	if err != nil || state.Version != 3 || state.Model != legacyNativeCheckpointEventModel || state.Schema != schema || state.Binding != request.Binding || state.SourceBytes != request.SourceBytes || !state.TrackPresence || state.Presence != bundle.Object.presence || state.DestinationPresence != bundle.Object.destination.Interface().(*wire).fieldPresence || state.NestedActive != bundle.Object.nestedActive || state.HasNestedCache != (bundle.Object.nested != nil) || len(state.Fields) != len(legacyEventNativeFields.fields) || len(state.Fields) > 32 {
		test.Fatal("nested root changed Version3/model/schema/source/presence/cache contracts", err)
	}
	for index, field := range bundle.Object.metadata.fields {
		if !field.nestedObject {
			continue
		}
		row := state.Fields[index]
		value := bundle.Object.destination.Elem().Field(field.index)
		child := legacyNativeNestedW83Child(bundle.Object, field.name)
		if row.Present != !value.IsNil() || row.Initialized != (bundle.Object.heaps[index].bytes != 0) {
			test.Fatal("nested row confused actual destination presence and parent heap initialization", field.name)
		}
		if row.Nested == nil {
			if !value.IsNil() || child != nil {
				test.Fatal("populated nested destination or real cache was omitted", field.name)
			}
			continue
		}
		model, err := legacyNativeCheckpointModel(legacyNativeNestedMetadata(value.Type()))
		if err != nil || row.Nested.Model != model {
			test.Fatal("nested row changed its actual model binding", field.name, err)
		}
		if child == nil {
			if row.Nested.Parser != (legacyNativeCheckpointRef{}) || (row.Nested.Value != nil) != !value.IsNil() {
				test.Fatal("independent/nil nested value invented a parser or lost its value", field.name)
			}
			continue
		}
		if row.Nested.Value != nil || !row.Nested.Parser.valid() {
			test.Fatal("real nested parser was replaced by a values-only surrogate", field.name)
		}
		childState, _ := legacyNativeNestedW83ReadControl(test, storage, row.Nested.Parser)
		childSchema, err := legacyNativeCheckpointModelSchema(child.metadata)
		if err != nil || childState.Version != 3 || childState.Model != model || childState.Schema != childSchema || childState.Binding != request.Binding || childState.SourceBytes != request.SourceBytes || childState.ParentDepth != child.parentDepth || childState.TrackPresence || childState.Phase != child.phase || childState.Position != child.position || childState.End != child.end || childState.Prefix != child.prefix || (childState.Error != (legacyNativeCheckpointRef{})) != (child.typeError != nil) {
			test.Fatal("actual child private-v3 control lost model, framing, separate presence, or remembered error", field.name, err)
		}
		if bundle.Object.nestedActive && bundle.Object.field == index {
			if childState.Scanner != state.Scanner {
				test.Fatal("active child checkpoint did not retain the real shared scanner")
			}
		} else {
			var zero legacyJSONValueScanner
			body, err := zero.checkpoint(context.Background())
			if err != nil || childState.Scanner.Hash != HashContent(body) || childState.Scanner.Bytes != len(body) {
				test.Fatal("inactive cached child copied another root key instead of owning a zero scanner", err)
			}
		}
	}
	return root, steps
}

func legacyNativeNestedW83Capture(test *testing.T, fixture legacyNativeNestedW83Fixture, storage *legacyNativeAnyW78CAS) (legacyNativeAnyW78Request, *legacyNativeObjectRestored) {
	test.Helper()
	request, bundle := legacyNativeNestedW83Prepare(test, fixture)
	storage.preload(test, request)
	var lastFailure error
	for attempt := 0; attempt < legacyNativeNestedW83SourceSteps && !legacyNativeNestedW83Stop(bundle, fixture); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		current := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeNestedW83Stop(bundle, fixture) }}
		complete, failure := legacyNativeEventW82Step(test, current, bundle, true, fixture.cut == "dispatch")
		cancel()
		lastFailure = failure
		if complete || failure != nil && !errors.Is(failure, context.Canceled) && !legacyNativeNestedW83Stop(bundle, fixture) {
			test.Fatal("actual nested Event terminated before its assigned process cut", fixture.name, bundle.Object.position, failure)
		}
	}
	if !legacyNativeNestedW83Stop(bundle, fixture) {
		test.Fatal("actual source never reached its finite nested process cut", fixture.name, bundle.Object.position)
	}
	if fixture.cut == "done-clean" && !errors.Is(lastFailure, context.Canceled) {
		test.Fatal("Done child cut did not actually cancel between completion and parent heap handoff", lastFailure)
	}
	if fixture.cut == "done-error" && lastFailure != legacyNativeNestedW83Child(bundle.Object, fixture.field).typeError {
		test.Fatal("Done child cut did not preserve the actual remembered type error", lastFailure)
	}
	state := legacyNativeNestedW83StateOf(test, bundle)
	if state.DestinationPresence == bundle.Object.presence {
		test.Fatal("fixture conflated root key presence with untouched wire.fieldPresence")
	}
	request.Root, _ = legacyNativeNestedW83CaptureRoot(test, bundle, request, storage)
	request.SuffixOffset = legacyNativeNestedW83Suffix(bundle.Object, request.PartBytes)
	if request.SuffixOffset < 0 || request.SuffixOffset > request.SourceBytes || request.SuffixOffset%request.PartBytes != 0 {
		test.Fatal("actual nested semantic suffix lost manifest geometry")
	}
	switch fixture.cut {
	case "key", "ignored-key", "scalar", "number", "diagnostic", "cached-scalar", "block-key":
		if request.SuffixOffset <= bundle.Object.position/request.PartBytes*request.PartBytes && fixture.cut != "cached-scalar" {
			test.Fatal("actual active child suffix stayed pinned to stale outer Event framing", fixture.name)
		}
		if request.SuffixOffset == 0 {
			test.Fatal("giant/key continuation did not exclude an already accepted RAW source part", fixture.name)
		}
	}
	legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
	request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
	return request, bundle
}

func legacyNativeNestedW83Independent(test *testing.T, fixture legacyNativeNestedW83Fixture, bundle *legacyNativeObjectRestored, failure error) {
	test.Helper()
	expected := legacyNativeNestedW83Initial(fixture)
	expectedFailure := json.Unmarshal([]byte(fixture.raw), expected)
	actual := bundle.Object.destination.Interface().(*wire)
	if !reflect.DeepEqual(actual, expected) || (failure == nil) != (expectedFailure == nil) {
		test.Fatal("nested visible full wire values, null/duplicate order, or acceptance differ from independent encoding/json", failure, expectedFailure)
	}
	if failure != nil {
		var actualTyped, expectedTyped *json.UnmarshalTypeError
		actualIsTyped, expectedIsTyped := errors.As(failure, &actualTyped), errors.As(expectedFailure, &expectedTyped)
		if actualIsTyped != expectedIsTyped || actualIsTyped && (actualTyped.Value != expectedTyped.Value || actualTyped.Type != expectedTyped.Type) || !actualIsTyped && (reflect.TypeOf(failure) != reflect.TypeOf(expectedFailure) || failure.Error() != expectedFailure.Error()) {
			test.Fatal("nested error kind/value/type or full number diagnostic differs from independent oracle", failure, expectedFailure)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fixture.raw), &fields); err != nil || bundle.Object.presence != eventPresence(fields) || actual.fieldPresence != legacyNativeNestedW83Initial(fixture).fieldPresence {
		test.Fatal("nested root presence changed or overwrote distinct destination presence", err)
	}
}

type legacyNativeNestedW83Report struct {
	PID               int                                `json:"pid"`
	RestoreSteps      int                                `json:"restore_steps"`
	RecaptureSteps    int                                `json:"recapture_steps"`
	RerestoreSteps    int                                `json:"rerestore_steps"`
	ContinuationSteps int                                `json:"continuation_steps"`
	Retained          int64                              `json:"retained"`
	Initial           legacyNativeNestedW83State         `json:"initial"`
	Recaptured        legacyNativeNestedW83State         `json:"recaptured"`
	Final             legacyNativeNestedW83State         `json:"final"`
	Error             legacyNativeObjectW79Error         `json:"error"`
	Reads             legacyNativeAnyW78Reads            `json:"reads"`
	Rereads           legacyNativeAnyW78Reads            `json:"rereads"`
	Dependencies      []legacyNativeAnyW78DependencyRead `json:"dependencies"`
}

func legacyNativeNestedW83Restore(test *testing.T, request legacyNativeAnyW78Request, source LegacyByteSource, storage *legacyNativeAnyW78CAS) (*legacyNativeObjectRestored, int) {
	test.Helper()
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpointRestore(constructor, request.Root, legacyNativeCheckpointEventModel, storage.io(), source, request.Binding, legacyNativeNestedW83Budget)
	cancel()
	if err != nil {
		test.Fatal("fresh nested Event restore construction", err)
	}
	defer work.close()
	steps := legacyNativeNestedW83Drain(test, work, storage)
	restored, err := work.take(context.Background())
	if err != nil || restored.Object == nil {
		test.Fatal("fresh child failed to restore actual wire/native Event", err)
	}
	legacyNativeAnyW78Released(test, work)
	if source.authority == 0 || restored.Source.authority != source.authority || restored.Source.Read == nil || restored.Source.Bytes != source.Bytes || restored.Source.PartBytes != source.PartBytes || !reflect.DeepEqual(restored.Source.Parts, source.Parts) || request.SuffixOffset != legacyNativeNestedW83Suffix(restored.Object, request.PartBytes) || storage.inputWrites != 0 {
		test.Fatal("nested restore lost the current caller authority, full source geometry, or semantic suffix")
	}
	return &restored, steps
}

func legacyNativeNestedW83PinnedDependencies(test *testing.T, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) map[ContentHash]int {
	test.Helper()
	allowed := make(map[ContentHash]int)
	root, _ := legacyNativeNestedW83ReadControl(test, storage, request.Root)
	for _, field := range root.Fields {
		if field.Nested == nil || field.Nested.Parser == (legacyNativeCheckpointRef{}) {
			continue
		}
		var child legacyNativeObjectCheckpointControl
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), field.Nested.Parser, &child); err != nil {
			test.Fatal("nested dependency child metadata", err)
		}
		if child.Number == (legacyNativeCheckpointRef{}) {
			continue
		}
		var number legacyNativeCheckpointTypedNumberState
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), child.Number, &number); err != nil || number.validate(true) != nil || number.SourceBytes != request.SourceBytes {
			test.Fatal("nested dependency number metadata", err)
		}
		if number.SourceParts.Count == 0 {
			continue
		}
		if number.PartBytes != request.PartBytes || number.SourceParts.Count > uint64(len(request.Parts)) {
			test.Fatal("nested dependency manifest escaped current source geometry")
		}
		reader, err := newLegacyNativeCheckpointListReader[legacyNativeCheckpointNumberSourcePart](number.SourceParts, storage.io())
		if err != nil {
			test.Fatal(err)
		}
		for count := 0; count <= len(request.Parts); count++ {
			part, present, err := reader.next(context.Background())
			if err != nil {
				reader.close()
				test.Fatal(err)
			}
			if !present {
				break
			}
			if count == len(request.Parts) || part.Ordinal < 0 || part.Ordinal >= len(request.Parts) || request.Parts[part.Ordinal].Hash != part.Hash || request.Parts[part.Ordinal].Bytes != int64(part.Bytes) {
				reader.close()
				test.Fatal("nested dependency differs from the current immutable source part", part)
			}
			if part.Ordinal*request.PartBytes >= request.SuffixOffset {
				allowed[part.Hash] = part.Bytes
			}
		}
		reader.close()
	}
	return allowed
}

func legacyNativeNestedW83RunGroup(test *testing.T, group string) {
	test.Helper()
	fixtures := legacyNativeNestedW83Fixtures()
	if len(fixtures) != 12 {
		test.Fatal("finite twelve-process nested schedule changed", len(fixtures))
	}
	cuts := 0
	for _, fixture := range fixtures {
		if fixture.group != group {
			continue
		}
		cuts++
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, original := legacyNativeNestedW83Capture(test, fixture, storage)
			pinned := legacyNativeNestedW83PinnedDependencies(test, request, storage)
			initial := legacyNativeNestedW83StateOf(test, original)
			_, failure := legacyNativeNestedW83Continue(test, original)
			final := legacyNativeNestedW83StateOf(test, original)
			legacyNativeNestedW83Independent(test, fixture, original, failure)
			legacyNativeAnyW78JSON(test, filepath.Join(storage.directory, "request.json"), request)
			current, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(current, os.Args[0], "-test.run=^TestLegacyNativeNestedW83ChildHelper$", "-test.count=1", "-test.timeout=20s")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeNestedW83Directory+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeNestedW83Directory+"="+storage.directory)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("fresh actual nested child failed: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(storage.directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeNestedW83Report
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			if report.PID <= 0 || report.PID == os.Getpid() || report.RestoreSteps <= 0 || report.RestoreSteps > legacyNativeNestedW83CheckpointSteps || report.RecaptureSteps <= 0 || report.RecaptureSteps > legacyNativeNestedW83CheckpointSteps || report.RerestoreSteps <= 0 || report.RerestoreSteps > legacyNativeNestedW83CheckpointSteps || report.ContinuationSteps <= 0 || report.ContinuationSteps > legacyNativeNestedW83SourceSteps || report.Retained <= 0 || report.Retained > legacyNativeNestedW83Budget || report.Reads.Forbidden != 0 || report.Rereads.Forbidden != 0 {
				test.Fatal("missing bounded fresh-process nested restore/recapture/continuation", report.PID, report.RestoreSteps, report.RecaptureSteps, report.RerestoreSteps, report.ContinuationSteps, report.Retained)
			}
			if !reflect.DeepEqual(report.Initial, initial) || !reflect.DeepEqual(report.Recaptured, initial) || !reflect.DeepEqual(report.Final, final) || !reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)) {
				test.Fatalf("nested parent/child/frame/value/presence/alias/error state diverged: initial=%v recaptured=%v final=%v error=%v", reflect.DeepEqual(report.Initial, initial), reflect.DeepEqual(report.Recaptured, initial), reflect.DeepEqual(report.Final, final), reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)))
			}
			if len(report.Reads.Ranges) != 0 || fixture.cut != "done-error" && len(report.Rereads.Ranges) == 0 {
				test.Fatal("nested restore/recapture replayed RAW or nonterminal continuation consumed no suffix", report.Reads, report.Rereads)
			}
			for _, read := range report.Rereads.Ranges {
				if read[0] < request.SuffixOffset || read[0]%request.PartBytes != 0 || read[1] != min(request.PartBytes, request.SourceBytes-read[0]) {
					test.Fatal("fresh nested continuation consumed accepted prefix or nonmanifest source range", read)
				}
			}
			for _, dependency := range report.Dependencies {
				if !dependency.Restoring && pinned[dependency.Hash] != dependency.Bytes {
					test.Fatal("nested semantic continuation read an undeclared or consumed input CAS part", dependency)
				}
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
			test.Logf("actual nested Event child PID=%d restore=%d recapture=%d rerestore=%d continue=%d suffix=%d", report.PID, report.RestoreSteps, report.RecaptureSteps, report.RerestoreSteps, report.ContinuationSteps, request.SuffixOffset)
		})
	}
	if cuts != 3 {
		test.Fatal("finite three-cut nested parent group schedule changed", group, cuts)
	}
	test.Log(legacyNativeNestedW83Unfinished)
}

func legacyNativeNestedW83OldV3Stable(test *testing.T) {
	test.Helper()
	fixture := legacyNativeNestedW83Fixture{name: "nonnested-old-private-v3", raw: `{"id":"stable","seq":7,"agent_message":false,"output":null}`}
	request, bundle := legacyNativeNestedW83Prepare(test, fixture)
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	root, _ := legacyNativeEventW82CaptureRoot(test, bundle, request, storage)
	state, expected := legacyNativeNestedW83ReadControl(test, storage, root)
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonical, expected) {
		test.Fatal("old nonnested private-v3 canonical wire bytes changed", err)
	}
	if state.Version != 3 || state.NestedActive || state.HasNestedCache || bytes.Contains(expected, []byte(`"nested"`)) || bytes.Contains(expected, []byte(`"nested_active"`)) || bytes.Contains(expected, []byte(`"has_nested_cache"`)) {
		test.Fatal("nonnested old private-v3 canonical body gained optional nested fields or a version bump")
	}
	request.Root = root
	reads := &legacyNativeAnyW78Reads{}
	fresh := legacyNativeAnyW78Source(test, request, reads)
	if fresh.authority == bundle.Source.authority {
		test.Fatal("nonnested no-op restore reused the original caller grant")
	}
	restored, _ := legacyNativeNestedW83Restore(test, request, fresh, storage)
	recaptured, _ := legacyNativeNestedW83CaptureRoot(test, restored, request, storage)
	actualState, _ := legacyNativeNestedW83ReadControl(test, storage, recaptured)
	for index, original := range state.Fields {
		actual := actualState.Fields[index]
		capacity := actual.Value.State.Capacity
		if capacity < original.Value.State.Capacity || capacity > 2*original.Value.State.Capacity+64 {
			test.Fatal("restored field capacity escaped conservative allocation bounds", index, capacity)
		}
		actual.Value.State.Capacity = original.Value.State.Capacity
		if !reflect.DeepEqual(actual, original) {
			test.Fatal("nonnested field changed data, references, or semantics beyond current allocation capacity", index)
		}
		actualState.Fields[index] = actual
	}
	if !reflect.DeepEqual(actualState, state) || len(reads.Ranges) != 0 || reads.Forbidden != 0 || !reflect.DeepEqual(legacyNativeNestedW83StateOf(test, bundle), legacyNativeNestedW83StateOf(test, restored)) {
		test.Fatal("nonnested private-v3 lost its unchanged format, current grant, or actual native state")
	}
	legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
	test.Log("Nonnested private-v3 canonical wire format is unchanged; no-op restoration may normalize only actual allocation capacity, not data bytes/references or native semantics. No historical binary compatibility claim.")
}

func TestLegacyNativeNestedCheckpointW83DispatchProcessContinuation(test *testing.T) {
	test.Run("old-nonnested-private-v3-raw-byte-stability", legacyNativeNestedW83OldV3Stable)
	legacyNativeNestedW83RunGroup(test, "dispatch")
}

func TestLegacyNativeNestedCheckpointW83ValueProcessContinuation(test *testing.T) {
	legacyNativeNestedW83RunGroup(test, "values")
}

func TestLegacyNativeNestedCheckpointW83CacheProcessContinuation(test *testing.T) {
	legacyNativeNestedW83RunGroup(test, "cache")
}

func TestLegacyNativeNestedCheckpointW83ErrorProcessContinuation(test *testing.T) {
	legacyNativeNestedW83RunGroup(test, "errors")
}

func TestLegacyNativeNestedW83ChildHelper(test *testing.T) {
	directory := os.Getenv(legacyNativeNestedW83Directory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeAnyW78Request
	if err := json.Unmarshal(body, &request); err != nil || len(request.Parts) == 0 || len(request.Parts) > 4 || request.SourceBytes <= 0 || request.SourceBytes > 4*LegacyNormalizationPartBytes {
		test.Fatal("nested child request escaped finite four-part source geometry", err)
	}
	report := legacyNativeNestedW83Report{PID: os.Getpid()}
	source := legacyNativeAnyW78Source(test, request, &report.Reads)
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request)
	restored, steps := legacyNativeNestedW83Restore(test, request, source, storage)
	report.RestoreSteps, report.Initial = steps, legacyNativeNestedW83StateOf(test, restored)
	root, steps := legacyNativeNestedW83CaptureRoot(test, restored, request, storage)
	report.RecaptureSteps = steps
	if report.Reads.Forbidden != 0 || len(report.Reads.Ranges) != 0 {
		test.Fatal("nested restore or immediate before-first-step recapture replayed original RAW")
	}
	recaptured := request
	recaptured.Root = root
	fresh := legacyNativeAnyW78Source(test, recaptured, &report.Rereads)
	if fresh.authority == source.authority {
		test.Fatal("second nested restore did not receive a genuinely fresh current caller grant")
	}
	continued, steps := legacyNativeNestedW83Restore(test, recaptured, fresh, storage)
	report.RerestoreSteps, report.Recaptured = steps, legacyNativeNestedW83StateOf(test, continued)
	if len(report.Rereads.Ranges) != 0 || report.Rereads.Forbidden != 0 || !reflect.DeepEqual(report.Initial, report.Recaptured) {
		test.Fatal("immediate pre-step recapture changed actual nested state or reread RAW")
	}
	retained, err := continued.Object.retained()
	if err != nil || retained <= 0 || retained > legacyNativeNestedW83Budget {
		test.Fatal("restored actual parent/child storage escaped current admission", retained, err)
	}
	report.Retained = retained
	storage.restoring = false
	var failure error
	report.ContinuationSteps, failure = legacyNativeNestedW83Continue(test, continued)
	report.Final, report.Error = legacyNativeNestedW83StateOf(test, continued), legacyNativeObjectW79ErrorState(failure)
	report.Dependencies = storage.dependencies
	if storage.inputWrites != 0 {
		test.Fatal("fresh nested child rewrote immutable original source CAS")
	}
	legacyNativeAnyW78JSON(test, filepath.Join(directory, "report.json"), report)
}
