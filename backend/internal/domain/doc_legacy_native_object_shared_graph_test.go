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

const legacyNativeSharedW92Directory = "CXTHUB_W92_SHARED_ANY_DIRECTORY"

func legacyNativeSharedW92Raw(replace bool) string {
	value := `{"kept":true,"nested":[{"value":"owned"},null,7]}`
	raw := `{"input":` + value + `,"output":` + value + `,"id":"` + strings.Repeat(`\u007a`, LegacyNormalizationPartBytes/2) + `","seq":9`
	if replace {
		raw += `,"output":{"replacement":[false,"fresh"]}`
	}
	return raw + `}`
}

func legacyNativeSharedW92Cut(native *legacyNativeObject) bool {
	return native.field == native.metadata.match("id") && !native.containerActive &&
		native.scalar != nil && native.scalar.text != nil && native.scalar.text.Len() > 0 &&
		native.scalar.position >= LegacyNormalizationPartBytes && native.scalar.position < native.scalar.end
}

func legacyNativeSharedW92Graph(test *testing.T, bundle *legacyNativeObjectRestored, shared bool) {
	test.Helper()
	native := bundle.Object
	value := native.destination.Interface().(*wire)
	input := legacyNativeAnyFieldW84Parser(native, "input")
	output := legacyNativeAnyFieldW84Parser(native, "output")
	if input == nil || output == nil || input == output || native.containerActive ||
		!input.finished || !output.finished || input.rootMap == nil || output.rootMap == nil {
		test.Fatal("fixture lacks two distinct finished actual Any field caches")
	}
	for _, name := range []string{"input", "output"} {
		parser := legacyNativeAnyFieldW84Parser(native, name)
		legacyNativeAnyW78HeapAndAliases(test, parser)
		if parser.rootArray != nil || parser.frame != nil || parser.pending || parser.scalar != nil || parser.number != nil ||
			parser.visible != (legacyNativeAnyValueOwner{}) || parser.result.owner.object != parser.rootMap ||
			parser.result.owner.array != nil || parser.result.validateOwnership() != nil {
			test.Fatal("finished cache lacks its real root/result owner", name)
		}
		stored := native.destination.Elem().Field(native.metadata.fields[native.metadata.match(name)].index).Interface()
		if parser.result.owner.matches(stored) != nil {
			test.Fatal("actual destination is detached from its cache owner", name)
		}
		visited := 0
		legacyNativeAnyFieldW84OwnerAliases(test, parser.result.owner, &visited)
		heap := parser.result.heap
		if name == "output" {
			heap.allocation(1, int64(unsafe.Sizeof(any(nil))))
		}
		index := native.metadata.match(name)
		if heap.err != nil || native.heaps[index] != heap || native.backingExcess[index] != parser.result.excess {
			test.Fatal("actual cached field lost payload/interface/excess accounting", name)
		}
	}
	if !reflect.DeepEqual(value.Input, value.Output) || !reflect.DeepEqual(input.result.value, output.result.value) {
		test.Fatal("finite owner-identity controls do not have equal decoded values")
	}
	if (input.rootMap == output.rootMap) != shared || (input.result.owner == output.result.owner) != shared ||
		(reflect.ValueOf(value.Input).Pointer() == reflect.ValueOf(value.Output).Pointer()) != shared {
		test.Fatal("whole parent split shared physical ownership or merged equal-but-separate owners", shared)
	}
	inputArray := input.rootMap.ownership["nested"].owner.array
	outputArray := output.rootMap.ownership["nested"].owner.array
	if inputArray == nil || outputArray == nil || (inputArray == outputArray) != shared ||
		(reflect.ValueOf(value.Input["nested"]).Pointer() == reflect.ValueOf(value.Output.(map[string]any)["nested"]).Pointer()) != shared {
		test.Fatal("actual nested array topology differs from its root topology")
	}
	inputChild, outputChild := inputArray.children.at(0), outputArray.children.at(0)
	if inputChild == nil || outputChild == nil || inputChild.owner.object == nil || outputChild.owner.object == nil ||
		(inputChild.owner.object == outputChild.owner.object) != shared {
		test.Fatal("actual nested map owner topology was not preserved")
	}
	if retained, err := native.retained(); err != nil || retained <= 0 || retained > legacyNativeEventW82FenceBudget {
		test.Fatal("actual parent escaped the unchanged retained budget", retained, err)
	}
}

