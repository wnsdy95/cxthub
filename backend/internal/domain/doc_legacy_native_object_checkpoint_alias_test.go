package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

const legacyNativeAliasW91Directory = "CXTHUB_W91_INITIAL_ALIAS_DIRECTORY"

func legacyNativeAliasW91Names() []string {
	return []string{"root-shared-input-output", "input-map-shared-in-output-array-backing"}
}

func legacyNativeAliasW91Initial(test *testing.T, name string) *wire {
	test.Helper()
	shared := map[string]any{"kept": true}
	switch name {
	case "root-shared-input-output":
		return &wire{Input: shared, Output: shared}
	case "input-map-shared-in-output-array-backing":
		backing := []any{shared, "hidden-backing-sentinel", shared, float64(7)}
		return &wire{Input: shared, Output: backing[:1:4]}
	default:
		test.Fatal("unknown finite initial-alias fixture", name)
		return nil
	}
}

func legacyNativeAliasW91Raw() (string, string) {
	prefix := `{"id":"` + strings.Repeat(`\u007a`, LegacyNormalizationPartBytes/2) + `","input":{"added":"` + strings.Repeat("p", LegacyCanonicalEncodingBytes/2+17) + `"}}`
	return prefix[:len(prefix)-1] + `,"seq":9}`, prefix
}

func legacyNativeAliasW91Value(test *testing.T, name string, actual *wire, raw string) {
	test.Helper()
	expected := legacyNativeAliasW91Initial(test, name)
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), expected); err != nil {
			test.Fatal("independent ordinary decoder rejected the fixture", err)
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		test.Fatal("actual normal continuation differs from independent encoding/json", name, len(actual.ID), actual.Seq)
	}
	input := reflect.ValueOf(actual.Input).Pointer()
	if input == 0 {
		test.Fatal("actual initial map became nil")
	}
	if name == "root-shared-input-output" {
		output, valid := actual.Output.(map[string]any)
		if !valid || reflect.ValueOf(output).Pointer() != input {
			test.Fatal("normal continuation split the real Input/Output map alias")
		}
		return
	}
	array, valid := actual.Output.([]any)
	if !valid || len(array) != 1 || cap(array) != 4 {
		test.Fatal("normal continuation changed the real output backing geometry")
	}
	backing := array[:cap(array)]
	for _, position := range []int{0, 2} {
		shared, valid := backing[position].(map[string]any)
		if !valid || reflect.ValueOf(shared).Pointer() != input {
			test.Fatal("visible or hidden backing slot lost its real Input map alias", position)
		}
	}
	expectedArray := expected.Output.([]any)
	if !reflect.DeepEqual(backing, expectedArray[:cap(expectedArray)]) {
		test.Fatal("normal continuation changed hidden backing or missed an aliased map update")
	}
}

