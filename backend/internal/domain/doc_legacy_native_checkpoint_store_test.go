package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func legacyNativeCheckpointStoreFixture(test *testing.T) (legacyNativeCheckpointIO, map[ContentHash][]byte) {
	test.Helper()
	objects := make(map[ContentHash][]byte)
	store := legacyNativeCheckpointIO{
		Put: func(ctx context.Context, hash ContentHash, body []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if HashContent(body) != hash || len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes {
				return ErrIntegrity
			}
			if previous, exists := objects[hash]; exists && !bytes.Equal(previous, body) {
				return ErrIntegrity
			}
			objects[hash] = bytes.Clone(body)
			return nil
		},
		Read: func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			body, exists := objects[hash]
			if !exists {
				return nil, ErrNotFound
			}
			if len(body) != length {
				return nil, ErrIntegrity
			}
			return bytes.Clone(body), nil
		},
	}
	return store, objects
}

func TestLegacyNativeCheckpointStoreTree(test *testing.T) {
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	ref, err := legacyNativeCheckpointWrite(ctx, store, []byte("leaf"))
	if err != nil {
		test.Fatal(err)
	}
	var builder legacyNativeCheckpointTreeBuilder
	const leaves = 1103
	var total uint64
	for ordinal := 0; ordinal < leaves; ordinal++ {
		weight := uint64(ordinal%7 + 1)
		if err := builder.append(ctx, store, ref, weight); err != nil {
			test.Fatal(err)
		}
		total += weight
	}
	tree, err := builder.finish(ctx, store)
	if err != nil || tree.Count != total || tree.Height < 2 {
		test.Fatalf("tree=%+v err=%v", tree, err)
	}
	cursor, err := newLegacyNativeCheckpointTreeCursor(tree, store)
	if err != nil {
		test.Fatal(err)
	}
	var position uint64
	for ordinal := 0; ordinal < leaves; ordinal++ {
		got, weight, exists, err := cursor.next(ctx)
		if err != nil || !exists || got != ref || weight != uint64(ordinal%7+1) {
			test.Fatalf("leaf %d: %v %d %v %v", ordinal, got, weight, exists, err)
		}
		seek, start, err := legacyNativeCheckpointLeafAt(ctx, store, tree, position+weight-1)
		if err != nil || seek != ref || start != position {
			test.Fatalf("seek %d: %v %d %v", ordinal, seek, start, err)
		}
		position += weight
	}
	if _, _, exists, err := cursor.next(ctx); err != nil || exists {
		test.Fatalf("EOF: %v %v", exists, err)
	}
	if _, _, err := legacyNativeCheckpointLeafAt(ctx, store, tree, total); err == nil {
		test.Fatal("accepted out-of-range seek")
	}
	cursor.close()
	builder.close()
}

func TestLegacyNativeCheckpointStoreBoundedLists(test *testing.T) {
	ctx := context.Background()
	store, objects := legacyNativeCheckpointStoreFixture(test)
	var writer legacyNativeCheckpointListWriter[int]
	for ordinal := 0; ordinal < 1057; ordinal++ {
		if err := writer.append(ctx, store, ordinal); err != nil {
			test.Fatal(err)
		}
	}
	tree, err := writer.finish(ctx, store)
	if err != nil {
		test.Fatal(err)
	}
	reader, err := newLegacyNativeCheckpointListReader[int](tree, store)
	if err != nil {
		test.Fatal(err)
	}
	for ordinal := 0; ordinal < 1057; ordinal++ {
		got, exists, err := reader.next(ctx)
		if err != nil || !exists || got != ordinal {
			test.Fatalf("item %d: %d %v %v", ordinal, got, exists, err)
		}
	}
	if _, exists, err := reader.next(ctx); err != nil || exists {
		test.Fatalf("EOF: %v %v", exists, err)
	}
	for _, count := range []int{33, 20000} {
		body, err := json.Marshal(legacyNativeCheckpointListPage[int]{Version: 1, Values: make([]int, count)})
		if err != nil {
			test.Fatal(err)
		}
		ref, err := legacyNativeCheckpointWrite(ctx, store, body)
		if err != nil {
			test.Fatal(err)
		}
		bad, err := newLegacyNativeCheckpointListReader[int](legacyNativeCheckpointTree{Ref: ref, Count: uint64(count)}, store)
		if err != nil {
			test.Fatal(err)
		}
		if _, _, err := bad.next(ctx); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("oversized page %d: %v", count, err)
		}
	}
	ref, err := legacyNativeCheckpointWrite(ctx, store, []byte(`{"v":1,"e":[1],"e":[2]}`))
	if err != nil {
		test.Fatal(err)
	}
	var page legacyNativeCheckpointListPage[int]
	if err := legacyNativeCheckpointReadJSON(ctx, store, ref, &page); !errors.Is(err, ErrIntegrity) {
		test.Fatalf("duplicate field: %v", err)
	}
	objects[ref.Hash][0] = '['
	if _, err := legacyNativeCheckpointRead(ctx, store, ref); !errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatalf("corrupt body: %v", err)
	}
	reader.close()
	writer.close()
}