func legacyNativeSharedW92Capture(test *testing.T, bundle *legacyNativeObjectRestored, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS, shared bool) legacyNativeAnyW78Request {
	test.Helper()
	legacyNativeSharedW92Graph(test, bundle, shared)
	before := legacyNativeAnyFieldW84StateOf(test, bundle)
	heaps := append([]docRangeHeaderHeapEstimate(nil), bundle.Object.heaps...)
	excess := append([]int64(nil), bundle.Object.backingExcess...)
	input, output := legacyNativeAnyFieldW84Parser(bundle.Object, "input"), legacyNativeAnyFieldW84Parser(bundle.Object, "output")
	inputRoot, outputRoot := input.rootMap, output.rootMap
	reads, writes, rawReads := storage.reads, storage.writes, 0
	source := bundle.Source
	source.Read = func(current context.Context, offset, length int) ([]byte, error) {
		rawReads++
		return nil, ErrIntegrity
	}
	storageIO := legacyNativeAnyFieldW84IO(test, storage)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	probe, err := newLegacyNativeObjectCheckpoint(canceled, bundle.Object, &bundle.Scanner, request.Binding, source, storageIO, legacyNativeEventW82FenceBudget)
	if probe != nil || !errors.Is(err, context.Canceled) {
		if probe != nil {
			probe.close()
		}
		test.Fatal("pre-canceled actual parent capture published work", err)
	}
	constructor, retire := context.WithCancel(context.Background())
	probe, err = newLegacyNativeObjectCheckpoint(constructor, bundle.Object, &bundle.Scanner, request.Binding, source, storageIO, legacyNativeEventW82FenceBudget)
	retire()
	if err != nil || probe == nil {
		test.Fatal("actual parent cancellation probe construction", err)
	}
	if ready, failure := probe.step(canceled); ready || !errors.Is(failure, context.Canceled) {
		probe.close()
		test.Fatal("canceled actual parent capture fabricated readiness", ready, failure)
	} else {
		legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, probe, failure)
	}
	constructor, retire = context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpoint(constructor, bundle.Object, &bundle.Scanner, request.Binding, source, storageIO, legacyNativeEventW82FenceBudget)
	retire()
	if err != nil || work == nil || storage.reads != reads || storage.writes != writes || rawReads != 0 {
		test.Fatal("parent capture construction/cancellation performed IO or retained a retired context", err)
	}
	defer work.close()
	legacyNativeAnyFieldW84Ready(test, work, storage)
	request.Root, err = work.take(context.Background())
	if err != nil || !request.Root.valid() {
		test.Fatal("whole parent did not publish its actual checkpoint", err)
	}
	legacyNativeAnyW78Released(test, work)
	legacyNativeSharedW92Graph(test, bundle, shared)
	if rawReads != 0 || storage.inputWrites != 0 || len(storage.dependencies) != 0 ||
		!reflect.DeepEqual(before, legacyNativeAnyFieldW84StateOf(test, bundle)) ||
		!reflect.DeepEqual(heaps, bundle.Object.heaps) || !reflect.DeepEqual(excess, bundle.Object.backingExcess) ||
		legacyNativeAnyFieldW84Parser(bundle.Object, "input") != input || legacyNativeAnyFieldW84Parser(bundle.Object, "output") != output ||
		input.rootMap != inputRoot || output.rootMap != outputRoot {
		test.Fatal("parent capture/cancellation changed its borrowed caches, replayed RAW, or rewrote immutable input CAS")
	}
	return request
}

