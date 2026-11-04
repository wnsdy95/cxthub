package domain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeAnyMapIDsOracleKey(position int) string {
	if position == 0 {
		return ""
	}
	return fmt.Sprintf("id-%04d", position)
}

func legacyNativeAnyMapIDsOracleSeed(test *testing.T, count int) (*legacyNativeAnyMap, []legacyNativeAnyMapCheckpointEntry) {
	test.Helper()
	native := newLegacyNativeAnyMap()
	entries := make([]legacyNativeAnyMapCheckpointEntry, 0, count)
	for position := 0; position < count; position++ {
		key := legacyNativeAnyMapIDsOracleKey(position)
		entries = append(entries, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), nil, 0))
	}
	return native, entries
}

func legacyNativeAnyMapIDsOracleChains(test *testing.T, native *legacyNativeAnyMap, entries []legacyNativeAnyMapCheckpointEntry, dirtyIDs []uint32) {
	test.Helper()
	firstID, lastID := uint32(0), uint32(0)
	if len(entries) != 0 {
		firstID, lastID = 1, uint32(len(entries))
	}
	dirtyHeadID, dirtyTailID := uint32(0), uint32(0)
	dirtyNext := make(map[uint32]uint32, len(dirtyIDs))
	for position, identity := range dirtyIDs {
		if identity == 0 || identity > lastID {
			test.Fatal("invalid expected dirty identity", identity, lastID)
		}
		dirtyNext[identity] = 0
		if position+1 < len(dirtyIDs) {
			dirtyNext[identity] = dirtyIDs[position+1]
		}
	}
	if len(dirtyIDs) != 0 {
		dirtyHeadID, dirtyTailID = dirtyIDs[0], dirtyIDs[len(dirtyIDs)-1]
	}
	if !native.valid() || native.links.length() != len(entries) || len(native.ownership) != len(entries) ||
		native.firstID != firstID || native.lastID != lastID || native.dirtyHeadID != dirtyHeadID || native.dirtyTailID != dirtyTailID ||
		native.dirtyCount != len(dirtyIDs) || len(dirtyNext) != len(dirtyIDs) {
		test.Fatal("promotion or acknowledgement changed stable/dirty ID roots", native.firstID, native.lastID, native.dirtyHeadID, native.dirtyTailID, native.dirtyCount)
	}
	for position, entry := range entries {
		identity := uint32(position + 1)
		nextID := identity + 1
		if identity == lastID {
			nextID = 0
		}
		row, found := native.ownership[entry.Key]
		node := native.link(identity)
		expectedDirtyNext, dirty := dirtyNext[identity]
		if !found || row.linkID != identity || node == nil || node.canonicalKey != entry.Key || node.nextID != nextID ||
			node.dirty != dirty || node.dirtyNextID != expectedDirtyNext {
			test.Fatal("identity changed owner, canonical key, or stable/dirty successor", position, row, node, nextID, dirty, expectedDirtyNext)
		}
	}
	if native.link(0) != nil || native.link(lastID+1) != nil || native.link(^uint32(0)) != nil {
		test.Fatal("zero or out-of-range identity resolved to a real key")
	}
}

