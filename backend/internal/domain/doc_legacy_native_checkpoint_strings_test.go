package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

const legacyNativeCheckpointStringsOracleBudget int64 = 64 << 20
const legacyNativeCheckpointStringsOracleSteps = 1 << 20

type legacyNativeCheckpointStringsOracleWork[Value any] interface {
	step(context.Context) (bool, error)
	take(context.Context) (Value, error)
	retainedBytes() (int64, error)
	close()
}

type legacyNativeCheckpointStringsOracleStore struct {
	io       legacyNativeCheckpointIO
	raw      legacyNativeCheckpointIO
	objects  map[ContentHash][]byte
	reads    int
	writes   int
	bytes    int
	metadata int
}

func legacyNativeCheckpointStringsOracleStorage(test *testing.T) *legacyNativeCheckpointStringsOracleStore {
	test.Helper()
	store, objects := legacyNativeCheckpointStoreFixture(test)
	storage := &legacyNativeCheckpointStringsOracleStore{raw: store, objects: objects}
	storage.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		storage.writes++
		storage.bytes += len(body)
		var object map[string]json.RawMessage
		if json.Unmarshal(body, &object) == nil && object["e"] != nil {
			var rows []json.RawMessage
			if err := json.Unmarshal(object["e"], &rows); err != nil || len(rows) > legacyNativeCheckpointPageItems {
				test.Fatal("capture emitted an unbounded metadata page", len(rows), err)
			}
		}
		return store.Put(ctx, hash, body)
	}
	storage.io.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		storage.reads++
		storage.bytes += length
		if length <= 0 || length > LegacyCanonicalEncodingBytes {
			test.Fatal("restore requested an unbounded CAS object", length)
		}
		return store.Read(ctx, hash, length)
	}
	return storage
}

func legacyNativeCheckpointStringsOracleFinish[Value any](test *testing.T, work legacyNativeCheckpointStringsOracleWork[Value], storage *legacyNativeCheckpointStringsOracleStore, budget int64) (Value, int64) {
	test.Helper()
	defer work.close()
	for range legacyNativeCheckpointStringsOracleSteps {
		reads, writes, bodyBytes, metadata := storage.reads, storage.writes, storage.bytes, storage.metadata
		ctx, cancel := context.WithCancel(context.Background())
		ready, err := work.step(ctx)
		cancel()
		if err != nil {
			test.Fatal("checkpoint helper rejected an actual admitted state", err)
		}
		maximumIO := 4 * (legacyNativeCheckpointTreeHeight + 2)
		if storage.reads-reads+storage.writes-writes > maximumIO || storage.bytes-bodyBytes > maximumIO*LegacyCanonicalEncodingBytes || storage.metadata-metadata > legacyNativeCheckpointPageItems {
			test.Fatal("checkpoint step escaped bounded rows or CAS IO", storage.reads-reads, storage.writes-writes, storage.metadata-metadata)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained < 0 || retained > budget {
			test.Fatal("checkpoint workspace escaped native admission", retained, budget, err)
		}
		if !ready {
			continue
		}
		ctx, cancel = context.WithCancel(context.Background())
		value, err := work.take(ctx)
		cancel()
		if err != nil {
			test.Fatal("ready checkpoint transfer failed", err)
		}
		if remaining, err := work.retainedBytes(); err != nil || remaining != 0 {
			test.Fatal("checkpoint transfer retained owned storage", remaining, err)
		}
		return value, retained
	}
	test.Fatal("checkpoint helper exhausted its finite schedule")
	var empty Value
	return empty, 0
}

func legacyNativeCheckpointStringsOracleFailed[Value any](test *testing.T, work legacyNativeCheckpointStringsOracleWork[Value], failure error) {
	test.Helper()
	if ready, err := work.step(context.Background()); ready || !errors.Is(err, failure) {
		test.Fatal("checkpoint failure was not sticky on step", ready, err, failure)
	}
	var empty Value
	if value, err := work.take(context.Background()); !errors.Is(err, failure) || !reflect.DeepEqual(value, empty) {
		test.Fatal("failed checkpoint transferred state or lost its original error", err, failure)
	}
	work.close()
	work.close()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("failed checkpoint retained decoded values or workspace", retained, err)
	}
}

func legacyNativeCheckpointStringsOracleReject[Value any](test *testing.T, work legacyNativeCheckpointStringsOracleWork[Value], constructorError, failure error) {
	test.Helper()
	if constructorError != nil {
		if !errors.Is(constructorError, failure) {
			test.Fatal("checkpoint constructor returned the wrong rejection", constructorError, failure)
		}
		return
	}
	defer work.close()
	for range legacyNativeCheckpointStringsOracleSteps {
		ready, err := work.step(context.Background())
		if err != nil {
			if !errors.Is(err, failure) || ready {
				test.Fatal("checkpoint returned the wrong terminal rejection", ready, err, failure)
			}
			legacyNativeCheckpointStringsOracleFailed(test, work, err)
			return
		}
		if ready {
			test.Fatal("malformed or unadmitted checkpoint became ready")
		}
	}
	test.Fatal("checkpoint rejection exhausted its finite schedule")
}