func legacyNativeSharedW92Restore(test *testing.T, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS, shared bool) (*legacyNativeObjectRestored, *legacyNativeAnyW78Reads) {
	test.Helper()
	reads := &legacyNativeAnyW78Reads{}
	source := legacyNativeAnyW78Source(test, request, reads)
	storageIO := legacyNativeAnyFieldW84IO(test, storage)
	beforeReads, beforeWrites := storage.reads, storage.writes
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	probe, err := newLegacyNativeObjectCheckpointRestore(canceled, request.Root, legacyNativeCheckpointEventModel, storageIO, source, request.Binding, legacyNativeEventW82FenceBudget)
	if probe != nil || !errors.Is(err, context.Canceled) {
		if probe != nil {
			probe.close()
		}
		test.Fatal("pre-canceled whole parent restore published work", err)
	}
	constructor, retire := context.WithCancel(context.Background())
	probe, err = newLegacyNativeObjectCheckpointRestore(constructor, request.Root, legacyNativeCheckpointEventModel, storageIO, source, request.Binding, legacyNativeEventW82FenceBudget)
	retire()
	if err != nil || probe == nil {
		test.Fatal("actual parent restore cancellation probe construction", err)
	}
	if ready, failure := probe.step(canceled); ready || !errors.Is(failure, context.Canceled) {
		probe.close()
		test.Fatal("canceled whole parent restore fabricated readiness", ready, failure)
	} else {
		legacyNativeObjectW79Failed[legacyNativeObjectRestored](test, probe, failure)
	}
	constructor, retire = context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpointRestore(constructor, request.Root, legacyNativeCheckpointEventModel, storageIO, source, request.Binding, legacyNativeEventW82FenceBudget)
	retire()
	if err != nil || work == nil || storage.reads != beforeReads || storage.writes != beforeWrites {
		test.Fatal("whole parent restore construction/cancellation performed IO", err)
	}
	defer work.close()
	legacyNativeAnyFieldW84Ready(test, work, storage)
	restored, err := work.take(context.Background())
	if err != nil || restored.Object == nil || restored.Object.metadata != &legacyEventNativeFields {
		test.Fatal("whole parent restore did not publish the actual wire/native model", err)
	}
	legacyNativeAnyW78Released(test, work)
	legacyNativeAnyFieldW84Source(test, restored.Source, source)
	if reads.Forbidden != 0 || len(reads.Ranges) != 0 || storage.inputWrites != 0 || len(storage.dependencies) != 0 ||
		!legacyNativeSharedW92Cut(restored.Object) || request.SuffixOffset != legacyNativeAnyFieldW84Suffix(restored.Object, request.PartBytes) {
		test.Fatal("whole parent restore replayed RAW, lost its exact ID cut, or touched immutable source CAS")
	}
	legacyNativeSharedW92Graph(test, &restored, shared)
	return &restored, reads
}

func legacyNativeSharedW92Snapshot(test *testing.T, bundle *legacyNativeObjectRestored) ContentHash {
	test.Helper()
	body, err := json.Marshal(legacyNativeAnyFieldW84StateOf(test, bundle))
	if err != nil {
		test.Fatal("actual private snapshot is not observable", err)
	}
	return HashContent(body)
}

