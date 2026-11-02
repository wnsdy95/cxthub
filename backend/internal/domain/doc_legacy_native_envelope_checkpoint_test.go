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
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"
)

const legacyNativeEnvelopeW80Directory = "CXTHUB_W80_NATIVE_ENVELOPE_DIRECTORY"
const legacyNativeEnvelopeW80Budget int64 = 64 << 20
const legacyNativeEnvelopeW80Steps = 4096

type legacyNativeEnvelopeW80Fixture struct {
	name   string
	raw    string
	cut    string
	prior  []string
	length int
}

func legacyNativeEnvelopeW80Fixtures() []legacyNativeEnvelopeW80Fixture {
	width := LegacyNormalizationPartBytes
	longText := strings.Repeat("\ud55c🙂", width/3) + `\n\uD83D\uDE42\uD800x\uDC00\"\\tail`
	hidden := []string{"prior-first", "prior-second", "hidden-third", "hidden-fourth"}
	growth := make([]string, 2048)
	for index := range growth {
		growth[index] = fmt.Sprintf("hidden-model-%04d", index)
	}
	maximum := int(^uint(0) >> 1)
	fixtures := []legacyNativeEnvelopeW80Fixture{
		{name: "list-partial-utf8-scan", raw: `{"source_models":["` + longText + `",null,"tail"],"source_model":"after-scan"`, cut: "scan"},
		{name: "list-unquote-ahead-of-parent", raw: `{"source_models":["` + strings.Repeat("a", 2*width) + `\n\uD83D\uDE42tail",null],"cwd":"after-unquote"`, cut: "unquote"},
		{name: "list-scalar-buffer-migration", raw: `{"source_models":["` + strings.Repeat("\xff", 2*width) + `"],"cwd":"after-scalar-growth"`, cut: "scalar-growth"},
		{name: "partial-prior-capacity-seed", raw: `{"source_models":[null,null,null],"cwd":"after-seed"`, cut: "seed", prior: hidden, length: 2},
		{name: "list-growth-previous-binding", raw: `{"source_models":[` + strings.Repeat("null,", len(growth)) + `null],"cwd":"after-growth"`, cut: "list-growth", prior: growth, length: 2},
		{name: "inactive-cache-hidden-tail-reuse", raw: `{"source_models":["fresh"],"cwd":"between","source_models":[null,null,null,null],"source_model":"after-reuse"`, cut: "cached", prior: hidden, length: 2},
		{name: "duplicate-whole-list-null", raw: `{"source_models":["first"],"source_models":null,"cwd":"after-null"`, cut: "null-list"},
		{name: "duplicate-empty-is-not-nil", raw: `{"source_models":["first"],"source_models":[],"cwd":"after-empty"`, cut: "empty"},
		{name: "planned-list-dispatch-hidden-capacity", raw: `{"source_models":[null,null,null,null],"cwd":"after-hidden"`, cut: "planned-list-dispatch", prior: hidden},
		{name: "signed-integers-null-retain", raw: `{"context_tokens":` + strconv.Itoa(-maximum-1) + `,"output_tokens":` + strconv.Itoa(maximum) + `,"compaction_count":-3,"context_tokens":null,"output_tokens":null,"compaction_count":null,"source_models":["after-integers"]`, cut: "integer-null"},
		{name: "remembered-error-later-valid-fields", raw: `{"context_tokens":false,"context_tokens":-17,"output_tokens":23,"source_models":["continued"],"cwd":"after-error"`, cut: "type-error"},
		{name: "string-scalar-with-inactive-list", raw: `{"source_models":["before",null],"source_model":"` + longText + `","source_models":[null,"after"],"source_model":null`, cut: "object-scalar"},
	}
	for index := range fixtures {
		fixtures[index].raw += `,"cir_version":"2","source_provider":"codex","captured_at":"2026-10-29T00:00:00Z","git_branch":"main","session_origin_id":"w80-envelope","fidelity":"full"}`
	}
	return fixtures
}

func legacyNativeEnvelopeW80Initial(fixture legacyNativeEnvelopeW80Fixture) *CIREnvelope {
	value := &CIREnvelope{}
	if fixture.prior != nil {
		value.SourceModels = make([]string, len(fixture.prior))
		copy(value.SourceModels, fixture.prior)
		value.SourceModels = value.SourceModels[:fixture.length]
	}
	return value
}

func legacyNativeEnvelopeW80List(native *legacyNativeObject) *legacyNativeStringList {
	index := native.metadata.match("source_models")
	if index < 0 || index >= len(native.lists) {
		return nil
	}
	return native.lists[index]
}

