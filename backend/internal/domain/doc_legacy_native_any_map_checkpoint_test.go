package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeAnyMapCheckpointOraclePut(test *testing.T, native *legacyNativeAnyMap, key string, capacity int, value any, excess int64) legacyNativeAnyMapCheckpointEntry {
	test.Helper()
	retainedCapacity := max(capacity, native.ownership[key].keyCapacity)
	heap := legacyNativeAnyMapTestValueHeap(test, value)
	heap.include(docRangeHeaderHeapEstimate{bytes: excess})
	if err := native.put(key, capacity, value, heap, excess); err != nil {
		test.Fatal("checkpoint fixture put failed", err)
	}
	return legacyNativeAnyMapCheckpointEntry{
		Key: key, Value: value, KeyCapacity: retainedCapacity, ValueBytes: heap.bytes, ValueExcess: excess,
	}
}

func legacyNativeAnyMapCheckpointOracleOpen(test *testing.T, native *legacyNativeAnyMap, full bool) *legacyNativeAnyMapCheckpoint {
	test.Helper()
	cursor, err := native.checkpoint(context.Background(), full)
	if err != nil || cursor == nil {
		test.Fatal("checkpoint construction failed", full, err)
	}
	test.Cleanup(cursor.close)
	return cursor
}

func legacyNativeAnyMapCheckpointOracleDrain(test *testing.T, cursor *legacyNativeAnyMapCheckpoint, expected []legacyNativeAnyMapCheckpointEntry) {
	test.Helper()
	for position, wanted := range expected {
		entry, ok, err := cursor.next(context.Background())
		if err != nil || !ok || !reflect.DeepEqual(entry, wanted) {
			test.Fatal("checkpoint changed insertion/dirty order, current payload, or ownership metadata", position, entry, wanted, ok, err)
		}
	}
	if entry, ok, err := cursor.next(context.Background()); err != nil || ok {
		test.Fatal("checkpoint did not report verified EOF after exactly the expected entries", entry, ok, err)
	}
}

func legacyNativeAnyMapCheckpointOracleCheck(test *testing.T, native *legacyNativeAnyMap, full bool, expected []legacyNativeAnyMapCheckpointEntry) *legacyNativeAnyMapCheckpoint {
	test.Helper()
	cursor := legacyNativeAnyMapCheckpointOracleOpen(test, native, full)
	legacyNativeAnyMapCheckpointOracleDrain(test, cursor, expected)
	return cursor
}

func legacyNativeAnyMapCheckpointOracleAcknowledge(test *testing.T, cursor *legacyNativeAnyMapCheckpoint, limit int, expected bool) {
	test.Helper()
	if complete, err := cursor.acknowledge(context.Background(), limit); err != nil || complete != expected {
		test.Fatal("bounded acknowledgement changed completion or rejected a verified checkpoint", limit, complete, expected, err)
	}
}

func legacyNativeAnyMapCheckpointOracleAccounting(test *testing.T, native *legacyNativeAnyMap, entries []legacyNativeAnyMapCheckpointEntry) {
	test.Helper()
	var excess int64
	for _, entry := range entries {
		var allocated, minimum docRangeHeaderHeapEstimate
		allocated.allocation(int64(entry.KeyCapacity), 1)
		minimum.allocation(int64(len(entry.Key)), 1)
		if allocated.err != nil || minimum.err != nil {
			test.Fatal("invalid expected checkpoint key accounting", allocated, minimum)
		}
		excess += allocated.bytes - minimum.bytes + entry.ValueExcess
	}
	legacyNativeAnyMapTestAccounting(test, native, excess)
}

func legacyNativeAnyMapCheckpointOracleSnapshot(native *legacyNativeAnyMap) legacyNativeAnyMap {
	snapshot := *native
	snapshot.decoded = make(map[string]any, len(native.decoded))
	for key, value := range native.decoded {
		snapshot.decoded[key] = value
	}
	snapshot.ownership = make(map[string]legacyNativeAnyMapOwnership, len(native.ownership))
	for key, row := range native.ownership {
		snapshot.ownership[key] = row
	}
	snapshot.links = legacyCanonicalPages[legacyNativeAnyMapLinks]{}
	for index := 0; index < native.links.length(); index++ {
		if err := snapshot.links.append(*native.links.at(index)); err != nil {
			panic(err)
		}
	}
	return snapshot
}

