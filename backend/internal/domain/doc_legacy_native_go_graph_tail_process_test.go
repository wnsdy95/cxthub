package domain

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

const legacyGoGraphTailW86ProcessEnvironment = "CXTHUB_W86_GO_GRAPH_TAIL_PROCESS"

type legacyGoGraphTailW86ProcessSourceState struct {
	identity legacyGoGraphSetW85ProcessIdentity
	kind     reflect.Type
	digest   legacyGoGraphSetW85ProcessDigest
	bits     uint64
}

func legacyGoGraphTailW86ProcessFixtures() []any {
	negativeZero := math.Float64frombits(1 << 63)
	sharedMap := map[string]any{"text": "tail\x00\n\ud55c\uae00\U0001f642", "zero": negativeZero, "bool": true}
	separateMap := map[string]any{"text": "tail\x00\n\ud55c\uae00\U0001f642", "zero": negativeZero, "bool": true}
	emptyMap := map[string]any{}
	emptyArray := make([]any, 0)
	nestedArray := make([]any, 2, 6)
	nestedBacking := nestedArray[:cap(nestedArray)]
	nestedBacking[0], nestedBacking[1] = sharedMap, float64(9)
	nestedBacking[2], nestedBacking[3] = negativeZero, emptyMap
	nestedBacking[4], nestedBacking[5] = []any(nil), "nested-hidden\x00\ud55c\uae00\U0001f642"
	separateArray := make([]any, len(nestedArray), cap(nestedArray))
	copy(separateArray[:cap(separateArray)], nestedBacking)
	nestedMap := map[string]any{"array": nestedArray, "shared": sharedMap, "empty": emptyArray}
	emptyCapacity := make([]any, 0, 5)
	emptyBacking := emptyCapacity[:cap(emptyCapacity)]
	emptyBacking[0], emptyBacking[1], emptyBacking[2] = sharedMap, nestedMap, negativeZero
	emptyBacking[3], emptyBacking[4] = emptyArray, "zero-visible-full-backing"
	array := make([]any, 2, 70)
	backing := array[:cap(array)]
	backing[0], backing[1] = sharedMap, "visible-prefix"
	backing[2], backing[3], backing[4] = negativeZero, map[string]any(nil), []any(nil)
	backing[5], backing[6], backing[7], backing[8] = emptyMap, emptyArray, sharedMap, separateMap
	backing[9], backing[10], backing[11] = nestedMap, nestedArray, separateArray
	backing[12], backing[13], backing[14], backing[15] = true, false, "", "hidden\x00\n\t\"\\\ud55c\uae00\U0001f642"
	backing[16], backing[17], backing[18], backing[19] = strings.Repeat("hidden-tail-\ud55c\uae00\U0001f642\x00\n", 8193), float64(0), emptyCapacity, nil
	for position := 20; position < len(backing); position++ {
		backing[position] = float64(position)
	}
	backing[31], backing[32], backing[33] = negativeZero, nestedArray, sharedMap
	backing[63], backing[64], backing[69] = emptyArray, separateMap, "last-hidden-slot"
	return []any{
		array, array, emptyCapacity, nestedArray, separateArray, sharedMap, separateMap, emptyMap, emptyArray, nestedMap,
		nil, negativeZero, map[string]any(nil), []any(nil), "root-text\x00\ud55c\uae00\U0001f642", float64(0), false, true,
		math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64, math.MaxFloat64, float64(0.1),
		sharedMap, nestedArray, emptyCapacity, emptyMap, emptyArray, separateArray,
		map[string]any{"hidden_owner": array, "nested_owner": nestedMap}, []any{sharedMap, nestedArray}, "last-root", float64(42),
	}
}