func legacyNativeEnvelopeW80Stop(native *legacyNativeObject, fixture legacyNativeEnvelopeW80Fixture) bool {
	list := legacyNativeEnvelopeW80List(native)
	switch fixture.cut {
	case "scan":
		return native.listActive && list != nil && list.phase == legacyNativeStringsString && list.position >= LegacyNormalizationPartBytes
	case "unquote":
		return native.listActive && list != nil && list.scalar != nil && list.scalar.text != nil && list.scalar.text.Len() > 0 && list.scalar.position >= LegacyNormalizationPartBytes && list.scalar.position < list.scalar.end
	case "scalar-growth":
		return native.listActive && list != nil && list.scalar != nil && list.scalar.previous != nil
	case "seed":
		return native.listActive && list != nil && !list.initialized && list.seeded > 0 && list.seeded < cap(list.decoded)
	case "list-growth":
		return native.listActive && list != nil && list.previous != nil && list.copied > 0 && list.copied < len(list.previous)
	case "cached":
		return !native.listActive && list != nil && list.phase == legacyNativeStringsDone && native.phase == legacyNativeObjectAfter && len(list.decoded) == 1 && list.decoded[0] == "fresh"
	case "null-list":
		return native.listActive && list != nil && list.nullList && list.phase == legacyNativeStringsNull && list.literal > 0 && list.literal < len("null")
	case "empty":
		return !native.listActive && list != nil && list.phase == legacyNativeStringsDone && native.phase == legacyNativeObjectAfter && list.decoded != nil && len(list.decoded) == 0 && cap(list.decoded) == 0
	case "planned-list-dispatch":
		return !native.listActive && list == nil && native.phase == legacyNativeObjectApply && native.field == native.metadata.match("source_models") && native.valueStarted && native.valueFirst == '[' && native.position == native.valueStart && native.valueEnd == native.end
	case "integer-null":
		return native.phase == legacyNativeObjectAfter && native.field == native.metadata.match("context_tokens") && native.valueFirst == 'n'
	case "type-error":
		return native.typeError != nil && native.phase == legacyNativeObjectAfter
	case "object-scalar":
		return list != nil && !native.listActive && native.scalar != nil && native.scalar.text != nil && native.scalar.text.Len() > 0 && native.scalar.position >= LegacyNormalizationPartBytes && native.scalar.position < native.scalar.end
	default:
		return false
	}
}

func legacyNativeEnvelopeW80Suffix(native *legacyNativeObject, width int) int {
	if native.listActive {
		list := legacyNativeEnvelopeW80List(native)
		if list == nil {
			return -1
		}
		position := list.position
		if list.phase == legacyNativeStringsString {
			position = min(position, list.stringStart)
		}
		if list.scalar != nil {
			position = min(position, list.scalar.position)
		}
		return position / width * width
	}
	return legacyNativeObjectW79Suffix(native, width)
}

type legacyNativeEnvelopeW80Array struct {
	Nil      bool     `json:"nil"`
	Length   int      `json:"length"`
	Capacity int      `json:"capacity"`
	Values   []string `json:"values"`
}

func legacyNativeEnvelopeW80ArrayOf(values []string) legacyNativeEnvelopeW80Array {
	state := legacyNativeEnvelopeW80Array{Nil: values == nil, Length: len(values), Capacity: cap(values)}
	if values != nil {
		state.Values = make([]string, cap(values))
		copy(state.Values, values[:cap(values)])
	}
	return state
}

func legacyNativeEnvelopeW80Binding(values, backing []string) bool {
	return cap(values) > 0 && cap(backing) > 0 && unsafe.SliceData(values[:cap(values)]) == unsafe.SliceData(backing[:cap(backing)])
}

type legacyNativeEnvelopeW80ListState struct {
	Control  []int                        `json:"control"`
	Flags    []bool                       `json:"flags"`
	Decoded  legacyNativeEnvelopeW80Array `json:"decoded"`
	Previous legacyNativeEnvelopeW80Array `json:"previous"`
	Scalar   []string                     `json:"scalar"`
	Binding  int                          `json:"binding"`
}

type legacyNativeEnvelopeW80State struct {
	Control     []int                             `json:"control"`
	Flags       []bool                            `json:"flags"`
	Key         []byte                            `json:"key"`
	Scanner     []byte                            `json:"scanner"`
	Value       CIREnvelope                       `json:"value"`
	Destination legacyNativeEnvelopeW80Array      `json:"destination"`
	List        *legacyNativeEnvelopeW80ListState `json:"list"`
	Scalar      []string                          `json:"scalar"`
	Error       legacyNativeObjectW79Error        `json:"error"`
}

func legacyNativeEnvelopeW80StateOf(test *testing.T, native *legacyNativeObject, scanner *legacyJSONValueScanner) legacyNativeEnvelopeW80State {
	test.Helper()
	value, ok := native.destination.Interface().(*CIREnvelope)
	if !ok || native.metadata != &legacyEnvelopeNativeFields {
		test.Fatal("checkpoint lost actual concrete envelope")
	}
	scannerBytes, err := scanner.checkpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	state := legacyNativeEnvelopeW80State{
		Control: []int{native.parentDepth, native.fieldLimit, native.position, native.end, native.valueStart, native.valueEnd, native.field, int(native.presence), int(native.phase), int(native.valueFirst), cap(native.key), cap(native.raw)},
		Flags:   []bool{native.trackPresence, native.pagedEvents, native.deferTypeErrors, native.skipValue, native.prefix, native.keyStarted, native.keyOverflow, native.valueStarted, native.listActive},
		Key:     bytes.Clone(native.key), Scanner: scannerBytes, Value: *value,
		Destination: legacyNativeEnvelopeW80ArrayOf(value.SourceModels), Error: legacyNativeObjectW79ErrorState(native.typeError),
	}
	state.Value.SourceModels = nil
	if native.scalar != nil {
		state.Scalar = legacyNativeAnyW78ScalarState(test, native.scalar)
	}
	if list := legacyNativeEnvelopeW80List(native); list != nil {
		state.List = &legacyNativeEnvelopeW80ListState{
			Control: []int{list.position, list.end, list.maximum, list.count, list.copied, list.stringStart, list.literal, int(list.phase)},
			Flags:   []bool{list.escaped, list.nullList, list.prefix},
			Decoded: legacyNativeEnvelopeW80ArrayOf(list.decoded), Previous: legacyNativeEnvelopeW80ArrayOf(list.previous),
		}
		if legacyNativeEnvelopeW80Binding(value.SourceModels, list.decoded) {
			state.List.Binding = 1
		} else if legacyNativeEnvelopeW80Binding(value.SourceModels, list.previous) {
			state.List.Binding = 2
		}
		if list.scalar != nil {
			state.List.Scalar = legacyNativeAnyW78ScalarState(test, list.scalar)
		}
	}
	return state
}

