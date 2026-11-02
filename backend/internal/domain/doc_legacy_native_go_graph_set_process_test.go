package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const legacyGoGraphSetW85ProcessEnvironment = "CXTHUB_W85_GO_GRAPH_SET_PROCESS"
const legacyGoGraphSetW85ProcessBudget = int64(16 << 20)
const legacyGoGraphSetW85ProcessSteps = 16384

type legacyGoGraphSetW85ProcessRequest struct {
	Mode      string                    `json:"mode"`
	Ref       legacyNativeCheckpointRef `json:"ref"`
	Binding   ContentHash               `json:"binding"`
	Budget    int64                     `json:"budget"`
	ParentPID int                       `json:"parent_pid"`
}

type legacyGoGraphSetW85ProcessDigest struct {
	Hash  [sha256.Size]byte `json:"hash"`
	Bytes int64             `json:"bytes"`
}

type legacyGoGraphSetW85ProcessReport struct {
	PID         int                                `json:"pid"`
	Mode        string                             `json:"mode"`
	Roots       int                                `json:"roots"`
	Digests     []legacyGoGraphSetW85ProcessDigest `json:"digests"`
	Peak        int64                              `json:"peak"`
	NativeBytes int64                              `json:"native_bytes"`
	Steps       int                                `json:"steps"`
	Reads       int                                `json:"reads"`
	Writes      int                                `json:"writes"`
	Failure     string                             `json:"failure"`
}

type legacyGoGraphSetW85ProcessWork interface {
	step(context.Context) (bool, error)
	retainedBytes() (int64, error)
	close()
}

type legacyGoGraphSetW85ProcessStore struct {
	test      *testing.T
	directory string
	current   context.Context
	calls     int
	bytes     int
	reads     int
	writes    int
}

func (store *legacyGoGraphSetW85ProcessStore) begin(ctx context.Context) {
	store.current, store.calls, store.bytes = ctx, 0, 0
}

func (store *legacyGoGraphSetW85ProcessStore) account(ctx context.Context, hash ContentHash, length int) {
	store.test.Helper()
	if store.current == nil || ctx != store.current || ctx.Err() != nil || ValidateContentHash(hash) != nil ||
		length <= 0 || length > LegacyCanonicalEncodingBytes {
		store.test.Fatal("CAS used a stale constructor context, invalid hash, or unbounded object", length)
	}
	store.calls++
	store.bytes += length
	if store.calls > legacyNativeCheckpointPageItems || store.bytes > LegacyCanonicalEncodingBytes {
		store.test.Fatal("CAS escaped the original 32-operation/64KiB step quantum", store.calls, store.bytes)
	}
}

func (store *legacyGoGraphSetW85ProcessStore) path(hash ContentHash) string {
	return filepath.Join(store.directory, strings.TrimPrefix(string(hash), "sha256:")+".cas")
}

