package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

const legacyNativeBlockRecordW81Budget int64 = 64 << 20
const legacyNativeBlockRecordW81Steps = 65536

type legacyNativeBlockRecordW81Work[Value any] interface {
	step(context.Context) (bool, error)
	take(context.Context) (Value, error)
	retainedBytes() (int64, error)
	close()
}

type legacyNativeBlockRecordW81Store struct {
	io          legacyNativeCheckpointIO
	objects     map[ContentHash][]byte
	reads       int
	writes      int
	transferred int
}

func legacyNativeBlockRecordW81Storage(test *testing.T) *legacyNativeBlockRecordW81Store {
	test.Helper()
	storage, objects := legacyNativeCheckpointStoreFixture(test)
	result := &legacyNativeBlockRecordW81Store{objects: objects}
	result.io.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		result.writes++
		result.transferred += len(body)
		if len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes {
			test.Fatal("record writer published an unbounded CAS body", len(body))
		}
		return storage.Put(ctx, hash, body)
	}
	result.io.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		result.reads++
		result.transferred += length
		if length <= 0 || length > LegacyCanonicalEncodingBytes {
			test.Fatal("record restore requested an unbounded CAS body", length)
		}
		return storage.Read(ctx, hash, length)
	}
	return result
}

func legacyNativeBlockRecordW81Ready[Value any](test *testing.T, work legacyNativeBlockRecordW81Work[Value], storage *legacyNativeBlockRecordW81Store, budget int64) {
	test.Helper()
	for range legacyNativeBlockRecordW81Steps {
		reads, writes, transferred := storage.reads, storage.writes, storage.transferred
		ctx, cancel := context.WithCancel(context.Background())
		ready, err := work.step(ctx)
		cancel()
		if err != nil {
			test.Fatal("record helper rejected an admitted state", err)
		}
		maximum := 4 * (legacyNativeCheckpointTreeHeight + 2)
		if storage.reads-reads+storage.writes-writes > maximum || storage.transferred-transferred > maximum*LegacyCanonicalEncodingBytes {
			test.Fatal("record helper escaped its bounded CAS quantum")
		}
		retained, err := work.retainedBytes()
		if err != nil || retained < 0 || retained > budget {
			test.Fatal("record helper escaped current admission", retained, budget, err)
		}
		if ready {
			readyReads, readyWrites := storage.reads, storage.writes
			if repeated, err := work.step(context.Background()); !repeated || err != nil || storage.reads != readyReads || storage.writes != readyWrites {
				test.Fatal("ready record helper lost readiness", repeated, err)
			}
			return
		}
	}
	test.Fatal("record helper exhausted its finite schedule")
}

func legacyNativeBlockRecordW81Finish[Value any](test *testing.T, work legacyNativeBlockRecordW81Work[Value], storage *legacyNativeBlockRecordW81Store, budget int64) Value {
	test.Helper()
	defer work.close()
	legacyNativeBlockRecordW81Ready(test, work, storage, budget)
	value, err := work.take(context.Background())
	if err != nil {
		test.Fatal("ready record transfer failed", err)
	}
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("record transfer retained owned storage", retained, err)
	}
	return value
}

func legacyNativeBlockRecordW81Failed[Value any](test *testing.T, work legacyNativeBlockRecordW81Work[Value], failure error) {
	test.Helper()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("terminal record failure retained owned buffers", retained, err)
	}
	if ready, err := work.step(context.Background()); ready || !errors.Is(err, failure) {
		test.Fatal("record step lost its sticky failure", ready, err, failure)
	}
	var empty Value
	if value, err := work.take(context.Background()); !errors.Is(err, failure) || !reflect.DeepEqual(value, empty) {
		test.Fatal("failed record transferred state or changed its error", err, failure)
	}
	work.close()
	work.close()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("closed record helper retained storage", retained, err)
	}
}

func legacyNativeBlockRecordW81Reject[Value any](test *testing.T, work legacyNativeBlockRecordW81Work[Value], constructorError, failure error) {
	test.Helper()
	if constructorError != nil {
		if !errors.Is(constructorError, failure) {
			test.Fatal("record constructor returned the wrong rejection", constructorError, failure)
		}
		return
	}
	defer work.close()
	for range legacyNativeBlockRecordW81Steps {
		ready, err := work.step(context.Background())
		if err != nil {
			if ready || !errors.Is(err, failure) {
				test.Fatal("record helper returned the wrong rejection", ready, err, failure)
			}
			legacyNativeBlockRecordW81Failed(test, work, err)
			return
		}
		if ready {
			test.Fatal("malformed or unadmitted record became ready")
		}
	}
	test.Fatal("record rejection exhausted its finite schedule")
}

func legacyNativeBlockRecordW81Record(block ContentBlock, capacities [2]int) legacyNativeBlockValue {
	record := legacyNativeBlockValue{block: block}
	for index, text := range [2]string{block.Type, block.Text} {
		record.heaps[index].allocation(int64(capacities[index]), 1)
		record.excess[index] = 2 * int64(capacities[index]-len(text))
	}
	return record
}