func TestLegacyNativeAnyMapIDsStableAcrossPromotionsAndPartialACK(test *testing.T) {
	for _, count := range []int{1, 2, 8, 9, 64, 65, 4096, 4097} {
		test.Run(fmt.Sprintf("entries_%d", count), func(test *testing.T) {
			native, entries := legacyNativeAnyMapIDsOracleSeed(test, count-1)
			staleRead := legacyNativeAnyMapCheckpointOracleOpen(test, native, true)
			staleACK := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, entries)
			key := legacyNativeAnyMapIDsOracleKey(count - 1)
			entries = append(entries, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), nil, 0))
			dirtyIDs := make([]uint32, count)
			for position := range dirtyIDs {
				dirtyIDs[position] = uint32(position + 1)
			}
			legacyNativeAnyMapIDsOracleChains(test, native, entries, dirtyIDs)
			before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
			legacyNativeAnyMapCheckpointOracleNextError(test, staleRead, context.Background(), ErrConflict)
			if complete, err := staleACK.acknowledge(context.Background(), 1); complete || !errors.Is(err, ErrConflict) || !reflect.DeepEqual(*native, before) {
				test.Fatal("promotion let a stale ID cursor acknowledge or mutate the owner", complete, err)
			}
			storageBefore, revisionBefore := native.links, native.revision
			entries[0] = legacyNativeAnyMapCheckpointOraclePut(test, native, strings.Clone(entries[0].Key), 16, "empty replacement", 0)
			entries[count-1] = legacyNativeAnyMapCheckpointOraclePut(test, native, strings.Clone(entries[count-1].Key), max(16, len(entries[count-1].Key)), true, 0)
			if native.links != storageBefore || native.revision != revisionBefore+2 {
				test.Fatal("duplicate or empty key appended storage or lost its mutation version")
			}
			legacyNativeAnyMapIDsOracleChains(test, native, entries, dirtyIDs)
			legacyNativeAnyMapCheckpointOracleCheck(test, native, true, entries)
			cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, entries)
			legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, 1, count == 1)
			dirtyIDs = dirtyIDs[1:]
			legacyNativeAnyMapIDsOracleChains(test, native, entries, dirtyIDs)
			entries[0] = legacyNativeAnyMapCheckpointOraclePut(test, native, "", 0, nil, 0)
			dirtyIDs = append(dirtyIDs, 1)
			key = fmt.Sprintf("after-%d", count)
			entries = append(entries, legacyNativeAnyMapCheckpointOraclePut(test, native, key, len(key), false, 0))
			dirtyIDs = append(dirtyIDs, uint32(len(entries)))
			legacyNativeAnyMapIDsOracleChains(test, native, entries, dirtyIDs)
			delta := make([]legacyNativeAnyMapCheckpointEntry, 0, len(dirtyIDs))
			for _, identity := range dirtyIDs {
				delta = append(delta, entries[int(identity)-1])
			}
			cursor = legacyNativeAnyMapCheckpointOracleCheck(test, native, false, delta)
			for len(dirtyIDs) != 0 {
				acknowledged := min(1024, len(dirtyIDs))
				legacyNativeAnyMapCheckpointOracleAcknowledge(test, cursor, acknowledged, acknowledged == len(dirtyIDs))
				dirtyIDs = dirtyIDs[acknowledged:]
				legacyNativeAnyMapIDsOracleChains(test, native, entries, dirtyIDs)
			}
			legacyNativeAnyMapCheckpointOracleCheck(test, native, false, nil)
			legacyNativeAnyMapCheckpointOracleCheck(test, native, true, entries)
			legacyNativeAnyMapCheckpointOracleAccounting(test, native, entries)
		})
	}
}

