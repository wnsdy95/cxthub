package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func legacyNativeCheckpointGraphInlineRestore(test *testing.T, saved legacyNativeCheckpointValue, storage *legacyNativeGoGraphSetW85Store) legacyNativeAnyValue {
	test.Helper()
	registry := newLegacyNativeCheckpointGraphRestored()
	defer registry.close()
	work, err := newLegacyNativeCheckpointGraphRestore(context.Background(), saved, storage.io, registry, legacyNativeGoGraphW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	legacyNativeGoGraphSetW85Ready[legacyNativeAnyValue](test, work, storage, legacyNativeGoGraphW85Budget)
	value, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	legacyNativeGoGraphSetW85Released[legacyNativeAnyValue](test, work)
	return value
}

func TestLegacyNativeCheckpointGraphInlineLegacyOracle(test *testing.T) {
	for _, text := range []string{"", "x", strings.Repeat("x", 63), strings.Repeat("x", 64), strings.Repeat("x", 65), "\x00\ud55c\U0001f642\xff\xfe\"\\\n<>&"} {
		var builder strings.Builder
		builder.Grow(len(text))
		for _, capacity := range []int{len(text), builder.Cap(), 80} {
			test.Run(strconv.Itoa(len(text))+"/"+strconv.Itoa(capacity), func(test *testing.T) {
				value, err := legacyNativeAnyCheckpointString(text, capacity)
				if err != nil {
					test.Fatal(err)
				}
				before, original, _, oldIdentity := legacyNativeCheckpointGraphShortCapture(test, value, true)
				after, candidate, _, newIdentity := legacyNativeCheckpointGraphShortCapture(test, value, false)
				inline := len(text) > 0 && len(text) <= 64 && capacity <= 64
				if !bytes.Equal(before.Wire, after.Wire) || oldIdentity != newIdentity || (after.Inline != nil) != inline {
					test.Fatal("inline selection changed wire, capacity or identity")
				}
				if inline {
					if string(after.Inline) != text || after.Text != (legacyNativeCheckpointText{}) || after.Body != (legacyNativeCheckpointRef{}) || candidate.writes != 0 || len(candidate.objects) != 0 || original.writes != 1 || string(original.objects[before.Text.Tree.Ref.Hash]) != text {
						test.Fatal("inline did not replace exactly one raw text object")
					}
				} else if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(original.objects, candidate.objects) || original.writes != candidate.writes {
					test.Fatal("legacy fallback changed wire or CAS bytes")
				}
				oldValue := legacyNativeCheckpointGraphInlineRestore(test, before, original)
				newValue := legacyNativeCheckpointGraphInlineRestore(test, after, candidate)
				if !reflect.DeepEqual(oldValue, newValue) || newValue.value != text {
					test.Fatal("raw bytes, capacity, heap or excess differ from legacy reader", oldValue, newValue)
				}
			})
		}
	}
}

func TestLegacyNativeCheckpointGraphInlineKeepsLegacyMapKeys(test *testing.T) {
	source := map[string]any{"": "empty-key-value", strings.Repeat("k", 64): "short-value", "\x00\xff\ud55c\U0001f642": "raw-key-value"}
	registry := newLegacyNativeCheckpointGoGraphRegistry()
	defer registry.close()
	value, _ := legacyNativeGoGraphW85Import(test, source, registry)
	before, original, _, oldIdentity := legacyNativeCheckpointGraphShortCapture(test, value, true)
	after, optimized, _, newIdentity := legacyNativeCheckpointGraphShortCapture(test, value, false)
	if !bytes.Equal(before.Wire, after.Wire) || oldIdentity != newIdentity || optimized.writes != original.writes-len(source) {
		test.Fatal("map key fallback changed root identity or removed non-value writes")
	}
	var pages [2]legacyNativeCheckpointListPage[legacyNativeCheckpointMember]
	for index, fixture := range []struct {
		saved legacyNativeCheckpointValue
		store *legacyNativeGoGraphSetW85Store
	}{{before, original}, {after, optimized}} {
		var owner legacyNativeCheckpointOwner
		if err := json.Unmarshal(fixture.store.objects[fixture.saved.Body.Hash], &owner); err != nil || owner.Members.Height != 0 || owner.Members.Count != uint64(len(source)) {
			test.Fatal("map oracle did not retain its bounded member page", owner, err)
		}
		if err := json.Unmarshal(fixture.store.objects[owner.Members.Ref.Hash], &pages[index]); err != nil || len(pages[index].Values) != len(source) {
			test.Fatal("map oracle member page failed to decode", err)
		}
	}
	for index, member := range pages[1].Values {
		previous := pages[0].Values[index]
		if !reflect.DeepEqual(member.Key, previous.Key) || member.Key.Inline != nil || !bytes.Equal(member.Value.Wire, previous.Value.Wire) {
			test.Fatal("map key lost exact legacy wire, capacity, Text tree or hash")
		}
		key := string(optimized.objects[member.Key.Text.Tree.Ref.Hash])
		if member.Key.Text.Bytes != len(key) || len(key) > 0 && (member.Key.Text.Tree.Ref.Hash != HashContent([]byte(key)) || !bytes.Equal(original.objects[previous.Key.Text.Tree.Ref.Hash], []byte(key))) {
			test.Fatal("map key lost its original raw CAS payload")
		}
		if expected, ok := source[key]; !ok || string(member.Value.Inline) != expected || member.Value.Text != (legacyNativeCheckpointText{}) || member.Value.Body != (legacyNativeCheckpointRef{}) {
			test.Fatal("ordinary map values no longer use exclusive inline text")
		}
	}
	result := legacyNativeCheckpointGraphInlineRestore(test, after, optimized)
	inspector := &legacyNativeGoGraphW85Inspector{}
	inspector.check(test, source, result)
}

func TestLegacyNativeCheckpointGraphInlineMalformed(test *testing.T) {
	value, err := legacyNativeAnyCheckpointString("raw", 8)
	if err != nil {
		test.Fatal(err)
	}
	saved, _, _, _ := legacyNativeCheckpointGraphShortCapture(test, value, false)
	for _, scenario := range []string{"empty", "oversize", "body", "text", "text-hash", "length", "capacity", "reference", "nil", "kind"} {
		test.Run(scenario, func(test *testing.T) {
			changed := saved
			changed.Wire = bytes.Clone(saved.Wire)
			switch scenario {
			case "empty":
				changed.Inline = []byte{}
			case "oversize":
				changed.Inline = make([]byte, 65)
			case "body":
				changed.Body = legacyNativeCheckpointRef{Hash: HashContent([]byte("raw")), Bytes: 3}
			case "text":
				changed.Text.Bytes = 3
			case "text-hash":
				changed.Text.Tree.Ref.Hash = HashContent([]byte("raw"))
			case "length":
				binary.LittleEndian.PutUint64(changed.Wire[24:32], 2)
			case "capacity":
				binary.LittleEndian.PutUint64(changed.Wire[32:40], 65)
			case "reference":
				binary.LittleEndian.PutUint64(changed.Wire[16:24], 1)
			case "nil":
				changed.Wire[2] = 1
			case "kind":
				changed.Wire[1] = legacyNativeCheckpointNull
			}
			registry := newLegacyNativeCheckpointGraphRestored()
			defer registry.close()
			store, _ := legacyNativeCheckpointStoreFixture(test)
			work, err := newLegacyNativeCheckpointGraphRestore(context.Background(), changed, store, registry, legacyNativeGoGraphW85Budget)
			if work != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("malformed inline descriptor returned owned work", work, err)
			}
		})
	}
	store, objects := legacyNativeCheckpointStoreFixture(test)
	encoded, err := json.Marshal(saved)
	if err != nil {
		test.Fatal(err)
	}
	for _, field := range []string{`"i":null`, `"i":""`, `"i":"???"`, `"i":"cmF3","unknown":0`, `"i":"cmF3","i":"cmF3"`, `"i":"cmF3\n"`} {
		body := bytes.Replace(encoded, []byte(`"i":"cmF3"`), []byte(field), 1)
		ref, err := legacyNativeCheckpointWrite(context.Background(), store, body)
		if err != nil {
			test.Fatal(err)
		}
		var decoded legacyNativeCheckpointValue
		if err := legacyNativeCheckpointReadJSON(context.Background(), store, ref, &decoded); !errors.Is(err, ErrIntegrity) {
			test.Fatal("noncanonical/unknown inline JSON accepted", field, err)
		}
	}
	ref, err := legacyNativeCheckpointWrite(context.Background(), store, encoded)
	if err != nil {
		test.Fatal(err)
	}
	var oldReader struct {
		Wire []byte                     `json:"v"`
		Body legacyNativeCheckpointRef  `json:"b"`
		Text legacyNativeCheckpointText `json:"t"`
	}
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, ref, &oldReader); !errors.Is(err, ErrIntegrity) {
		test.Fatal("original reader did not fail closed on inline text", err)
	}
	objects[ref.Hash] = bytes.Replace(encoded, []byte("cmF3"), []byte("YmFk"), 1)
	var current legacyNativeCheckpointValue
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, ref, &current); !errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("inline payload bypassed current CAS hash validation", err)
	}
}

