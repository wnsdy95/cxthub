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
)

const legacyNativeObjectW79Directory = "CXTHUB_W79_NATIVE_OBJECT_DIRECTORY"
const legacyNativeObjectW79Budget int64 = 32 << 20
const legacyNativeObjectW79Steps = 4096

type legacyNativeObjectW79Fixture struct {
	name        string
	raw         string
	model       uint8
	cut         string
	diagnostics legacyNativeNumberDiagnostics
}

func legacyNativeObjectW79Fixtures() []legacyNativeObjectW79Fixture {
	width := LegacyNormalizationPartBytes
	key := func(prefix, partial, suffix string) string {
		return prefix + strings.Repeat(" ", width-len(prefix)-len(partial)) + partial + suffix
	}
	valid := `"0.` + strings.Repeat("0", 2*width) + `1"`
	invalid := `"0.` + strings.Repeat("0", 2*width) + `x"`
	return []legacyNativeObjectW79Fixture{
		{name: "block-key", raw: key(`{"type":"text",`, `"te`, `xt":"later"}`), model: legacyNativeCheckpointBlockModel, cut: "key"},
		{name: "block-scalar-prefix", raw: `{"type":"text","text":"` + strings.Repeat("a", 2*width) + `\n\uD83D\uDE42tail"}`, model: legacyNativeCheckpointBlockModel, cut: "scalar"},
		{name: "locked-scalar-migration", raw: key(`{"provider":"codex",`, "", `"blob":"`+strings.Repeat("\xff", 2*width)+`","scheme":"signature"}`), model: legacyNativeCheckpointLockedModel, cut: "migration"},
		{name: "block-duplicate-empty-null", raw: `{"text":"","text":null,"type":"text"}`, model: legacyNativeCheckpointBlockModel, cut: "null"},
		{name: "block-null-untouched-presence", raw: `{"agent_message":false,"text":null,"type":"text"}`, model: legacyNativeCheckpointBlockModel, cut: "null"},
		{name: "provider-remembered-type-error", raw: `{"turn_id":false,"turn_id":"continued","create_time":7}`, model: legacyNativeCheckpointProviderModel, cut: "type-error"},
		{name: "number-unquote-private", raw: `{"create_time":` + valid + `,"turn_id":"later"}`, model: legacyNativeCheckpointProviderModel, cut: "unquote"},
		{name: "number-unquote-reused", raw: `{"create_time":17,"create_time":` + valid + `,"turn_id":"later"}`, model: legacyNativeCheckpointProviderModel, cut: "unquote"},
		{name: "number-unquote-null-prior", raw: `{"create_time":17,"create_time":null,"create_time":` + valid + `,"turn_id":"later"}`, model: legacyNativeCheckpointProviderModel, cut: "unquote"},
		{name: "number-validation", raw: `{"create_time":` + valid + `,"turn_id":"later"}`, model: legacyNativeCheckpointProviderModel, cut: "validate"},
		{name: "number-rejection", raw: `{"create_time":` + invalid + `,"turn_id":"unapplied"}`, model: legacyNativeCheckpointProviderModel, cut: "rejected"},
		{name: "number-diagnostic-prefix", raw: `{"create_time":` + invalid + `,"turn_id":"unapplied"}`, model: legacyNativeCheckpointProviderModel, cut: "diagnostic"},
		{name: "number-future-full-diagnostic", raw: `{"create_time":17,"create_time":` + invalid + `,"turn_id":"unapplied"}`, model: legacyNativeCheckpointProviderModel, cut: "unquote"},
		{name: "number-reject-only", raw: `{"create_time":` + invalid + `,"turn_id":"unapplied"}`, model: legacyNativeCheckpointProviderModel, cut: "unquote", diagnostics: legacyNativeNumberDiagnosticsRejectOnly},
	}
}

func legacyNativeObjectW79Destination(test *testing.T, model uint8) any {
	test.Helper()
	switch model {
	case legacyNativeCheckpointBlockModel:
		return &ContentBlock{}
	case legacyNativeCheckpointLockedModel:
		return &LockedBlob{}
	case legacyNativeCheckpointProviderModel:
		return &ProviderMetadata{}
	default:
		test.Fatal("fixture requested an unsupported leaf model", model)
		return nil
	}
}