func legacyNativeEnvelopeW80Heap(test *testing.T, native *legacyNativeObject, restored bool) int64 {
	test.Helper()
	retained, err := native.retained()
	if err != nil || retained <= 0 || retained > legacyNativeEnvelopeW80Budget {
		test.Fatal("envelope omitted native backing or exceeded admitted budget", retained, err)
	}
	list := legacyNativeEnvelopeW80List(native)
	if !restored || list == nil {
		return retained
	}
	if !list.initialized || list.seeded != cap(list.decoded) || len(list.capacities) != cap(list.decoded) || len(list.previousCaps) != cap(list.previous) {
		test.Fatal("restored envelope list metadata not fully seeded")
	}
	var payload docRangeHeaderHeapEstimate
	for index, text := range list.decoded[:cap(list.decoded)] {
		if list.capacities[index] < len(text) {
			test.Fatal("restored slot capacity omitted its materialized text", index)
		}
		if list.previous == nil || index < list.copied || index >= len(list.previous) {
			payload.allocation(int64(list.capacities[index]), 1)
		}
	}
	for index, text := range list.previous[:cap(list.previous)] {
		if list.previousCaps[index] < len(text) {
			test.Fatal("restored previous slot capacity omitted text", index)
		}
		if index >= list.copied {
			payload.allocation(int64(list.previousCaps[index]), 1)
		} else if text != "" && (unsafe.StringData(text) != unsafe.StringData(list.decoded[index]) || list.previousCaps[index] != list.capacities[index]) {
			test.Fatal("copied prefix lost its restored previous string alias", index)
		}
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	expected.include(legacyNativeStringsBacking(cap(list.decoded)))
	expected.include(payload)
	if payload.err != nil || expected.err != nil || list.payload.err != nil || list.estimate.err != nil || list.payload.bytes != payload.bytes || list.estimate.bytes != expected.bytes {
		test.Fatal("restored list trusts saved heap instead of reconstructed physical capacities", list.payload, payload, list.estimate, expected)
	}
	if native.listActive && (cap(list.decoded) > 0 && !list.owned || cap(list.previous) > 0 && !list.previousOwned || list.added.err != nil || list.added.bytes < payload.bytes) {
		test.Fatal("active restored list dropped owned backing or logical body charges")
	}
	if !native.listActive {
		index := native.metadata.match("source_models")
		if native.heaps[index].err != nil || native.heaps[index].bytes < expected.bytes {
			test.Fatal("inactive cached list payload not transferred to envelope field heap")
		}
	}
	return retained
}

func legacyNativeEnvelopeW80Continue(test *testing.T, native *legacyNativeObject, scanner *legacyJSONValueScanner, source LegacyByteSource) (int, error) {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeEnvelopeW80Steps; attempt++ {
		current, cancel := context.WithCancel(context.Background())
		complete, failure := legacyNativeObjectW79Step(test, current, native, scanner, source, legacyNativeNumberDiagnosticsFull, false)
		cancel()
		legacyNativeEnvelopeW80Heap(test, native, false)
		if complete || failure != nil {
			if native.phase != legacyNativeObjectDone || native.position != native.end || native.typeError != nil && failure != native.typeError {
				test.Fatal("envelope continuation lost final cursor or remembered error identity", native.position, failure)
			}
			return attempt, failure
		}
	}
	test.Fatal("envelope continuation exceeded finite schedule")
	return 0, ErrIntegrity
}

func legacyNativeEnvelopeW80Immutable(test *testing.T, storage *legacyNativeAnyW78CAS, request legacyNativeAnyW78Request, raw string) {
	test.Helper()
	for ordinal, part := range request.Parts {
		filename, err := storage.objectPath(part.Hash)
		if err != nil {
			test.Fatal(err)
		}
		body, err := os.ReadFile(filename)
		start := ordinal * request.PartBytes
		if err != nil || len(body) != int(part.Bytes) || HashContent(body) != part.Hash || !bytes.Equal(body, []byte(raw[start:start+int(part.Bytes)])) {
			test.Fatal("checkpoint changed immutable original RAW CAS", ordinal, err)
		}
	}
}

func legacyNativeEnvelopeW80CutStep(test *testing.T, current context.Context, native *legacyNativeObject, scanner *legacyJSONValueScanner, source LegacyByteSource, dispatch bool) (bool, error) {
	test.Helper()
	if !dispatch {
		return legacyNativeObjectW79Step(test, current, native, scanner, source, legacyNativeNumberDiagnosticsFull, true)
	}
	input := &legacyNormalizationInput{ctx: current, source: source, numberDiagnostics: legacyNativeNumberDiagnosticsFull, workBytes: ConversationManifestChunkBytes - 1, loads: 1}
	complete, failure := native.step(current, input, scanner)
	if input.failure != nil && !errors.Is(input.failure, context.Canceled) || input.unbounded || input.workBytes < ConversationManifestChunkBytes-1 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.fields > native.fieldActionLimit() {
		test.Fatal("planned string dispatch escaped its actual one-byte shared quantum", input.failure, input.workBytes, input.loads, input.fields)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, failure
}

func legacyNativeEnvelopeW80Capture(test *testing.T, fixture legacyNativeEnvelopeW80Fixture, storage *legacyNativeAnyW78CAS) (legacyNativeAnyW78Request, *legacyNativeObject, legacyJSONValueScanner, LegacyByteSource) {
	test.Helper()
	request, source := legacyNativeAnyW78FullSource(test, fixture.raw)
	request.Binding = HashContent([]byte("W80 actual envelope " + fixture.name))
	if len(request.Parts) > 4 {
		test.Fatal("envelope fixture exceeds finite four-part source bound")
	}
	storage.preload(test, request)
	value := legacyNativeEnvelopeW80Initial(fixture)
	native, err := newLegacyNativeObject(reflect.ValueOf(value), &legacyEnvelopeNativeFields, 1, false)
	if err != nil {
		test.Fatal(err)
	}
	if value.SourceModels != nil {
		heap, failure := legacyNativeValueHeap(context.Background(), reflect.ValueOf(value.SourceModels))
		if failure != nil {
			test.Fatal(failure)
		}
		native.heaps[native.metadata.match("source_models")] = heap
	}
	native.start(0, len(fixture.raw))
	native.fieldLimit, native.deferTypeErrors = 1, true
	var scanner legacyJSONValueScanner
	for attempt := 0; attempt < legacyNativeEnvelopeW80Steps && !legacyNativeEnvelopeW80Stop(native, fixture); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		cut := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeEnvelopeW80Stop(native, fixture) }}
		complete, failure := legacyNativeEnvelopeW80CutStep(test, cut, native, &scanner, source, fixture.cut == "planned-list-dispatch")
		cancel()
		if complete || failure != nil && !errors.Is(failure, context.Canceled) {
			test.Fatal("envelope fixture failed before its actual native cut", fixture.name, native.position, failure)
		}
	}
	if !legacyNativeEnvelopeW80Stop(native, fixture) {
		test.Fatal("envelope fixture never reached assigned concrete cut", fixture.name, native.position)
	}
	if fixture.cut == "planned-list-dispatch" && scanner.complete {
		test.Fatal("planned dispatch fixture bypassed the actual incomplete-scanner frontier")
	}
	before := legacyNativeEnvelopeW80StateOf(test, native, &scanner)
	captureReads := 0
	captureSource := source
	captureSource.Read = func(current context.Context, offset, length int) ([]byte, error) {
		captureReads++
		return source.Read(current, offset, length)
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpoint(constructor, native, &scanner, request.Binding, captureSource, storage.io(), legacyNativeEnvelopeW80Budget)
	cancel()
	if err != nil {
		test.Fatal("actual envelope checkpoint construction", err)
	}
	defer work.close()
	legacyNativeAnyW78Drain(test, work, storage, legacyNativeEnvelopeW80Budget)
	request.Root, err = work.take(context.Background())
	if err != nil || !request.Root.valid() {
		test.Fatal("actual envelope checkpoint did not hand off a root", err)
	}
	legacyNativeAnyW78Released(test, work)
	if captureReads != 0 || !reflect.DeepEqual(before, legacyNativeEnvelopeW80StateOf(test, native, &scanner)) {
		test.Fatal("capture replayed RAW or mutated borrowed envelope/list/scanner", captureReads)
	}
	legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
	request.SuffixOffset = legacyNativeEnvelopeW80Suffix(native, request.PartBytes)
	if request.SuffixOffset < 0 || request.SuffixOffset >= request.SourceBytes {
		test.Fatal("actual envelope cut has no finite future source suffix")
	}
	if fixture.cut == "unquote" && request.SuffixOffset <= native.position/request.PartBytes*request.PartBytes {
		test.Fatal("active list semantic cursor incorrectly pinned to stale parent position")
	}
	request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
	return request, native, scanner, source
}