func legacyGoGraphTailW86ProcessSnapshot(test *testing.T, values []any) []legacyGoGraphTailW86ProcessSourceState {
	test.Helper()
	seen := make(map[legacyGoGraphSetW85ProcessIdentity]bool)
	var result []legacyGoGraphTailW86ProcessSourceState
	var visit func(any, int)
	visit = func(value any, depth int) {
		if len(result) >= 512 || depth > 32 {
			test.Fatal("source snapshot escaped the finite full-backing fixture grammar")
		}
		identity := legacyGoGraphSetW85ProcessIdentityOf(value)
		state := legacyGoGraphTailW86ProcessSourceState{identity: identity, kind: reflect.TypeOf(value)}
		encoded := value
		if array, ok := value.([]any); ok {
			encoded = array[:cap(array)]
		}
		state.digest = legacyGoGraphSetW85ProcessDigestValue(test, encoded)
		if number, ok := value.(float64); ok {
			state.bits = math.Float64bits(number)
		}
		result = append(result, state)
		if identity.pointer != 0 {
			if seen[identity] {
				return
			}
			seen[identity] = true
		}
		switch decoded := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(decoded))
			for key := range decoded {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				visit(decoded[key], depth+1)
			}
		case []any:
			for _, child := range decoded[:cap(decoded)] {
				visit(child, depth+1)
			}
		}
	}
	for _, value := range values {
		visit(value, 0)
	}
	return result
}

func legacyGoGraphTailW86ProcessPort(store *legacyGoGraphSetW85ProcessStore) legacyNativeCheckpointIO {
	port := store.port()
	return legacyNativeCheckpointIO{
		Put: func(ctx context.Context, hash ContentHash, body []byte) error {
			legacyNativeGoGraphSetW85Metadata(store.test, body)
			return port.Put(ctx, hash, body)
		},
		Read: func(ctx context.Context, hash ContentHash, limit int) ([]byte, error) {
			body, err := port.Read(ctx, hash, limit)
			if err == nil {
				legacyNativeGoGraphSetW85Metadata(store.test, body)
			}
			return body, err
		},
	}
}

func legacyGoGraphTailW86ProcessFormat(test *testing.T, expected []any, descriptors []legacyNativeCheckpointValue, store *legacyGoGraphSetW85ProcessStore) {
	test.Helper()
	if len(expected) != legacyNativeCheckpointPageItems || len(descriptors) != len(expected) {
		test.Fatal("portable full-backing descriptors lost the original 32-root bound")
	}
	for rootIndex, descriptor := range descriptors {
		state, err := legacyNativeCheckpointGraphRestoreDescriptor(context.Background(), descriptor)
		if err != nil {
			test.Fatal("existing wire decoder rejected a real captured descriptor", rootIndex, err)
		}
		if state.Kind != legacyNativeCheckpointArray && state.Kind != legacyNativeCheckpointObject || state.Nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.begin(ctx)
		var record legacyNativeCheckpointOwner
		err = legacyNativeCheckpointReadJSON(ctx, legacyGoGraphTailW86ProcessPort(store), descriptor.Body, &record)
		cancel()
		if err != nil || record.Kind != state.Kind || record.Identity != state.Reference || record.Members.Count != record.Length {
			test.Fatal("existing owner codec lost its real member list or identity", rootIndex, err)
		}
		var length, visible uint64
		var fullBacking bool
		switch decoded := expected[rootIndex].(type) {
		case []any:
			if state.Kind != legacyNativeCheckpointArray || state.Length != uint64(len(decoded)) || state.Capacity != uint64(cap(decoded)) || record.Capacity != state.Capacity {
				test.Fatal("array wire no longer describes physical capacity and visible length", rootIndex)
			}
			length = uint64(len(decoded))
			if cap(decoded) > len(decoded) {
				fullBacking, visible, length = true, uint64(len(decoded)), uint64(cap(decoded))
			}
		case map[string]any:
			if state.Kind != legacyNativeCheckpointObject || state.Length != uint64(len(decoded)) {
				test.Fatal("map wire changed its visible length", rootIndex)
			}
			length = uint64(len(decoded))
		default:
			test.Fatal("scalar unexpectedly acquired an owner record", rootIndex)
		}
		if record.FullBacking != fullBacking || record.Visible != visible || record.Length != length {
			test.Fatal("actual owner record violated FullBacking/Visible without a parallel schema", rootIndex, record.FullBacking, record.Visible, record.Length)
		}
	}
}