func TestLegacyNativeCheckpointStoreScalarMigration(test *testing.T) {
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	prefix := strings.Repeat("복원🙂", 300000)
	previous, current := &strings.Builder{}, &strings.Builder{}
	previous.Grow(len(prefix))
	previous.WriteString(prefix)
	current.Grow(2 * previous.Cap())
	current.WriteString(prefix[:11])
	scalar := &legacyNativeScalar{position: 17, end: 32, maximum: 2*len(prefix) + 37, previous: previous, text: current, copied: 11, expanded: true}
	writer, err := newLegacyNativeCheckpointScalarWriter(ctx, scalar, store)
	if err != nil {
		test.Fatal(err)
	}
	for {
		ready, err := writer.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if ready {
			break
		}
	}
	saved, err := writer.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if saved.State.Bytes != len(prefix) || saved.State.Capacity != current.Cap() || scalar.copied != 11 || current.Len() != 11 {
		test.Fatal("capture changed migration or logical prefix")
	}
	restore, err := newLegacyNativeCheckpointScalarRestore(ctx, saved, store, 1024, 64<<20)
	if err != nil {
		test.Fatal(err)
	}
	for {
		ready, err := restore.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if ready {
			break
		}
	}
	got, err := restore.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if got.text.String() != prefix || got.text.Cap() != current.Cap() || got.previous != nil || got.position != scalar.position || got.end != scalar.end || !got.expanded {
		test.Fatal("logical scalar state or capacity changed")
	}
	if _, err := newLegacyNativeCheckpointScalarRestore(ctx, saved, store, 1024, 1); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatalf("budget not admitted: %v", err)
	}
}

func TestLegacyNativeCheckpointStoreStickyFailure(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store, _ := legacyNativeCheckpointStoreFixture(test)
	var list legacyNativeCheckpointListWriter[int]
	if err := list.append(ctx, store, 1); err != nil {
		test.Fatal(err)
	}
	cancel()
	if _, err := list.finish(ctx, store); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if err := list.append(context.Background(), store, 2); !errors.Is(err, context.Canceled) {
		test.Fatalf("lost sticky cancellation: %v", err)
	}
	if retained, err := list.retainedBytes(); err != nil || retained != 0 {
		test.Fatalf("failure retained %d: %v", retained, err)
	}
	failure := errors.New("CAS unavailable")
	reads, writes := 0, 0
	store.Put = func(context.Context, ContentHash, []byte) error {
		writes++
		return failure
	}
	text, err := newLegacyNativeCheckpointTextWriter(context.Background(), 3, func(context.Context, int, int) ([]byte, error) {
		reads++
		return []byte("abc"), nil
	}, store)
	if err != nil {
		test.Fatal(err)
	}
	if ready, err := text.step(context.Background()); err != nil || ready || reads != 1 || writes != 0 {
		test.Fatalf("read phase did not yield before CAS: ready=%v reads=%d writes=%d err=%v", ready, reads, writes, err)
	}
	if _, err := text.step(context.Background()); !errors.Is(err, failure) {
		test.Fatal(err)
	}
	if _, err := text.step(context.Background()); !errors.Is(err, failure) {
		test.Fatalf("lost sticky CAS failure: %v", err)
	}
	if _, err := text.take(context.Background()); !errors.Is(err, failure) {
		test.Fatal(err)
	}
	if reads != 1 || writes != 1 {
		test.Fatalf("retried IO after sticky failure: reads=%d writes=%d", reads, writes)
	}
	if retained, err := text.retainedBytes(); err != nil || retained != 0 {
		test.Fatalf("failure retained %d: %v", retained, err)
	}
}