func legacyNativeObjectW79Stop(native *legacyNativeObject, fixture legacyNativeObjectW79Fixture) bool {
	switch fixture.cut {
	case "key":
		return native.phase == legacyNativeObjectKey && native.keyStarted && len(native.key) > 0 && native.position == LegacyNormalizationPartBytes
	case "scalar":
		return native.scalar != nil && native.scalar.position == LegacyNormalizationPartBytes && native.scalar.text != nil && native.scalar.text.Len() > 0
	case "migration":
		return native.scalar != nil && native.scalar.previous != nil
	case "null":
		return native.phase == legacyNativeObjectAfter && native.valueFirst == 'n'
	case "type-error":
		return native.typeError != nil && native.phase == legacyNativeObjectAfter
	case "unquote":
		return native.typedNumber != nil && !native.typedNumber.decoded && native.typedNumber.scalar != nil && native.typedNumber.scalar.position == LegacyNormalizationPartBytes
	case "validate":
		return native.typedNumber != nil && native.typedNumber.decoded && !native.typedNumber.rejected && native.typedNumber.validated > 0 && native.typedNumber.validated < native.typedNumber.scalar.text.Len()
	case "rejected":
		return native.typedNumber != nil && native.typedNumber.rejected && !native.typedNumber.complete && native.typedNumber.errorText == nil
	case "diagnostic":
		return native.typedNumber != nil && native.typedNumber.rejected && native.typedNumber.errorText != nil && native.typedNumber.errorText.position >= LegacyNormalizationPartBytes && native.typedNumber.errorText.position < native.typedNumber.end
	default:
		return false
	}
}

func legacyNativeObjectW79Step(test *testing.T, ctx context.Context, native *legacyNativeObject, scanner *legacyJSONValueScanner, source LegacyByteSource, diagnostics legacyNativeNumberDiagnostics, onePage bool) (bool, error) {
	test.Helper()
	input := &legacyNormalizationInput{ctx: ctx, source: source, numberDiagnostics: diagnostics}
	if onePage {
		input.loads = 1
	}
	complete, failure := native.step(ctx, input, scanner)
	if input.failure != nil && !errors.Is(input.failure, context.Canceled) {
		test.Fatal("native object source failed", input.failure)
	}
	if input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.fields < 0 || input.fields > native.fieldActionLimit() {
		test.Fatal("native object escaped shared quantum", input.workBytes, input.loads, input.fields)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete, failure
}

func legacyNativeObjectW79Continue(test *testing.T, native *legacyNativeObject, scanner *legacyJSONValueScanner, source LegacyByteSource, diagnostics legacyNativeNumberDiagnostics) (int, error) {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeObjectW79Steps; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		complete, failure := legacyNativeObjectW79Step(test, ctx, native, scanner, source, diagnostics, false)
		cancel()
		if complete || failure != nil {
			if complete && (native.phase != legacyNativeObjectDone || native.position != native.end) {
				test.Fatal("native object completed at wrong cursor")
			}
			if native.typeError != nil && native.phase == legacyNativeObjectDone && failure != native.typeError {
				test.Fatal("native object replaced its remembered error identity")
			}
			if number := native.typedNumber; number != nil && number.complete && number.diagnostics == legacyNativeNumberDiagnosticsFull && (failure != number.semantic || number.errorText == nil || failure.Error() != number.errorText.text.String()) {
				test.Fatal("native number lost exact completed error identity or bytes")
			}
			return attempt, failure
		}
	}
	test.Fatal("native object exceeded finite continuation schedule")
	return 0, ErrIntegrity
}

func legacyNativeObjectW79Suffix(native *legacyNativeObject, width int) int {
	position := native.position
	if native.field >= 0 && native.valueStarted && native.phase == legacyNativeObjectValue {
		position = min(position, native.valueStart)
	}
	if native.phase == legacyNativeObjectApply && native.scalar == nil && native.typedNumber == nil && !native.skipValue {
		position = min(position, native.valueStart)
	}
	if native.scalar != nil {
		position = min(position, native.scalar.position)
	}
	if number := native.typedNumber; number != nil && !number.rejected && !number.decoded && number.scalar != nil {
		position = min(position, number.scalar.position)
	}
	return position / width * width
}

type legacyNativeObjectW79Error struct {
	Class string                     `json:"class"`
	Text  string                     `json:"text"`
	Typed legacyNativeAnyW78Semantic `json:"typed"`
}

func legacyNativeObjectW79ErrorState(failure error) legacyNativeObjectW79Error {
	if failure == nil {
		return legacyNativeObjectW79Error{}
	}
	if typed, ok := failure.(*json.UnmarshalTypeError); ok && typed == nil {
		return legacyNativeObjectW79Error{}
	}
	var typed *json.UnmarshalTypeError
	errors.As(failure, &typed)
	return legacyNativeObjectW79Error{Class: reflect.TypeOf(failure).String(), Text: failure.Error(), Typed: legacyNativeAnyW78Error(typed)}
}

