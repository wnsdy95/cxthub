package domain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyNativeGoGraphOwnerWireCompatibility(test *testing.T) {
	for _, body := range []string{
		`{"v":1,"i":7,"k":4,"n":1,"c":3,"m":3,"r":1,"e":{"r":{"h":"","n":0},"n":0,"h":0},"d":{"r":{"h":"","n":0},"n":0,"h":0}}`,
		`{"v":1,"i":7,"k":4,"n":3,"c":3,"m":3,"r":1,"e":{"r":{"h":"","n":0},"n":0,"h":0},"d":{"r":{"h":"","n":0},"n":0,"h":0},"f":true,"l":1}`,
		`{"v":1,"i":7,"k":4,"n":1,"c":3,"m":3,"r":1,"e":{"r":{"h":"","n":0},"n":0,"h":0},"d":{"r":{"h":"","n":0},"n":0,"h":0},"a":{"b":{"v":null,"b":{"h":"","n":0},"t":{"n":0,"t":{"r":{"h":"","n":0},"n":0,"h":0}}},"o":2}}`,
	} {
		storage := legacyNativeGoGraphSetW85Storage(test)
		ctx := context.Background()
		storage.current = ctx
		ref, err := legacyNativeCheckpointWrite(ctx, storage.io, []byte(body))
		if err != nil {
			test.Fatal(err)
		}
		var record legacyNativeCheckpointOwner
		if err := legacyNativeCheckpointReadJSON(ctx, storage.io, ref, &record); err != nil {
			test.Fatal("existing canonical owner bytes were rejected", err)
		}
		encoded, err := json.Marshal(record)
		if err != nil || string(encoded) != body {
			test.Fatal("owner packing changed the existing canonical wire order", string(encoded), err)
		}
		reordered := strings.Replace(body, `"v":1,"i":7,"k":4`, `"v":1,"k":4,"i":7`, 1)
		ref, err = legacyNativeCheckpointWrite(ctx, storage.io, []byte(reordered))
		if err != nil {
			test.Fatal(err)
		}
		if err := legacyNativeCheckpointReadJSON(ctx, storage.io, ref, &record); !errors.Is(err, ErrIntegrity) {
			test.Fatal("owner packing accepted noncanonical key order", err)
		}
	}
}

func TestLegacyNativeGoGraphLayoutSortQuantum(test *testing.T) {
	values := make([]any, 256)
	for index := range values {
		values[index] = []any{float64(index)}
	}
	storage := legacyNativeGoGraphSetW85Storage(test)
	work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{values}, HashContent([]byte(test.Name())), storage.io, legacyNativeGoGraphSetW85Budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	partialSort := false
	for attempt := 0; attempt < legacyNativeGoGraphSetW85Steps && (work.layout == nil || !work.layout.ready); attempt++ {
		var before uint64
		var visits int64
		if work.layout != nil {
			before = work.layout.order.work
			visits = work.layout.visits
		}
		if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, work, storage); err != nil || ready {
			test.Fatal("layout could not sort within its bounded pre-import schedule", ready, err)
		}
		if work.layout != nil && !work.layout.ready {
			if work.layout.visits-visits > legacyNativeCheckpointPageItems {
				test.Fatal("layout exceeded the existing page-sized discovery quantum", visits, work.layout.visits)
			}
			if work.layout.order.work < before || work.layout.order.work-before > legacyNativeCheckpointPageItems {
				test.Fatal("layout exceeded the existing page-sized sort action quantum", before, work.layout.order.work)
			}
			partialSort = partialSort || work.layout.phase == 1 && work.layout.order.work > 0
		}
	}
	if work.layout == nil || !work.layout.ready || !partialSort || work.registry.entries != 0 || storage.reads != 0 || storage.writes != 0 {
		test.Fatal("layout did not yield during sorting before native import and IO")
	}
}

func TestLegacyNativeGoGraphLayoutDiscoveryLifetime(test *testing.T) {
	for _, overlap := range []bool{false, true} {
		name := "disjoint"
		if overlap {
			name = "overlap"
		}
		test.Run(name, func(test *testing.T) {
			backing := []any{true, nil, "tail"}
			second := []any{false, nil}
			if overlap {
				second = backing[1:2:3]
			}
			disjoint := make([]any, 256)
			for index := range disjoint {
				disjoint[index] = map[string]any{"items": []any{float64(index)}}
			}
			roots := []any{map[string]any{"first": backing[:1:3], "second": second, "disjoint": disjoint}, second}
			storage := legacyNativeGoGraphSetW85Storage(test)
			binding := HashContent([]byte(test.Name()))
			work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			for attempt := 0; attempt < legacyNativeGoGraphSetW85Steps && (work.layout == nil || !work.layout.ready); attempt++ {
				if ready, err := legacyNativeGoGraphSetW85Step[legacyNativeCheckpointRef](test, work, storage); err != nil || ready {
					test.Fatal("layout did not reach its bounded pre-import boundary", ready, err)
				}
			}
			layout := work.layout
			if layout == nil || !layout.ready || work.prepare == nil || work.registry.entries != 0 || storage.reads != 0 || storage.writes != 0 {
				test.Fatal("prepass boundary was not reached before import and IO")
			}
			retained, err := layout.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			var empty docRangeHeaderHeapEstimate
			empty.allocation(1, int64(unsafe.Sizeof(*layout)))
			if layout.root != nil || layout.roots != nil || layout.frame != nil || layout.current != nil || layout.nodes != 0 || layout.visits != 0 || layout.depth != 0 || layout.views.length() != 0 || layout.backings.length() != 0 {
				test.Fatal("completed discovery retained temporary map or view indexes")
			}
			if overlap {
				if layout.groups.length() != 1 || layout.allocations == 0 || retained <= empty.bytes {
					test.Fatal("required overlap backing was released before import")
				}
				var expected = empty
				layout.groups.retained(&expected)
				expected.allocation(1, int64(unsafe.Sizeof(legacyNativeGoLayoutGroup{})))
				expected.include(legacyNativeAnyArrayBacking(3))
				if expected.err != nil || retained != expected.bytes {
					test.Fatal("overlap retained discovery-only metadata or omitted the required backing", retained, expected)
				}
			} else if layout.allocations != 0 || layout.groups.length() != 0 || retained != empty.bytes {
				test.Fatal("completed disjoint discovery retained its temporary graph index", retained, empty.bytes)
			}
			legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointRef](test, work, storage, legacyNativeGoGraphSetW85Budget)
			var owned docRangeHeaderHeapEstimate
			for entry := work.registry.first; entry != nil; entry = entry.next {
				owned.include(docRangeHeaderHeapEstimate{bytes: entry.owned})
			}
			for index := 0; index < layout.groups.length(); index++ {
				group := *layout.groups.at(index)
				if group.owner == nil || group.entry == nil {
					test.Fatal("required backing did not transfer to the production owner")
				}
				owned.include(group.owner.metadataHeap())
			}
			if owned.err != nil || work.registry.owned != owned.bytes {
				test.Fatal("shared resource metadata was omitted or charged more than once", work.registry.owned, owned)
			}
			saved, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
			legacyNativeGoGraphSetW85Values(test, roots, actual)
			first := actual[0].value.(map[string]any)["first"].([]any)
			if overlap && &first[:3][1] != &actual[1].value.([]any)[0] {
				test.Fatal("retaining the required layout lost shared physical slots")
			}
		})
	}
}