type legacyNativeEnvelopeW80Report struct {
	PID               int                                `json:"pid"`
	RestoreSteps      int                                `json:"restore_steps"`
	ContinuationSteps int                                `json:"continuation_steps"`
	Retained          int64                              `json:"retained"`
	Initial           legacyNativeEnvelopeW80State       `json:"initial"`
	Final             legacyNativeEnvelopeW80State       `json:"final"`
	Error             legacyNativeObjectW79Error         `json:"error"`
	Reads             legacyNativeAnyW78Reads            `json:"reads"`
	Dependencies      []legacyNativeAnyW78DependencyRead `json:"dependencies"`
}

func TestLegacyNativeEnvelopeCheckpointW80ProcessContinuation(test *testing.T) {
	fixtures := legacyNativeEnvelopeW80Fixtures()
	if len(fixtures) != 12 {
		test.Fatal("finite envelope schedule changed without audit", len(fixtures))
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, native, scanner, source := legacyNativeEnvelopeW80Capture(test, fixture, storage)
			initial := legacyNativeEnvelopeW80StateOf(test, native, &scanner)
			_, failure := legacyNativeEnvelopeW80Continue(test, native, &scanner, source)
			final := legacyNativeEnvelopeW80StateOf(test, native, &scanner)
			independent := legacyNativeEnvelopeW80Initial(fixture)
			independentError := json.Unmarshal([]byte(fixture.raw), independent)
			if !reflect.DeepEqual(native.destination.Interface(), independent) || !reflect.DeepEqual(legacyNativeObjectW79ErrorState(failure), legacyNativeObjectW79ErrorState(independentError)) {
				test.Fatal("actual native envelope differs from independent encoding/json", failure, independentError)
			}
			legacyNativeAnyW78JSON(test, filepath.Join(storage.directory, "request.json"), request)
			current, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(current, os.Args[0], "-test.run=^TestLegacyNativeEnvelopeW80ChildHelper$", "-test.count=1", "-test.timeout=20s")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeEnvelopeW80Directory+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeEnvelopeW80Directory+"="+storage.directory)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("fresh native envelope child failed: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(storage.directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeEnvelopeW80Report
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			if report.PID <= 0 || report.PID == os.Getpid() || report.RestoreSteps <= 0 || report.ContinuationSteps <= 0 || report.Retained <= 0 || report.Retained > legacyNativeEnvelopeW80Budget || report.Reads.Forbidden != 0 || len(report.Reads.Ranges) == 0 {
				test.Fatal("missing actual fresh-process suffix-only envelope continuation", report.PID, report.RestoreSteps, report.ContinuationSteps, report.Reads)
			}
			if !reflect.DeepEqual(report.Initial, initial) || !reflect.DeepEqual(report.Final, final) || !reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)) {
				test.Fatalf("envelope/scanner/hidden-backing/alias/scalar/error diverged: initial=%v final=%v error=%v", reflect.DeepEqual(report.Initial, initial), reflect.DeepEqual(report.Final, final), reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)))
			}
			for _, read := range report.Reads.Ranges {
				if read[0] < request.SuffixOffset || read[0]%request.PartBytes != 0 || read[1] != min(request.PartBytes, request.SourceBytes-read[0]) {
					test.Fatal("envelope child read consumed prefix or nonmanifest range", read)
				}
			}
			for _, dependency := range report.Dependencies {
				if !dependency.Restoring {
					test.Fatal("envelope continued semantics through original CAS prefix", dependency)
				}
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
			binding := 0
			if initial.List != nil {
				binding = initial.List.Binding
			}
			test.Logf("actual envelope child=%d restore=%d continue=%d suffix=%d binding=%d", report.PID, report.RestoreSteps, report.ContinuationSteps, request.SuffixOffset, binding)
		})
	}
	test.Log("Envelope/string-list continuation only. Full Event blocks/replacement, presence/encoder/four spools/sort/proof, atomic stage/worker integration, and original performance gates remain explicitly unfinished.")
}