func legacyNativeObjectW79Values(test *testing.T, destination any) []string {
	test.Helper()
	value := reflect.ValueOf(destination).Elem()
	values := make([]string, value.NumField())
	for index := range values {
		field := value.Field(index)
		switch field.Kind() {
		case reflect.String:
			values[index] = field.Type().String() + ":" + strconv.Quote(field.String())
		case reflect.Pointer:
			if field.Type() != reflect.TypeFor[*json.Number]() {
				test.Fatal("nonleaf field in process fixture", field.Type())
			}
			values[index] = "*json.Number:nil"
			if !field.IsNil() {
				values[index] = "*json.Number:" + strconv.Quote(field.Elem().String())
			}
		default:
			test.Fatal("unsupported field in leaf process fixture", field.Type())
		}
	}
	return values
}

type legacyNativeObjectW79State struct {
	Control     []int                      `json:"control"`
	Flags       []bool                     `json:"flags"`
	Key         []byte                     `json:"key"`
	Scanner     []byte                     `json:"scanner"`
	Values      []string                   `json:"values"`
	Initialized []bool                     `json:"initialized"`
	Scalar      []string                   `json:"scalar"`
	Number      []string                   `json:"number"`
	Error       legacyNativeObjectW79Error `json:"error"`
}

func legacyNativeObjectW79StateOf(test *testing.T, native *legacyNativeObject, scanner *legacyJSONValueScanner) legacyNativeObjectW79State {
	test.Helper()
	scannerBytes, err := scanner.checkpoint(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	state := legacyNativeObjectW79State{
		Control: []int{native.parentDepth, native.fieldLimit, native.position, native.end, native.valueStart, native.valueEnd, native.field, int(native.presence), int(native.phase), int(native.valueFirst), cap(native.key), cap(native.raw)},
		Flags:   []bool{native.trackPresence, native.pagedEvents, native.deferTypeErrors, native.skipValue, native.prefix, native.keyStarted, native.keyOverflow, native.valueStarted},
		Key:     bytes.Clone(native.key), Scanner: scannerBytes,
		Values: legacyNativeObjectW79Values(test, native.destination.Interface()), Error: legacyNativeObjectW79ErrorState(native.typeError),
	}
	for _, heap := range native.heaps {
		if heap.err != nil || heap.bytes < 0 {
			test.Fatal("invalid native field heap", heap)
		}
		state.Initialized = append(state.Initialized, heap.bytes != 0)
	}
	if native.scalar != nil {
		state.Scalar = legacyNativeAnyW78ScalarState(test, native.scalar)
	}
	if number := native.typedNumber; number != nil {
		if number.target == nil {
			test.Fatal("typed number lost target")
		}
		prior, _ := native.destination.Elem().Field(native.metadata.fields[number.field].index).Interface().(*json.Number)
		if number.newTarget && prior != nil || !number.newTarget && prior != number.target {
			test.Fatal("typed number target does not preserve private/reused alias")
		}
		state.Control = append(state.Control, number.sourceBytes, number.from, number.end, number.field, number.validated, int(number.mode), int(number.diagnostics))
		state.Flags = append(state.Flags, number.quoted, number.decoded, number.rejected, number.complete, number.newTarget, prior == number.target)
		state.Number = []string{string(*number.target), legacyNativeObjectW79ErrorState(number.semantic).Text}
		if number.scalar != nil {
			state.Number = append(state.Number, "scalar")
			state.Number = append(state.Number, legacyNativeAnyW78ScalarState(test, number.scalar)...)
		}
		if number.errorText != nil {
			state.Number = append(state.Number, "diagnostic")
			state.Number = append(state.Number, legacyNativeAnyW78ScalarState(test, number.errorText)...)
		}
	}
	return state
}

type legacyNativeObjectW79Request struct {
	Source      legacyNativeAnyW78Request     `json:"source"`
	Model       uint8                         `json:"model"`
	Diagnostics legacyNativeNumberDiagnostics `json:"diagnostics"`
}

func legacyNativeObjectW79Capture(test *testing.T, fixture legacyNativeObjectW79Fixture, storage *legacyNativeAnyW78CAS) (legacyNativeObjectW79Request, *legacyNativeObject, legacyJSONValueScanner, LegacyByteSource) {
	test.Helper()
	request, source := legacyNativeAnyW78FullSource(test, fixture.raw)
	request.Binding = HashContent([]byte("W79 leaf nativeObject " + fixture.name))
	if len(request.Parts) > 4 {
		test.Fatal("fixture exceeds four original source parts")
	}
	storage.preload(test, request)
	native, err := newLegacyNativeObject(reflect.ValueOf(legacyNativeObjectW79Destination(test, fixture.model)), legacyNativeCheckpointModelMetadata(fixture.model), 1, true)
	if err != nil {
		test.Fatal(err)
	}
	native.start(0, len(fixture.raw))
	native.fieldLimit, native.deferTypeErrors = 1, true
	var scanner legacyJSONValueScanner
	for attempt := 0; attempt < legacyNativeObjectW79Steps && !legacyNativeObjectW79Stop(native, fixture); attempt++ {
		base, cancel := context.WithCancel(context.Background())
		cut := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeObjectW79Stop(native, fixture) }}
		complete, failure := legacyNativeObjectW79Step(test, cut, native, &scanner, source, fixture.diagnostics, true)
		cancel()
		if complete || failure != nil && !errors.Is(failure, context.Canceled) {
			test.Fatal("fixture failed before actual native cut", fixture.name, native.position, failure)
		}
	}
	if !legacyNativeObjectW79Stop(native, fixture) {
		test.Fatal("fixture did not reach actual native cut", fixture.name, native.position)
	}
	before := legacyNativeObjectW79StateOf(test, native, &scanner)
	captureReads := 0
	captureSource := source
	captureSource.Read = func(current context.Context, offset, length int) ([]byte, error) {
		captureReads++
		return source.Read(current, offset, length)
	}
	constructorContext, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpoint(constructorContext, native, &scanner, request.Binding, captureSource, storage.io(), legacyNativeObjectW79Budget)
	cancel()
	if err != nil {
		test.Fatal("object checkpoint construction", err)
	}
	legacyNativeAnyW78Drain(test, work, storage, legacyNativeObjectW79Budget)
	request.Root, err = work.take(context.Background())
	if err != nil || !request.Root.valid() {
		test.Fatal("object checkpoint did not publish root", err)
	}
	legacyNativeAnyW78Released(test, work)
	if !reflect.DeepEqual(before, legacyNativeObjectW79StateOf(test, native, &scanner)) {
		test.Fatal("capture mutated borrowed native state")
	}
	if captureReads != 0 {
		test.Fatalf("capture replayed RAW input instead of accepted native state: %d reads", captureReads)
	}
	request.SuffixOffset = legacyNativeObjectW79Suffix(native, request.PartBytes)
	request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
	return legacyNativeObjectW79Request{Source: request, Model: fixture.model, Diagnostics: fixture.diagnostics}, native, scanner, source
}