func legacyNativeAliasW91Accounting(test *testing.T, native *legacyNativeObject, stage string) {
	test.Helper()
	if native.metadata != &legacyEventNativeFields || len(native.heaps) != len(native.metadata.fields) ||
		len(native.backingExcess) != len(native.metadata.fields) || native.containerActive || native.nestedActive ||
		native.blockActive || native.eventActive || native.listActive || native.number != nil || native.typedNumber != nil {
		test.Fatal("fixture left the actual ordinary initial-map decode path", stage)
	}
	for _, parser := range native.containers {
		if parser != nil {
			test.Fatal("initial nonnil map unexpectedly used a native container cache", stage)
		}
	}
	if len(native.lists) != 0 || len(native.blocks) != 0 || len(native.events) != 0 || len(native.nested) != 0 {
		test.Fatal("unrelated aggregate caches entered the finite alias fixture", stage)
	}
	var required docRangeHeaderHeapEstimate
	required.allocation(1, int64(unsafe.Sizeof(*native)))
	if native.mutation != nil {
		required.allocation(1, int64(unsafe.Sizeof(*native.mutation)))
	}
	if native.checkpointScanner != nil {
		required.allocation(1, int64(unsafe.Sizeof(*native.checkpointScanner)))
	}
	required.allocation(int64(cap(native.key)+cap(native.raw)), 1)
	required.allocation(int64(cap(native.heaps)), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
	required.allocation(int64(cap(native.backingExcess)), int64(unsafe.Sizeof(int64(0))))
	required.allocation(int64(cap(native.containers)), int64(unsafe.Sizeof((*legacyNativeAny)(nil))))
	if native.scalar != nil {
		native.scalar.retained(&required)
	}
	if native.typeError != nil {
		required.allocation(1, int64(unsafe.Sizeof(*native.typeError)))
		required.allocation(int64(len(native.typeError.Value)), 1)
		required.allocation(int64(len(native.typeError.Struct)), 1)
		required.allocation(int64(len(native.typeError.Field)), 1)
	}
	for index, field := range native.metadata.fields {
		recorded := native.heaps[index]
		value := native.destination.Elem().Field(field.index)
		if recorded.err != nil || recorded.bytes < 0 || native.backingExcess[index] < 0 {
			test.Fatal("actual field ownership is invalid", stage, field.name, recorded.err)
		}
		if recorded.bytes == 0 {
			if !value.IsZero() {
				test.Errorf("%s: nonzero %s lost all retained heap ownership", stage, field.name)
			}
			continue
		}
		fresh, err := legacyNativeValueHeap(context.Background(), value)
		if err != nil || fresh.err != nil {
			test.Fatal("independent full-backing heap walk failed", stage, field.name, err, fresh.err)
		}
		fresh.include(docRangeHeaderHeapEstimate{bytes: native.backingExcess[index]})
		if fresh.err != nil {
			test.Fatal("actual backing excess overflowed the field lower bound", stage, field.name, fresh.err)
		}
		if recorded.bytes < fresh.bytes {
			test.Errorf("%s: %s retained a stale alias heap: recorded=%d current-full-backing-minimum=%d deficit=%d", stage, field.name, recorded.bytes, fresh.bytes, fresh.bytes-recorded.bytes)
		}
		required.include(docRangeHeaderHeapEstimate{bytes: max(recorded.bytes, fresh.bytes)})
	}
	retained, err := native.retained()
	if err != nil || required.err != nil || retained < required.bytes || retained > legacyNativeEventW82FenceBudget {
		test.Errorf("%s: actual retained bytes omit current alias payload or exceed the unchanged budget: retained=%d required=%d budget=%d errors=%v/%v", stage, retained, required.bytes, legacyNativeEventW82FenceBudget, err, required.err)
	}
}

func legacyNativeAliasW91AdvanceInput(test *testing.T, bundle *legacyNativeObjectRestored) {
	test.Helper()
	stop := func() bool {
		return bundle.Object.phase == legacyNativeObjectAfter && bundle.Object.field == bundle.Object.metadata.match("input")
	}
	for attempt := 0; attempt < legacyNativeEventW82SourceSteps; attempt++ {
		if stop() {
			return
		}
		current, cancel := context.WithCancel(context.Background())
		cut := &legacyNativeAnyW78CutContext{Context: current, cancel: cancel, stop: stop}
		complete, failure := legacyNativeEventW82Step(test, cut, bundle, false, false)
		cancel()
		if failure != nil && !(errors.Is(failure, context.Canceled) && stop()) {
			test.Fatal("actual normal decoder failed before the Input update boundary", failure)
		}
		if complete {
			test.Fatal("actual normal decoder passed the Input boundary without its checkpoint cut")
		}
	}
	test.Fatal("actual Input continuation exceeded the existing finite source schedule")
}

type legacyNativeAliasW91Request struct {
	Name      string                    `json:"name"`
	ParentPID int                       `json:"parent_pid"`
	Source    legacyNativeAnyW78Request `json:"source"`
}

type legacyNativeAliasW91Report struct {
	PID         int         `json:"pid"`
	SourceReads int         `json:"source_reads"`
	Steps       int         `json:"steps"`
	Retained    int64       `json:"retained"`
	Digest      ContentHash `json:"digest"`
}

func legacyNativeAliasW91Process(test *testing.T, name string, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) {
	test.Helper()
	legacyGoGraphSetW85ProcessMetadataWrite(test, storage.directory, "w91-alias-request.json", legacyNativeAliasW91Request{Name: name, ParentPID: os.Getpid(), Source: request})
	current, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(current, os.Args[0], "-test.run=^TestLegacyNativeObjectCheckpointW91InitialAliasChild$", "-test.count=1", "-test.timeout=20s")
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, legacyNativeAliasW91Directory+"=") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, legacyNativeAliasW91Directory+"="+storage.directory)
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("fresh-process alias continuation failed: %v\n%s", err, output)
	}
	var report legacyNativeAliasW91Report
	legacyGoGraphSetW85ProcessMetadataRead(test, storage.directory, "w91-alias-report.json", &report)
	raw, _ := legacyNativeAliasW91Raw()
	expected := legacyNativeAliasW91Initial(test, name)
	if err := json.Unmarshal([]byte(raw), expected); err != nil {
		test.Fatal(err)
	}
	body, err := json.Marshal(expected)
	if err != nil || report.PID <= 0 || report.PID == os.Getpid() || report.PID != command.ProcessState.Pid() ||
		report.SourceReads <= 0 || report.Steps <= 0 || report.Steps > legacyNativeEventW82SourceSteps ||
		report.Retained <= 0 || report.Retained > legacyNativeEventW82FenceBudget || report.Digest != HashContent(body) {
		test.Fatal("fresh process did not restore and finish the actual second checkpoint", report, err)
	}
}