func TestLegacyNativeEnvelopeW80ChildHelper(test *testing.T) {
	directory := os.Getenv(legacyNativeEnvelopeW80Directory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeAnyW78Request
	if err := json.Unmarshal(body, &request); err != nil || len(request.Parts) > 4 || request.SourceBytes > 4*LegacyNormalizationPartBytes {
		test.Fatal("child envelope request exceeds finite fixture", err)
	}
	report := legacyNativeEnvelopeW80Report{PID: os.Getpid()}
	source := legacyNativeAnyW78Source(test, request, &report.Reads)
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request)
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpointRestore(constructor, request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), source, request.Binding, legacyNativeEnvelopeW80Budget)
	cancel()
	if err != nil {
		test.Fatal("child actual envelope restore construction", err)
	}
	defer work.close()
	report.RestoreSteps = legacyNativeAnyW78Drain(test, work, storage, legacyNativeEnvelopeW80Budget)
	restored, err := work.take(context.Background())
	if err != nil || restored.Object == nil || restored.Object.metadata != &legacyEnvelopeNativeFields {
		test.Fatal("child did not restore the actual envelope model", err)
	}
	legacyNativeAnyW78Released(test, work)
	if restored.Source.authority == 0 || restored.Source.authority != source.authority || restored.Source.Read == nil || restored.Source.Bytes != source.Bytes || restored.Source.PartBytes != source.PartBytes || !reflect.DeepEqual(restored.Source.Parts, source.Parts) {
		test.Fatal("restored envelope reused saved authority instead of fresh current source")
	}
	if request.SuffixOffset != legacyNativeEnvelopeW80Suffix(restored.Object, request.PartBytes) || report.Reads.Forbidden != 0 || len(report.Reads.Ranges) != 0 {
		test.Fatal("restore replayed RAW or restored wrong true future semantic cursor")
	}
	report.Retained = legacyNativeEnvelopeW80Heap(test, restored.Object, true)
	report.Initial = legacyNativeEnvelopeW80StateOf(test, restored.Object, &restored.Scanner)
	storage.restoring = false
	var failure error
	report.ContinuationSteps, failure = legacyNativeEnvelopeW80Continue(test, restored.Object, &restored.Scanner, restored.Source)
	report.Final = legacyNativeEnvelopeW80StateOf(test, restored.Object, &restored.Scanner)
	report.Error = legacyNativeObjectW79ErrorState(failure)
	report.Dependencies = storage.dependencies
	legacyNativeAnyW78JSON(test, filepath.Join(directory, "report.json"), report)
}