func TestLegacyNativeCheckpointGraphInlineOwnedClones(test *testing.T) {
	ctx := context.Background()
	value, err := legacyNativeAnyCheckpointString("raw", 8)
	if err != nil {
		test.Fatal(err)
	}
	saved, _, _, _ := legacyNativeCheckpointGraphShortCapture(test, value, false)
	backing := make([]byte, 4096)
	copy(backing, saved.Inline)
	saved.Inline = backing[:3]
	plain := saved
	plain.Inline = nil
	var allocation docRangeHeaderHeapEstimate
	allocation.allocation(int64(cap(backing)), 1)
	for _, measure := range []func(legacyNativeCheckpointValue) docRangeHeaderHeapEstimate{
		legacyNativeAnyCheckpointDescriptorHeap,
		legacyNativeCheckpointGraphRestoreSavedHeap,
		func(saved legacyNativeCheckpointValue) docRangeHeaderHeapEstimate {
			var heap docRangeHeaderHeapEstimate
			legacyNativeCheckpointGraphCapturedValueHeap(&heap, saved)
			return heap
		},
	} {
		before, after := measure(plain), measure(saved)
		if before.err != nil || after.err != nil || after.bytes-before.bytes != allocation.bytes {
			test.Fatal("inline descriptor did not charge its entire retained allocation", before, after)
		}
	}
	storage := legacyNativeGoGraphSetW85Storage(test)
	registry := newLegacyNativeCheckpointGraphRestored()
	defer registry.close()
	graph, err := newLegacyNativeCheckpointGraphRestore(ctx, saved, storage.io, registry, legacyNativeGoGraphW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer graph.close()
	source := LegacyByteSource{Bytes: 1, authority: 1, Parts: []ConversationManifestChunk{{Hash: HashContent([]byte("0")), Bytes: 1}}, Read: func(context.Context, int, int) ([]byte, error) {
		test.Fatal("independent inline restore replayed source bytes")
		return nil, ErrIntegrity
	}}
	field, err := newLegacyNativeObjectAnyCheckpointRestore(ctx, legacyNativeObjectCheckpointAnyField{Destination: &saved}, false, true, HashContent([]byte("inline-field")), source, storage.io, legacyNativeGoGraphW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer field.close()
	if cap(graph.frame.saved.Inline) != 3 || cap(field.state.Destination.Inline) != 3 || &graph.frame.saved.Inline[0] == &field.state.Destination.Inline[0] || &graph.frame.saved.Inline[0] == &backing[0] || &field.state.Destination.Inline[0] == &backing[0] {
		test.Fatal("restore constructors retained a shared raw buffer")
	}
	copy(backing, "bad")
	saved.Wire[0] = 255
	legacyNativeGoGraphSetW85Ready[legacyNativeAnyValue](test, graph, storage, legacyNativeGoGraphW85Budget)
	restored, err := graph.take(ctx)
	if err != nil || restored.value != "raw" {
		test.Fatal("graph clone changed with caller bytes", restored, err)
	}
	legacyNativeGoGraphSetW85Ready[legacyNativeObjectAnyRestored](test, field, storage, legacyNativeGoGraphW85Budget)
	actual, err := field.take(ctx)
	if err != nil || actual.Value != "raw" || storage.reads != 0 || storage.writes != 0 {
		test.Fatal("field clone changed with caller bytes or used CAS", actual, err)
	}
}

func TestLegacyNativeCheckpointGraphInlinePageBounds(test *testing.T) {
	value, err := legacyNativeAnyCheckpointString(strings.Repeat("\xff", 64), 64)
	if err != nil {
		test.Fatal(err)
	}
	saved, _, _, _ := legacyNativeCheckpointGraphShortCapture(test, value, false)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	var writer legacyNativeCheckpointListWriter[legacyNativeCheckpointMember]
	defer writer.close()
	member := legacyNativeCheckpointMember{Key: saved, Value: saved}
	row, err := json.Marshal(member)
	if err != nil {
		test.Fatal(err)
	}
	for position := 0; position < 32; position++ {
		if err := writer.append(context.Background(), store, member); err != nil {
			test.Fatal(err)
		}
		if position < 31 && writer.bytes != int64(position+1)*int64(4*len(row)+1024) {
			test.Fatal("inline rows lost metadata-inclusive retention")
		}
	}
	tree, err := writer.finish(context.Background(), store)
	if err != nil || tree.Count != 32 || tree.Height != 0 || tree.Ref.Bytes > LegacyCanonicalEncodingBytes || tree.Ref.Bytes <= 32*128 {
		test.Fatal("full inline page changed its original items/total-byte bounds", tree, err)
	}
	var page legacyNativeCheckpointListPage[legacyNativeCheckpointMember]
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, tree.Ref, &page); err != nil || len(page.Values) != 32 {
		test.Fatal("full inline page did not round trip", err)
	}
	page.Values = append(page.Values, member)
	ref, err := legacyNativeCheckpointWriteJSON(context.Background(), store, page)
	if err != nil {
		test.Fatal(err)
	}
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, ref, &page); !errors.Is(err, ErrIntegrity) {
		test.Fatal("inline pages accepted a 33rd row", err)
	}
	test.Logf("32 max-inline member rows including wire/body/text/page metadata: %d bytes", tree.Ref.Bytes)
	roots := make([]any, 32)
	for index := range roots {
		roots[index] = strings.Repeat("\xff", 64)
	}
	storage := legacyNativeGoGraphSetW85Storage(test)
	binding := HashContent([]byte("inline-max-root-page"))
	root, identities, _ := legacyNativeGoGraphSetTrivialCapture(test, roots, binding, storage, false)
	if identities != 32 || storage.writes != 1 || root.Bytes <= 32*64 || root.Bytes > LegacyCanonicalEncodingBytes {
		test.Fatal("32 inline roots changed total metadata byte bound", root, identities, storage.writes)
	}
	values := legacyNativeGoGraphSetW85Restore(test, root, binding, storage, 0)
	legacyNativeGoGraphSetW85Values(test, roots, values)
	test.Logf("32 max-inline roots including binding and all descriptor metadata: %d bytes", root.Bytes)
}