func TestLegacyNativeObjectCheckpointW91InitialAliasContinuation(test *testing.T) {
	for _, name := range legacyNativeAliasW91Names() {
		test.Run(name, func(test *testing.T) {
			raw, prefix := legacyNativeAliasW91Raw()
			initial := legacyNativeAliasW91Initial(test, name)
			stop := func(native *legacyNativeObject) bool {
				return native.field == native.metadata.match("id") && native.scalar != nil &&
					native.scalar.position >= LegacyNormalizationPartBytes && native.scalar.position < native.scalar.end
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, initial, stop)
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			legacyNativeAliasW91Value(test, name, native.destination.Interface().(*wire), "")
			test.Run("before-first-checkpoint-accounting", func(test *testing.T) {
				legacyNativeAliasW91Accounting(test, native, "original active ID")
			})
			request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			value := restored.Object.destination.Interface().(*wire)
			legacyNativeAliasW91Value(test, name, value, "")
			if !stop(restored.Object) || reflect.ValueOf(value.Input).Pointer() == reflect.ValueOf(initial.Input).Pointer() ||
				reflect.ValueOf(value.Output).Pointer() == reflect.ValueOf(initial.Output).Pointer() {
				test.Fatal("first restore missed the live scalar cut or borrowed original physical alias storage")
			}
			test.Run("before-input-update-accounting", func(test *testing.T) {
				legacyNativeAliasW91Accounting(test, restored.Object, "restored active ID")
			})
			inputIdentity := reflect.ValueOf(value.Input).Pointer()
			outputIdentity := reflect.ValueOf(value.Output).Pointer()
			outputField := restored.Object.metadata.match("output")
			beforeHeap := restored.Object.heaps[outputField].bytes
			legacyNativeAliasW91AdvanceInput(test, &restored)
			legacyNativeAliasW91Value(test, name, value, prefix)
			if reflect.ValueOf(value.Input).Pointer() != inputIdentity || reflect.ValueOf(value.Output).Pointer() != outputIdentity || value.Seq != 0 {
				test.Fatal("ordinary Input update replaced physical storage or consumed the later Seq field")
			}
			if initial.Input["added"] != nil {
				test.Fatal("restored decoder mutated the original checkpoint producer's map")
			}
			test.Logf("%s: real normal Input update; output heap before=%d after=%d", name, beforeHeap, restored.Object.heaps[outputField].bytes)
			test.Run("after-input-update-accounting", func(test *testing.T) {
				legacyNativeAliasW91Accounting(test, restored.Object, "normal Input update with unchanged physical aliases")
			})
			second := legacyNativeEventW82FenceCapture(test, restored.Object, &restored.Scanner, request, restored.Source, storage)
			second.SuffixOffset = restored.Object.position / second.PartBytes * second.PartBytes
			second.Suffix = bytes.Clone(second.Suffix[second.SuffixOffset:])
			recaptured := legacyNativeEventW82FenceRestore(test, second, storage)
			legacyNativeAliasW91Value(test, name, recaptured.Object.destination.Interface().(*wire), prefix)
			if recaptured.Object.phase != legacyNativeObjectAfter || recaptured.Object.field != recaptured.Object.metadata.match("input") || recaptured.Object.position != restored.Object.position {
				test.Fatal("actual second checkpoint lost the post-Input continuation cursor")
			}
			test.Run("second-checkpoint-accounting", func(test *testing.T) {
				legacyNativeAliasW91Accounting(test, recaptured.Object, "actual second checkpoint restore")
			})
			if steps, err := legacyNativeEventW82Continue(test, &recaptured); err != nil || steps <= 0 {
				test.Fatal("actual second checkpoint did not finish normal decoding", steps, err)
			}
			legacyNativeAliasW91Value(test, name, recaptured.Object.destination.Interface().(*wire), raw)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &fields); err != nil || recaptured.Object.presence != eventPresence(fields) {
				test.Fatal("continued native presence differs from ordinary JSON keys", err)
			}
			test.Run("final-accounting", func(test *testing.T) {
				legacyNativeAliasW91Accounting(test, recaptured.Object, "finished normal decoder")
			})
			test.Run("second-checkpoint-fresh-process", func(test *testing.T) {
				legacyNativeAliasW91Process(test, name, second, storage)
			})
			if storage.inputWrites != 0 {
				test.Fatal("alias regression rewrote immutable original source CAS")
			}
		})
	}
}