func TestLegacyNativeAnyMapIDsRejectCorruptionAndFailedGrowth(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		limit   int
		corrupt func(*legacyNativeAnyMap)
	}{
		{name: "zero_owned_id", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			row := native.ownership[legacyNativeAnyMapIDsOracleKey(1)]
			row.linkID = 0
			native.ownership[legacyNativeAnyMapIDsOracleKey(1)] = row
		}},
		{name: "out_of_range_owned_id", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			row := native.ownership[legacyNativeAnyMapIDsOracleKey(1)]
			row.linkID = ^uint32(0)
			native.ownership[legacyNativeAnyMapIDsOracleKey(1)] = row
		}},
		{name: "aliased_owned_id", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			row := native.ownership[legacyNativeAnyMapIDsOracleKey(1)]
			row.linkID = 1
			native.ownership[legacyNativeAnyMapIDsOracleKey(1)] = row
		}},
		{name: "aliased_canonical_key", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			native.link(2).canonicalKey = ""
		}},
		{name: "early_zero_successor", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			native.link(1).nextID, native.link(1).dirtyNextID = 0, 0
		}},
		{name: "past_end_successor", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			identity := uint32(native.links.length() + 1)
			native.link(1).nextID, native.link(1).dirtyNextID = identity, identity
		}},
		{name: "maximum_successor", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			native.link(1).nextID, native.link(1).dirtyNextID = ^uint32(0), ^uint32(0)
		}},
		{name: "self_cycle_survivor", limit: 1, corrupt: func(native *legacyNativeAnyMap) {
			native.link(1).nextID, native.link(1).dirtyNextID = 1, 1
		}},
		{name: "cycle_inside_batch", limit: 3, corrupt: func(native *legacyNativeAnyMap) {
			native.link(2).nextID, native.link(2).dirtyNextID = 1, 1
		}},
		{name: "cycle_at_survivor", limit: 2, corrupt: func(native *legacyNativeAnyMap) {
			native.link(2).nextID, native.link(2).dirtyNextID = 1, 1
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, full := range []bool{false, true} {
				native, _ := legacyNativeAnyMapIDsOracleSeed(test, 9)
				fixture.corrupt(native)
				before, storageBefore := legacyNativeAnyMapCheckpointOracleSnapshot(native), native.links
				cursor, err := native.checkpoint(context.Background(), full)
				if cursor != nil {
					test.Cleanup(cursor.close)
				}
				for stepIndex := 0; err == nil && stepIndex <= len(native.ownership); stepIndex++ {
					if cursor == nil {
						test.Fatal("corrupt checkpoint returned neither a cursor nor an error")
					}
					var present bool
					_, present, err = cursor.next(context.Background())
					if !present && err == nil {
						test.Fatal("malformed ID chain reached a false verified EOF", full)
					}
				}
				if !errors.Is(err, ErrIntegrity) || native.links != storageBefore || !reflect.DeepEqual(*native, before) {
					test.Fatal("malformed ID chain did not reject finitely and without mutation", full, err)
				}
			}
			native, entries := legacyNativeAnyMapIDsOracleSeed(test, 9)
			cursor := legacyNativeAnyMapCheckpointOracleCheck(test, native, false, entries)
			fixture.corrupt(native)
			before, storageBefore := legacyNativeAnyMapCheckpointOracleSnapshot(native), native.links
			if complete, err := cursor.acknowledge(context.Background(), fixture.limit); complete || !errors.Is(err, ErrIntegrity) ||
				native.links != storageBefore || !reflect.DeepEqual(*native, before) {
				test.Fatal("bounded ID preflight cleared a batch before rejecting its member or surviving head", complete, err)
			}
		})
	}
	for _, count := range []int{1, 8, 64, 4096} {
		test.Run(fmt.Sprintf("future_heap_overflow_%d", count), func(test *testing.T) {
			native, entries := legacyNativeAnyMapIDsOracleSeed(test, count)
			cursor := legacyNativeAnyMapCheckpointOracleOpen(test, native, true)
			var currentLinks, keyHeap docRangeHeaderHeapEstimate
			native.links.retained(&currentLinks)
			storage, futureLinks, err := native.links.appendRequirements()
			if err != nil || currentLinks.err != nil || futureLinks.err != nil || storage <= 0 || futureLinks.bytes <= currentLinks.bytes {
				test.Fatal("fixture did not target a real paged storage promotion", count, storage, currentLinks, futureLinks, err)
			}
			key := "next"
			keyHeap.allocation(int64(len(key)), 1)
			backing := legacyNativeAnyMapBacking(count+1, int64(unsafe.Sizeof(any(nil))))
			metadata := legacyNativeAnyMapMetadata(count+1, currentLinks)
			if keyHeap.err != nil || backing.err != nil || metadata.err != nil {
				test.Fatal("overflow fixture could not measure current physical storage")
			}
			childHeap := docRangeHeaderHeapEstimate{bytes: math.MaxInt64 - native.payload.bytes - keyHeap.bytes - backing.bytes - metadata.bytes}
			before, storageBefore := legacyNativeAnyMapCheckpointOracleSnapshot(native), native.links
			if err := native.put(key, len(key), nil, childHeap, 0); !errors.Is(err, ErrIntegrity) ||
				native.links != storageBefore || !reflect.DeepEqual(*native, before) {
				test.Fatal("future physical heap overflow grew backing or changed an owned ID", count, err)
			}
			legacyNativeAnyMapCheckpointOracleDrain(test, cursor, entries)
			legacyNativeAnyMapCheckpointOracleAccounting(test, native, entries)
		})
	}
	test.Run("append_plan_overflow", func(test *testing.T) {
		native, _ := legacyNativeAnyMapIDsOracleSeed(test, 64)
		before := legacyNativeAnyMapCheckpointOracleSnapshot(native)
		native.links.nodes, before.links.nodes = int(^uint(0)>>1), int(^uint(0)>>1)
		storageBefore := native.links
		if _, _, err := native.links.appendRequirements(); !errors.Is(err, ErrIntegrity) {
			test.Fatal("append forecast accepted overflowing node accounting", err)
		}
		if _, err := native.putBytes("next"); !errors.Is(err, ErrIntegrity) {
			test.Fatal("map preview concealed failed shared append requirements", err)
		}
		if err := native.put("next", 4, nil, docRangeHeaderHeapEstimate{}, 0); !errors.Is(err, ErrIntegrity) ||
			native.links != storageBefore || !reflect.DeepEqual(*native, before) {
			test.Fatal("failed shared append changed backing, IDs, or map ownership", err)
		}
	})
}