type legacyNativeObjectW79Report struct {
	PID               int                                `json:"pid"`
	RestoreSteps      int                                `json:"restore_steps"`
	ContinuationSteps int                                `json:"continuation_steps"`
	Initial           legacyNativeObjectW79State         `json:"initial"`
	Final             legacyNativeObjectW79State         `json:"final"`
	Error             legacyNativeObjectW79Error         `json:"error"`
	Reads             legacyNativeAnyW78Reads            `json:"reads"`
	Dependencies      []legacyNativeAnyW78DependencyRead `json:"dependencies"`
	Consumed          int                                `json:"consumed"`
	DiagnosticPrefix  int                                `json:"diagnostic_prefix"`
}

func TestLegacyNativeObjectCheckpointW79ProcessContinuation(test *testing.T) {
	for _, fixture := range legacyNativeObjectW79Fixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, original, scanner, source := legacyNativeObjectW79Capture(test, fixture, storage)
			initial := legacyNativeObjectW79StateOf(test, original, &scanner)
			_, failure := legacyNativeObjectW79Continue(test, original, &scanner, source, fixture.diagnostics)
			final := legacyNativeObjectW79StateOf(test, original, &scanner)
			independent := legacyNativeObjectW79Destination(test, fixture.model)
			independentError := json.Unmarshal([]byte(fixture.raw), independent)
			if !reflect.DeepEqual(legacyNativeObjectW79Values(test, independent), final.Values) {
				test.Fatal("native partial/final leaf values differ from independent encoding/json")
			}
			if fixture.diagnostics == legacyNativeNumberDiagnosticsFull {
				if !reflect.DeepEqual(legacyNativeObjectW79ErrorState(failure), legacyNativeObjectW79ErrorState(independentError)) {
					test.Fatal("native error differs from independent encoding/json", failure, independentError)
				}
			} else if independentError == nil || !errors.Is(failure, errLegacyNativeNumberRejected) {
				test.Fatal("RejectOnly silently weakened rejection", failure, independentError)
			}
			legacyNativeAnyW78JSON(test, filepath.Join(storage.directory, "request.json"), request)
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyNativeObjectW79ChildHelper$", "-test.count=1", "-test.timeout=20s")
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, legacyNativeObjectW79Directory+"=") {
					command.Env = append(command.Env, variable)
				}
			}
			command.Env = append(command.Env, legacyNativeObjectW79Directory+"="+storage.directory)
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("fresh nativeObject child failed: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(storage.directory, "report.json"))
			if err != nil {
				test.Fatal(err)
			}
			var report legacyNativeObjectW79Report
			if err := json.Unmarshal(body, &report); err != nil {
				test.Fatal(err)
			}
			if report.PID <= 0 || report.PID == os.Getpid() || report.RestoreSteps <= 0 || report.ContinuationSteps <= 0 || report.Reads.Forbidden != 0 || len(report.Reads.Ranges) == 0 {
				test.Fatal("missing actual fresh-process, suffix-only continuation", report.PID, report.RestoreSteps, report.ContinuationSteps, report.Reads)
			}
			if !reflect.DeepEqual(report.Initial, initial) || !reflect.DeepEqual(report.Final, final) || !reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)) {
				test.Fatalf("restored model/scanner/error/target/partial fields diverged: initial=%v final=%v error=%v", reflect.DeepEqual(report.Initial, initial), reflect.DeepEqual(report.Final, final), reflect.DeepEqual(report.Error, legacyNativeObjectW79ErrorState(failure)))
			}
			for _, read := range report.Reads.Ranges {
				if read[0] < request.Source.SuffixOffset || read[0]%request.Source.PartBytes != 0 || read[1] != min(request.Source.PartBytes, request.Source.SourceBytes-read[0]) {
					test.Fatal("child read a consumed semantic prefix or nonmanifest range", read)
				}
			}
			for _, dependency := range report.Dependencies {
				if !dependency.Restoring && !dependency.Diagnostic {
					test.Fatal("original CAS dependency used for semantic prefix replay", dependency)
				}
				matched := false
				for ordinal, part := range request.Source.Parts {
					start := ordinal * request.Source.PartBytes
					if dependency.Hash == part.Hash && dependency.Bytes == int(part.Bytes) && start+int(part.Bytes) <= report.Consumed {
						if dependency.Diagnostic && start+int(part.Bytes) <= report.DiagnosticPrefix {
							test.Fatal("already emitted diagnostic prefix reread from original CAS", dependency)
						}
						matched = true
					}
				}
				if !matched {
					test.Fatal("dependency contains unconsumed semantic tail", dependency)
				}
			}
			for ordinal, part := range request.Source.Parts {
				path, err := storage.objectPath(part.Hash)
				if err != nil {
					test.Fatal(err)
				}
				body, err := os.ReadFile(path)
				if err != nil {
					test.Fatal(err)
				}
				start := ordinal * request.Source.PartBytes
				if len(body) != int(part.Bytes) || HashContent(body) != part.Hash || !bytes.Equal(body, []byte(fixture.raw)[start:start+int(part.Bytes)]) {
					test.Fatal("checkpoint changed original immutable input CAS")
				}
			}
			test.Logf("actual leaf continuation: child=%d restore=%d continue=%d suffix=%d", report.PID, report.RestoreSteps, report.ContinuationSteps, request.Source.SuffixOffset)
		})
	}
	test.Log("Block/Locked/Provider scalar leaves only; no Event/envelope, list, stage, worker, or broader-goal completion claim.")
}