func legacyNativeEnvelopeW80Reject(test *testing.T, request legacyNativeAnyW78Request, source LegacyByteSource, binding ContentHash, model uint8, budget int64, storage legacyNativeCheckpointIO) error {
	test.Helper()
	work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Root, model, storage, source, binding, budget)
	if err != nil {
		if work != nil {
			legacyNativeAnyW78Released(test, work)
		}
		return err
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeEnvelopeW80Steps; attempt++ {
		ready, failure := work.step(context.Background())
		if failure != nil {
			legacyNativeObjectW79Failed[legacyNativeObjectRestored](test, work, failure)
			return failure
		}
		if ready {
			test.Fatal("invalid envelope checkpoint quietly became ready")
		}
	}
	test.Fatal("invalid envelope checkpoint did not fail finitely")
	return nil
}

func TestLegacyNativeEnvelopeCheckpointW80Validation(test *testing.T) {
	test.Run("done-cache-unbound-destination", func(test *testing.T) {
		storage := legacyNativeAnyW78Store(test)
		request, _, _, source := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[5], storage)
		var state legacyNativeObjectCheckpointControl
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil {
			test.Fatal(err)
		}
		index := legacyEnvelopeNativeFields.match("source_models")
		state.Fields[index].Strings = legacyNativeCheckpointStrings{Nil: true}
		state.Fields[index].StringBinding = 0
		state.Fields[index].Present, state.Fields[index].Initialized = false, false
		var err error
		request.Root, err = legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), state)
		if err != nil {
			test.Fatal(err)
		}
		if err := legacyNativeEnvelopeW80Reject(test, request, source, request.Binding, legacyNativeCheckpointEnvelopeModel, legacyNativeEnvelopeW80Budget, storage.io()); err == nil {
			test.Fatal("nonempty cached backing escaped destination ownership")
		}
	})
	storage := legacyNativeAnyW78Store(test)
	request, _, _, source := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	rootBody, err := storage.io().Read(context.Background(), request.Root.Hash, request.Root.Bytes)
	if err != nil {
		test.Fatal(err)
	}
	var original legacyNativeObjectCheckpointControl
	if err := json.Unmarshal(rootBody, &original); err != nil || original.Version != 3 || !original.ListActive || !original.HasStringCache || len(original.Fields) != len(legacyEnvelopeNativeFields.fields) {
		test.Fatal("actual envelope did not use version-three bounded object schema", err)
	}
	for _, name := range []string{"binding", "expected-model", "unfinished-event", "schema", "version", "cursor", "phase", "field", "source-size", "no-authority", "manifest", "unknown-key", "duplicate-key", "fields-over-bound", "root-hash", "root-short"} {
		test.Run(name, func(test *testing.T) {
			changed, current, binding, model := request, source, request.Binding, legacyNativeCheckpointEnvelopeModel
			body := bytes.Clone(rootBody)
			state := original
			rootOnly := false
			switch name {
			case "binding":
				binding, rootOnly = HashContent([]byte("different W80 envelope binding")), true
			case "expected-model":
				model, rootOnly = legacyNativeCheckpointBlockModel, true
			case "unfinished-event":
				model, rootOnly = legacyNativeCheckpointEventModel, true
			case "source-size":
				current.Bytes--
			case "no-authority":
				current.authority = 0
			case "manifest":
				current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
				current.Parts[0].Bytes--
			case "schema", "version", "cursor", "phase", "field":
				rootOnly = true
				switch name {
				case "schema":
					state.Schema = HashContent([]byte("different closed envelope schema"))
				case "version":
					state.Version = 1
				case "cursor":
					state.Position = state.End + 1
				case "phase":
					state.Phase = 255
				case "field":
					state.Field = len(legacyEnvelopeNativeFields.fields)
				}
				body, err = json.Marshal(state)
			case "unknown-key", "duplicate-key":
				rootOnly = true
				addition := `,"unexpected":0}`
				if name == "duplicate-key" {
					addition = `,"model":1}`
				}
				body = append(body[:len(body)-1], addition...)
			case "fields-over-bound":
				rootOnly = true
				rows, failure := json.Marshal(original.Fields)
				if failure != nil || len(original.Fields) == 0 {
					test.Fatal("missing actual envelope field rows", failure)
				}
				row, failure := json.Marshal(original.Fields[0])
				if failure != nil {
					test.Fatal(failure)
				}
				values := bytes.Repeat(append(bytes.Clone(row), ','), 32)
				values = append(values, row...)
				replacement := append([]byte(`"fields":[`), values...)
				replacement = append(replacement, ']')
				needle := append([]byte(`"fields":`), rows...)
				if bytes.Count(body, needle) != 1 {
					test.Fatal("ambiguous actual envelope fields")
				}
				body = bytes.Replace(body, needle, replacement, 1)
				var decoded legacyNativeObjectCheckpointControl
				if failure := json.Unmarshal(body, &decoded); failure == nil || cap(decoded.Fields) > 32 {
					test.Fatal("envelope field decoder exceeded pre-allocation row bound", cap(decoded.Fields), failure)
				}
			}
			if err != nil || len(body) > LegacyCanonicalEncodingBytes {
				test.Fatal("adversarial envelope root exceeds bounded metadata", len(body), err)
			}
			if !bytes.Equal(body, rootBody) {
				changed.Root, err = legacyNativeCheckpointWrite(context.Background(), storage.io(), body)
				if err != nil {
					test.Fatal(err)
				}
			}
			storageIO := storage.io()
			readOriginal := storageIO.Read
			dependentReads := 0
			storageIO.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
				if rootOnly && hash != changed.Root.Hash {
					dependentReads++
					return nil, ErrConflict
				}
				body, failure := readOriginal(current, hash, length)
				if failure == nil && hash == changed.Root.Hash {
					if name == "root-hash" {
						body[0] ^= 1
					} else if name == "root-short" {
						body = body[:len(body)-1]
					}
				}
				return body, failure
			}
			failure := legacyNativeEnvelopeW80Reject(test, changed, current, binding, model, legacyNativeEnvelopeW80Budget, storageIO)
			if failure == nil || errors.Is(failure, ErrConflict) || dependentReads != 0 {
				test.Fatal("bad envelope root reached dependent data before validation", failure, dependentReads)
			}
		})
	}
	index := legacyEnvelopeNativeFields.match("source_models")
	parserRef := original.Fields[index].StringParser
	if !parserRef.valid() {
		test.Fatal("actual active list missing from envelope root")
	}
	parserBody, err := storage.io().Read(context.Background(), parserRef.Hash, parserRef.Bytes)
	if err != nil {
		test.Fatal(err)
	}
	var saved legacyNativeCheckpointStringListState
	if err := json.Unmarshal(parserBody, &saved); err != nil || !saved.HasScalar {
		test.Fatal("expected actual active list scalar metadata", err)
	}
	for _, name := range []string{"cursor", "end", "phase", "count", "copied", "capacity", "length", "items-count", "scalar-cursor", "scalar-range", "scalar-phase", "parser-hash", "parser-short", "binding", "alias-shape", "inactive-not-done"} {
		test.Run("list-"+name, func(test *testing.T) {
			state := saved
			root := original
			root.Fields = append(legacyNativeObjectCheckpointFields(nil), original.Fields...)
			changed := request
			storageIO := storage.io()
			switch name {
			case "cursor":
				state.Position = state.SourceBytes + 1
			case "end":
				state.End = state.Position - 1
			case "phase":
				state.Phase = 255
			case "count":
				state.Count = state.Decoded.Capacity + 1
			case "copied":
				state.Copied = state.Previous.Capacity + 1
			case "capacity":
				state.Decoded.Capacity = MaxFinalizedDocBytes + 1
			case "length":
				state.Decoded.Length = state.Decoded.Capacity + 1
			case "items-count":
				state.Decoded.Items.Count++
			case "scalar-cursor":
				state.Scalar.State.Position = state.SourceBytes + 1
			case "scalar-range":
				state.Scalar.State.End = state.SourceBytes + 1
			case "scalar-phase":
				state.Phase = legacyNativeStringsStart
			case "binding":
				root.Fields[index].StringBinding = 3
			case "alias-shape":
				root.Fields[index].StringBinding = 1
				root.Fields[index].Strings.Nil = false
				root.Fields[index].Strings.Capacity = state.Decoded.Capacity + 1
			case "inactive-not-done":
				root.ListActive = false
			case "parser-hash", "parser-short":
				readOriginal := storageIO.Read
				storageIO.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
					body, failure := readOriginal(current, hash, length)
					if failure == nil && hash == parserRef.Hash {
						if name == "parser-hash" {
							body[0] ^= 1
						} else {
							body = body[:len(body)-1]
						}
					}
					return body, failure
				}
			}
			if name != "parser-hash" && name != "parser-short" {
				body, failure := json.Marshal(state)
				if failure != nil {
					test.Fatal(failure)
				}
				root.Fields[index].StringParser, failure = legacyNativeCheckpointWrite(context.Background(), storage.io(), body)
				if failure != nil {
					test.Fatal(failure)
				}
				changed.Root, failure = legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), root)
				if failure != nil {
					test.Fatal(failure)
				}
			}
			if failure := legacyNativeEnvelopeW80Reject(test, changed, source, request.Binding, legacyNativeCheckpointEnvelopeModel, legacyNativeEnvelopeW80Budget, storageIO); failure == nil {
				test.Fatal("invalid envelope/list/scalar binding accepted")
			}
		})
	}
}