func legacyGoGraphTailW86ProcessValidate(test *testing.T, actual []legacyNativeAnyValue) int64 {
	test.Helper()
	expected := legacyGoGraphTailW86ProcessFixtures()
	if len(expected) != legacyNativeCheckpointPageItems || len(actual) != len(expected) {
		test.Fatal("fresh process lost the exact bounded full-backing root set", len(actual))
	}
	forward := make(map[legacyGoGraphSetW85ProcessIdentity]legacyGoGraphSetW85ProcessOwner)
	reverse := make(map[legacyGoGraphSetW85ProcessOwner]legacyGoGraphSetW85ProcessIdentity)
	visits := 0
	var visit func(any, legacyNativeAnyValue, int)
	visit = func(wanted any, value legacyNativeAnyValue, depth int) {
		visits++
		if visits > 512 || depth > 32 {
			test.Fatal("native validation escaped its finite full-backing fixture grammar")
		}
		if err := value.validateOwnership(); err != nil || reflect.TypeOf(wanted) != reflect.TypeOf(value.value) {
			test.Fatal("full backing lost its exact type or actual native ownership", reflect.TypeOf(wanted), reflect.TypeOf(value.value), err)
		}
		identity := legacyGoGraphSetW85ProcessIdentityOf(wanted)
		if identity.pointer != 0 {
			owner := legacyGoGraphSetW85ProcessOwner{object: value.owner.object, array: value.owner.array}
			if owner == (legacyGoGraphSetW85ProcessOwner{}) {
				test.Fatal("nonnil full-backing container has no real native owner")
			}
			if previous, found := forward[identity]; found {
				if previous != owner {
					test.Fatal("restore split a visible/hidden/cross-root shared child")
				}
				return
			}
			if previous, found := reverse[owner]; found && previous != identity {
				test.Fatal("restore coalesced separate equal-valued full-backing containers")
			}
			forward[identity], reverse[owner] = owner, identity
		}
		switch decoded := wanted.(type) {
		case map[string]any:
			container := value.value.(map[string]any)
			if (decoded == nil) != (container == nil) || len(decoded) != len(container) {
				test.Fatal("full-backing map changed typed nilness, empty state, or length")
			}
			if decoded == nil {
				return
			}
			cursor, err := value.owner.mapCheckpoint(context.Background(), true)
			if err != nil {
				test.Fatal("restored hidden map lacks a real native checkpoint", err)
			}
			defer cursor.close()
			seen := make(map[string]bool)
			for position := 0; position <= len(decoded); position++ {
				entry, available, err := cursor.next(context.Background())
				if err != nil || available != (position < len(decoded)) {
					test.Fatal("hidden map lost native metadata or exact EOF", position, err)
				}
				if !available {
					break
				}
				childExpected, found := decoded[entry.Key]
				if !found || seen[entry.Key] || entry.KeyCapacity < len(entry.Key) {
					test.Fatal("hidden map returned an unknown, duplicate, or invalid key")
				}
				seen[entry.Key] = true
				child := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes}, excess: entry.ValueExcess, owner: entry.owner}
				if text, ok := entry.Value.(string); ok {
					if entry.ValueExcess < 0 || entry.ValueExcess%2 != 0 || entry.ValueExcess > legacyGoGraphSetW85ProcessBudget {
						test.Fatal("hidden string has invalid native backing metadata")
					}
					child.capacity = len(text) + int(entry.ValueExcess/2)
				}
				physical, found := container[entry.Key]
				if !found || !legacyNativeCheckpointGoGraphSetSame(physical, child.value) {
					test.Fatal("hidden map native metadata does not describe its actual Go value")
				}
				visit(childExpected, child, depth+1)
			}
		case []any:
			container := value.value.([]any)
			if (decoded == nil) != (container == nil) || len(decoded) != len(container) || cap(decoded) != cap(container) {
				test.Fatal("restore changed physical array len/cap or nil/empty state", len(decoded), cap(decoded), len(container), cap(container))
			}
			if decoded == nil {
				return
			}
			if value.owner.array == nil {
				test.Fatal("nonnil array lost its actual native owner")
			}
			if cap(decoded) > len(decoded) {
				if err := value.owner.array.validateTail(true); err != nil {
					test.Fatal("restored hidden slots lack complete native tail metadata", err)
				}
			}
			physical := container[:cap(container)]
			for position, childExpected := range decoded[:cap(decoded)] {
				var child legacyNativeAnyValue
				var err error
				if position < len(decoded) {
					child, err = value.owner.arrayValue(context.Background(), position)
				} else {
					child, err = value.owner.array.tailValue(context.Background(), position)
				}
				if err != nil || !legacyNativeCheckpointGoGraphSetSame(physical[position], child.value) {
					test.Fatal("native visible/tail metadata does not describe the actual backing slot", position, err)
				}
				visit(childExpected, child, depth+1)
			}
		case float64:
			if math.Float64bits(decoded) != math.Float64bits(value.value.(float64)) {
				test.Fatal("restore changed full-backing float bits, including hidden negative zero")
			}
		default:
			if !reflect.DeepEqual(wanted, value.value) {
				test.Fatal("restore changed a visible or hidden primitive")
			}
		}
	}
	var retained int64
	for rootIndex, wanted := range expected {
		visit(wanted, actual[rootIndex], 0)
		rootBytes := actual[rootIndex].heap.bytes + actual[rootIndex].owner.bytes
		if rootBytes < 0 || rootBytes > legacyGoGraphSetW85ProcessBudget-retained {
			test.Fatal("full-backing native roots escaped the original 16MiB conservative handoff budget")
		}
		retained += rootBytes
	}
	if retained <= 0 {
		test.Fatal("full-backing native roots were handed off with empty accounting")
	}
	return retained
}