func legacyNativeBlockRecordW81Capture(test *testing.T, record legacyNativeBlockValue, initialized bool, storage *legacyNativeBlockRecordW81Store) legacyNativeCheckpointBlockRecord {
	test.Helper()
	before := record
	reads := storage.reads
	writer, err := newLegacyNativeCheckpointBlockRecordWriter(context.Background(), record, initialized, storage.io, 2<<20)
	if err != nil {
		test.Fatal(err)
	}
	saved := legacyNativeBlockRecordW81Finish[legacyNativeCheckpointBlockRecord](test, writer, storage, 2<<20)
	if !reflect.DeepEqual(record, before) || storage.reads != reads {
		test.Fatal("record capture changed its borrowed input or read CAS")
	}
	if saved.Values.Nil || saved.Values.Length != 2 || saved.Values.Capacity != 2 || saved.Values.Items.Count != 2 {
		test.Fatal("record did not save exactly the two actual ContentBlock fields", saved.Values)
	}
	return saved
}

func TestLegacyNativeCheckpointBlockRecordW81RoundTrip(test *testing.T) {
	var typeBuilder, textBuilder strings.Builder
	typeBuilder.Grow(33)
	typeBuilder.WriteString("text")
	textBuilder.Grow(257)
	textBuilder.WriteString("한글🙂\x00\"\\\n")
	for _, fixture := range []struct {
		name        string
		block       ContentBlock
		initialized bool
		capacities  [2]int
	}{
		{name: "empty-conservative"},
		{name: "empty-initialized", initialized: true},
		{name: "conservative-exact-not-builder-rounded", block: ContentBlock{Type: "text", Text: "abc한🙂"}},
		{name: "initialized-excess", block: ContentBlock{Type: typeBuilder.String(), Text: textBuilder.String()}, initialized: true, capacities: [2]int{typeBuilder.Cap(), textBuilder.Cap()}},
		{name: "empty-with-retained-capacity", initialized: true, capacities: [2]int{16, 64}},
		{name: "giant-borrowed-unicode", block: ContentBlock{Type: "text", Text: strings.Repeat("한🙂", 1<<20)}},
		{name: "opaque-invalid-utf8", block: ContentBlock{Type: "\xff", Text: "\x00\xfe\xff한🙂"}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeBlockRecordW81Storage(test)
			record := legacyNativeBlockRecordW81Record(fixture.block, fixture.capacities)
			if !fixture.initialized {
				record.heaps, record.excess = [2]docRangeHeaderHeapEstimate{}, [2]int64{}
				fixture.capacities = [2]int{len(fixture.block.Type), len(fixture.block.Text)}
			}
			saved := legacyNativeBlockRecordW81Capture(test, record, fixture.initialized, storage)
			writes := storage.writes
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), saved, storage.io, 0, legacyNativeBlockRecordW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			actual := legacyNativeBlockRecordW81Finish[legacyNativeBlockValue](test, restore, storage, legacyNativeBlockRecordW81Budget)
			if actual.block != fixture.block || storage.writes != writes {
				test.Fatal("record restore changed exact bytes or wrote CAS")
			}
			for index, text := range [2]string{actual.block.Type, actual.block.Text} {
				var expected docRangeHeaderHeapEstimate
				expected.allocation(int64(fixture.capacities[index]), 1)
				if actual.heaps[index].err != nil || actual.heaps[index].bytes != expected.bytes || actual.excess[index] != 2*int64(fixture.capacities[index]-len(text)) {
					test.Fatal("record restore failed to recompute body-only current heap and excess", index, actual.heaps[index], actual.excess[index])
				}
				original := [2]string{fixture.block.Type, fixture.block.Text}[index]
				if len(text) > 0 && unsafe.StringData(text) == unsafe.StringData(original) {
					test.Fatal("restored record borrowed its original text", index)
				}
			}
			for hash, body := range storage.objects {
				if HashContent(body) != hash {
					test.Fatal("record capture or restore mutated an immutable CAS object")
				}
			}
		})
	}
}