func TestLegacyNativeObjectCheckpointW91InitialAliasChild(test *testing.T) {
	directory := os.Getenv(legacyNativeAliasW91Directory)
	if directory == "" {
		test.Skip("idle finite initial-alias fresh-process helper")
	}
	var request legacyNativeAliasW91Request
	legacyGoGraphSetW85ProcessMetadataRead(test, directory, "w91-alias-request.json", &request)
	if request.ParentPID <= 0 || request.ParentPID == os.Getpid() || request.ParentPID != os.Getppid() ||
		!request.Source.Root.valid() || request.Source.SourceBytes > 4*LegacyNormalizationPartBytes ||
		len(request.Source.Parts) == 0 || len(request.Source.Parts) > 4 || request.Source.SuffixOffset < 2*LegacyNormalizationPartBytes {
		test.Fatal("child lacks a real direct-parent post-Input checkpoint and bounded current suffix")
	}
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request.Source)
	restored := legacyNativeEventW82FenceRestore(test, request.Source, storage)
	raw, prefix := legacyNativeAliasW91Raw()
	value := restored.Object.destination.Interface().(*wire)
	legacyNativeAliasW91Value(test, request.Name, value, prefix)
	legacyNativeAliasW91Accounting(test, restored.Object, "fresh-process second checkpoint before continuation")
	inputIdentity, outputIdentity := reflect.ValueOf(value.Input).Pointer(), reflect.ValueOf(value.Output).Pointer()
	sourceReads := 0
	read := restored.Source.Read
	restored.Source.Read = func(current context.Context, offset, length int) ([]byte, error) {
		sourceReads++
		return read(current, offset, length)
	}
	storage.restoring = false
	steps, err := legacyNativeEventW82Continue(test, &restored)
	if err != nil || steps <= 0 || sourceReads == 0 || reflect.ValueOf(value.Input).Pointer() != inputIdentity || reflect.ValueOf(value.Output).Pointer() != outputIdentity {
		test.Fatal("fresh normal continuation failed, skipped source work, or replaced physical aliases", steps, err)
	}
	legacyNativeAliasW91Value(test, request.Name, value, raw)
	legacyNativeAliasW91Accounting(test, restored.Object, "fresh-process normal decoder complete")
	retained, err := restored.Object.retained()
	if err != nil || storage.inputWrites != 0 {
		test.Fatal("fresh-process alias continuation lost accounting or immutable source CAS", err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	legacyGoGraphSetW85ProcessMetadataWrite(test, directory, "w91-alias-report.json", legacyNativeAliasW91Report{
		PID: os.Getpid(), SourceReads: sourceReads, Steps: steps, Retained: retained, Digest: HashContent(body),
	})
}