func legacyNativeCheckpointStringsOracleClone(values []string) []string {
	if values == nil {
		return nil
	}
	cloned := make([]string, cap(values))
	copy(cloned, values[:cap(values)])
	return cloned[:len(values)]
}

func legacyNativeCheckpointStringsOracleBacking(test *testing.T, got, want []string) {
	test.Helper()
	if (got == nil) != (want == nil) || len(got) != len(want) || cap(got) != cap(want) || !reflect.DeepEqual(got[:cap(got)], want[:cap(want)]) {
		test.Fatal("checkpoint changed nil, length, capacity, or a hidden backing slot", len(got), cap(got), len(want), cap(want))
	}
}

func legacyNativeCheckpointStringsOracleObjects(test *testing.T, objects, original map[ContentHash][]byte) {
	test.Helper()
	for hash, body := range original {
		if !bytes.Equal(objects[hash], body) {
			test.Fatal("capture changed an existing immutable CAS object", hash)
		}
	}
	for hash, body := range objects {
		if HashContent(body) != hash {
			test.Fatal("checkpoint mutated stored bytes after publication", hash)
		}
	}
}

func legacyNativeCheckpointStringsOracleListHeap(test *testing.T, list *legacyNativeStringList, restored bool) {
	test.Helper()
	var retained docRangeHeaderHeapEstimate
	list.retained(&retained)
	if retained.err != nil || list.heap().err != nil {
		test.Fatal("native list retained an invalid heap ledger", retained, list.heap())
	}
	if !list.initialized {
		return
	}
	if len(list.capacities) != cap(list.decoded) || len(list.previousCaps) != cap(list.previous) {
		test.Fatal("list metadata omitted hidden backing slots")
	}
	var payload docRangeHeaderHeapEstimate
	for ordinal := 0; ordinal < cap(list.decoded); ordinal++ {
		text, capacity := list.decoded[:cap(list.decoded)][ordinal], list.capacities[ordinal]
		if list.previous != nil && ordinal >= list.copied && ordinal < len(list.previous) {
			text, capacity = list.previous[ordinal], list.previousCaps[ordinal]
		}
		if capacity < len(text) {
			test.Fatal("logical string capacity is smaller than its live body", ordinal, capacity, len(text))
		}
		payload.allocation(int64(capacity), 1)
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(list.decoded)))
	expected.include(legacyNativeStringsBacking(cap(list.decoded)))
	expected.include(payload)
	if payload.err != nil || expected.err != nil || list.payload.bytes != payload.bytes || list.heap().bytes != expected.bytes {
		test.Fatal("list did not recompute its logical payload and physical current backing", list.payload, payload, list.heap(), expected)
	}
	if restored && list.phase != legacyNativeStringsDone {
		if cap(list.decoded) > 0 && !list.owned || cap(list.previous) > 0 && !list.previousOwned || list.added.bytes < payload.bytes {
			test.Fatal("active restore borrowed newly allocated backing or payload", list.owned, list.previousOwned, list.added, payload)
		}
		var minimum docRangeHeaderHeapEstimate
		minimum.allocation(1, int64(unsafe.Sizeof(*list)))
		minimum.allocation(int64(cap(list.capacities)), int64(unsafe.Sizeof(int(0))))
		minimum.allocation(int64(cap(list.previousCaps)), int64(unsafe.Sizeof(int(0))))
		minimum.include(legacyNativeStringsBacking(cap(list.decoded)))
		minimum.include(legacyNativeStringsBacking(cap(list.previous)))
		minimum.include(payload)
		if list.scalar != nil {
			list.scalar.retained(&minimum)
		}
		if minimum.err != nil || retained.bytes < minimum.bytes {
			test.Fatal("restored list omitted actual retained migration storage", retained, minimum)
		}
	}
}

func legacyNativeCheckpointStringsOracleListStep(test *testing.T, list *legacyNativeStringList, source LegacyByteSource, workBytes int, restored bool) bool {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	input := &legacyNormalizationInput{ctx: ctx, source: source, workBytes: workBytes}
	ready, err := list.step(ctx, input)
	cancel()
	if err != nil || input.workBytes < workBytes || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded {
		test.Fatal("continued native list escaped its original quantum or authenticated source", ready, err, input.workBytes, input.loads)
	}
	for _, page := range input.pages {
		clear(page)
	}
	legacyNativeCheckpointStringsOracleListHeap(test, list, restored)
	return ready
}

func legacyNativeCheckpointStringsOracleListFinish(test *testing.T, list *legacyNativeStringList, source LegacyByteSource, restored bool) {
	test.Helper()
	for range 512 {
		if legacyNativeCheckpointStringsOracleListStep(test, list, source, 0, restored) {
			return
		}
	}
	test.Fatal("continued native list exhausted its finite schedule")
}