func TestLegacyNativeObjectW79ChildHelper(test *testing.T) {
	directory := os.Getenv(legacyNativeObjectW79Directory)
	if directory == "" {
		return
	}
	body, err := os.ReadFile(filepath.Join(directory, "request.json"))
	if err != nil {
		test.Fatal(err)
	}
	var request legacyNativeObjectW79Request
	if err := json.Unmarshal(body, &request); err != nil || len(request.Source.Parts) > 4 || request.Source.SourceBytes > 4*LegacyNormalizationPartBytes {
		test.Fatal("child request exceeds finite leaf fixture", err)
	}
	report := legacyNativeObjectW79Report{PID: os.Getpid()}
	source := legacyNativeAnyW78Source(test, request.Source, &report.Reads)
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request.Source)
	constructorContext, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpointRestore(constructorContext, request.Source.Root, request.Model, storage.io(), source, request.Source.Binding, legacyNativeObjectW79Budget)
	cancel()
	if err != nil {
		test.Fatal("child restore construction", err)
	}
	defer work.close()
	report.RestoreSteps = legacyNativeAnyW78Drain(test, work, storage, legacyNativeObjectW79Budget)
	restored, err := work.take(context.Background())
	if err != nil || restored.Object == nil || restored.Object.metadata != legacyNativeCheckpointModelMetadata(request.Model) || restored.Object.destination.Type() != reflect.PointerTo(legacyNativeCheckpointModelMetadata(request.Model).model) {
		test.Fatal("child restored wrong concrete model", err)
	}
	legacyNativeAnyW78Released(test, work)
	if restored.Source.authority != source.authority || restored.Source.authority == 0 || restored.Source.Read == nil || restored.Source.Bytes != source.Bytes || restored.Source.PartBytes != source.PartBytes || !reflect.DeepEqual(restored.Source.Parts, source.Parts) {
		test.Fatal("restore lost fresh current source authority/manifest")
	}
	if request.Source.SuffixOffset != legacyNativeObjectW79Suffix(restored.Object, request.Source.PartBytes) || report.Reads.Forbidden != 0 {
		test.Fatal("forbidden-prefix boundary is not the true pending semantic position")
	}
	if number := restored.Object.typedNumber; number != nil {
		if number.authority != source.authority {
			test.Fatal("typed number reused serialized source grant")
		}
		report.Consumed = number.end
		if !number.rejected && !number.decoded {
			report.Consumed = number.scalar.position
		}
		if number.errorText != nil {
			report.DiagnosticPrefix = number.errorText.position
		}
	}
	retained, heapErr := restored.Object.retained()
	if heapErr != nil || retained <= 0 || retained > legacyNativeObjectW79Budget {
		test.Fatal("restored native backing not accounted", retained, heapErr)
	}
	report.Initial = legacyNativeObjectW79StateOf(test, restored.Object, &restored.Scanner)
	storage.restoring = false
	storage.diagnostic = func() bool { return restored.Object.typedNumber != nil && restored.Object.typedNumber.rejected }
	var failure error
	report.ContinuationSteps, failure = legacyNativeObjectW79Continue(test, restored.Object, &restored.Scanner, restored.Source, request.Diagnostics)
	report.Final = legacyNativeObjectW79StateOf(test, restored.Object, &restored.Scanner)
	report.Error = legacyNativeObjectW79ErrorState(failure)
	report.Dependencies = storage.dependencies
	legacyNativeAnyW78JSON(test, filepath.Join(directory, "report.json"), report)
}