func legacyNativeAnyMapCheckpointOracleSeed(test *testing.T) (*legacyNativeAnyMap, []legacyNativeAnyMapCheckpointEntry) {
	test.Helper()
	native := newLegacyNativeAnyMap()
	var expected []legacyNativeAnyMapCheckpointEntry
	for position, key := range []string{"", "middle", "last"} {
		expected = append(expected, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), float64(position), 0))
	}
	return native, expected
}

func legacyNativeAnyMapCheckpointOracleNextError(test *testing.T, cursor *legacyNativeAnyMapCheckpoint, ctx context.Context, expected error) {
	test.Helper()
	if _, ok, err := cursor.next(ctx); ok || !errors.Is(err, expected) {
		test.Fatal("checkpoint next changed the current-context, stale, or integrity error", ok, err, expected)
	}
}

func legacyNativeAnyMapCheckpointOracleParse(test *testing.T, parser *legacyNativeAny, raw string) *legacyNativeAny {
	test.Helper()
	var err error
	if parser == nil {
		parser, err = newLegacyNativeAny(0, len(raw), 0, 64)
	} else {
		err = parser.start(0, len(raw), 0, 64)
	}
	if err != nil {
		test.Fatal("nativeAny parser could not start the map reuse fixture", err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	var frame legacyJSONValueScanner
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		complete, stepErr := parser.step(ctx, input, &frame)
		cancel()
		if stepErr != nil || input.failure != nil {
			test.Fatal("actual nativeAny map parser rejected the reuse fixture", stepIndex, stepErr, input.failure)
		}
		if input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
			input.fields < 0 || input.fields > 64 || input.values < 0 || input.values > legacyNativeAnyValueActions {
			test.Fatal("nativeAny map parsing escaped its owner's shared quantum", input.workBytes, input.loads, input.fields, input.values)
		}
		for _, page := range input.pages {
			clear(page)
		}
		if complete {
			if !parser.finished || parser.rootMap == nil || parser.position != len(raw) || !reflect.DeepEqual(parser.result.value, parser.rootMap.values()) {
				test.Fatal("nativeAny parser did not finish with its actual owned root map")
			}
			return parser
		}
	}
	test.Fatal("nativeAny map reuse fixture exhausted its finite parser schedule")
	return nil
}

func legacyNativeAnyMapCheckpointOracleParsedEntries(test *testing.T, native *legacyNativeAnyMap, keys []string, expected map[string]any) []legacyNativeAnyMapCheckpointEntry {
	test.Helper()
	entries := make([]legacyNativeAnyMapCheckpointEntry, 0, len(keys))
	for _, key := range keys {
		row, found := native.ownership[key]
		value, exists := expected[key]
		if !found || !exists {
			test.Fatal("parser checkpoint expectation lost a real key", key, found, exists)
		}
		if err := row.owner.matches(native.decoded[key]); err != nil {
			test.Fatal("parser checkpoint lost its actual child owner", key, err)
		}
		entries = append(entries, legacyNativeAnyMapCheckpointEntry{
			Key: key, Value: value, KeyCapacity: row.keyCapacity, ValueBytes: row.valueBytes, ValueExcess: row.valueExcess, owner: row.owner,
		})
	}
	return entries
}

func TestLegacyNativeAnyMapCheckpointFullOrderAndParserReuse(test *testing.T) {
	test.Run("fresh_empty", func(test *testing.T) {
		native := newLegacyNativeAnyMap()
		for _, full := range []bool{false, true} {
			cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, full, nil)
			legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, true)
		}
		legacyNativeAnyMapCheckpointOracleAccounting(test, native, nil)
	})
	for _, fixture := range []struct {
		name     string
		position int
	}{
		{name: "empty_first", position: 0},
		{name: "empty_middle", position: 2},
		{name: "empty_last", position: 3},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			native := newLegacyNativeAnyMap()
			canonical := strings.Clone("first-owned-key")
			keys := []string{canonical, "\ud55c\uae00🙂", strings.Repeat("huge-key-", ConversationManifestChunkBytes/4+19)}
			keys = append(keys, "")
			copy(keys[fixture.position+1:], keys[fixture.position:len(keys)-1])
			keys[fixture.position] = ""
			var expected []legacyNativeAnyMapCheckpointEntry
			replaced := -1
			for position, key := range keys {
				capacity := len(key)
				if key == canonical {
					capacity += 32
					replaced = position
				}
				expected = append(expected, legacyNativeAnyMapCheckpointOraclePut(test, native, key, capacity, map[string]any{"old": true}, 0))
			}
			legacyNativeAnyMapCheckpointOracleCheck(test, native, true, expected)
			for position, value := range []any{
				[]any{"array", map[string]any{"old-child": true}},
				"scalar",
				nil,
				map[string]any(nil),
				[]any(nil),
				map[string]any{"new-child": []any{nil, false, float64(2.5)}},
			} {
				capacity := len(canonical)
				if position == 1 {
					capacity += 128
				}
				var excess int64
				if position == 1 {
					excess = 18
				}
				entry := legacyNativeAnyMapCheckpointOraclePut(test, native, strings.Clone(canonical), capacity, value, excess)
				entry.Key = canonical
				expected[replaced] = entry
				legacyNativeAnyMapCheckpointOracleCheck(test, native, true, expected)
				legacyNativeAnyMapCheckpointOracleAccounting(test, native, expected)
			}
			expected[fixture.position] = legacyNativeAnyMapCheckpointOraclePut(test, native, "", 24, nil, 0)
			legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
			cursor := legacyNativeAnyMapCheckpointOracleOpen(test, native, true)
			for _, wanted := range expected {
				entry, ok, err := cursor.next(context.Background())
				if err != nil || !ok || !reflect.DeepEqual(entry, wanted) {
					test.Fatal("full checkpoint changed a final replacement", entry, wanted, ok, err)
				}
				if entry.Key != "" && unsafe.StringData(entry.Key) != unsafe.StringData(wanted.Key) {
					test.Fatal("checkpoint cloned a key or retained an equal-key replacement's backing")
				}
			}
			legacyNativeAnyMapCheckpointOracleDrain(test, cursor, nil)
			legacyNativeAnyMapCheckpointOracleAccounting(test, native, expected)
		})
	}
	test.Run("actual_nativeAny_reuses_root_map", func(test *testing.T) {
		first := `{"keep":1,"replace":{"old":true},"":null,"replace":{"new":false},"keep":2}`
		second := `{"replace":null,"new":true,"":false,"replace":{"final":3}}`
		var expected map[string]any
		if err := json.Unmarshal([]byte(first), &expected); err != nil {
			test.Fatal(err)
		}
		parser := legacyNativeAnyMapCheckpointOracleParse(test, nil, first)
		owner, visible := parser.rootMap, parser.rootMap.values()
		firstEntries := legacyNativeAnyMapCheckpointOracleParsedEntries(test, owner, []string{"keep", "replace", ""}, expected)
		initial := legacyNativeAnyMapCheckpointOracleCheck(test, owner, true, firstEntries)
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, initial, 1024, true)
		if err := json.Unmarshal([]byte(second), &expected); err != nil {
			test.Fatal(err)
		}
		parser = legacyNativeAnyMapCheckpointOracleParse(test, parser, second)
		if parser.rootMap != owner || !reflect.DeepEqual(owner.values(), expected) || !reflect.DeepEqual(visible, expected) {
			test.Fatal("nativeAny restart replaced its owned map or changed actual native map reuse semantics", owner.values(), expected)
		}
		full := legacyNativeAnyMapCheckpointOracleParsedEntries(test, owner, []string{"keep", "replace", "", "new"}, expected)
		delta := legacyNativeAnyMapCheckpointOracleParsedEntries(test, owner, []string{"replace", "new", ""}, expected)
		legacyNativeAnyMapCheckpointOracleCheck(test, owner, true, full)
		cursor := legacyNativeAnyMapCheckpointOracleCheck(test, owner, false, delta)
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, false)
		legacyNativeAnyMapCheckpointOracleCheck(test, owner, false, delta[1:])
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1024, true)
		legacyNativeAnyMapCheckpointOracleCheck(test, owner, false, nil)
		legacyNativeAnyMapTestAccounting(test, owner, owner.backingExcess())
	})
}