func TestLegacyNativeCheckpointBlockRecordW81Adversarial(test *testing.T) {
	storage := legacyNativeBlockRecordW81Storage(test)
	record := legacyNativeBlockRecordW81Record(ContentBlock{Type: "text", Text: "한🙂body"}, [2]int{4, len("한🙂body")})
	saved := legacyNativeBlockRecordW81Capture(test, record, true, storage)
	for _, fixture := range []struct {
		name   string
		change func(*legacyNativeCheckpointBlockRecord)
	}{
		{name: "nil-values", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Nil = true }},
		{name: "negative-length", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Length = -1 }},
		{name: "one-field", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Length = 1 }},
		{name: "extra-field", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Length = 3 }},
		{name: "short-capacity", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Capacity = 1 }},
		{name: "extra-capacity", change: func(value *legacyNativeCheckpointBlockRecord) {
			value.Values.Capacity = 3
			value.Values.Items.Count = 3
		}},
		{name: "wrong-count", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Items.Count = 1 }},
		{name: "empty-tree", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Items = legacyNativeCheckpointTree{} }},
		{name: "invalid-hash", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Items.Ref.Hash = "bad" }},
		{name: "zero-ref-bytes", change: func(value *legacyNativeCheckpointBlockRecord) { value.Values.Items.Ref.Bytes = 0 }},
		{name: "oversized-ref", change: func(value *legacyNativeCheckpointBlockRecord) {
			value.Values.Items.Ref.Bytes = LegacyCanonicalEncodingBytes + 1
		}},
		{name: "height", change: func(value *legacyNativeCheckpointBlockRecord) {
			value.Values.Items.Height = legacyNativeCheckpointTreeHeight + 1
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			changed := saved
			fixture.change(&changed)
			reads := storage.reads
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), changed, storage.io, 128, legacyNativeBlockRecordW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeBlockValue](test, restore, err, ErrIntegrity)
			if storage.reads != reads {
				test.Fatal("malformed record descriptor read dependent CAS")
			}
		})
	}
	for _, fixture := range []struct {
		name   string
		change func(*legacyNativeBlockValue)
	}{
		{name: "negative-heap", change: func(value *legacyNativeBlockValue) { value.heaps[0].bytes = -1 }},
		{name: "heap-error", change: func(value *legacyNativeBlockValue) { value.heaps[1].err = ErrIntegrity }},
		{name: "header-not-body", change: func(value *legacyNativeBlockValue) { value.heaps[0].bytes += int64(unsafe.Sizeof(string(""))) }},
		{name: "short-body-heap", change: func(value *legacyNativeBlockValue) { value.heaps[1].bytes-- }},
		{name: "negative-excess", change: func(value *legacyNativeBlockValue) { value.excess[0] = -2 }},
		{name: "odd-excess", change: func(value *legacyNativeBlockValue) { value.excess[1] = 1 }},
		{name: "unmatched-excess", change: func(value *legacyNativeBlockValue) { value.excess[1] = 2 }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			changed := record
			fixture.change(&changed)
			writer, err := newLegacyNativeCheckpointBlockRecordWriter(context.Background(), changed, true, storage.io, legacyNativeBlockRecordW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeCheckpointBlockRecord](test, writer, err, ErrIntegrity)
		})
	}
	for _, sourceBytes := range []int{-1, MaxFinalizedDocBytes + 1} {
		restore, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), saved, storage.io, sourceBytes, legacyNativeBlockRecordW81Budget)
		legacyNativeBlockRecordW81Reject[legacyNativeBlockValue](test, restore, err, ErrIntegrity)
	}
	test.Run("missing-io", func(test *testing.T) {
		writer, err := newLegacyNativeCheckpointBlockRecordWriter(context.Background(), record, true, legacyNativeCheckpointIO{}, legacyNativeBlockRecordW81Budget)
		legacyNativeBlockRecordW81Reject[legacyNativeCheckpointBlockRecord](test, writer, err, ErrIntegrity)
		restore, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), saved, legacyNativeCheckpointIO{}, 128, legacyNativeBlockRecordW81Budget)
		legacyNativeBlockRecordW81Reject[legacyNativeBlockValue](test, restore, err, ErrIntegrity)
	})
	var page legacyNativeCheckpointListPage[legacyNativeCheckpointStringValueRow]
	if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io, saved.Values.Items.Ref, &page); err != nil || len(page.Values) != 2 || saved.Values.Items.Height != 0 {
		test.Fatal("small actual record did not expose two bounded string rows", err)
	}
	first, err := json.Marshal(page.Values[0])
	if err != nil {
		test.Fatal(err)
	}
	second, err := json.Marshal(page.Values[1])
	if err != nil {
		test.Fatal(err)
	}
	for _, fixture := range []struct {
		name string
		body string
	}{
		{name: "null-row", body: `{"v":1,"e":[null,` + string(second) + `]}`},
		{name: "wrong-row-kind", body: `{"v":1,"e":[true,` + string(second) + `]}`},
		{name: "missing-row-fields", body: `{"v":1,"e":[{},` + string(second) + `]}`},
		{name: "missing-physical-row", body: `{"v":1,"e":[` + string(first) + `]}`},
		{name: "extra-physical-row", body: `{"v":1,"e":[` + string(first) + `,` + string(second) + `,` + string(first) + `]}`},
		{name: "page-version", body: `{"v":0,"e":[` + string(first) + `,` + string(second) + `]}`},
		{name: "duplicate-page-key", body: `{"v":1,"e":[` + string(first) + `,` + string(second) + `],"e":[]}`},
		{name: "unknown-page-key", body: `{"v":1,"e":[` + string(first) + `,` + string(second) + `],"grant":1}`},
		{name: "overfull-page", body: `{"v":1,"e":[` + strings.Repeat(string(first)+`,`, legacyNativeCheckpointPageItems) + string(second) + `]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			ref, err := legacyNativeCheckpointWrite(context.Background(), storage.io, []byte(fixture.body))
			if err != nil {
				test.Fatal(err)
			}
			changed := saved
			changed.Values.Items = legacyNativeCheckpointTree{Ref: ref, Count: 2}
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), changed, storage.io, 128, legacyNativeBlockRecordW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeBlockValue](test, restore, err, ErrIntegrity)
		})
	}
	for _, fixture := range []struct {
		name   string
		change func(*legacyNativeCheckpointStringValueRow)
	}{
		{name: "row-capacity-too-small", change: func(value *legacyNativeCheckpointStringValueRow) { value.Capacity = value.Text.Bytes - 1 }},
		{name: "row-capacity-negative", change: func(value *legacyNativeCheckpointStringValueRow) { value.Capacity = -1 }},
		{name: "row-capacity-over-bound", change: func(value *legacyNativeCheckpointStringValueRow) {
			value.Capacity = int(2*legacyNativeValueCheckpointPayloadLimit() + 65)
		}},
		{name: "row-text-negative", change: func(value *legacyNativeCheckpointStringValueRow) { value.Text.Bytes = -1 }},
		{name: "row-text-count", change: func(value *legacyNativeCheckpointStringValueRow) { value.Text.Tree.Count++ }},
		{name: "row-text-missing-tree", change: func(value *legacyNativeCheckpointStringValueRow) { value.Text.Tree = legacyNativeCheckpointTree{} }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			changedPage := legacyNativeCheckpointListPage[legacyNativeCheckpointStringValueRow]{Version: 1, Values: append([]legacyNativeCheckpointStringValueRow(nil), page.Values...)}
			fixture.change(&changedPage.Values[1])
			ref, err := legacyNativeCheckpointWriteJSON(context.Background(), storage.io, changedPage)
			if err != nil {
				test.Fatal(err)
			}
			changed := saved
			changed.Values.Items.Ref = ref
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), changed, storage.io, 128, legacyNativeBlockRecordW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeBlockValue](test, restore, err, ErrIntegrity)
		})
	}
	for _, name := range []string{"row-hash", "text-hash", "missing", "short-read"} {
		test.Run(name, func(test *testing.T) {
			current := storage.io
			read := current.Read
			target := saved.Values.Items.Ref.Hash
			if name == "text-hash" {
				target = page.Values[1].Text.Tree.Ref.Hash
			}
			failure := ErrPreparedDocRangeObject
			if name == "missing" {
				failure = ErrNotFound
			}
			current.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
				body, err := read(ctx, hash, length)
				if err != nil || hash != target {
					return body, err
				}
				if name == "missing" {
					return nil, ErrNotFound
				}
				if name == "short-read" {
					return body[:len(body)-1], nil
				}
				body[0] ^= 1
				return body, nil
			}
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), saved, current, 128, legacyNativeBlockRecordW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeBlockValue](test, restore, err, failure)
		})
	}
}

func legacyNativeBlockRecordW81Lifecycle[Value any](test *testing.T, storage *legacyNativeBlockRecordW81Store, factory func(context.Context, legacyNativeCheckpointIO, int64) (legacyNativeBlockRecordW81Work[Value], error)) {
	test.Helper()
	test.Run("pre-canceled-constructor", func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reads, writes := storage.reads, storage.writes
		work, err := factory(ctx, storage.io, legacyNativeBlockRecordW81Budget)
		if err == nil {
			work.close()
		}
		if !errors.Is(err, context.Canceled) || storage.reads != reads || storage.writes != writes {
			test.Fatal("canceled record constructor ignored cancellation or performed IO", err)
		}
	})
	test.Run("constructor-context-not-retained", func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		work, err := factory(ctx, storage.io, legacyNativeBlockRecordW81Budget)
		cancel()
		if err != nil {
			test.Fatal(err)
		}
		legacyNativeBlockRecordW81Finish[Value](test, work, storage, legacyNativeBlockRecordW81Budget)
	})
	for _, boundary := range []string{"before-step", "after-step", "ready-take"} {
		test.Run(boundary, func(test *testing.T) {
			work, err := factory(context.Background(), storage.io, legacyNativeBlockRecordW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			if boundary == "after-step" {
				if _, err := work.step(context.Background()); err != nil {
					test.Fatal(err)
				}
			}
			if boundary == "ready-take" {
				legacyNativeBlockRecordW81Ready(test, work, storage, legacyNativeBlockRecordW81Budget)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			reads, writes := storage.reads, storage.writes
			if boundary == "ready-take" {
				if _, err := work.take(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal("canceled ready transfer ignored current context", err)
				}
			} else if ready, err := work.step(ctx); ready || !errors.Is(err, context.Canceled) {
				test.Fatal("canceled record step ignored current context", ready, err)
			}
			legacyNativeBlockRecordW81Failed(test, work, context.Canceled)
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("canceled record helper performed CAS IO")
			}
		})
	}
	test.Run("premature-take", func(test *testing.T) {
		work, err := factory(context.Background(), storage.io, legacyNativeBlockRecordW81Budget)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := work.take(context.Background()); !errors.Is(err, ErrIntegrity) {
			test.Fatal("unfinished record transferred state", err)
		}
		legacyNativeBlockRecordW81Failed(test, work, ErrIntegrity)
	})
	test.Run("pre-admission-before-io", func(test *testing.T) {
		reads, writes := storage.reads, storage.writes
		work, err := factory(context.Background(), storage.io, 1)
		legacyNativeBlockRecordW81Reject[Value](test, work, err, ErrDocumentResourceLimit)
		if storage.reads != reads || storage.writes != writes {
			test.Fatal("record helper performed IO before admitting workspace")
		}
	})
	for _, budget := range []int64{0, -1} {
		work, err := factory(context.Background(), storage.io, budget)
		legacyNativeBlockRecordW81Reject[Value](test, work, err, ErrIntegrity)
	}
	test.Run("sticky-io-failure", func(test *testing.T) {
		failure := errors.New("W81 record CAS unavailable")
		current := storage.io
		current.Read = func(context.Context, ContentHash, int) ([]byte, error) { return nil, failure }
		current.Put = func(context.Context, ContentHash, []byte) error { return failure }
		work, err := factory(context.Background(), current, legacyNativeBlockRecordW81Budget)
		legacyNativeBlockRecordW81Reject[Value](test, work, err, failure)
	})
	test.Run("cancellation-during-io", func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		current := storage.io
		current.Put = func(currentContext context.Context, hash ContentHash, body []byte) error {
			err := storage.io.Put(currentContext, hash, body)
			cancel()
			return err
		}
		current.Read = func(currentContext context.Context, hash ContentHash, length int) ([]byte, error) {
			body, err := storage.io.Read(currentContext, hash, length)
			cancel()
			return body, err
		}
		work, err := factory(context.Background(), current, legacyNativeBlockRecordW81Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		for range legacyNativeBlockRecordW81Steps {
			ready, err := work.step(ctx)
			if err != nil {
				if ready || !errors.Is(err, context.Canceled) {
					test.Fatal("record helper ignored cancellation from its CAS callback", ready, err)
				}
				legacyNativeBlockRecordW81Failed(test, work, context.Canceled)
				return
			}
			if ready {
				test.Fatal("record completed without observing canceled IO")
			}
		}
		test.Fatal("record IO cancellation exhausted its finite schedule")
	})
}

func TestLegacyNativeCheckpointBlockRecordW81Lifecycle(test *testing.T) {
	storage := legacyNativeBlockRecordW81Storage(test)
	record := legacyNativeBlockValue{block: ContentBlock{Type: "text", Text: strings.Repeat("body🙂", LegacyCanonicalEncodingBytes/4)}}
	saved := legacyNativeBlockRecordW81Capture(test, record, false, storage)
	test.Run("writer", func(test *testing.T) {
		legacyNativeBlockRecordW81Lifecycle[legacyNativeCheckpointBlockRecord](test, storage, func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeBlockRecordW81Work[legacyNativeCheckpointBlockRecord], error) {
			return newLegacyNativeCheckpointBlockRecordWriter(ctx, record, false, current, budget)
		})
	})
	test.Run("restore", func(test *testing.T) {
		legacyNativeBlockRecordW81Lifecycle[legacyNativeBlockValue](test, storage, func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeBlockRecordW81Work[legacyNativeBlockValue], error) {
			return newLegacyNativeCheckpointBlockRecordRestore(ctx, saved, current, 128, budget)
		})
	})
}

func legacyNativeBlockRecordW81Control(test *testing.T, list *legacyNativeBlockList, binding ContentHash, sourceBytes int) legacyNativeCheckpointBlockListState {
	test.Helper()
	before := *list
	decoded := append([]ContentBlock(nil), list.decoded[:cap(list.decoded)]...)
	previous := append([]ContentBlock(nil), list.previous[:cap(list.previous)]...)
	payloads := append([]docRangeHeaderHeapEstimate(nil), list.payloads...)
	excess := append([]int64(nil), list.excess...)
	state, err := legacyNativeCheckpointBlockListControlFor(list, binding, sourceBytes)
	if err != nil || legacyNativeCheckpointBlockListValidate(state, binding, sourceBytes, false) != nil {
		test.Fatal("logical validation rejected an actual native list state", err, list.phase, list.position, list.count)
	}
	if !reflect.DeepEqual(*list, before) || !reflect.DeepEqual(append([]ContentBlock(nil), list.decoded[:cap(list.decoded)]...), decoded) || !reflect.DeepEqual(append([]ContentBlock(nil), list.previous[:cap(list.previous)]...), previous) || !reflect.DeepEqual(append([]docRangeHeaderHeapEstimate(nil), list.payloads...), payloads) || !reflect.DeepEqual(append([]int64(nil), list.excess...), excess) {
		test.Fatal("logical validation mutated native storage")
	}
	if state.Position != list.position || state.End != list.end || state.ParentDepth != list.parentDepth || state.Maximum != list.maximum || state.Count != list.count || state.Copied != list.copied || state.ChildStart != list.childStart || state.Literal != list.literal || state.Seeded != list.seeded || state.Phase != list.phase || state.Prefix != list.prefix || state.Paged != list.paged || state.Initialized != list.initialized || state.HasFrame != (list.frame != nil) {
		test.Fatal("logical descriptor changed native scalar control or flags")
	}
	if state.Decoded.Nil != (list.decoded == nil) || state.Decoded.Length != len(list.decoded) || state.Decoded.Capacity != cap(list.decoded) || state.Previous.Nil != (list.previous == nil) || state.Previous.Length != len(list.previous) || state.Previous.Capacity != cap(list.previous) {
		test.Fatal("logical descriptor lost nil, visible length, or full hidden capacity")
	}
	if list.paged && (state.HasData != (list.data != nil) || list.data != nil && (state.DataCount != list.data.count || state.DataRecords.Count != uint64(list.data.records.length()))) {
		test.Fatal("paged logical descriptor lost physical records or visible count")
	}
	return state
}

func legacyNativeBlockRecordW81ListStep(test *testing.T, list *legacyNativeBlockList, source LegacyByteSource, workBytes int) bool {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := &legacyNormalizationInput{ctx: ctx, source: source, workBytes: workBytes}
	ready, err := list.step(ctx, input)
	if err != nil || input.workBytes < workBytes || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded {
		test.Fatal("native list fixture escaped original bounded execution", ready, err, input.workBytes, input.loads)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return ready
}

func TestLegacyNativeCheckpointBlockListW81ControlValidation(test *testing.T) {
	binding := HashContent([]byte("W81 actual block list control"))
	test.Run("actual-states", func(test *testing.T) {
		for _, paged := range []bool{false, true} {
			for _, raw := range []string{"[]", "null", `[null,{}, {"type":"text","text":"한🙂"}]`} {
				buffer, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(raw))
				list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
				if err != nil {
					test.Fatal(err)
				}
				list.paged = paged
				legacyNativeBlockRecordW81Control(test, list, binding, source.Bytes)
				complete := false
				for range 4096 {
					ready := legacyNativeBlockRecordW81ListStep(test, list, source, 0)
					reads := len(buffer.reads)
					legacyNativeBlockRecordW81Control(test, list, binding, source.Bytes)
					if len(buffer.reads) != reads {
						test.Fatal("logical control capture reread RAW")
					}
					if ready {
						complete = true
						break
					}
				}
				if !complete {
					test.Fatal("native fixture exhausted its finite schedule")
				}
				if list.data != nil || len(list.decoded) > 0 {
					previousCapacity, previousRows := cap(list.decoded), 0
					if list.data != nil {
						previousRows = list.data.records.length()
					}
					short := "[null]"
					buffer, source = legacyNativeScalarCheckpointOracleRawSource(test, []byte(short))
					if err := list.start(0, len(short)); err != nil {
						test.Fatal(err)
					}
					legacyNativeBlockRecordW81Control(test, list, binding, source.Bytes)
					for range 4096 {
						ready := legacyNativeBlockRecordW81ListStep(test, list, source, 0)
						state := legacyNativeBlockRecordW81Control(test, list, binding, source.Bytes)
						if ready {
							if paged && state.DataRecords.Count != uint64(max(previousRows, 1)) || !paged && state.Decoded.Capacity != max(previousCapacity, 1) {
								test.Fatal("short duplicate fixture did not retain a hidden physical tail")
							}
							break
						}
					}
				}
			}
		}
	})
	test.Run("partial-seed-migration-active-child", func(test *testing.T) {
		prior := make([]ContentBlock, 300)
		for index := range prior {
			prior[index] = ContentBlock{Type: "text", Text: "old한🙂"}
		}
		raw := `[null,null,null]`
		_, source := legacyNativeScalarCheckpointOracleRawSource(test, []byte(raw))
		list, err := newLegacyNativeBlockList(prior, legacyNativeBlocksTestHeap(test, prior), 0, len(raw))
		if err != nil {
			test.Fatal(err)
		}
		legacyNativeBlockRecordW81ListStep(test, list, source, 0)
		if list.initialized || list.seeded == 0 || list.seeded >= cap(list.decoded) {
			test.Fatal("native prior fixture did not expose partial seeding")
		}
		legacyNativeBlockRecordW81Control(test, list, binding, source.Bytes)
		prior = prior[:2:2]
		list, err = newLegacyNativeBlockList(prior, legacyNativeBlocksTestHeap(test, prior), 0, len(raw))
		if err != nil {
			test.Fatal(err)
		}
		migration := false
		for range 128 {
			ready := legacyNativeBlockRecordW81ListStep(test, list, source, ConversationManifestChunkBytes-2*legacyNativeBlocksWidth()-1)
			state := legacyNativeBlockRecordW81Control(test, list, binding, source.Bytes)
			if list.previous != nil {
				migration = true
				changed := state
				changed.Copied = changed.Previous.Length + 1
				if err := legacyNativeCheckpointBlockListValidate(changed, binding, source.Bytes, false); !errors.Is(err, ErrIntegrity) {
					test.Fatal("logical validator accepted an invalid migration cursor", err)
				}
			}
			if ready {
				break
			}
		}
		if !migration {
			test.Fatal("bounded native reserve did not expose physical migration")
		}
		raw = `[{"type":"text","text":"` + strings.Repeat("a", 3*LegacyNormalizationPartBytes) + `"}]`
		_, source = legacyNativeScalarCheckpointOracleRawSource(test, []byte(raw))
		list, err = newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
		if err != nil {
			test.Fatal(err)
		}
		legacyNativeBlockRecordW81ListStep(test, list, source, 0)
		if list.object == nil || list.phase != legacyNativeBlocksApply {
			test.Fatal("actual giant string did not expose an active original child")
		}
		state := legacyNativeBlockRecordW81Control(test, list, binding, source.Bytes)
		state.Phase = legacyNativeBlocksAfter
		state.Object = legacyNativeCheckpointRef{Hash: HashContent([]byte("not a child in this phase")), Bytes: 32}
		if err := legacyNativeCheckpointBlockListValidate(state, binding, source.Bytes, false); !errors.Is(err, ErrIntegrity) {
			test.Fatal("logical validator accepted a child outside Apply", err)
		}
	})
	test.Run("descriptor-mutations", func(test *testing.T) {
		prior := []ContentBlock{{Type: "text", Text: "kept"}, {Text: "hidden"}, {Text: "tail"}, {Text: "last"}}
		raw := "[null]"
		list, err := newLegacyNativeBlockList(prior[:1], legacyNativeBlocksTestHeap(test, prior[:1]), 0, len(raw))
		if err != nil {
			test.Fatal(err)
		}
		original := legacyNativeBlockRecordW81Control(test, list, binding, len(raw))
		var inspected legacyNativeCheckpointBlockListState
		var inspectionError error
		allocations := testing.AllocsPerRun(8, func() {
			inspected, inspectionError = legacyNativeCheckpointBlockListControlFor(list, binding, len(raw))
		})
		if inspectionError != nil || allocations != 0 || !reflect.DeepEqual(inspected, original) {
			test.Fatal("bounded logical inspection allocated or changed its result", allocations, inspectionError)
		}
		for _, fixture := range []struct {
			name   string
			change func(*legacyNativeCheckpointBlockListState)
		}{
			{name: "version", change: func(value *legacyNativeCheckpointBlockListState) { value.Version = 255 }},
			{name: "binding", change: func(value *legacyNativeCheckpointBlockListState) { value.Binding = HashContent([]byte("wrong")) }},
			{name: "source-length", change: func(value *legacyNativeCheckpointBlockListState) { value.SourceBytes++ }},
			{name: "negative-cursor", change: func(value *legacyNativeCheckpointBlockListState) { value.Position = -1 }},
			{name: "cursor-past-end", change: func(value *legacyNativeCheckpointBlockListState) { value.Position = value.End + 1 }},
			{name: "end-past-source", change: func(value *legacyNativeCheckpointBlockListState) { value.End = value.SourceBytes + 1 }},
			{name: "depth-zero", change: func(value *legacyNativeCheckpointBlockListState) { value.ParentDepth = 0 }},
			{name: "depth-over-limit", change: func(value *legacyNativeCheckpointBlockListState) { value.ParentDepth = 10001 }},
			{name: "maximum-negative", change: func(value *legacyNativeCheckpointBlockListState) { value.Maximum = -1 }},
			{name: "count-over-maximum", change: func(value *legacyNativeCheckpointBlockListState) { value.Count = value.Maximum + 1 }},
			{name: "negative-count", change: func(value *legacyNativeCheckpointBlockListState) { value.Count = -1 }},
			{name: "copied-negative", change: func(value *legacyNativeCheckpointBlockListState) { value.Copied = -1 }},
			{name: "child-start-past-cursor", change: func(value *legacyNativeCheckpointBlockListState) { value.ChildStart = value.Position + 1 }},
			{name: "literal-over-null", change: func(value *legacyNativeCheckpointBlockListState) { value.Literal = 5 }},
			{name: "seeded-over-capacity", change: func(value *legacyNativeCheckpointBlockListState) { value.Seeded = value.Decoded.Capacity + 1 }},
			{name: "invalid-phase", change: func(value *legacyNativeCheckpointBlockListState) { value.Phase = 255 }},
			{name: "invalid-block-alias-mask", change: func(value *legacyNativeCheckpointBlockListState) { value.BlockAliases = 4 }},
			{name: "nil-with-capacity", change: func(value *legacyNativeCheckpointBlockListState) { value.Decoded.Nil = true }},
			{name: "short-capacity", change: func(value *legacyNativeCheckpointBlockListState) { value.Decoded.Capacity = value.Decoded.Length - 1 }},
			{name: "negative-array-length", change: func(value *legacyNativeCheckpointBlockListState) { value.Decoded.Length = -1 }},
			{name: "nonpaged-data", change: func(value *legacyNativeCheckpointBlockListState) { value.HasData = true }},
			{name: "nonpaged-data-count", change: func(value *legacyNativeCheckpointBlockListState) { value.DataCount = 1 }},
			{name: "paged-with-ordinary-array", change: func(value *legacyNativeCheckpointBlockListState) { value.Paged = true }},
			{name: "empty-previous-not-nil", change: func(value *legacyNativeCheckpointBlockListState) { value.Previous.Nil = false }},
		} {
			test.Run(fixture.name, func(test *testing.T) {
				changed := original
				fixture.change(&changed)
				if err := legacyNativeCheckpointBlockListValidate(changed, binding, len(raw), false); !errors.Is(err, ErrIntegrity) {
					test.Fatal("logical validator accepted malformed native control", err)
				}
			})
		}
		for _, sourceBytes := range []int{-1, 0, len(raw) - 1, MaxFinalizedDocBytes + 1} {
			if err := legacyNativeCheckpointBlockListValidate(original, binding, sourceBytes, false); !errors.Is(err, ErrIntegrity) {
				test.Fatal("logical validator accepted the wrong source extent", sourceBytes, err)
			}
		}
		storage := legacyNativeBlockRecordW81Storage(test)
		var rows legacyNativeCheckpointListWriter[legacyNativeCheckpointBlockRecord]
		defer rows.close()
		for _, block := range list.decoded[:cap(list.decoded)] {
			saved := legacyNativeBlockRecordW81Capture(test, legacyNativeBlockValue{block: block}, false, storage)
			if err := rows.append(context.Background(), storage.io, saved); err != nil {
				test.Fatal(err)
			}
		}
		tree, err := rows.finish(context.Background(), storage.io)
		if err != nil {
			test.Fatal(err)
		}
		complete := original
		complete.Decoded.Records = tree
		if err := legacyNativeCheckpointBlockListValidate(complete, binding, len(raw), true); err != nil {
			test.Fatal("complete validation rejected actual full-capacity record rows", err)
		}
		for _, name := range []string{"missing-tree", "wrong-count", "missing-ref", "excessive-height"} {
			test.Run(name, func(test *testing.T) {
				changed := complete
				switch name {
				case "missing-tree":
					changed.Decoded.Records = legacyNativeCheckpointTree{}
				case "wrong-count":
					changed.Decoded.Records.Count--
				case "missing-ref":
					changed.Decoded.Records.Ref = legacyNativeCheckpointRef{}
				case "excessive-height":
					changed.Decoded.Records.Height = legacyNativeCheckpointTreeHeight + 1
				}
				if err := legacyNativeCheckpointBlockListValidate(changed, binding, len(raw), true); !errors.Is(err, ErrIntegrity) {
					test.Fatal("complete validator accepted a malformed physical record tree", err)
				}
			})
		}
	})
	test.Run("complete-tree-frame-object-shapes", func(test *testing.T) {
		storage := legacyNativeBlockRecordW81Storage(test)
		list, err := newLegacyNativeBlockList(nil, docRangeHeaderHeapEstimate{}, 0, 6)
		if err != nil {
			test.Fatal(err)
		}
		original := legacyNativeBlockRecordW81Control(test, list, binding, 6)
		if err := legacyNativeCheckpointBlockListValidate(original, binding, 6, true); err != nil {
			test.Fatal("empty actual descriptor required invented array trees", err)
		}
		ref, err := legacyNativeCheckpointWrite(context.Background(), storage.io, []byte("actual bounded descriptor object"))
		if err != nil {
			test.Fatal(err)
		}
		for _, fixture := range []struct {
			name   string
			change func(*legacyNativeCheckpointBlockListState)
		}{
			{name: "zero-array-nonzero-tree", change: func(value *legacyNativeCheckpointBlockListState) {
				value.Decoded.Records = legacyNativeCheckpointTree{Ref: ref, Count: 1}
			}},
			{name: "previous-zero-nonzero-tree", change: func(value *legacyNativeCheckpointBlockListState) {
				value.Previous.Records = legacyNativeCheckpointTree{Ref: ref, Count: 1}
			}},
			{name: "absent-data-nonzero-tree", change: func(value *legacyNativeCheckpointBlockListState) {
				value.DataRecords = legacyNativeCheckpointTree{Ref: ref, Count: 1}
			}},
			{name: "frame-without-flag", change: func(value *legacyNativeCheckpointBlockListState) { value.Frame = ref }},
			{name: "frame-flag-without-ref", change: func(value *legacyNativeCheckpointBlockListState) { value.HasFrame = true }},
			{name: "child-outside-apply", change: func(value *legacyNativeCheckpointBlockListState) { value.Object = ref }},
			{name: "apply-without-child", change: func(value *legacyNativeCheckpointBlockListState) { value.Phase = legacyNativeBlocksApply }},
		} {
			test.Run(fixture.name, func(test *testing.T) {
				changed := original
				fixture.change(&changed)
				if err := legacyNativeCheckpointBlockListValidate(changed, binding, 6, true); !errors.Is(err, ErrIntegrity) {
					test.Fatal("complete logical validation accepted a noncanonical tree or child shape", err)
				}
			})
		}
		body, err := json.Marshal(original)
		if err != nil || bytes.Contains(body, []byte("borrowed")) || bytes.Contains(body, []byte("owned")) || bytes.Contains(body, []byte("heap")) || bytes.Contains(body, []byte("added")) {
			test.Fatal("logical DTO serialized allocation or authority grants", err)
		}
	})
}