func (store *legacyGoGraphSetW85ProcessStore) port() legacyNativeCheckpointIO {
	return legacyNativeCheckpointIO{
		Put: func(ctx context.Context, hash ContentHash, body []byte) error {
			store.account(ctx, hash, len(body))
			if HashContent(body) != hash {
				store.test.Fatal("producer supplied a CAS object with the wrong content hash")
			}
			if existing, err := os.ReadFile(store.path(hash)); err == nil {
				if !bytes.Equal(existing, body) {
					store.test.Fatal("producer changed an existing immutable CAS object")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			} else if err := os.WriteFile(store.path(hash), body, 0600); err != nil {
				return err
			}
			store.writes++
			return ctx.Err()
		},
		Read: func(ctx context.Context, hash ContentHash, limit int) ([]byte, error) {
			store.account(ctx, hash, limit)
			file, err := os.Open(store.path(hash))
			if err != nil {
				return nil, err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil {
				return nil, err
			}
			if info.Size() != int64(limit) {
				return nil, ErrPreparedDocRangeObject
			}
			body, err := io.ReadAll(io.LimitReader(file, int64(limit)))
			store.reads++
			return body, err
		},
	}
}

func legacyGoGraphSetW85ProcessMetadataWrite(test *testing.T, directory, name string, metadata any) {
	test.Helper()
	body, err := json.Marshal(metadata)
	if err != nil || len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes {
		test.Fatal("small process metadata escaped its 64KiB bound", len(body), err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil {
		test.Fatal(err)
	}
}

func legacyGoGraphSetW85ProcessMetadataRead(test *testing.T, directory, name string, target any) {
	test.Helper()
	file, err := os.Open(filepath.Join(directory, name))
	if err != nil {
		test.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > LegacyCanonicalEncodingBytes {
		test.Fatal("process metadata is absent or exceeds 64KiB", err)
	}
	decoder := json.NewDecoder(io.LimitReader(file, info.Size()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		test.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		test.Fatal("process metadata has trailing data", err)
	}
}

func legacyGoGraphSetW85ProcessFixtures() []any {
	negativeZero := math.Float64frombits(1 << 63)
	sharedMap := map[string]any{"text": "native\x00\n\ud55c\uae00\U0001f642", "zero": negativeZero, "true": true}
	separateMap := map[string]any{"text": "native\x00\n\ud55c\uae00\U0001f642", "zero": negativeZero, "true": true}
	sharedArray := make([]any, 3, 7)
	sharedArray[0], sharedArray[1] = sharedMap, negativeZero
	separateArray := make([]any, 3, 7)
	copy(separateArray, sharedArray)
	emptyMap := map[string]any{}
	emptyArray := make([]any, 0)
	emptyCapacity := make([]any, 0, 5)
	wrapper := map[string]any{"map": sharedMap, "array": sharedArray, "separate_map": separateMap, "separate_array": separateArray}
	var deep any = sharedMap
	for depth := 0; depth < 12; depth++ {
		deep = map[string]any{"next": deep, "shared": sharedArray}
	}
	return []any{
		sharedMap, sharedMap, separateMap, sharedArray, sharedArray, separateArray, wrapper,
		nil, false, true, negativeZero, math.SmallestNonzeroFloat64, math.MaxFloat64,
		map[string]any(nil), emptyMap, []any(nil), emptyArray, emptyCapacity,
		strings.Repeat("large-native-text-", 8193), wrapper, float64(0), -math.SmallestNonzeroFloat64,
		float64(0.1), "\x00\n\t\"\\\ud55c\uae00\U0001f642", sharedMap, deep, []any{sharedMap, sharedArray},
		map[string]any{"deep": deep}, emptyMap, emptyCapacity, sharedArray[:2:7], sharedArray[:3:3],
	}
}

func legacyGoGraphSetW85ProcessDigestValue(test *testing.T, value any) legacyGoGraphSetW85ProcessDigest {
	test.Helper()
	encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
	if err != nil {
		test.Fatal("existing canonical encoder rejected a fixture", err)
	}
	defer encoding.release()
	hasher := sha256.New()
	var result legacyGoGraphSetW85ProcessDigest
	for stepIndex := 0; stepIndex < legacyGoGraphSetW85ProcessSteps; stepIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		chunk, complete, err := encoding.Next(ctx)
		cancel()
		if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes ||
			encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > LegacyCanonicalEncodingBytes {
			test.Fatal("existing canonical stream escaped its bounded output/storage contract", err)
		}
		if _, err := hasher.Write(chunk); err != nil {
			test.Fatal(err)
		}
		result.Bytes += int64(len(chunk))
		if complete {
			if result.Bytes == 0 {
				test.Fatal("canonical stream returned empty success")
			}
			copy(result.Hash[:], hasher.Sum(nil))
			return result
		}
	}
	test.Fatal("canonical stream exhausted the finite step schedule")
	return result
}

func legacyGoGraphSetW85ProcessDrive(test *testing.T, work legacyGoGraphSetW85ProcessWork, store *legacyGoGraphSetW85ProcessStore,
	budget, reserved int64, shared func() (int64, error), report *legacyGoGraphSetW85ProcessReport, cancelStep bool) error {
	test.Helper()
	if budget <= 0 || reserved < 0 || reserved > legacyGoGraphSetW85ProcessBudget-budget {
		test.Fatal("invalid aggregate fixture budget reservation", budget, reserved)
	}
	measure := func() {
		retained, err := work.retainedBytes()
		if err != nil || retained < 0 || retained > budget {
			test.Fatal("work has invalid retained bytes", retained, budget, err)
		}
		if shared != nil {
			registered, err := shared()
			if err != nil || registered < 0 || registered > budget-retained {
				test.Fatal("work plus shared registry escaped the original aggregate budget", retained, registered, budget, err)
			}
			retained += registered
		}
		report.Peak = max(report.Peak, retained+reserved)
	}
	for stepIndex := 0; stepIndex < legacyGoGraphSetW85ProcessSteps; stepIndex++ {
		measure()
		ctx, cancel := context.WithCancel(context.Background())
		if cancelStep && stepIndex == 2 {
			cancel()
		}
		store.begin(ctx)
		complete, err := work.step(ctx)
		cancel()
		report.Steps++
		measure()
		if err != nil {
			if complete {
				test.Fatal("failed step exposed successful completion", err)
			}
			return err
		}
		if complete {
			if cancelStep {
				test.Fatal("cancellation fixture completed before its scheduled cancellation")
			}
			return nil
		}
	}
	test.Fatal("graph work exhausted its finite step schedule")
	return ErrIntegrity
}

type legacyGoGraphSetW85ProcessIdentity struct {
	kind     reflect.Kind
	pointer  uintptr
	length   int
	capacity int
}

type legacyGoGraphSetW85ProcessOwner struct {
	object *legacyNativeAnyMap
	array  *legacyNativeAnyArray
}

func legacyGoGraphSetW85ProcessIdentityOf(value any) legacyGoGraphSetW85ProcessIdentity {
	switch decoded := value.(type) {
	case map[string]any:
		return legacyGoGraphSetW85ProcessIdentity{kind: reflect.Map, pointer: reflect.ValueOf(decoded).Pointer()}
	case []any:
		return legacyGoGraphSetW85ProcessIdentity{kind: reflect.Slice, pointer: reflect.ValueOf(decoded).Pointer(), length: len(decoded), capacity: cap(decoded)}
	default:
		return legacyGoGraphSetW85ProcessIdentity{}
	}
}

func legacyGoGraphSetW85ProcessValidate(test *testing.T, actual []legacyNativeAnyValue) int64 {
	test.Helper()
	expected := legacyGoGraphSetW85ProcessFixtures()
	if len(actual) != len(expected) || len(actual) != legacyNativeCheckpointPageItems {
		test.Fatal("restore changed the exact 32-root set", len(actual))
	}
	forward := make(map[legacyGoGraphSetW85ProcessIdentity]legacyGoGraphSetW85ProcessOwner)
	reverse := make(map[legacyGoGraphSetW85ProcessOwner]legacyGoGraphSetW85ProcessIdentity)
	visits := 0
	var visit func(any, legacyNativeAnyValue, int)
	visit = func(wanted any, value legacyNativeAnyValue, depth int) {
		visits++
		if visits > 512 || depth > 32 {
			test.Fatal("native validation exceeded its finite fixture grammar")
		}
		if err := value.validateOwnership(); err != nil || reflect.TypeOf(wanted) != reflect.TypeOf(value.value) {
			test.Fatal("restored value lost its exact type or actual native ownership", reflect.TypeOf(wanted), reflect.TypeOf(value.value), err)
		}
		identity := legacyGoGraphSetW85ProcessIdentityOf(wanted)
		if identity.pointer != 0 {
			owner := legacyGoGraphSetW85ProcessOwner{object: value.owner.object, array: value.owner.array}
			if owner == (legacyGoGraphSetW85ProcessOwner{}) {
				test.Fatal("nonnil container lacks a real native owner")
			}
			if previous, found := forward[identity]; found {
				if previous != owner {
					test.Fatal("the producer or restore registry split a cross-root/descendant alias")
				}
				return
			}
			if previous, found := reverse[owner]; found && previous != identity {
				test.Fatal("equal-valued but distinct containers or slice shapes were coalesced")
			}
			forward[identity], reverse[owner] = owner, identity
		}
		switch decoded := wanted.(type) {
		case map[string]any:
			container := value.value.(map[string]any)
			if (decoded == nil) != (container == nil) || len(decoded) != len(container) {
				test.Fatal("restored map changed typed nilness or length")
			}
			if decoded == nil {
				return
			}
			cursor, err := value.owner.mapCheckpoint(context.Background(), true)
			if err != nil {
				test.Fatal("actual native map checkpoint rejected restored ownership", err)
			}
			defer cursor.close()
			seen := make(map[string]bool)
			for position := 0; position <= len(decoded); position++ {
				entry, available, err := cursor.next(context.Background())
				if err != nil || available != (position < len(decoded)) {
					test.Fatal("restored native map metadata lost an entry or its exact EOF", position, err)
				}
				if !available {
					break
				}
				childExpected, found := decoded[entry.Key]
				if !found || seen[entry.Key] || entry.KeyCapacity < len(entry.Key) {
					test.Fatal("native map returned an unknown, duplicate, or invalid key")
				}
				seen[entry.Key] = true
				child := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes}, excess: entry.ValueExcess, owner: entry.owner}
				if text, ok := entry.Value.(string); ok {
					if entry.ValueExcess < 0 || entry.ValueExcess%2 != 0 || entry.ValueExcess > legacyGoGraphSetW85ProcessBudget {
						test.Fatal("native string metadata has invalid backing excess")
					}
					child.capacity = len(text) + int(entry.ValueExcess/2)
				}
				visit(childExpected, child, depth+1)
			}
		case []any:
			container := value.value.([]any)
			if (decoded == nil) != (container == nil) || len(decoded) != len(container) || cap(decoded) != cap(container) {
				test.Fatal("restored array changed typed nilness, empty state, length, or physical capacity", len(container), cap(container), len(decoded), cap(decoded))
			}
			for position, childExpected := range decoded {
				child, err := value.owner.arrayValue(context.Background(), position)
				if err != nil {
					test.Fatal("restored array lacks real child metadata", err)
				}
				visit(childExpected, child, depth+1)
			}
		case float64:
			if math.Float64bits(decoded) != math.Float64bits(value.value.(float64)) {
				test.Fatal("restore changed finite float bits, including negative zero")
			}
		default:
			if !reflect.DeepEqual(wanted, value.value) {
				test.Fatal("restore changed a primitive value")
			}
		}
	}
	var retained int64
	for rootIndex, wanted := range expected {
		visit(wanted, actual[rootIndex], 0)
		rootBytes := actual[rootIndex].heap.bytes + actual[rootIndex].owner.bytes
		if rootBytes < 0 || rootBytes > legacyGoGraphSetW85ProcessBudget-retained {
			test.Fatal("returned native roots escaped the original conservative handoff budget")
		}
		retained += rootBytes
	}
	if retained == 0 {
		test.Fatal("nonempty native roots were handed off with zero retained memory")
	}
	return retained
}

func legacyGoGraphSetW85ProcessCapture(test *testing.T, directory string) legacyGoGraphSetW85ProcessRequest {
	test.Helper()
	values := legacyGoGraphSetW85ProcessFixtures()
	before := make([]legacyGoGraphSetW85ProcessDigest, len(values))
	identities := make([]legacyGoGraphSetW85ProcessIdentity, len(values))
	for rootIndex, value := range values {
		before[rootIndex] = legacyGoGraphSetW85ProcessDigestValue(test, value)
		identities[rootIndex] = legacyGoGraphSetW85ProcessIdentityOf(value)
	}
	request := legacyGoGraphSetW85ProcessRequest{Mode: "restore", Binding: HashContent([]byte("W85-exclusive-quiescent-root-set")), Budget: legacyGoGraphSetW85ProcessBudget, ParentPID: os.Getpid()}
	store := &legacyGoGraphSetW85ProcessStore{test: test, directory: directory}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeCheckpointGoGraphSet(constructor, values, request.Binding, store.port(), request.Budget)
	cancel()
	if err != nil || work == nil || store.reads != 0 || store.writes != 0 {
		test.Fatal("root-set constructor failed or performed eager CAS IO", err)
	}
	defer work.close()
	report := &legacyGoGraphSetW85ProcessReport{}
	if err := legacyGoGraphSetW85ProcessDrive(test, work, store, request.Budget, 0, nil, report, false); err != nil {
		test.Fatal("real root-set capture failed", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.begin(ctx)
	request.Ref, err = work.take(ctx)
	cancel()
	if err != nil || !request.Ref.valid() || store.writes == 0 || report.Steps == 0 {
		test.Fatal("root-set capture returned empty success", err)
	}
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("captured set retained producers or registry after handoff", retained, err)
	}
	for rootIndex, value := range values {
		if legacyGoGraphSetW85ProcessIdentityOf(value) != identities[rootIndex] || legacyGoGraphSetW85ProcessDigestValue(test, value) != before[rootIndex] {
			test.Fatal("capture mutated the caller's borrowed graph", rootIndex)
		}
	}
	if math.Float64bits(values[10].(float64)) != 1<<63 || math.Float64bits(values[0].(map[string]any)["zero"].(float64)) != 1<<63 {
		test.Fatal("capture rewrote borrowed negative-zero bits")
	}
	return request
}

func legacyGoGraphSetW85ProcessLaunch(test *testing.T, directory string, request legacyGoGraphSetW85ProcessRequest) legacyGoGraphSetW85ProcessReport {
	test.Helper()
	legacyGoGraphSetW85ProcessMetadataWrite(test, directory, "request.json", request)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyNativeCheckpointGoGraphSetW85ProcessChild$", "-test.count=1", "-test.timeout=20s")
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, legacyGoGraphSetW85ProcessEnvironment+"=") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, legacyGoGraphSetW85ProcessEnvironment+"="+directory)
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("fresh-process graph-set case %s failed: %v\n%s", request.Mode, err, output)
	}
	var report legacyGoGraphSetW85ProcessReport
	legacyGoGraphSetW85ProcessMetadataRead(test, directory, "report.json", &report)
	if report.PID <= 0 || report.PID == os.Getpid() || report.PID != command.ProcessState.Pid() || report.Mode != request.Mode ||
		report.Peak < 0 || report.Peak > request.Budget || report.Steps > legacyGoGraphSetW85ProcessSteps*legacyNativeCheckpointPageItems {
		test.Fatal("child report does not identify a real bounded fresh process", report)
	}
	test.Logf("W85 root-set preparation only: child pid=%d mode=%s roots=%d steps=%d reads=%d writes=%d peak=%d error=%s", report.PID, report.Mode, report.Roots, report.Steps, report.Reads, report.Writes, report.Peak, report.Failure)
	return report
}

func legacyGoGraphSetW85ProcessSuccess(test *testing.T, report legacyGoGraphSetW85ProcessReport) {
	test.Helper()
	expected := legacyGoGraphSetW85ProcessFixtures()
	if report.Failure != "" || report.Roots != len(expected) || len(report.Digests) != len(expected) || report.Reads == 0 ||
		report.Steps == 0 || report.Peak == 0 || report.NativeBytes <= 0 || report.NativeBytes > legacyGoGraphSetW85ProcessBudget {
		test.Fatal("fresh process returned empty success or omitted real restoration/accounting", report)
	}
	for rootIndex, value := range expected {
		if report.Digests[rootIndex] != legacyGoGraphSetW85ProcessDigestValue(test, value) {
			test.Fatal("fresh process changed canonical native value bytes", rootIndex)
		}
	}
}

func TestLegacyNativeCheckpointGoGraphSetW85ProcessSharedRoots(test *testing.T) {
	directory := test.TempDir()
	request := legacyGoGraphSetW85ProcessCapture(test, directory)
	report := legacyGoGraphSetW85ProcessLaunch(test, directory, request)
	legacyGoGraphSetW85ProcessSuccess(test, report)
	if report.Writes != 0 {
		test.Fatal("read-only restore unexpectedly wrote CAS objects")
	}
}

func TestLegacyNativeCheckpointGoGraphSetW85ProcessRecapture(test *testing.T) {
	directory := test.TempDir()
	request := legacyGoGraphSetW85ProcessCapture(test, directory)
	request.Mode = "recapture"
	first := legacyGoGraphSetW85ProcessLaunch(test, directory, request)
	legacyGoGraphSetW85ProcessSuccess(test, first)
	if first.Writes == 0 {
		test.Fatal("recapture did not use the existing graph writer and real CAS")
	}
	request.Mode = "graph-restore"
	second := legacyGoGraphSetW85ProcessLaunch(test, directory, request)
	legacyGoGraphSetW85ProcessSuccess(test, second)
	if first.PID == second.PID || second.Writes != 0 || !reflect.DeepEqual(first.Digests, second.Digests) {
		test.Fatal("second fresh process changed the recaptured shared native graph")
	}
}

func TestLegacyNativeCheckpointGoGraphSetW85ProcessFences(test *testing.T) {
	for _, mode := range []string{"wrong-binding", "corrupt-hash", "invalid-format", "small-budget", "cancel"} {
		test.Run(mode, func(test *testing.T) {
			directory := test.TempDir()
			request := legacyGoGraphSetW85ProcessCapture(test, directory)
			request.Mode = mode
			switch mode {
			case "wrong-binding":
				request.Binding = HashContent([]byte("W85-different-root-binding"))
			case "corrupt-hash":
				store := &legacyGoGraphSetW85ProcessStore{test: test, directory: directory}
				body, err := os.ReadFile(store.path(request.Ref.Hash))
				if err != nil || len(body) != request.Ref.Bytes {
					test.Fatal("cannot corrupt the actual bounded root CAS object", err)
				}
				body[0] ^= 1
				if err := os.WriteFile(store.path(request.Ref.Hash), body, 0600); err != nil {
					test.Fatal(err)
				}
			case "invalid-format":
				body := []byte(`{"v":255,"e":[]}`)
				request.Ref = legacyNativeCheckpointRef{Hash: HashContent(body), Bytes: len(body)}
				store := &legacyGoGraphSetW85ProcessStore{test: test, directory: directory}
				if err := os.WriteFile(store.path(request.Ref.Hash), body, 0600); err != nil {
					test.Fatal(err)
				}
			case "small-budget":
				request.Budget = 1
			}
			report := legacyGoGraphSetW85ProcessLaunch(test, directory, request)
			if report.Failure == "" || report.Roots != 0 || len(report.Digests) != 0 || report.NativeBytes != 0 || report.Writes != 0 {
				test.Fatal("failed fresh-process restore exposed native roots, empty success, or CAS writes", report)
			}
			if mode == "small-budget" && (report.Reads != 0 || report.Steps != 0) {
				test.Fatal("one-byte budget was not rejected before allocation/IO", report)
			}
			if mode == "cancel" && (report.Reads == 0 || report.Steps != 3) {
				test.Fatal("cancellation did not interrupt actual bounded restore progress", report)
			}
		})
	}
}

func legacyGoGraphSetW85ProcessRecapture(test *testing.T, values []legacyNativeAnyValue, store *legacyGoGraphSetW85ProcessStore, report *legacyGoGraphSetW85ProcessReport) {
	test.Helper()
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	budget := legacyGoGraphSetW85ProcessBudget - report.NativeBytes - LegacyCanonicalEncodingBytes
	if budget <= 0 {
		test.Fatal("recapture cannot retain roots and bounded descriptor metadata together")
	}
	descriptors := make([]legacyNativeCheckpointValue, 0, len(values))
	for _, value := range values {
		constructor, cancel := context.WithCancel(context.Background())
		store.current = nil
		writer, err := newLegacyNativeCheckpointGraphWriter(constructor, value, store.port(), identity, budget)
		cancel()
		if err != nil || writer == nil {
			test.Fatal("existing shared graph writer rejected a restored real native owner", err)
		}
		if err := legacyGoGraphSetW85ProcessDrive(test, writer, store, budget, report.NativeBytes+LegacyCanonicalEncodingBytes, identity.retainedBytes, report, false); err != nil {
			writer.close()
			test.Fatal("existing shared graph writer failed", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.begin(ctx)
		descriptor, err := writer.take(ctx)
		cancel()
		writer.close()
		if retained, retainedErr := writer.retainedBytes(); err != nil || retainedErr != nil || retained != 0 {
			test.Fatal("graph writer did not hand off and release its private state", err, retained, retainedErr)
		}
		descriptors = append(descriptors, descriptor)
	}
	if retained, err := identity.retainedBytes(); err != nil || retained <= 0 {
		test.Fatal("per-root writer handoff erased its shared identity", retained, err)
	}
	legacyGoGraphSetW85ProcessMetadataWrite(test, store.directory, "descriptors.json", descriptors)
	identity.close()
	if retained, err := identity.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("graph writer identity close leaked registry metadata", retained, err)
	}
	legacyGoGraphSetW85ProcessValidate(test, values)
}

func legacyGoGraphSetW85ProcessGraphRestore(test *testing.T, store *legacyGoGraphSetW85ProcessStore, report *legacyGoGraphSetW85ProcessReport) []legacyNativeAnyValue {
	test.Helper()
	var descriptors []legacyNativeCheckpointValue
	legacyGoGraphSetW85ProcessMetadataRead(test, store.directory, "descriptors.json", &descriptors)
	if len(descriptors) != legacyNativeCheckpointPageItems {
		test.Fatal("recaptured descriptors lost the exact bounded root set")
	}
	registry := newLegacyNativeCheckpointGraphRestored()
	defer registry.close()
	values := make([]legacyNativeAnyValue, 0, len(descriptors))
	var scalarBytes int64
	for _, descriptor := range descriptors {
		reserved := int64(LegacyCanonicalEncodingBytes) + scalarBytes
		budget := legacyGoGraphSetW85ProcessBudget - reserved
		constructor, cancel := context.WithCancel(context.Background())
		store.current = nil
		restore, err := newLegacyNativeCheckpointGraphRestore(constructor, descriptor, store.port(), registry, budget)
		cancel()
		if err != nil || restore == nil {
			test.Fatal("existing graph restore rejected the portable descriptors", err)
		}
		if err := legacyGoGraphSetW85ProcessDrive(test, restore, store, budget, reserved, registry.retainedBytes, report, false); err != nil {
			restore.close()
			test.Fatal("existing shared graph restore failed", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.begin(ctx)
		value, err := restore.take(ctx)
		cancel()
		restore.close()
		if retained, retainedErr := restore.retainedBytes(); err != nil || retainedErr != nil || retained != 0 {
			test.Fatal("graph restore handoff leaked private retained state", err, retained, retainedErr)
		}
		values = append(values, value)
		if value.owner.object == nil && value.owner.array == nil {
			if value.heap.bytes < 0 || value.heap.bytes > legacyGoGraphSetW85ProcessBudget-reserved {
				test.Fatal("handed-off scalar memory escaped the shared restore budget")
			}
			scalarBytes += value.heap.bytes
		}
	}
	if retained, err := registry.retainedBytes(); err != nil || retained <= 0 {
		test.Fatal("per-root restore handoff erased the shared restore registry", retained, err)
	}
	registry.close()
	if retained, err := registry.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("shared restore registry close leaked memory", retained, err)
	}
	return values
}

func TestLegacyNativeCheckpointGoGraphSetW85ProcessChild(test *testing.T) {
	directory := os.Getenv(legacyGoGraphSetW85ProcessEnvironment)
	if directory == "" {
		test.Skip("idle W85 fresh-process helper; exclude from the three parent group patterns")
	}
	var request legacyGoGraphSetW85ProcessRequest
	legacyGoGraphSetW85ProcessMetadataRead(test, directory, "request.json", &request)
	if request.ParentPID <= 0 || request.ParentPID == os.Getpid() || request.Budget <= 0 || request.Budget > legacyGoGraphSetW85ProcessBudget ||
		!request.Ref.valid() || ValidateContentHash(request.Binding) != nil {
		test.Fatal("child did not receive a bounded, source-free process request")
	}
	report := legacyGoGraphSetW85ProcessReport{PID: os.Getpid(), Mode: request.Mode}
	store := &legacyGoGraphSetW85ProcessStore{test: test, directory: directory}
	var values []legacyNativeAnyValue
	if request.Mode == "graph-restore" {
		values = legacyGoGraphSetW85ProcessGraphRestore(test, store, &report)
	} else {
		var expected error
		switch request.Mode {
		case "restore", "recapture":
		case "wrong-binding", "invalid-format":
			expected = ErrIntegrity
		case "corrupt-hash":
			expected = ErrPreparedDocRangeObject
		case "small-budget":
			expected = ErrDocumentResourceLimit
		case "cancel":
			expected = context.Canceled
		default:
			test.Fatal("unknown fresh-process fixture mode", request.Mode)
		}
		constructor, cancel := context.WithCancel(context.Background())
		restore, err := newLegacyNativeCheckpointGoGraphSetRestore(constructor, request.Ref, request.Binding, store.port(), request.Budget)
		cancel()
		if store.reads != 0 || store.writes != 0 {
			test.Fatal("set restore constructor performed eager IO")
		}
		if err == nil && restore == nil {
			test.Fatal("restore constructor returned empty success")
		}
		if err != nil && restore != nil {
			restore.close()
			test.Fatal("failed restore constructor exposed a live work object", err)
		}
		if err == nil {
			defer restore.close()
			err = legacyGoGraphSetW85ProcessDrive(test, restore, store, request.Budget, 0, nil, &report, request.Mode == "cancel")
			if err == nil {
				ctx, cancel := context.WithCancel(context.Background())
				store.begin(ctx)
				values, err = restore.take(ctx)
				cancel()
				if err == nil && expected == nil {
					if repeated, duplicateErr := restore.take(context.Background()); len(repeated) != 0 || !errors.Is(duplicateErr, ErrIntegrity) {
						test.Fatal("duplicate set handoff returned roots or lost its exact closed-state error", duplicateErr)
					}
				}
			}
			if expected != nil {
				reads, writes := store.reads, store.writes
				if _, retry := restore.step(context.Background()); !errors.Is(retry, expected) {
					test.Fatal("failed restore did not preserve its sticky error", retry, expected)
				}
				if taken, retry := restore.take(context.Background()); len(taken) != 0 || !errors.Is(retry, expected) {
					test.Fatal("failed restore exposed roots or lost its sticky take error", retry)
				}
				if store.reads != reads || store.writes != writes {
					test.Fatal("failed restore retried CAS IO")
				}
			}
			restore.close()
			if retained, retainedErr := restore.retainedBytes(); retainedErr != nil || retained != 0 {
				test.Fatal("restore success/failure/close retained shared producers or native registry", retained, retainedErr)
			}
		}
		if expected != nil {
			if !errors.Is(err, expected) || len(values) != 0 {
				test.Fatal("fresh-process fence changed its exact error or exposed roots", err, expected)
			}
			report.Failure = expected.Error()
		} else if err != nil {
			test.Fatal("fresh-process set restore failed", err)
		}
	}
	if report.Failure == "" {
		report.NativeBytes = legacyGoGraphSetW85ProcessValidate(test, values)
		report.Roots = len(values)
		for _, value := range values {
			report.Digests = append(report.Digests, legacyGoGraphSetW85ProcessDigestValue(test, value.value))
		}
		if request.Mode == "recapture" {
			legacyGoGraphSetW85ProcessRecapture(test, values, store, &report)
		}
	}
	report.Reads, report.Writes = store.reads, store.writes
	legacyGoGraphSetW85ProcessMetadataWrite(test, directory, "report.json", report)
	test.Logf("W85 native-root preparation child pid=%d mode=%s roots=%d; not full Event/stage/worker recovery", report.PID, report.Mode, report.Roots)
}