func TestLegacyNativeAnyMapCheckpointDirtyDedupAndBoundedAcknowledge(test *testing.T) {
	test.Run("overlapping_tails_and_mutation_after_partial_ack", func(test *testing.T) {
		native, full := legacyNativeAnyMapCheckpointOracleSeed(test)
		initial := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, full)
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, initial, 1024, true)
		full[2] = legacyNativeAnyMapCheckpointOraclePut(test, native, "last", 4, "changed-last", 0)
		full = append(full, legacyNativeAnyMapCheckpointOraclePut(test, native, "added", 5, true, 0))
		full[0] = legacyNativeAnyMapCheckpointOraclePut(test, native, "", 0, nil, 0)
		for range 9 {
			for _, position := range []int{2, 3, 0} {
				entry := full[position]
				full[position] = legacyNativeAnyMapCheckpointOraclePut(test, native, strings.Clone(entry.Key), entry.KeyCapacity, entry.Value, entry.ValueExcess)
			}
		}
		legacyNativeAnyMapCheckpointOracleCheck(test, native, true, full)
		delta := []legacyNativeAnyMapCheckpointEntry{full[2], full[3], full[0]}
		cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, delta)
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, false)
		legacyNativeAnyMapCheckpointOracleCheck(test, native, false, delta[1:])
		full[2] = legacyNativeAnyMapCheckpointOraclePut(test, native, "last", 4, full[2].Value, 0)
		full = append(full, legacyNativeAnyMapCheckpointOraclePut(test, native, "after", 5, false, 0))
		before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
		if complete, err := cursor.acknowledge(context.Background(), 1024); complete || !errors.Is(err, ErrConflict) {
			test.Fatal("old partial acknowledgement cleared a newly dirty revision", complete, err)
		}
		if !reflect.DeepEqual(*native, before) {
			test.Fatal("stale acknowledgement mutated the dirty owner")
		}
		delta = []legacyNativeAnyMapCheckpointEntry{full[3], full[0], full[2], full[4]}
		legacyNativeAnyMapCheckpointOracleCheck(test, native, true, full)
		cursor = legacyNativeAnyMapCheckpointOracleCheck(test, native, false, delta)
		for position := range delta {
			legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, position == len(delta)-1)
			legacyNativeAnyMapCheckpointOracleCheck(test, native, false, delta[position+1:])
			legacyNativeAnyMapCheckpointOracleCheck(test, native, true, full)
		}
		legacyNativeAnyMapCheckpointOracleAccounting(test, native, full)
	})
	test.Run("limit_1024_and_empty_key_at_clear_boundary", func(test *testing.T) {
		native := newLegacyNativeAnyMap()
		expected := make([]legacyNativeAnyMapCheckpointEntry, 0, 1027)
		for position := 0; position < 1027; position++ {
			key := fmt.Sprintf("key-%04d", position)
			if position == 1024 {
				key = ""
			}
			expected = append(expected, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), float64(position), 0))
		}
		cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, true, expected)
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1024, false)
		legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected[1024:])
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, false)
		legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected[1025:])
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1024, true)
		legacyNativeAnyMapCheckpointOracleCheck(test, native, false, nil)
		legacyNativeAnyMapCheckpointOracleCheck(test, native, true, expected)
		legacyNativeAnyMapCheckpointOracleAccounting(test, native, expected)
	})
}