func legacyNativeObjectW79Failed[Value any](test *testing.T, work legacyNativeAnyCheckpointTask[Value], first error) {
	test.Helper()
	if first == nil {
		test.Fatal("expected sticky checkpoint failure")
	}
	if ready, err := work.step(context.Background()); ready || err != first {
		test.Fatal("checkpoint failure not sticky by identity", ready, first, err)
	}
	var zero Value
	if value, err := work.take(context.Background()); err != first || !reflect.DeepEqual(value, zero) {
		test.Fatal("failed checkpoint published a root/native bundle", err)
	}
	if retained, err := work.retainedBytes(); retained != 0 || err != nil {
		test.Fatal("failed checkpoint retained owned state", retained, err)
	}
	legacyNativeAnyW78Released(test, work)
}

func legacyNativeObjectW79RejectRestore(test *testing.T, request legacyNativeObjectW79Request, source LegacyByteSource, binding ContentHash, model uint8, budget int64, storage legacyNativeCheckpointIO) {
	test.Helper()
	work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Source.Root, model, storage, source, binding, budget)
	if err != nil {
		if work != nil {
			legacyNativeAnyW78Released(test, work)
		}
		return
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeObjectW79Steps; attempt++ {
		ready, failure := work.step(context.Background())
		if failure != nil {
			legacyNativeObjectW79Failed[legacyNativeObjectRestored](test, work, failure)
			return
		}
		if ready {
			test.Fatal("invalid leaf checkpoint became ready")
		}
	}
	test.Fatal("invalid leaf checkpoint did not fail finitely")
}