func legacyNativeCheckpointStringsOracleListCapture(test *testing.T, list *legacyNativeStringList, source LegacyByteSource, raw *legacySourceProofBuffer, storage *legacyNativeCheckpointStringsOracleStore) legacyNativeCheckpointRef {
	test.Helper()
	before := *list
	decoded, previous := legacyNativeCheckpointStringsOracleClone(list.decoded), legacyNativeCheckpointStringsOracleClone(list.previous)
	capacities, previousCaps := append([]int(nil), list.capacities...), append([]int(nil), list.previousCaps...)
	var scalar legacyNativeScalar
	if list.scalar != nil {
		scalar = *list.scalar
	}
	original := make(map[ContentHash][]byte, len(storage.objects))
	for hash, body := range storage.objects {
		original[hash] = bytes.Clone(body)
	}
	reads := len(raw.reads)
	ctx, cancel := context.WithCancel(context.Background())
	writer, err := newLegacyNativeCheckpointStringListWriter(ctx, list, storage.io, source.Bytes, legacyNativeCheckpointStringsOracleBudget)
	cancel()
	if err != nil {
		test.Fatal("actual native list capture was rejected", err)
	}
	saved, _ := legacyNativeCheckpointStringsOracleFinish[legacyNativeCheckpointRef](test, writer, storage, legacyNativeCheckpointStringsOracleBudget)
	legacyNativeCheckpointStringsOracleBacking(test, list.decoded, decoded)
	legacyNativeCheckpointStringsOracleBacking(test, list.previous, previous)
	if !reflect.DeepEqual(*list, before) || !reflect.DeepEqual(list.capacities, capacities) || !reflect.DeepEqual(list.previousCaps, previousCaps) || list.scalar != nil && *list.scalar != scalar || len(raw.reads) != reads {
		test.Fatal("capture changed the native machine, metadata, scalar, or read RAW bytes")
	}
	legacyNativeCheckpointStringsOracleObjects(test, storage.objects, original)
	return saved
}