func TestLegacyNativeAnyMapCheckpointStaleContextAndRejectedOperations(test *testing.T) {
	test.Run("observed_EOF_required", func(test *testing.T) {
		for _, readEntry := range []bool{false, true} {
			test.Run(fmt.Sprintf("read_entry_%t", readEntry), func(test *testing.T) {
				native := newLegacyNativeAnyMap()
				expected := []legacyNativeAnyMapCheckpointEntry{legacyNativeAnyMapCheckpointOraclePut(test, native, "", 0, nil, 0)}
				cursor := legacyNativeAnyMapCheckpointOracleOpen(test, native, false)
				if readEntry {
					if _, ok, err := cursor.next(context.Background()); err != nil || !ok {
						test.Fatal("empty key was treated as EOF", ok, err)
					}
				}
				before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
				if complete, err := cursor.acknowledge(context.Background(), 1); complete || !errors.Is(err, ErrIntegrity) {
					test.Fatal("acknowledgement succeeded without an observed ok=false EOF", complete, err)
				}
				if !reflect.DeepEqual(*native, before) {
					test.Fatal("premature acknowledgement changed dirty ownership")
				}
				legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
			})
		}
		native := newLegacyNativeAnyMap()
		cursor := legacyNativeAnyMapCheckpointOracleOpen(test, native, true)
		if complete, err := cursor.acknowledge(context.Background(), 1); complete || !errors.Is(err, ErrIntegrity) {
			test.Fatal("empty map skipped the observed EOF requirement", complete, err)
		}
		cursor = legacyNativeAnyMapCheckpointOracleCheck(test, native, true, nil)
		legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, true)
	})
	test.Run("invalid_limits", func(test *testing.T) {
		for _, limit := range []int{-1, 0, 1025, int(^uint(0) >> 1)} {
			test.Run(fmt.Sprintf("limit_%d", limit), func(test *testing.T) {
				native, expected := legacyNativeAnyMapCheckpointOracleSeed(test)
				cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, true, expected)
				before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
				if complete, err := cursor.acknowledge(context.Background(), limit); complete || !errors.Is(err, ErrIntegrity) {
					test.Fatal("invalid clear limit was accepted", limit, complete, err)
				}
				if !reflect.DeepEqual(*native, before) {
					test.Fatal("invalid clear limit mutated the owner")
				}
				legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
			})
		}
	})
	test.Run("successful_put_invalidates_full_and_delta", func(test *testing.T) {
		for _, full := range []bool{false, true} {
			for _, insert := range []bool{false, true} {
				test.Run(fmt.Sprintf("full_%t_insert_%t", full, insert), func(test *testing.T) {
					native, expected := legacyNativeAnyMapCheckpointOracleSeed(test)
					reading := legacyNativeAnyMapCheckpointOracleOpen(test, native, full)
					acknowledging := legacyNativeAnyMapCheckpointOracleCheck(test, native, full, expected)
					currentContext := legacyNativeAnyMapCheckpointOracleOpen(test, native, full)
					if insert {
						expected = append(expected, legacyNativeAnyMapCheckpointOraclePut(test, native, "new", 3, nil, 0))
					} else {
						expected[0] = legacyNativeAnyMapCheckpointOraclePut(test, native, "", 0, expected[0].Value, 0)
					}
					before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
					legacyNativeAnyMapCheckpointOracleNextError(test, reading, context.Background(), ErrConflict)
					if complete, err := acknowledging.acknowledge(context.Background(), 1); complete || !errors.Is(err, ErrConflict) {
						test.Fatal("successful put failed to invalidate an EOF checkpoint", complete, err)
					}
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					legacyNativeAnyMapCheckpointOracleNextError(test, currentContext, ctx, context.Canceled)
					if !reflect.DeepEqual(*native, before) {
						test.Fatal("stale or canceled checkpoint operation mutated the owner")
					}
					legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
				})
			}
		}
	})
	test.Run("current_context_nil_and_closed_cursors", func(test *testing.T) {
		native, expected := legacyNativeAnyMapCheckpointOracleSeed(test)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
		if cursor, err := native.checkpoint(ctx, false); cursor != nil || !errors.Is(err, context.Canceled) {
			test.Fatal("checkpoint construction ignored its current canceled context", cursor, err)
		}
		var missing *legacyNativeAnyMap
		if _, err := missing.checkpoint(context.Background(), true); !errors.Is(err, ErrIntegrity) {
			test.Fatal("nil map accepted checkpoint construction", err)
		}
		if _, err := missing.checkpoint(ctx, true); !errors.Is(err, context.Canceled) {
			test.Fatal("nil map hid the current context error", err)
		}
		var absent *legacyNativeAnyMapCheckpoint
		legacyNativeAnyMapCheckpointOracleNextError(test, absent, context.Background(), ErrIntegrity)
		legacyNativeAnyMapCheckpointOracleNextError(test, absent, ctx, context.Canceled)
		if complete, err := absent.acknowledge(context.Background(), 1); complete || !errors.Is(err, ErrIntegrity) {
			test.Fatal("nil cursor accepted acknowledgement", complete, err)
		}
		absent.close()
		closed := legacyNativeAnyMapCheckpointOracleOpen(test, native, false)
		closed.close()
		if !reflect.DeepEqual(*closed, legacyNativeAnyMapCheckpoint{}) {
			test.Fatal("close retained borrowed owner or key references")
		}
		legacyNativeAnyMapCheckpointOracleNextError(test, closed, context.Background(), ErrIntegrity)
		if complete, err := closed.acknowledge(ctx, 0); complete || !errors.Is(err, context.Canceled) {
			test.Fatal("closed cursor or bad limit hid the current context error", complete, err)
		}
		origin, cancelOrigin := context.WithCancel(context.Background())
		fresh, err := native.checkpoint(origin, false)
		cancelOrigin()
		if err != nil || fresh == nil {
			test.Fatal("active creation context rejected a checkpoint", err)
		}
		test.Cleanup(fresh.close)
		legacyNativeAnyMapCheckpointOracleDrain(test, fresh, expected)
		canceledRead := legacyNativeAnyMapCheckpointOracleOpen(test, native, false)
		legacyNativeAnyMapCheckpointOracleNextError(test, canceledRead, ctx, context.Canceled)
		canceledAck := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
		if complete, err := canceledAck.acknowledge(ctx, 1024); complete || !errors.Is(err, context.Canceled) {
			test.Fatal("canceled acknowledgement cleared pending rows", complete, err)
		}
		if !reflect.DeepEqual(*native, before) {
			test.Fatal("canceled or invalid calls changed native ownership")
		}
		legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
	})
	test.Run("rejected_put_preserves_cursor_and_owner", func(test *testing.T) {
		failure := errors.New("checkpoint fixture child heap failure")
		for _, fixture := range []struct {
			name     string
			key      string
			capacity int
			heap     docRangeHeaderHeapEstimate
			excess   int64
			want     error
		}{
			{name: "negative_capacity", key: "middle", capacity: -1, want: ErrIntegrity},
			{name: "short_capacity", key: "middle", capacity: 5, want: ErrIntegrity},
			{name: "negative_heap", key: "middle", capacity: 6, heap: docRangeHeaderHeapEstimate{bytes: -1}, want: ErrIntegrity},
			{name: "child_error", key: "new", capacity: 3, heap: docRangeHeaderHeapEstimate{err: failure}, want: failure},
			{name: "negative_excess", key: "new", capacity: 3, excess: -1, want: ErrIntegrity},
			{name: "excess_above_heap", key: "middle", capacity: 6, heap: docRangeHeaderHeapEstimate{bytes: 1}, excess: 2, want: ErrIntegrity},
			{name: "key_overflow", key: "new", capacity: int(^uint(0) >> 1), want: ErrIntegrity},
			{name: "payload_overflow", key: "middle", capacity: 6, heap: docRangeHeaderHeapEstimate{bytes: math.MaxInt64}, want: ErrIntegrity},
		} {
			test.Run(fixture.name, func(test *testing.T) {
				native, expected := legacyNativeAnyMapCheckpointOracleSeed(test)
				cursor := legacyNativeAnyMapCheckpointOracleOpen(test, native, true)
				before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
				if err := native.put(fixture.key, fixture.capacity, nil, fixture.heap, fixture.excess); !errors.Is(err, fixture.want) {
					test.Fatal("invalid put changed error identity", err, fixture.want)
				}
				if !reflect.DeepEqual(*native, before) {
					test.Fatal("rejected put changed owner values, links, revision, or accounting")
				}
				legacyNativeAnyMapCheckpointOracleDrain(test, cursor, expected)
				legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
				legacyNativeAnyMapCheckpointOracleAccounting(test, native, expected)
			})
		}
	})
	test.Run("corrupt_rows_and_cycles_fail_finitely", func(test *testing.T) {
		for _, fixture := range []struct {
			name    string
			corrupt func(*legacyNativeAnyMap)
		}{
			{name: "key_capacity", corrupt: func(native *legacyNativeAnyMap) {
				row := native.ownership["middle"]
				row.keyCapacity = 0
				native.ownership["middle"] = row
			}},
			{name: "negative_value_bytes", corrupt: func(native *legacyNativeAnyMap) {
				row := native.ownership["middle"]
				row.valueBytes = -1
				native.ownership["middle"] = row
			}},
			{name: "value_excess", corrupt: func(native *legacyNativeAnyMap) {
				row := native.ownership["middle"]
				row.valueExcess = row.valueBytes + 1
				native.ownership["middle"] = row
			}},
			{name: "canonical_key", corrupt: func(native *legacyNativeAnyMap) {
				row := native.ownership["middle"]
				native.link(row.linkID).canonicalKey = "wrong"
				native.ownership["middle"] = row
			}},
			{name: "same_size_decoded_mismatch", corrupt: func(native *legacyNativeAnyMap) {
				delete(native.decoded, "middle")
				native.decoded["wrong"] = nil
			}},
			{name: "stable_and_dirty_self_cycle", corrupt: func(native *legacyNativeAnyMap) {
				row := native.ownership[""]
				native.link(row.linkID).nextID = row.linkID
				native.link(row.linkID).dirtyNextID = row.linkID
				native.ownership[""] = row
			}},
		} {
			for _, full := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s_full_%t", fixture.name, full), func(test *testing.T) {
					native, _ := legacyNativeAnyMapCheckpointOracleSeed(test)
					fixture.corrupt(native)
					before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
					cursor, err := native.checkpoint(context.Background(), full)
					if cursor != nil {
						test.Cleanup(cursor.close)
					}
					for stepIndex := 0; err == nil && stepIndex <= len(native.decoded); stepIndex++ {
						if cursor == nil {
							test.Fatal("corrupt checkpoint returned neither a cursor nor an error")
						}
						var ok bool
						_, ok, err = cursor.next(context.Background())
						if !ok && err == nil {
							test.Fatal("corrupt chain reached a false verified EOF")
						}
					}
					if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(*native, before) {
						test.Fatal("corrupt checkpoint failed to reject finitely without owner mutation", err)
					}
				})
			}
		}
	})
	test.Run("malformed_tips_reject_all_puts_atomically", func(test *testing.T) {
		for _, fixture := range []struct {
			name    string
			corrupt func(*legacyNativeAnyMap)
		}{
			{name: "stable_tail_missing", corrupt: func(native *legacyNativeAnyMap) { native.lastID = ^uint32(0) }},
			{name: "stable_tail_has_next", corrupt: func(native *legacyNativeAnyMap) {
				row := native.ownership["last"]
				native.link(row.linkID).nextID = native.firstID
				native.ownership["last"] = row
			}},
			{name: "dirty_tail_missing", corrupt: func(native *legacyNativeAnyMap) { native.dirtyTailID = ^uint32(0) }},
			{name: "dirty_tail_has_next", corrupt: func(native *legacyNativeAnyMap) {
				row := native.ownership["last"]
				native.link(row.linkID).dirtyNextID = native.firstID
				native.ownership["last"] = row
			}},
		} {
			for _, key := range []string{"new", "middle", "last"} {
				test.Run(fixture.name+"_put_"+key, func(test *testing.T) {
					native, _ := legacyNativeAnyMapCheckpointOracleSeed(test)
					fixture.corrupt(native)
					before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
					if err := native.put(key, len(key), nil, docRangeHeaderHeapEstimate{}, 0); !errors.Is(err, ErrIntegrity) {
						test.Fatal("put accepted malformed stable/dirty tip metadata", err)
					}
					if !reflect.DeepEqual(*native, before) {
						test.Fatal("malformed-tip rejection changed values, links, revision, or accounting")
					}
				})
			}
		}
	})
	test.Run("revision_overflow_is_atomic", func(test *testing.T) {
		for _, key := range []string{"new", "last"} {
			native, _ := legacyNativeAnyMapCheckpointOracleSeed(test)
			native.revision = ^uint64(0)
			before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
			if err := native.put(key, len(key), nil, docRangeHeaderHeapEstimate{}, 0); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(*native, before) {
				test.Fatal("revision overflow was accepted or changed committed ownership", key, err)
			}
		}
	})
	test.Run("late_ack_corruption_does_not_clear_earlier_dirty_rows", func(test *testing.T) {
		native, expected := legacyNativeAnyMapCheckpointOracleSeed(test)
		cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, expected)
		row := native.ownership["middle"]
		native.link(row.linkID).dirtyNextID = 0
		native.ownership["middle"] = row
		before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
		if complete, err := cursor.acknowledge(context.Background(), 3); complete || !errors.Is(err, ErrIntegrity) {
			test.Fatal("acknowledgement accepted later malformed dirty linkage", complete, err)
		}
		if !reflect.DeepEqual(*native, before) {
			test.Fatal("failed acknowledgement irreversibly cleared an earlier dirty row")
		}
	})
}