func legacyGoGraphTailW86ProcessCapture(test *testing.T, directory string) legacyGoGraphSetW85ProcessRequest {
	test.Helper()
	values := legacyGoGraphTailW86ProcessFixtures()
	before := legacyGoGraphTailW86ProcessSnapshot(test, values)
	request := legacyGoGraphSetW85ProcessRequest{Mode: "restore", Binding: HashContent([]byte("W86-non-overlap-full-backing-root-set")), Budget: legacyGoGraphSetW85ProcessBudget, ParentPID: os.Getpid()}
	store := &legacyGoGraphSetW85ProcessStore{test: test, directory: directory}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeCheckpointGoGraphSet(constructor, values, request.Binding, legacyGoGraphTailW86ProcessPort(store), request.Budget)
	cancel()
	if err != nil || work == nil || store.reads != 0 || store.writes != 0 {
		test.Fatal("real full-backing set constructor failed or performed eager CAS IO", err)
	}
	defer work.close()
	report := &legacyGoGraphSetW85ProcessReport{}
	if err := legacyGoGraphSetW85ProcessDrive(test, work, store, request.Budget, 0, nil, report, false); err != nil {
		test.Fatal("real importer/graph-set full-backing capture failed", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.begin(ctx)
	request.Ref, err = work.take(ctx)
	cancel()
	if err != nil || !request.Ref.valid() || store.writes == 0 || report.Steps == 0 {
		test.Fatal("real full-backing capture returned empty success", err)
	}
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("full-backing capture handoff retained producers or registry", retained, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	store.begin(ctx)
	var state legacyNativeCheckpointGoGraphSetState
	err = legacyNativeCheckpointReadJSON(ctx, legacyGoGraphTailW86ProcessPort(store), request.Ref, &state)
	cancel()
	if err != nil || state.validate(request.Binding) != nil {
		test.Fatal("existing set codec rejected its actual disk CAS record", err)
	}
	legacyGoGraphTailW86ProcessFormat(test, values, state.Roots, store)
	if after := legacyGoGraphTailW86ProcessSnapshot(test, values); !reflect.DeepEqual(before, after) {
		test.Fatal("capture changed source identities, physical len/cap, full backing, or float bits")
	}
	return request
}

func legacyGoGraphTailW86ProcessRecapture(test *testing.T, values []legacyNativeAnyValue, store *legacyGoGraphSetW85ProcessStore, report *legacyGoGraphSetW85ProcessReport) {
	test.Helper()
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	reserved := report.NativeBytes + LegacyCanonicalEncodingBytes
	budget := legacyGoGraphSetW85ProcessBudget - reserved
	if budget <= 0 || len(values) != legacyNativeCheckpointPageItems {
		test.Fatal("recapture cannot retain the real native roots and bounded descriptor metadata")
	}
	descriptors := make([]legacyNativeCheckpointValue, 0, len(values))
	for _, value := range values {
		constructor, cancel := context.WithCancel(context.Background())
		store.current = nil
		reads, writes := store.reads, store.writes
		writer, err := newLegacyNativeCheckpointGraphWriter(constructor, value, legacyGoGraphTailW86ProcessPort(store), identity, budget)
		cancel()
		if err != nil || writer == nil || store.reads != reads || store.writes != writes {
			test.Fatal("existing graph writer rejected native full backing or performed eager CAS IO", err)
		}
		if err := legacyGoGraphSetW85ProcessDrive(test, writer, store, budget, reserved, identity.retainedBytes, report, false); err != nil {
			writer.close()
			test.Fatal("existing graph writer failed to recapture actual native full backing", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.begin(ctx)
		descriptor, err := writer.take(ctx)
		cancel()
		writer.close()
		if retained, retainedErr := writer.retainedBytes(); err != nil || retainedErr != nil || retained != 0 {
			test.Fatal("native full-backing writer handoff leaked private state", err, retained, retainedErr)
		}
		descriptors = append(descriptors, descriptor)
	}
	if retained, err := identity.retainedBytes(); err != nil || retained <= 0 {
		test.Fatal("recapture lost its real shared graph identity before handoff", retained, err)
	}
	identity.close()
	if retained, err := identity.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("recapture close leaked shared graph identity metadata", retained, err)
	}
	legacyGoGraphTailW86ProcessFormat(test, legacyGoGraphTailW86ProcessFixtures(), descriptors, store)
	legacyGoGraphSetW85ProcessMetadataWrite(test, store.directory, "descriptors.json", descriptors)
	if retained := legacyGoGraphTailW86ProcessValidate(test, values); retained != report.NativeBytes {
		test.Fatal("existing graph writer mutated real native backing or ownership accounting")
	}
}

func legacyGoGraphTailW86ProcessLaunch(test *testing.T, directory string, request legacyGoGraphSetW85ProcessRequest) legacyGoGraphSetW85ProcessReport {
	test.Helper()
	legacyGoGraphSetW85ProcessMetadataWrite(test, directory, "w86-tail-request.json", request)
	if err := os.Remove(filepath.Join(directory, "w86-tail-report.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyNativeCheckpointGoGraphSetW86TailProcessChild$", "-test.count=1", "-test.timeout=20s")
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, legacyGoGraphTailW86ProcessEnvironment+"=") && !strings.HasPrefix(variable, legacyGoGraphSetW85ProcessEnvironment+"=") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, legacyGoGraphTailW86ProcessEnvironment+"="+directory)
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("fresh-process full-backing case %s failed: %v\n%s", request.Mode, err, output)
	}
	var report legacyGoGraphSetW85ProcessReport
	legacyGoGraphSetW85ProcessMetadataRead(test, directory, "w86-tail-report.json", &report)
	if report.PID <= 0 || report.PID == os.Getpid() || report.PID != command.ProcessState.Pid() || report.Mode != request.Mode ||
		report.Peak <= 0 || report.Peak > request.Budget || report.Steps <= 0 || report.Steps > legacyGoGraphSetW85ProcessSteps*legacyNativeCheckpointPageItems {
		test.Fatal("full-backing child report does not identify a real bounded fresh process", report)
	}
	return report
}

func legacyGoGraphTailW86ProcessSuccess(test *testing.T, report legacyGoGraphSetW85ProcessReport) {
	test.Helper()
	expected := legacyGoGraphTailW86ProcessFixtures()
	if report.Failure != "" || report.Roots != len(expected) || len(report.Digests) != len(expected) || report.Reads == 0 ||
		report.NativeBytes <= 0 || report.NativeBytes > legacyGoGraphSetW85ProcessBudget {
		test.Fatal("fresh full-backing process returned empty success or omitted native restoration/accounting", report)
	}
	if report.Mode == "recapture" && report.Writes == 0 || report.Mode != "recapture" && report.Writes != 0 {
		test.Fatal("full-backing process did not follow its actual read-only restore or real graph-writer path", report)
	}
	for rootIndex, value := range expected {
		if report.Digests[rootIndex] != legacyGoGraphSetW85ProcessDigestValue(test, value) {
			test.Fatal("fresh process changed visible canonical bytes after full-backing native validation", rootIndex)
		}
	}
	test.Logf("W86 full-backing preparation only: parent pid=%d child pid=%d mode=%s roots=%d steps=%d reads=%d writes=%d peak=%d native=%d; no overlap/currentness/stage claim", os.Getpid(), report.PID, report.Mode, report.Roots, report.Steps, report.Reads, report.Writes, report.Peak, report.NativeBytes)
}

func TestLegacyNativeCheckpointGoGraphSetW86TailProcessRestore(test *testing.T) {
	directory := test.TempDir()
	request := legacyGoGraphTailW86ProcessCapture(test, directory)
	report := legacyGoGraphTailW86ProcessLaunch(test, directory, request)
	legacyGoGraphTailW86ProcessSuccess(test, report)
}

func TestLegacyNativeCheckpointGoGraphSetW86TailProcessRecapture(test *testing.T) {
	directory := test.TempDir()
	request := legacyGoGraphTailW86ProcessCapture(test, directory)
	request.Mode = "recapture"
	first := legacyGoGraphTailW86ProcessLaunch(test, directory, request)
	legacyGoGraphTailW86ProcessSuccess(test, first)
	request.Mode = "graph-restore"
	second := legacyGoGraphTailW86ProcessLaunch(test, directory, request)
	legacyGoGraphTailW86ProcessSuccess(test, second)
	if first.PID == second.PID || !reflect.DeepEqual(first.Digests, second.Digests) {
		test.Fatal("another fresh process reused a PID or changed recaptured visible bytes")
	}
}

func TestLegacyNativeCheckpointGoGraphSetW86TailProcessChild(test *testing.T) {
	directory := os.Getenv(legacyGoGraphTailW86ProcessEnvironment)
	if directory == "" {
		test.Skip("idle W86 full-backing child helper; not a parent group")
	}
	var request legacyGoGraphSetW85ProcessRequest
	legacyGoGraphSetW85ProcessMetadataRead(test, directory, "w86-tail-request.json", &request)
	if request.ParentPID <= 0 || request.ParentPID == os.Getpid() || request.ParentPID != os.Getppid() ||
		request.Budget != legacyGoGraphSetW85ProcessBudget || !request.Ref.valid() || ValidateContentHash(request.Binding) != nil {
		test.Fatal("full-backing child did not receive a bounded source-free direct-parent request")
	}
	if request.Mode != "restore" && request.Mode != "recapture" && request.Mode != "graph-restore" {
		test.Fatal("unknown full-backing child mode", request.Mode)
	}
	report := legacyGoGraphSetW85ProcessReport{PID: os.Getpid(), Mode: request.Mode}
	store := &legacyGoGraphSetW85ProcessStore{test: test, directory: directory}
	var values []legacyNativeAnyValue
	if request.Mode == "graph-restore" {
		values = legacyGoGraphSetW85ProcessGraphRestore(test, store, &report)
	} else {
		constructor, cancel := context.WithCancel(context.Background())
		restore, err := newLegacyNativeCheckpointGoGraphSetRestore(constructor, request.Ref, request.Binding, legacyGoGraphTailW86ProcessPort(store), request.Budget)
		cancel()
		if err != nil || restore == nil || store.reads != 0 || store.writes != 0 {
			test.Fatal("real full-backing restore constructor failed or performed eager CAS IO", err)
		}
		defer restore.close()
		if err := legacyGoGraphSetW85ProcessDrive(test, restore, store, request.Budget, 0, nil, &report, false); err != nil {
			test.Fatal("real fresh-process full-backing set restore failed", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.begin(ctx)
		values, err = restore.take(ctx)
		cancel()
		if err != nil || len(values) == 0 || store.reads == 0 {
			test.Fatal("full-backing restore returned empty success without actual disk CAS reads", err)
		}
		if repeated, err := restore.take(context.Background()); len(repeated) != 0 || !errors.Is(err, ErrIntegrity) {
			test.Fatal("duplicate full-backing handoff exposed roots or lost its closed-state error", err)
		}
		restore.close()
		if retained, err := restore.retainedBytes(); err != nil || retained != 0 {
			test.Fatal("full-backing restore handoff retained private producers or shared registry", retained, err)
		}
	}
	report.NativeBytes = legacyGoGraphTailW86ProcessValidate(test, values)
	report.Roots = len(values)
	for _, value := range values {
		report.Digests = append(report.Digests, legacyGoGraphSetW85ProcessDigestValue(test, value.value))
	}
	if request.Mode == "recapture" {
		legacyGoGraphTailW86ProcessRecapture(test, values, store, &report)
	}
	report.Reads, report.Writes = store.reads, store.writes
	legacyGoGraphSetW85ProcessMetadataWrite(test, directory, "w86-tail-report.json", report)
	test.Logf("W86 native full-backing preparation child pid=%d parent pid=%d mode=%s roots=%d; not full Event/stage/worker recovery", report.PID, request.ParentPID, report.Mode, report.Roots)
}