func TestLegacyNativeCheckpointStringsValuesOracle(test *testing.T) {
	var giant strings.Builder
	giant.Grow(4*ConversationManifestChunkBytes + 129)
	giant.WriteString(strings.Repeat("한🙂", 3*ConversationManifestChunkBytes/7))
	backing := make([]string, 67)
	for ordinal := range backing {
		backing[ordinal] = "slot-" + strconv.Itoa(ordinal)
	}
	backing[0], backing[32], backing[66] = "visible", "", giant.String()
	for _, fixture := range []struct {
		name     string
		values   []string
		explicit bool
	}{
		{name: "nil"},
		{name: "non-nil-empty", values: make([]string, 0)},
		{name: "zero-length-hidden", values: []string{"first", "", "hidden\x00\xff"}[:0]},
		{name: "full-capacity-pages-conservative", values: backing[:1]},
		{name: "full-capacity-explicit-slots", values: backing[:2], explicit: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeCheckpointStringsOracleStorage(test)
			before := legacyNativeCheckpointStringsOracleClone(fixture.values)
			seen := make([]bool, cap(fixture.values))
			var capacities func(context.Context, int) (int, error)
			if fixture.explicit {
				capacities = func(ctx context.Context, ordinal int) (int, error) {
					if err := ctx.Err(); err != nil {
						return 0, err
					}
					if ordinal < 0 || ordinal >= len(seen) {
						test.Fatal("slot metadata was outside full capacity", ordinal)
					}
					seen[ordinal] = true
					storage.metadata++
					if ordinal == 66 {
						return giant.Cap(), nil
					}
					return len(fixture.values[:cap(fixture.values)][ordinal]), nil
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			writer, err := newLegacyNativeCheckpointStringValuesWriter(ctx, fixture.values, capacities, storage.io, 2<<20)
			cancel()
			if err != nil {
				test.Fatal("borrowed payload was charged as owned writer storage", err)
			}
			saved, _ := legacyNativeCheckpointStringsOracleFinish[legacyNativeCheckpointStrings](test, writer, storage, 2<<20)
			if err := saved.validate(true); err != nil || saved.Nil != (fixture.values == nil) || saved.Length != len(fixture.values) || saved.Capacity != cap(fixture.values) || saved.Items.Count != uint64(cap(fixture.values)) {
				test.Fatal("value descriptor omitted physical hidden capacity", saved, err)
			}
			if fixture.explicit {
				for ordinal, visited := range seen {
					if !visited {
						test.Fatal("hidden slot metadata was never captured", ordinal)
					}
				}
			}
			legacyNativeCheckpointStringsOracleBacking(test, fixture.values, before)
			ctx, cancel = context.WithCancel(context.Background())
			restore, err := newLegacyNativeCheckpointStringValuesRestore(ctx, saved, storage.io, MaxFinalizedDocBytes, legacyNativeCheckpointStringsOracleBudget)
			cancel()
			if err != nil {
				test.Fatal(err)
			}
			got, charged := legacyNativeCheckpointStringsOracleFinish[legacyNativeCheckpointStringValues](test, restore, storage, legacyNativeCheckpointStringsOracleBudget)
			legacyNativeCheckpointStringsOracleBacking(test, got.Values, before)
			if len(got.Capacities) != cap(got.Values) {
				test.Fatal("restored capacities omitted hidden slots")
			}
			var payload, minimum docRangeHeaderHeapEstimate
			for ordinal, capacity := range got.Capacities {
				text := got.Values[:cap(got.Values)][ordinal]
				expected := len(text)
				if fixture.explicit && ordinal == 66 {
					expected = giant.Cap()
				}
				if capacity != expected {
					test.Fatal("restored slot capacity does not preserve admitted exact or explicit backing", ordinal, capacity, expected)
				}
				payload.allocation(int64(capacity), 1)
			}
			minimum.include(payload)
			minimum.include(legacyNativeStringsBacking(cap(got.Values)))
			minimum.allocation(int64(cap(got.Capacities)), int64(unsafe.Sizeof(int(0))))
			if payload.err != nil || got.Payload.err != nil || got.Payload.bytes != payload.bytes || charged < minimum.bytes {
				test.Fatal("restored payload trusts saved heap or omits native backing", got.Payload, payload, charged, minimum)
			}
			legacyNativeCheckpointStringsOracleObjects(test, storage.objects, nil)
		})
	}
}

func TestLegacyNativeCheckpointStringsNativeCuts(test *testing.T) {
	giant := `["` + strings.Repeat("\xff", 2*ConversationManifestChunkBytes+37) + `\uD83D\uDE42한",null]`
	for _, fixture := range []struct {
		name      string
		raw       string
		prior     []string
		workBytes int
		cut       func(*legacyNativeStringList) bool
	}{
		{name: "unseeded-hidden-prior", raw: `[null,null,null]`, prior: []string{"head", "hidden", "tail", "last"}[:1], cut: func(list *legacyNativeStringList) bool { return !list.initialized && list.seeded == 0 }},
		{name: "partial-metadata-seed", raw: `["fresh",null,null]`, prior: []string{"head", "hidden", "tail", "last"}[:1], workBytes: ConversationManifestChunkBytes - int(unsafe.Sizeof(string(""))+unsafe.Sizeof(int(0))), cut: func(list *legacyNativeStringList) bool { return !list.initialized && list.seeded > 0 }},
		{name: "partial-string-scan", raw: giant, cut: func(list *legacyNativeStringList) bool {
			return list.phase == legacyNativeStringsString && list.position > list.stringStart
		}},
		{name: "actual-scalar-migration", raw: giant, cut: func(list *legacyNativeStringList) bool {
			return list.scalar != nil && list.scalar.previous != nil && list.scalar.copied > 0 && list.scalar.copied < list.scalar.previous.Len()
		}},
		{name: "previous-and-decoded-tails", raw: `[` + strings.Repeat(`"value",`, 20000) + `null]`, cut: func(list *legacyNativeStringList) bool {
			return list.previous != nil && list.copied > 0 && list.copied < len(list.previous)
		}},
		{name: "partial-whole-null", raw: `null`, workBytes: ConversationManifestChunkBytes - 2, cut: func(list *legacyNativeStringList) bool {
			return list.phase == legacyNativeStringsNull && list.literal == 1
		}},
		{name: "cached-non-nil-empty", raw: `[]`, prior: []string{"discard", "hidden"}[:1], cut: func(list *legacyNativeStringList) bool { return list.phase == legacyNativeStringsDone }},
		{name: "cached-hidden-scalar-capacity", raw: `["first","` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `",null]`, cut: func(list *legacyNativeStringList) bool { return list.phase == legacyNativeStringsDone }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			prior := legacyNativeCheckpointStringsOracleClone(fixture.prior)
			want := legacyNativeCheckpointStringsOracleClone(fixture.prior)
			if err := json.Unmarshal([]byte(fixture.raw), &want); err != nil {
				test.Fatal("native JSON fixture was invalid", err)
			}
			list, err := newLegacyNativeStringList(prior, legacyNativeStringsTestHeap(test, prior), 0, len(fixture.raw))
			if err != nil {
				test.Fatal(err)
			}
			raw, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(fixture.raw))
			for stepIndex := 0; stepIndex < 512 && !fixture.cut(list); stepIndex++ {
				if legacyNativeCheckpointStringsOracleListStep(test, list, source, fixture.workBytes, false) && !fixture.cut(list) {
					test.Fatal("actual list completed before its required cut")
				}
			}
			if !fixture.cut(list) {
				test.Fatal("actual list never exposed its required finite cut")
			}
			storage := legacyNativeCheckpointStringsOracleStorage(test)
			saved := legacyNativeCheckpointStringsOracleListCapture(test, list, source, raw, storage)
			reads := len(raw.reads)
			ctx, cancel := context.WithCancel(context.Background())
			restore, err := newLegacyNativeCheckpointStringListRestore(ctx, saved, storage.io, source.Bytes, legacyNativeCheckpointStringsOracleBudget)
			cancel()
			if err != nil {
				test.Fatal(err)
			}
			restored, charged := legacyNativeCheckpointStringsOracleFinish[*legacyNativeStringList](test, restore, storage, legacyNativeCheckpointStringsOracleBudget)
			if restored == nil || len(raw.reads) != reads || restored.position != list.position || restored.end != list.end || restored.maximum != list.maximum || restored.count != list.count || restored.copied != list.copied || restored.stringStart != list.stringStart || restored.literal != list.literal || restored.phase != list.phase || restored.escaped != list.escaped || restored.nullList != list.nullList || restored.prefix != list.prefix {
				test.Fatal("restore changed semantic list state or replayed RAW bytes")
			}
			legacyNativeCheckpointStringsOracleBacking(test, restored.decoded, list.decoded)
			legacyNativeCheckpointStringsOracleBacking(test, restored.previous, list.previous)
			legacyNativeCheckpointStringsOracleListHeap(test, restored, true)
			var native docRangeHeaderHeapEstimate
			restored.retained(&native)
			if charged < native.bytes {
				test.Fatal("restore transferred more native memory than it admitted", charged, native)
			}
			if list.scalar != nil {
				expected := list.scalar.text
				if list.scalar.previous != nil {
					expected = list.scalar.previous
				}
				if restored.scalar == nil || restored.scalar.position != list.scalar.position || restored.scalar.end != list.scalar.end || restored.scalar.maximum != list.scalar.maximum || restored.scalar.expanded != list.scalar.expanded || restored.scalar.previous != nil || restored.scalar.text == nil || expected == nil || restored.scalar.text.String() != expected.String() || restored.scalar.text.Cap() != list.scalar.text.Cap() {
					test.Fatal("restore lost the full accepted scalar prefix during actual migration")
				}
			} else if restored.scalar != nil {
				test.Fatal("restore fabricated an active scalar")
			}
			for ordinal := 0; ordinal < restored.copied && restored.previous != nil; ordinal++ {
				if restored.decoded[ordinal] != "" && unsafe.StringData(restored.decoded[ordinal]) != unsafe.StringData(restored.previous[ordinal]) {
					test.Fatal("copied migration prefix retained duplicate string bodies", ordinal)
				}
			}
			legacyNativeCheckpointStringsOracleListFinish(test, list, source, false)
			legacyNativeCheckpointStringsOracleListFinish(test, restored, source, true)
			if !reflect.DeepEqual(restored.values(), want) || !reflect.DeepEqual(list.values(), want) {
				test.Fatal("continued checkpoint disagreed with encoding/json")
			}
			legacyNativeCheckpointStringsOracleBacking(test, restored.decoded, list.decoded)
			for _, next := range []string{`["replacement"]`, `[null,null,null]`, `[]`, `[null,null]`, `null`, `[]`} {
				if err := json.Unmarshal([]byte(next), &want); err != nil {
					test.Fatal(err)
				}
				_, future := legacyNativeScalarCheckpointOracleRawSource(test, []byte(next))
				for _, current := range []*legacyNativeStringList{list, restored} {
					if err := current.start(0, len(next)); err != nil {
						test.Fatal(err)
					}
					legacyNativeCheckpointStringsOracleListFinish(test, current, future, false)
					if !reflect.DeepEqual(current.values(), want) {
						test.Fatal("cached parser reuse changed null or hidden-tail semantics", next)
					}
				}
				legacyNativeCheckpointStringsOracleBacking(test, restored.decoded, list.decoded)
			}
		})
	}
}