func TestLegacyNativeAnyMapIDsPutPreviewAndParserBudget(test *testing.T) {
	for _, count := range []int{0, 1, 7, 8, 63, 64, 4095, 4096} {
		test.Run(fmt.Sprintf("before_entries_%d", count), func(test *testing.T) {
			native, entries := legacyNativeAnyMapIDsOracleSeed(test, count)
			storage, futureHeap, err := native.links.appendRequirements()
			if err != nil || futureHeap.err != nil {
				test.Fatal("shared storage forecast failed", err, futureHeap)
			}
			before, storageBefore := legacyNativeAnyMapCheckpointOracleSnapshot(native), native.links
			var work, duplicateWork int
			var previewError, duplicateError error
			allocations := testing.AllocsPerRun(32, func() {
				work, previewError = native.putBytes("next")
				if count != 0 {
					duplicateWork, duplicateError = native.putBytes("")
				}
			})
			if allocations != 0 || previewError != nil || duplicateError != nil || work != legacyNativeAnyMapPutBytes+storage ||
				count != 0 && duplicateWork != legacyNativeAnyMapPutBytes || storage < 0 || storage > LegacyCanonicalEncodingBytes || work > ConversationManifestChunkBytes ||
				native.links != storageBefore || !reflect.DeepEqual(*native, before) {
				test.Fatal("put preview allocated, mutated, or omitted shared paged allocation/copy work", count, allocations, work, storage, duplicateWork, previewError, duplicateError)
			}
			if storage == 0 {
				entries = append(entries, legacyNativeAnyMapCheckpointOraclePut(test, native, "next", 4, nil, 0))
			} else {
				parser := &legacyNativeAny{rootMap: native, finished: true}
				raw := `{"next":null}`
				if err := parser.start(0, len(raw), 0, 64); err != nil {
					test.Fatal("parser could not reuse the boundary map", err)
				}
				buffer := &legacySourceProofBuffer{body: []byte(raw)}
				source := legacyNormalizationPreparationInput(test, buffer, true)
				var frame legacyJSONValueScanner
				keyInput := &legacyNormalizationInput{ctx: context.Background(), source: source, values: legacyNativeAnyValueActions - 1}
				if complete, err := parser.step(context.Background(), keyInput, &frame); complete || err != nil || keyInput.failure != nil ||
					parser.frame == nil || parser.frame.object != native || parser.frame.phase != legacyNativeAnyColon || parser.frame.key != "next" ||
					parser.pending || keyInput.values != legacyNativeAnyValueActions || keyInput.workBytes > ConversationManifestChunkBytes || keyInput.loads > 2 {
					test.Fatal("actual parser did not stop after decoding its real key", complete, err)
				}
				valueInput := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - legacyNativeAnyMapPutBytes}
				if complete, err := parser.step(context.Background(), valueInput, &frame); complete || err != nil || valueInput.failure != nil ||
					!parser.pending || parser.keyToken || parser.frame.key != "next" || valueInput.values != 0 || valueInput.workBytes > ConversationManifestChunkBytes || valueInput.loads > 2 ||
					native.links != storageBefore || !reflect.DeepEqual(*native, before) {
					test.Fatal("actual parser grew ID storage before its shared allocation/copy budget was available", complete, err)
				}
				parserBefore, frameBefore := *parser, *parser.frame
				for _, remaining := range []int{1, legacyNativeAnyMapPutBytes, work - 1} {
					input := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - remaining}
					if complete, err := parser.step(context.Background(), input, &frame); complete || err != nil || input.failure != nil ||
						input.workBytes != ConversationManifestChunkBytes-remaining || input.values != 0 || input.fields != 0 || input.loads != 0 || input.semanticBytes != 0 || input.unbounded ||
						!reflect.DeepEqual(*parser, parserBefore) || !reflect.DeepEqual(*parser.frame, frameBefore) || native.links != storageBefore || !reflect.DeepEqual(*native, before) {
						test.Fatal("pending map commit mutated or grew backing while its remaining budget was too small", remaining, complete, err)
					}
				}
				canceledContext, cancel := context.WithCancel(context.Background())
				cancel()
				canceledInput := &legacyNormalizationInput{ctx: canceledContext, source: source}
				if complete, err := parser.step(canceledContext, canceledInput, &frame); complete || !errors.Is(err, context.Canceled) || canceledInput.workBytes != 0 ||
					!reflect.DeepEqual(*parser, parserBefore) || !reflect.DeepEqual(*parser.frame, frameBefore) || native.links != storageBefore || !reflect.DeepEqual(*native, before) {
					test.Fatal("canceled parser committed a pending paged ID append", complete, err)
				}
				readyInput := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - work}
				if complete, err := parser.step(context.Background(), readyInput, &frame); complete || err != nil || readyInput.failure != nil || parser.pending ||
					parser.frame.phase != legacyNativeAnyAfter || readyInput.workBytes != ConversationManifestChunkBytes || readyInput.values != 1 || readyInput.fields != 0 ||
					readyInput.loads != 0 || readyInput.semanticBytes != 0 || readyInput.unbounded || native.links.length() != count+1 {
					test.Fatal("actual map commit did not consume exactly the previewed shared work", complete, err, readyInput.workBytes, work)
				}
				finishInput := &legacyNormalizationInput{ctx: context.Background(), source: source}
				if complete, err := parser.step(context.Background(), finishInput, &frame); !complete || err != nil || finishInput.failure != nil || !parser.finished ||
					parser.rootMap != native || parser.position != len(raw) || !reflect.DeepEqual(parser.result.value, native.values()) {
					test.Fatal("budget-deferred parser could not finish with its actual owned map", complete, err)
				}
				entries = append(entries, legacyNativeAnyMapCheckpointOracleParsedEntries(test, native, []string{"next"}, map[string]any{"next": nil})...)
			}
			owned, err := native.links.ownedBytes()
			if err != nil || owned != futureHeap.bytes {
				test.Fatal("actual committed links disagree with previewed post-append physical heap", owned, futureHeap, err)
			}
			dirtyIDs := make([]uint32, len(entries))
			for position := range dirtyIDs {
				dirtyIDs[position] = uint32(position + 1)
			}
			legacyNativeAnyMapIDsOracleChains(test, native, entries, dirtyIDs)
			legacyNativeAnyMapCheckpointOracleCheck(test, native, true, entries)
			legacyNativeAnyMapCheckpointOracleCheck(test, native, false, entries)
			legacyNativeAnyMapCheckpointOracleAccounting(test, native, entries)
		})
	}
}