func legacyNativeSharedW92Finish(test *testing.T, bundle *legacyNativeObjectRestored, shared, replace bool) (int, int64, ContentHash) {
	test.Helper()
	legacyNativeSharedW92Graph(test, bundle, shared)
	value := bundle.Object.destination.Interface().(*wire)
	inputParser, outputParser := legacyNativeAnyFieldW84Parser(bundle.Object, "input"), legacyNativeAnyFieldW84Parser(bundle.Object, "output")
	inputRoot, outputRoot := inputParser.rootMap, outputParser.rootMap
	outputOwner := outputParser.result.owner
	inputIdentity, outputIdentity := reflect.ValueOf(value.Input).Pointer(), reflect.ValueOf(value.Output).Pointer()
	inputBefore := legacyNativeAnyW78Native(test, value.Input)
	steps, err := legacyNativeAnyFieldW84Continue(test, bundle)
	if err != nil || steps <= 0 || steps > legacyNativeAnyFieldW84SourceSteps || value.Seq != 9 ||
		legacyNativeAnyFieldW84Parser(bundle.Object, "input") != inputParser || inputParser.rootMap != inputRoot ||
		reflect.ValueOf(value.Input).Pointer() != inputIdentity || legacyNativeAnyW78Native(test, value.Input) != inputBefore ||
		outputOwner.matches(outputRoot.values()) != nil || reflect.ValueOf(outputRoot.values()).Pointer() != outputIdentity {
		test.Fatal("ordinary suffix continuation changed Input or the original physical owner graph", steps, err)
	}
	if !replace {
		if legacyNativeAnyFieldW84Parser(bundle.Object, "output") != outputParser || reflect.ValueOf(value.Output).Pointer() != outputIdentity {
			test.Fatal("Seq-only continuation replaced an actual finished Output cache or map")
		}
		legacyNativeSharedW92Graph(test, bundle, shared)
	} else {
		next := legacyNativeAnyFieldW84Parser(bundle.Object, "output")
		if next == nil || !next.finished || next.rootMap == nil || next.rootMap == outputRoot ||
			next.rootMap == inputRoot || next.result.owner.matches(value.Output) != nil ||
			reflect.ValueOf(value.Output).Pointer() == outputIdentity || reflect.ValueOf(value.Output).Pointer() == inputIdentity {
			test.Fatal("ordinary Output replacement retained an old owner or overwrote the shared Input map")
		}
		legacyNativeAnyW78HeapAndAliases(test, next)
		if inputParser.result.validateOwnership() != nil || outputOwner.validate() != nil {
			test.Fatal("Output replacement invalidated the original finished owner metadata")
		}
	}
	raw := legacyNativeSharedW92Raw(replace)
	var expected wire
	if err := json.Unmarshal([]byte(raw), &expected); err != nil || !reflect.DeepEqual(value, &expected) {
		test.Fatal("actual continuation differs from independent encoding/json", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || bundle.Object.presence != eventPresence(fields) {
		test.Fatal("continued actual parent lost its source presence", err)
	}
	legacyNativeAnyFieldW84StateOf(test, bundle)
	retained, err := bundle.Object.retained()
	if err != nil || retained <= 0 || retained > legacyNativeEventW82FenceBudget {
		test.Fatal("continued actual parent escaped the unchanged retained budget", retained, err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return steps, retained, HashContent(body)
}

type legacyNativeSharedW92Request struct {
	ParentPID int                       `json:"parent_pid"`
	Shared    bool                      `json:"shared"`
	Replace   bool                      `json:"replace"`
	Snapshot  ContentHash               `json:"snapshot"`
	Source    legacyNativeAnyW78Request `json:"source"`
}

type legacyNativeSharedW92Report struct {
	PID         int         `json:"pid"`
	Snapshot    ContentHash `json:"snapshot"`
	SourceReads int         `json:"source_reads"`
	Steps       int         `json:"steps"`
	Retained    int64       `json:"retained"`
	Digest      ContentHash `json:"digest"`
}

func legacyNativeSharedW92Process(test *testing.T, request legacyNativeSharedW92Request, storage *legacyNativeAnyW78CAS) {
	test.Helper()
	request.Source.Suffix = nil
	legacyGoGraphSetW85ProcessMetadataWrite(test, storage.directory, "w92-shared-request.json", request)
	current, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(current, os.Args[0], "-test.run=^TestLegacyNativeObjectCheckpointW92SharedAnyChild$", "-test.count=1", "-test.timeout=20s")
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, legacyNativeSharedW92Directory+"=") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, legacyNativeSharedW92Directory+"="+storage.directory)
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("fresh-process whole parent shared-cache continuation failed: %v\n%s", err, output)
	}
	var report legacyNativeSharedW92Report
	legacyGoGraphSetW85ProcessMetadataRead(test, storage.directory, "w92-shared-report.json", &report)
	var expected wire
	if err := json.Unmarshal([]byte(legacyNativeSharedW92Raw(request.Replace)), &expected); err != nil {
		test.Fatal(err)
	}
	body, err := json.Marshal(&expected)
	if err != nil || report.PID <= 0 || report.PID == os.Getpid() || report.PID != command.ProcessState.Pid() ||
		report.Snapshot != request.Snapshot || report.SourceReads <= 0 || report.Steps <= 0 ||
		report.Steps > legacyNativeAnyFieldW84SourceSteps || report.Retained <= 0 ||
		report.Retained > legacyNativeEventW82FenceBudget || report.Digest != HashContent(body) {
		test.Fatal("fresh process did not restore the actual second checkpoint and finish its suffix", report, err)
	}
}

func TestLegacyNativeObjectCheckpointW92SharedFinishedAnyFields(test *testing.T) {
	for _, shared := range []bool{true, false} {
		for _, replace := range []bool{false, true} {
			name := "shared-owner"
			if !shared {
				name = "equal-values-separate-owners"
			}
			if replace {
				name += "/output-replacement"
			} else {
				name += "/seq-only"
			}
			test.Run(name, func(test *testing.T) {
				raw := legacyNativeSharedW92Raw(replace)
				request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, nil, legacyNativeSharedW92Cut)
				storage := legacyNativeAnyW78Store(test)
				storage.preload(test, request)
				bundle := &legacyNativeObjectRestored{Object: native, Scanner: *scanner, Source: source}
				legacyNativeSharedW92Graph(test, bundle, false)
				before := legacyNativeAnyFieldW84StateOf(test, bundle)
				if shared {
					input := legacyNativeAnyFieldW84Parser(native, "input")
					output := legacyNativeAnyFieldW84Parser(native, "output")
					output.rootMap, output.result = input.rootMap, input.result
					native.destination.Interface().(*wire).Output = input.rootMap.values()
					field := native.metadata.match("output")
					heap := input.result.heap
					heap.allocation(1, int64(unsafe.Sizeof(any(nil))))
					native.heaps[field], native.backingExcess[field] = heap, input.result.excess
				}
				legacyNativeSharedW92Graph(test, bundle, shared)
				if !reflect.DeepEqual(before, legacyNativeAnyFieldW84StateOf(test, bundle)) {
					test.Fatal("quiescent real-owner handoff changed decoded values or parser control")
				}
				snapshot := legacyNativeSharedW92Snapshot(test, bundle)
				request = legacyNativeSharedW92Capture(test, bundle, request, storage, shared)
				request.SuffixOffset = legacyNativeAnyFieldW84Suffix(native, request.PartBytes)
				if request.SuffixOffset != LegacyNormalizationPartBytes || len(request.Parts) != 4 ||
					request.SourceBytes > 4*LegacyNormalizationPartBytes || native.destination.Interface().(*wire).Seq != 0 {
					test.Fatal("finite escaped-ID cut lost its accepted prefix or consumed later Seq")
				}
				request.Suffix = bytes.Clone(request.Suffix[request.SuffixOffset:])
				restored, reads := legacyNativeSharedW92Restore(test, request, storage, shared)
				if restored.Source.authority == source.authority || !reflect.DeepEqual(before, legacyNativeAnyFieldW84StateOf(test, restored)) ||
					legacyNativeAnyFieldW84Parser(restored.Object, "input").rootMap == legacyNativeAnyFieldW84Parser(native, "input").rootMap ||
					legacyNativeAnyFieldW84Parser(restored.Object, "output").rootMap == legacyNativeAnyFieldW84Parser(native, "output").rootMap ||
					reflect.ValueOf(restored.Object.destination.Interface().(*wire).Input).Pointer() == reflect.ValueOf(native.destination.Interface().(*wire).Input).Pointer() ||
					reflect.ValueOf(restored.Object.destination.Interface().(*wire).Output).Pointer() == reflect.ValueOf(native.destination.Interface().(*wire).Output).Pointer() {
					test.Fatal("first whole parent restore changed its private snapshot or borrowed producer storage")
				}
				second := legacyNativeSharedW92Capture(test, restored, request, storage, shared)
				if reads.Forbidden != 0 || len(reads.Ranges) != 0 || legacyNativeSharedW92Snapshot(test, restored) != snapshot {
					test.Fatal("actual second capture changed the private graph or reread accepted RAW")
				}
				recaptured, rereads := legacyNativeSharedW92Restore(test, second, storage, shared)
				if recaptured.Source.authority == restored.Source.authority || legacyNativeSharedW92Snapshot(test, recaptured) != snapshot {
					test.Fatal("second whole parent restore lost its snapshot or fresh source authority")
				}
				legacyNativeSharedW92Finish(test, recaptured, shared, replace)
				if rereads.Forbidden != 0 || len(rereads.Ranges) == 0 {
					test.Fatal("actual continuation skipped suffix reads or replayed accepted RAW")
				}
				legacyNativeSharedW92Graph(test, bundle, shared)
				legacyNativeSharedW92Graph(test, restored, shared)
				if legacyNativeSharedW92Snapshot(test, bundle) != snapshot || legacyNativeSharedW92Snapshot(test, restored) != snapshot {
					test.Fatal("independent continuation mutated a prior checkpoint producer")
				}
				legacyNativeSharedW92Process(test, legacyNativeSharedW92Request{
					ParentPID: os.Getpid(), Shared: shared, Replace: replace, Snapshot: snapshot, Source: second,
				}, storage)
				if storage.inputWrites != 0 || len(storage.dependencies) != 0 {
					test.Fatal("finite shared-cache regression touched immutable original source CAS")
				}
				legacyNativeEnvelopeW80Immutable(test, storage, request, raw)
			})
		}
	}
}