func TestLegacyNativeCheckpointStoreConservativeKey(test *testing.T) {
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	native := newLegacyNativeAnyMap()
	if err := native.put("a", 1, true, docRangeHeaderHeapEstimate{}, 0); err != nil {
		test.Fatal(err)
	}
	value, err := legacyNativeAnyCheckpointContainer(native, nil)
	if err != nil {
		test.Fatal(err)
	}
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	writer, err := newLegacyNativeCheckpointGraphWriter(ctx, value, store, identity, 32<<20)
	if err != nil {
		test.Fatal(err)
	}
	for {
		ready, err := writer.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if ready {
			break
		}
	}
	saved, err := writer.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	registry := newLegacyNativeCheckpointGraphRestored()
	defer registry.close()
	restore, err := newLegacyNativeCheckpointGraphRestore(ctx, saved, store, registry, 32<<20)
	if err != nil {
		test.Fatal(err)
	}
	for {
		ready, err := restore.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if ready {
			break
		}
	}
	got, err := restore.take(ctx)
	if err != nil || got.validateOwnership() != nil {
		test.Fatal("restored key ownership", err)
	}
	var actual strings.Builder
	actual.Grow(1)
	if got.owner.object.ownership["a"].keyCapacity != actual.Cap() || got.owner.object.values()["a"] != true {
		test.Fatal("restored key physical capacity or value was not transferred")
	}
}

func TestLegacyNativeCheckpointStoreRunSourceFences(test *testing.T) {
	for _, fixture := range legacyNativeAnyW78Fixtures() {
		if fixture.cut != "opening" && fixture.cut != "closing" {
			continue
		}
		test.Run(fixture.cut, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, _, _, _ := legacyNativeAnyW78Capture(test, fixture, storage)
			var state legacyNativeAnyCheckpointControl
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil {
				test.Fatal(err)
			}
			if err := state.validate(request.Binding, request.SourceBytes, true); err != nil {
				test.Fatal("original captured state rejected", err)
			}
			state.End = state.Position + 1
			if err := state.validate(request.Binding, request.SourceBytes, true); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("accepted run beyond source end: opening=%d closing=%d err=%v", state.OpeningRemaining, state.ClosingRemaining, err)
			}
		})
	}
}

func TestLegacyNativeCheckpointStoreGraphDecodeAdmission(test *testing.T) {
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	native := newLegacyNativeAnyMap()
	if err := native.put("a", 1, true, docRangeHeaderHeapEstimate{}, 0); err != nil {
		test.Fatal(err)
	}
	value, err := legacyNativeAnyCheckpointContainer(native, nil)
	if err != nil {
		test.Fatal(err)
	}
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	writer, err := newLegacyNativeCheckpointGraphWriter(ctx, value, store, identity, 32<<20)
	if err != nil {
		test.Fatal(err)
	}
	for {
		ready, err := writer.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if ready {
			break
		}
	}
	saved, err := writer.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	registry := newLegacyNativeCheckpointGraphRestored()
	defer registry.close()
	reads := 0
	counted := store
	counted.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		reads++
		return store.Read(current, hash, length)
	}
	restore, err := newLegacyNativeCheckpointGraphRestore(ctx, saved, counted, registry, 32<<20)
	if err != nil {
		test.Fatal(err)
	}
	if ready, err := restore.step(ctx); err != nil || ready || restore.frame.phase != legacyNativeCheckpointGraphRestoreBody {
		test.Fatalf("owner decode precondition: ready=%v err=%v", ready, err)
	}
	workspace, err := restore.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	registered, err := registry.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	restore.budget = workspace + registered + 1
	before := reads
	if _, err := restore.step(ctx); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatalf("decode not pre-admitted: %v", err)
	}
	if reads != before {
		test.Fatalf("read and decoded owner before budget rejection: %d reads", reads-before)
	}
}