func TestLegacyNativeCheckpointStringsAdversarial(test *testing.T) {
	storage := legacyNativeCheckpointStringsOracleStorage(test)
	ctx := context.Background()
	values := []string{"visible", "hidden"}[:1]
	writer, err := newLegacyNativeCheckpointStringValuesWriter(ctx, values, nil, storage.io, legacyNativeCheckpointStringsOracleBudget)
	if err != nil {
		test.Fatal(err)
	}
	saved, _ := legacyNativeCheckpointStringsOracleFinish[legacyNativeCheckpointStrings](test, writer, storage, legacyNativeCheckpointStringsOracleBudget)
	for _, fixture := range []struct {
		name   string
		change func(*legacyNativeCheckpointStrings)
	}{
		{name: "negative-length", change: func(value *legacyNativeCheckpointStrings) { value.Length = -1 }},
		{name: "length-over-capacity", change: func(value *legacyNativeCheckpointStrings) { value.Length = value.Capacity + 1 }},
		{name: "negative-capacity", change: func(value *legacyNativeCheckpointStrings) { value.Capacity = -1 }},
		{name: "oversized-capacity", change: func(value *legacyNativeCheckpointStrings) { value.Capacity = MaxFinalizedDocBytes + 1 }},
		{name: "nil-with-backing", change: func(value *legacyNativeCheckpointStrings) { value.Nil = true }},
		{name: "missing-items", change: func(value *legacyNativeCheckpointStrings) { value.Items = legacyNativeCheckpointTree{} }},
		{name: "wrong-row-count", change: func(value *legacyNativeCheckpointStrings) { value.Items.Count-- }},
		{name: "invalid-tree-height", change: func(value *legacyNativeCheckpointStrings) { value.Items.Height = legacyNativeCheckpointTreeHeight + 1 }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			bad := saved
			fixture.change(&bad)
			restore, err := newLegacyNativeCheckpointStringValuesRestore(ctx, bad, storage.io, 128, legacyNativeCheckpointStringsOracleBudget)
			legacyNativeCheckpointStringsOracleReject[legacyNativeCheckpointStringValues](test, restore, err, ErrIntegrity)
		})
	}
	for _, capacity := range []int{-1, len(values[0]) - 1, 2*int(legacyNativeValueCheckpointPayloadLimit()) + 65} {
		test.Run("invalid-slot-capacity-"+strconv.Itoa(capacity), func(test *testing.T) {
			writer, err := newLegacyNativeCheckpointStringValuesWriter(ctx, values, func(context.Context, int) (int, error) { return capacity, nil }, storage.io, legacyNativeCheckpointStringsOracleBudget)
			legacyNativeCheckpointStringsOracleReject[legacyNativeCheckpointStrings](test, writer, err, ErrIntegrity)
		})
	}
	var page legacyNativeCheckpointListPage[json.RawMessage]
	if saved.Items.Height != 0 || legacyNativeCheckpointReadJSON(ctx, storage.io, saved.Items.Ref, &page) != nil || len(page.Values) != cap(values) {
		test.Fatal("real value capture did not expose a bounded leaf page")
	}
	for _, fixture := range []struct {
		name  string
		body  []byte
		count int
	}{
		{name: "null-row", body: []byte(`{"v":1,"e":[null]}`), count: 1},
		{name: "wrong-row-kind", body: []byte(`{"v":1,"e":[true]}`), count: 1},
		{name: "missing-row-fields", body: []byte(`{"v":1,"e":[{}]}`), count: 1},
		{name: "wrong-page-version", body: []byte(`{"v":0,"e":[` + string(page.Values[0]) + `]}`), count: 1},
		{name: "duplicate-page-field", body: []byte(`{"v":1,"e":[` + string(page.Values[0]) + `],"e":[` + string(page.Values[0]) + `]}`), count: 1},
		{name: "oversized-row-page", body: []byte(`{"v":1,"e":[` + strings.Repeat(string(page.Values[0])+`,`, legacyNativeCheckpointPageItems) + string(page.Values[0]) + `]}`), count: legacyNativeCheckpointPageItems + 1},
		{name: "row-count-mismatch", body: []byte(`{"v":1,"e":[` + string(page.Values[0]) + `]}`), count: 2},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			ref, err := legacyNativeCheckpointWrite(ctx, storage.raw, fixture.body)
			if err != nil {
				test.Fatal(err)
			}
			bad := legacyNativeCheckpointStrings{Length: 1, Capacity: fixture.count, Items: legacyNativeCheckpointTree{Ref: ref, Count: uint64(fixture.count)}}
			restore, err := newLegacyNativeCheckpointStringValuesRestore(ctx, bad, storage.io, 128, legacyNativeCheckpointStringsOracleBudget)
			legacyNativeCheckpointStringsOracleReject[legacyNativeCheckpointStringValues](test, restore, err, ErrIntegrity)
		})
	}
	rawText := `["` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `",null]`
	raw, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(rawText))
	list, err := newLegacyNativeStringList(nil, docRangeHeaderHeapEstimate{}, 0, source.Bytes)
	if err != nil {
		test.Fatal(err)
	}
	for stepIndex := 0; stepIndex < 16 && list.scalar == nil; stepIndex++ {
		if legacyNativeCheckpointStringsOracleListStep(test, list, source, 0, false) {
			test.Fatal("scalar adversarial fixture finished before an actual partial unquote")
		}
	}
	if list.scalar == nil || list.scalar.position >= list.scalar.end {
		test.Fatal("scalar adversarial fixture did not expose an actual partial unquote")
	}
	root := legacyNativeCheckpointStringsOracleListCapture(test, list, source, raw, storage)
	var state legacyNativeCheckpointStringListState
	if err := legacyNativeCheckpointReadJSON(ctx, storage.io, root, &state); err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name   string
		change func(*legacyNativeCheckpointStringListState)
	}{
		{name: "list-version", change: func(value *legacyNativeCheckpointStringListState) { value.Version = 0 }},
		{name: "list-source-binding", change: func(value *legacyNativeCheckpointStringListState) { value.SourceBytes++ }},
		{name: "list-negative-position", change: func(value *legacyNativeCheckpointStringListState) { value.Position = -1 }},
		{name: "list-end-outside-source", change: func(value *legacyNativeCheckpointStringListState) { value.End = source.Bytes + 1 }},
		{name: "list-negative-maximum", change: func(value *legacyNativeCheckpointStringListState) { value.Maximum = -1 }},
		{name: "list-count-over-capacity", change: func(value *legacyNativeCheckpointStringListState) { value.Count = value.Decoded.Capacity + 1 }},
		{name: "list-copied-without-previous", change: func(value *legacyNativeCheckpointStringListState) { value.Copied = 1 }},
		{name: "list-negative-seeded", change: func(value *legacyNativeCheckpointStringListState) { value.Seeded = -1 }},
		{name: "list-invalid-phase", change: func(value *legacyNativeCheckpointStringListState) { value.Phase = 255 }},
		{name: "list-invalid-literal", change: func(value *legacyNativeCheckpointStringListState) { value.Literal = 5 }},
		{name: "list-string-start-outside-source", change: func(value *legacyNativeCheckpointStringListState) { value.StringStart = source.Bytes + 1 }},
		{name: "scalar-presence-mismatch", change: func(value *legacyNativeCheckpointStringListState) { value.HasScalar = false }},
		{name: "scalar-position-after-end", change: func(value *legacyNativeCheckpointStringListState) {
			value.Scalar.State.Position = value.Scalar.State.End + 1
		}},
		{name: "scalar-end-outside-source", change: func(value *legacyNativeCheckpointStringListState) { value.Scalar.State.End = source.Bytes + 1 }},
		{name: "scalar-prefix-count", change: func(value *legacyNativeCheckpointStringListState) { value.Scalar.Text.Bytes++ }},
		{name: "list-decoded-row-count", change: func(value *legacyNativeCheckpointStringListState) { value.Decoded.Items.Count++ }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			bad := state
			fixture.change(&bad)
			ref, err := legacyNativeCheckpointWriteJSON(ctx, storage.io, bad)
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyNativeCheckpointStringListRestore(ctx, ref, storage.io, source.Bytes, legacyNativeCheckpointStringsOracleBudget)
			legacyNativeCheckpointStringsOracleReject[*legacyNativeStringList](test, restore, err, ErrIntegrity)
		})
	}
	for _, sourceBytes := range []int{-1, MaxFinalizedDocBytes + 1, source.Bytes - 1} {
		test.Run("list-source-range-"+strconv.Itoa(sourceBytes), func(test *testing.T) {
			restore, err := newLegacyNativeCheckpointStringListRestore(ctx, root, storage.io, sourceBytes, legacyNativeCheckpointStringsOracleBudget)
			legacyNativeCheckpointStringsOracleReject[*legacyNativeStringList](test, restore, err, ErrIntegrity)
		})
	}
	test.Run("value-hash-corruption", func(test *testing.T) {
		storage.objects[saved.Items.Ref.Hash][0] ^= 0xff
		defer func() { storage.objects[saved.Items.Ref.Hash][0] ^= 0xff }()
		restore, err := newLegacyNativeCheckpointStringValuesRestore(ctx, saved, storage.io, 128, legacyNativeCheckpointStringsOracleBudget)
		legacyNativeCheckpointStringsOracleReject[legacyNativeCheckpointStringValues](test, restore, err, ErrPreparedDocRangeObject)
	})
	test.Run("list-root-hash-corruption", func(test *testing.T) {
		storage.objects[root.Hash][0] ^= 0xff
		defer func() { storage.objects[root.Hash][0] ^= 0xff }()
		restore, err := newLegacyNativeCheckpointStringListRestore(ctx, root, storage.io, source.Bytes, legacyNativeCheckpointStringsOracleBudget)
		legacyNativeCheckpointStringsOracleReject[*legacyNativeStringList](test, restore, err, ErrPreparedDocRangeObject)
	})
	test.Run("scalar-prefix-hash-corruption", func(test *testing.T) {
		ref, _, err := legacyNativeCheckpointLeafAt(ctx, storage.io, state.Scalar.Text.Tree, 0)
		if err != nil || len(storage.objects[ref.Hash]) == 0 {
			test.Fatal("actual scalar prefix has no authenticated body", err)
		}
		storage.objects[ref.Hash][0] ^= 0xff
		defer func() { storage.objects[ref.Hash][0] ^= 0xff }()
		restore, err := newLegacyNativeCheckpointStringListRestore(ctx, root, storage.io, source.Bytes, legacyNativeCheckpointStringsOracleBudget)
		legacyNativeCheckpointStringsOracleReject[*legacyNativeStringList](test, restore, err, ErrPreparedDocRangeObject)
	})
	test.Run("native-array-pre-admission", func(test *testing.T) {
		bad := saved
		bad.Capacity = min(MaxFinalizedDocBytes, 1<<16)
		bad.Items.Count = uint64(bad.Capacity)
		reads := storage.reads
		restore, err := newLegacyNativeCheckpointStringValuesRestore(ctx, bad, storage.io, 128, 1<<20)
		legacyNativeCheckpointStringsOracleReject[legacyNativeCheckpointStringValues](test, restore, err, ErrDocumentResourceLimit)
		if storage.reads != reads {
			test.Fatal("restore read row metadata before admitting declared array backing")
		}
	})
}