func TestLegacyNativeObjectCheckpointW92SharedAnyChild(test *testing.T) {
	directory := os.Getenv(legacyNativeSharedW92Directory)
	if directory == "" {
		test.Skip("idle finite shared-cache fresh-process helper")
	}
	var request legacyNativeSharedW92Request
	legacyGoGraphSetW85ProcessMetadataRead(test, directory, "w92-shared-request.json", &request)
	if request.ParentPID <= 0 || request.ParentPID == os.Getpid() || request.ParentPID != os.Getppid() ||
		!request.Source.Root.valid() || request.Source.SourceBytes <= LegacyNormalizationPartBytes ||
		request.Source.SourceBytes > 4*LegacyNormalizationPartBytes || len(request.Source.Parts) != 4 || request.Source.PartBytes != LegacyNormalizationPartBytes ||
		request.Source.SuffixOffset != LegacyNormalizationPartBytes || ValidateContentHash(request.Snapshot) != nil {
		test.Fatal("child lacks a real direct-parent second checkpoint at the bounded escaped-ID cut")
	}
	storage := &legacyNativeAnyW78CAS{directory: directory, restoring: true}
	storage.sourceManifest(request.Source)
	for offset := request.Source.SuffixOffset; offset < request.Source.SourceBytes; offset += request.Source.PartBytes {
		part := request.Source.Parts[offset/request.Source.PartBytes]
		path, err := storage.objectPath(part.Hash)
		if err != nil {
			test.Fatal(err)
		}
		body, err := os.ReadFile(path)
		if err != nil || len(body) != int(part.Bytes) || len(body) > LegacyCanonicalEncodingBytes || HashContent(body) != part.Hash {
			test.Fatal("fixture suffix does not match the unchanged original CAS part", err)
		}
		request.Source.Suffix = append(request.Source.Suffix, body...)
	}
	restored, reads := legacyNativeSharedW92Restore(test, request.Source, storage, request.Shared)
	snapshot := legacyNativeSharedW92Snapshot(test, restored)
	if snapshot != request.Snapshot {
		test.Fatal("fresh process lost the actual second checkpoint's private snapshot")
	}
	storage.restoring = false
	steps, retained, digest := legacyNativeSharedW92Finish(test, restored, request.Shared, request.Replace)
	if reads.Forbidden != 0 || len(reads.Ranges) == 0 || storage.inputWrites != 0 || len(storage.dependencies) != 0 {
		test.Fatal("fresh process skipped suffix IO, replayed accepted RAW, or touched immutable source CAS")
	}
	legacyNativeEnvelopeW80Immutable(test, storage, request.Source, legacyNativeSharedW92Raw(request.Replace))
	legacyGoGraphSetW85ProcessMetadataWrite(test, directory, "w92-shared-report.json", legacyNativeSharedW92Report{
		PID: os.Getpid(), Snapshot: snapshot, SourceReads: len(reads.Ranges), Steps: steps, Retained: retained, Digest: digest,
	})
}