func TestLegacyNativeObjectCheckpointW79BindingAndCAS(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeObjectW79Capture(test, legacyNativeObjectW79Fixtures()[7], storage)
	source := legacyNativeAnyW78Source(test, request.Source, &legacyNativeAnyW78Reads{})
	for _, name := range []string{"binding", "expected-model", "unknown-model", "schema", "unknown-key", "duplicate-key", "fields-over-bound", "root-hash", "root-short", "no-grant", "source-size", "consumed-manifest"} {
		test.Run(name, func(test *testing.T) {
			changed, current, binding, model := request, source, request.Source.Binding, request.Model
			storageIO := storage.io()
			readOriginal := storageIO.Read
			dependentReads := 0
			rootOnly := false
			switch name {
			case "binding":
				binding, rootOnly = HashContent([]byte("wrong W79 binding")), true
			case "expected-model":
				model, rootOnly = legacyNativeCheckpointLockedModel, true
			case "unknown-model":
				model, rootOnly = 255, true
			case "no-grant":
				current.authority = 0
			case "source-size":
				current.Bytes--
			case "consumed-manifest":
				current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
				current.Parts[0].Hash = HashContent([]byte("wrong consumed original part"))
			case "schema", "unknown-key", "duplicate-key", "fields-over-bound":
				body, err := readOriginal(context.Background(), request.Source.Root.Hash, request.Source.Root.Bytes)
				if err != nil {
					test.Fatal(err)
				}
				var state legacyNativeObjectCheckpointControl
				if err := json.Unmarshal(body, &state); err != nil {
					test.Fatal(err)
				}
				if name == "schema" {
					state.Schema = HashContent([]byte("wrong closed model schema"))
					body, err = json.Marshal(state)
				} else if name == "fields-over-bound" {
					previous, failure := json.Marshal(state.Fields)
					if failure != nil || len(state.Fields) == 0 {
						test.Fatal("missing captured field metadata", failure)
					}
					row, failure := json.Marshal(state.Fields[0])
					if failure != nil {
						test.Fatal(failure)
					}
					fields := bytes.Repeat(append(bytes.Clone(row), ','), 63)
					fields = append(fields, row...)
					replacement := append([]byte(`"fields":[`), fields...)
					replacement = append(replacement, ']')
					needle := append([]byte(`"fields":`), previous...)
					if bytes.Count(body, needle) != 1 {
						test.Fatal("ambiguous captured field metadata")
					}
					body = bytes.Replace(body, needle, replacement, 1)
					var decoded legacyNativeObjectCheckpointControl
					if failure := json.Unmarshal(body, &decoded); failure == nil || cap(decoded.Fields) > 3 {
						test.Fatal("field metadata decode allocated beyond closed leaf bound", failure, cap(decoded.Fields))
					}
				} else {
					addition := `,"unknown":0}`
					if name == "duplicate-key" {
						addition = `,"model":5}`
					}
					body = append(bytes.Clone(body[:len(body)-1]), addition...)
				}
				if err != nil || len(body) > LegacyCanonicalEncodingBytes {
					test.Fatal("invalid bounded adversarial metadata fixture", len(body), err)
				}
				changed.Source.Root, err = legacyNativeCheckpointWrite(context.Background(), storage.io(), body)
				if err != nil {
					test.Fatal(err)
				}
				rootOnly = true
			}
			storageIO.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
				if rootOnly && hash != changed.Source.Root.Hash {
					dependentReads++
					return nil, errors.New("dependent read before model/binding/schema validation")
				}
				body, err := readOriginal(ctx, hash, length)
				if hash == changed.Source.Root.Hash && err == nil {
					if name == "root-hash" {
						body[0] ^= 1
					} else if name == "root-short" {
						body = body[:len(body)-1]
					}
				}
				return body, err
			}
			legacyNativeObjectW79RejectRestore(test, changed, current, binding, model, legacyNativeObjectW79Budget, storageIO)
			if dependentReads != 0 {
				test.Fatal("invalid model/binding/schema metadata loaded a body", dependentReads)
			}
		})
	}
	test.Run("unregistered-native-error-type", func(test *testing.T) {
		currentStore := legacyNativeAnyW78Store(test)
		captured, native, scanner, current := legacyNativeObjectW79Capture(test, legacyNativeObjectW79Fixtures()[5], currentStore)
		if native.typeError == nil {
			test.Fatal("fixture did not retain actual native type error")
		}
		native.typeError.Type = reflect.TypeFor[chan int]()
		work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, &scanner, captured.Source.Binding, current, currentStore.io(), legacyNativeObjectW79Budget)
		if err != nil {
			return
		}
		defer work.close()
		for attempt := 0; attempt < legacyNativeObjectW79Steps; attempt++ {
			ready, failure := work.step(context.Background())
			if failure != nil {
				legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
				return
			}
			if ready {
				test.Fatal("unregistered concrete error type reached wire")
			}
		}
		test.Fatal("unknown error type did not fail finitely")
	})
}