func TestLegacyNativeCheckpointGraphInlineRestoreFences(test *testing.T) {
	for _, scenario := range []string{"cancel", "cancel-after-allocation", "budget", "capacity", "registry-closed", "registry-failure"} {
		test.Run(scenario, func(test *testing.T) {
			value, err := legacyNativeAnyCheckpointString("raw", 8)
			if err != nil {
				test.Fatal(err)
			}
			saved, storage, _, _ := legacyNativeCheckpointGraphShortCapture(test, value, false)
			if scenario == "capacity" {
				binary.LittleEndian.PutUint64(saved.Wire[32:40], 9)
			}
			registry := newLegacyNativeCheckpointGraphRestored()
			defer registry.close()
			work, err := newLegacyNativeCheckpointGraphRestore(context.Background(), saved, storage.io, registry, legacyNativeGoGraphW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var current context.Context = ctx
			expected := ErrIntegrity
			switch scenario {
			case "cancel":
				cancel()
				expected = context.Canceled
			case "cancel-after-allocation":
				calls := 0
				current = legacyNativeGoGraphSetTrivialBatchContext{Context: ctx, check: func() error {
					calls++
					if calls == 2 {
						cancel()
						return context.Canceled
					}
					return nil
				}}
				expected = context.Canceled
			case "budget":
				retained, err := work.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				shared, err := registry.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				work.budget, expected = retained+shared, ErrDocumentResourceLimit
			case "registry-closed":
				registry.close()
			case "registry-failure":
				registry.failure, expected = ErrConflict, ErrConflict
			}
			if ready, err := work.step(current); ready || !errors.Is(err, expected) || storage.reads != 0 || storage.writes != 0 {
				test.Fatal("inline restore bypassed a current fence", ready, err, expected)
			}
			legacyNativeGoGraphSetW85Failed[legacyNativeAnyValue](test, work, storage, expected)
		})
	}
}

func TestLegacyNativeCheckpointGraphInlineNumberAndCapture(test *testing.T) {
	number := json.Number("1.25e2")
	list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{Kind: EventToolCall, ProviderMetadata: &ProviderMetadata{CreateTime: &number}}})
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("inline-number")), storage.io, legacyNativeGoGraphW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for steps := 0; steps < legacyNativeGoGraphW85StepLimit && work.graph.position <= legacyCanonicalSourceNumberRoot; steps++ {
		if err := list.mutation.enter(); !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("inline fields lost their production mutation barrier", err)
		}
		if ready, err := legacyNativeGoGraphSetW85Step(test, work.graph, storage); ready || err != nil {
			test.Fatal("field roots did not finish before metadata publication", ready, err)
		}
	}
	if work.graph.position <= legacyCanonicalSourceNumberRoot || work.graph.state.Roots[0].Inline == nil || storage.writes != 1 {
		test.Fatal("inline fields did not retain exactly the numeric text CAS", storage.writes)
	}
	saved := work.graph.state.Roots[legacyCanonicalSourceNumberRoot]
	if saved.Inline != nil || saved.Text.Bytes != len(number) || string(storage.objects[saved.Text.Tree.Ref.Hash]) != string(number) {
		test.Fatal("CreateTime numeric reference lost its original Text form")
	}
	if err := legacyCanonicalSourceNumberDescriptor(context.Background(), saved, saved.Text); err != nil {
		test.Fatal("numeric reference no longer verifies", err)
	}
	work.close()
	if err := list.mutation.enter(); err != nil {
		test.Fatal("inline capture did not release mutation exclusion", err)
	}
	list.mutation.leave()
}