func legacyNativeCheckpointStringsOracleLifecycle[Value any](test *testing.T, storage *legacyNativeCheckpointStringsOracleStore, constructor func(context.Context, legacyNativeCheckpointIO, int64) (legacyNativeCheckpointStringsOracleWork[Value], error)) {
	test.Helper()
	test.Run("pre-canceled-constructor", func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reads, writes := storage.reads, storage.writes
		work, err := constructor(ctx, storage.io, legacyNativeCheckpointStringsOracleBudget)
		if err == nil {
			work.close()
		}
		if !errors.Is(err, context.Canceled) || storage.reads != reads || storage.writes != writes {
			test.Fatal("canceled constructor performed CAS work or ignored cancellation", err)
		}
	})
	test.Run("constructor-context-not-retained", func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		work, err := constructor(ctx, storage.io, legacyNativeCheckpointStringsOracleBudget)
		cancel()
		if err != nil {
			test.Fatal(err)
		}
		legacyNativeCheckpointStringsOracleFinish[Value](test, work, storage, legacyNativeCheckpointStringsOracleBudget)
	})
	for _, boundary := range []string{"before-first-step", "after-first-step", "take"} {
		test.Run("sticky-cancel-"+boundary, func(test *testing.T) {
			work, err := constructor(context.Background(), storage.io, legacyNativeCheckpointStringsOracleBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			if boundary == "after-first-step" {
				if _, err := work.step(context.Background()); err != nil {
					test.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			reads, writes := storage.reads, storage.writes
			if boundary == "take" {
				if _, err := work.take(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal("canceled transfer ignored context", err)
				}
			} else if ready, err := work.step(ctx); ready || !errors.Is(err, context.Canceled) {
				test.Fatal("canceled step ignored context", ready, err)
			}
			legacyNativeCheckpointStringsOracleFailed(test, work, context.Canceled)
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("canceled or sticky helper performed CAS IO")
			}
		})
	}
	test.Run("premature-take", func(test *testing.T) {
		work, err := constructor(context.Background(), storage.io, legacyNativeCheckpointStringsOracleBudget)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := work.take(context.Background()); !errors.Is(err, ErrIntegrity) {
			test.Fatal("premature transfer accepted unfinished state", err)
		}
		legacyNativeCheckpointStringsOracleFailed(test, work, ErrIntegrity)
	})
	test.Run("pre-admission-before-io", func(test *testing.T) {
		reads, writes := storage.reads, storage.writes
		work, err := constructor(context.Background(), storage.io, 1)
		legacyNativeCheckpointStringsOracleReject[Value](test, work, err, ErrDocumentResourceLimit)
		if storage.reads != reads || storage.writes != writes {
			test.Fatal("helper read or wrote CAS before admitting native workspace")
		}
	})
	test.Run("sticky-io-failure", func(test *testing.T) {
		failure := errors.New("strings oracle CAS unavailable")
		broken := storage.io
		broken.Read = func(context.Context, ContentHash, int) ([]byte, error) { return nil, failure }
		broken.Put = func(context.Context, ContentHash, []byte) error { return failure }
		work, err := constructor(context.Background(), broken, legacyNativeCheckpointStringsOracleBudget)
		legacyNativeCheckpointStringsOracleReject[Value](test, work, err, failure)
	})
}

func TestLegacyNativeCheckpointStringsLifecycle(test *testing.T) {
	storage := legacyNativeCheckpointStringsOracleStorage(test)
	ctx := context.Background()
	values := []string{strings.Repeat("kept🙂", LegacyCanonicalEncodingBytes/4), "hidden"}[:1]
	writer, err := newLegacyNativeCheckpointStringValuesWriter(ctx, values, nil, storage.io, legacyNativeCheckpointStringsOracleBudget)
	if err != nil {
		test.Fatal(err)
	}
	saved, _ := legacyNativeCheckpointStringsOracleFinish[legacyNativeCheckpointStrings](test, writer, storage, legacyNativeCheckpointStringsOracleBudget)
	rawText := `["` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `",null]`
	raw, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(rawText))
	list, err := newLegacyNativeStringList(values, legacyNativeStringsTestHeap(test, values), 0, source.Bytes)
	if err != nil {
		test.Fatal(err)
	}
	legacyNativeCheckpointStringsOracleListStep(test, list, source, 0, false)
	root := legacyNativeCheckpointStringsOracleListCapture(test, list, source, raw, storage)
	test.Run("value-writer", func(test *testing.T) {
		legacyNativeCheckpointStringsOracleLifecycle[legacyNativeCheckpointStrings](test, storage, func(current context.Context, store legacyNativeCheckpointIO, budget int64) (legacyNativeCheckpointStringsOracleWork[legacyNativeCheckpointStrings], error) {
			return newLegacyNativeCheckpointStringValuesWriter(current, values, nil, store, budget)
		})
	})
	test.Run("value-restore", func(test *testing.T) {
		legacyNativeCheckpointStringsOracleLifecycle[legacyNativeCheckpointStringValues](test, storage, func(current context.Context, store legacyNativeCheckpointIO, budget int64) (legacyNativeCheckpointStringsOracleWork[legacyNativeCheckpointStringValues], error) {
			return newLegacyNativeCheckpointStringValuesRestore(current, saved, store, source.Bytes, budget)
		})
	})
	test.Run("list-writer", func(test *testing.T) {
		legacyNativeCheckpointStringsOracleLifecycle[legacyNativeCheckpointRef](test, storage, func(current context.Context, store legacyNativeCheckpointIO, budget int64) (legacyNativeCheckpointStringsOracleWork[legacyNativeCheckpointRef], error) {
			return newLegacyNativeCheckpointStringListWriter(current, list, store, source.Bytes, budget)
		})
	})
	test.Run("list-restore", func(test *testing.T) {
		legacyNativeCheckpointStringsOracleLifecycle[*legacyNativeStringList](test, storage, func(current context.Context, store legacyNativeCheckpointIO, budget int64) (legacyNativeCheckpointStringsOracleWork[*legacyNativeStringList], error) {
			return newLegacyNativeCheckpointStringListRestore(current, root, store, source.Bytes, budget)
		})
	})
}