func legacyNativeEnvelopeW80Lifecycle[Value any](test *testing.T, storage *legacyNativeAnyW78CAS, factory func(context.Context, legacyNativeCheckpointIO, int64) (legacyNativeAnyCheckpointTask[Value], error)) {
	test.Helper()
	for _, phase := range []string{"constructor", "step", "io", "take"} {
		test.Run("cancel-"+phase, func(test *testing.T) {
			current, cancel := context.WithCancel(context.Background())
			defer cancel()
			storageIO := storage.io()
			if phase == "io" {
				put, read := storageIO.Put, storageIO.Read
				storageIO.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
					failure := put(ctx, hash, body)
					cancel()
					return failure
				}
				storageIO.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
					body, failure := read(ctx, hash, length)
					cancel()
					return body, failure
				}
			}
			if phase == "constructor" {
				cancel()
				work, failure := factory(current, storageIO, legacyNativeEnvelopeW80Budget)
				if work != nil || !errors.Is(failure, context.Canceled) {
					if work != nil {
						work.close()
					}
					test.Fatal("envelope constructor ignored cancellation", failure)
				}
				return
			}
			constructor, retire := context.WithCancel(context.Background())
			work, failure := factory(constructor, storageIO, legacyNativeEnvelopeW80Budget)
			retire()
			if failure != nil {
				test.Fatal(failure)
			}
			defer work.close()
			if phase == "take" {
				legacyNativeAnyW78Drain(test, work, storage, legacyNativeEnvelopeW80Budget)
				cancel()
				value, failure := work.take(current)
				var zero Value
				if !reflect.DeepEqual(value, zero) || !errors.Is(failure, context.Canceled) {
					test.Fatal("canceled envelope handoff published state", failure)
				}
				legacyNativeObjectW79Failed[Value](test, work, failure)
				return
			}
			if phase == "step" {
				cancel()
			}
			for attempt := 0; attempt < legacyNativeEnvelopeW80Steps; attempt++ {
				ready, failure := work.step(current)
				if failure != nil {
					if !errors.Is(failure, context.Canceled) {
						test.Fatal("envelope lost explicit current cancellation", failure)
					}
					legacyNativeObjectW79Failed[Value](test, work, failure)
					return
				}
				if ready {
					test.Fatal("canceled envelope operation quietly became ready")
				}
			}
			test.Fatal("envelope cancellation exceeded finite schedule")
		})
	}
	for _, budget := range []int64{1, LegacyCanonicalEncodingBytes, legacyNativeAnyCheckpointScratch} {
		test.Run(fmt.Sprintf("budget-%d", budget), func(test *testing.T) {
			reads, writes := storage.reads, storage.writes
			work, failure := factory(context.Background(), storage.io(), budget)
			if work != nil {
				work.close()
			}
			if !errors.Is(failure, ErrDocumentResourceLimit) || storage.reads != reads || storage.writes != writes {
				test.Fatal("envelope did IO or allocated native state before workspace admission", failure, storage.reads-reads, storage.writes-writes)
			}
		})
	}
	test.Run("sticky-store-failure", func(test *testing.T) {
		injected := errors.New("W80 envelope immutable storage unavailable")
		storage.failPut, storage.failRead = injected, injected
		defer func() { storage.failPut, storage.failRead = nil, nil }()
		work, failure := factory(context.Background(), storage.io(), legacyNativeEnvelopeW80Budget)
		if failure != nil {
			test.Fatal(failure)
		}
		defer work.close()
		for attempt := 0; attempt < legacyNativeEnvelopeW80Steps; attempt++ {
			ready, failure := work.step(context.Background())
			if failure != nil {
				if !errors.Is(failure, injected) {
					test.Fatal("envelope replaced storage error identity", failure)
				}
				legacyNativeObjectW79Failed[Value](test, work, failure)
				return
			}
			if ready {
				test.Fatal("failed envelope storage operation quietly became ready")
			}
		}
		test.Fatal("envelope storage failure exceeded finite schedule")
	})
}