func TestLegacyNativeObjectCheckpointW79Lifecycle(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, native, scanner, source := legacyNativeObjectW79Capture(test, legacyNativeObjectW79Fixtures()[1], storage)
	for _, operation := range []string{"capture", "restore"} {
		for _, phase := range []string{"constructor", "step", "io", "take"} {
			test.Run(operation+"-cancel-"+phase, func(test *testing.T) {
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				storageIO := storage.io()
				if phase == "io" {
					put, read := storageIO.Put, storageIO.Read
					storageIO.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
						err := put(ctx, hash, body)
						cancel()
						return err
					}
					storageIO.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
						body, err := read(ctx, hash, length)
						cancel()
						return body, err
					}
				}
				if phase == "constructor" {
					cancel()
					if operation == "capture" {
						work, err := newLegacyNativeObjectCheckpoint(base, native, &scanner, request.Source.Binding, source, storageIO, legacyNativeObjectW79Budget)
						if work != nil || !errors.Is(err, context.Canceled) {
							test.Fatal("capture ignored canceled constructor", err)
						}
					} else {
						work, err := newLegacyNativeObjectCheckpointRestore(base, request.Source.Root, request.Model, storageIO, source, request.Source.Binding, legacyNativeObjectW79Budget)
						if work != nil || !errors.Is(err, context.Canceled) {
							test.Fatal("restore ignored canceled constructor", err)
						}
					}
					return
				}
				if operation == "capture" {
					work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, &scanner, request.Source.Binding, source, storageIO, legacyNativeObjectW79Budget)
					if err != nil {
						test.Fatal(err)
					}
					defer work.close()
					if phase == "take" {
						legacyNativeAnyW78Drain(test, work, storage, legacyNativeObjectW79Budget)
						cancel()
						root, failure := work.take(base)
						if root != (legacyNativeCheckpointRef{}) || !errors.Is(failure, context.Canceled) {
							test.Fatal("canceled take published root", failure)
						}
						legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
						return
					}
					if phase == "step" {
						cancel()
					}
					for attempt := 0; attempt < legacyNativeObjectW79Steps; attempt++ {
						ready, failure := work.step(base)
						if failure != nil {
							if !errors.Is(failure, context.Canceled) {
								test.Fatal("capture lost cancellation", failure)
							}
							legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
							return
						}
						if ready {
							test.Fatal("canceled capture reached readiness")
						}
					}
				} else {
					work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Source.Root, request.Model, storageIO, source, request.Source.Binding, legacyNativeObjectW79Budget)
					if err != nil {
						test.Fatal(err)
					}
					defer work.close()
					if phase == "take" {
						legacyNativeAnyW78Drain(test, work, storage, legacyNativeObjectW79Budget)
						cancel()
						bundle, failure := work.take(base)
						if bundle.Object != nil || !errors.Is(failure, context.Canceled) {
							test.Fatal("canceled take exposed object", failure)
						}
						legacyNativeObjectW79Failed[legacyNativeObjectRestored](test, work, failure)
						return
					}
					if phase == "step" {
						cancel()
					}
					for attempt := 0; attempt < legacyNativeObjectW79Steps; attempt++ {
						ready, failure := work.step(base)
						if failure != nil {
							if !errors.Is(failure, context.Canceled) {
								test.Fatal("restore lost cancellation", failure)
							}
							legacyNativeObjectW79Failed[legacyNativeObjectRestored](test, work, failure)
							return
						}
						if ready {
							test.Fatal("canceled restore reached readiness")
						}
					}
				}
				test.Fatal("cancellation did not terminate finitely")
			})
		}
	}
	test.Run("immutable-put-failure", func(test *testing.T) {
		injected := errors.New("W79 immutable store unavailable")
		storage.failPut = injected
		defer func() { storage.failPut = nil }()
		work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, &scanner, request.Source.Binding, source, storage.io(), legacyNativeObjectW79Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		for attempt := 0; attempt < legacyNativeObjectW79Steps; attempt++ {
			ready, failure := work.step(context.Background())
			if failure != nil {
				if !errors.Is(failure, injected) {
					test.Fatal(failure)
				}
				legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
				return
			}
			if ready {
				test.Fatal("failed put published readiness")
			}
		}
		test.Fatal("immutable store failure did not terminate")
	})
}

func TestLegacyNativeObjectCheckpointW79Admission(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, native, scanner, source := legacyNativeObjectW79Capture(test, legacyNativeObjectW79Fixtures()[1], storage)
	for _, budget := range []int64{1, LegacyCanonicalEncodingBytes, legacyNativeAnyCheckpointScratch} {
		test.Run(fmt.Sprintf("budget-%d", budget), func(test *testing.T) {
			reads, writes := storage.reads, storage.writes
			legacyNativeObjectW79RejectRestore(test, request, source, request.Source.Binding, request.Model, budget, storage.io())
			if storage.reads != reads {
				test.Fatal("restore read metadata before transient workspace admission")
			}
			work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, &scanner, request.Source.Binding, source, storage.io(), budget)
			if err == nil || work != nil {
				if work != nil {
					work.close()
				}
				test.Fatal("capture admitted impossible native plus transient workspace budget", err)
			}
			if storage.writes != writes {
				test.Fatal("capture performed IO before budget admission")
			}
		})
	}
	test.Run("pre-read-scratch-reservation", func(test *testing.T) {
		work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Source.Root, request.Model, storage.io(), source, request.Source.Binding, legacyNativeObjectW79Budget)
		if err != nil {
			test.Fatal(err)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained <= 0 {
			test.Fatal("invalid restore baseline accounting", retained, err)
		}
		legacyNativeAnyW78Released(test, work)
		reads := storage.reads
		legacyNativeObjectW79RejectRestore(test, request, source, request.Source.Binding, request.Model, retained+legacyNativeAnyCheckpointScratch-1, storage.io())
		if storage.reads != reads {
			test.Fatal("root decode began without full reserved transient scratch")
		}
	})
	test.Run("capture-handoff-guard", func(test *testing.T) {
		work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, &scanner, request.Source.Binding, source, storage.io(), legacyNativeObjectW79Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		oldLimit := native.fieldLimit
		native.fieldLimit++
		defer func() { native.fieldLimit = oldLimit }()
		ready, failure := work.step(context.Background())
		if ready || !errors.Is(failure, ErrConflict) {
			test.Fatal("capture admitted changed borrowed controller", ready, failure)
		}
		legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
	})
}