func TestLegacyNativeEnvelopeCheckpointW80Lifecycle(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, native, scanner, source := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[3], storage)
	test.Run("capture", func(test *testing.T) {
		legacyNativeEnvelopeW80Lifecycle(test, storage, func(current context.Context, storageIO legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef], error) {
			work, failure := newLegacyNativeObjectCheckpoint(current, native, &scanner, request.Binding, source, storageIO, budget)
			if failure != nil {
				return nil, failure
			}
			return work, nil
		})
	})
	test.Run("restore", func(test *testing.T) {
		legacyNativeEnvelopeW80Lifecycle(test, storage, func(current context.Context, storageIO legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeObjectRestored], error) {
			work, failure := newLegacyNativeObjectCheckpointRestore(current, request.Root, legacyNativeCheckpointEnvelopeModel, storageIO, source, request.Binding, budget)
			if failure != nil {
				return nil, failure
			}
			return work, nil
		})
	})
	test.Run("capture-quiescent-handoff", func(test *testing.T) {
		work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, &scanner, request.Binding, source, storage.io(), legacyNativeEnvelopeW80Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		previous := native.fieldLimit
		native.fieldLimit++
		defer func() { native.fieldLimit = previous }()
		ready, failure := work.step(context.Background())
		if ready || !errors.Is(failure, ErrConflict) {
			test.Fatal("envelope capture admitted a changed borrowed controller", ready, failure)
		}
		legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
	})
	test.Run("restore-pre-read-transient-admission", func(test *testing.T) {
		work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), source, request.Binding, legacyNativeEnvelopeW80Budget)
		if err != nil {
			test.Fatal(err)
		}
		retained, err := work.retainedBytes()
		legacyNativeAnyW78Released(test, work)
		if err != nil || retained <= 0 {
			test.Fatal("invalid envelope restore workspace baseline", retained, err)
		}
		reads := storage.reads
		failure := legacyNativeEnvelopeW80Reject(test, request, source, request.Binding, legacyNativeCheckpointEnvelopeModel, retained+legacyNativeAnyCheckpointScratch-1, storage.io())
		if !errors.Is(failure, ErrDocumentResourceLimit) || storage.reads != reads {
			test.Fatal("envelope read root metadata without full transient scratch admission", failure, storage.reads-reads)
		}
	})
}
