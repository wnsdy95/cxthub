package domain

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeEventsHeapReviewObserver struct {
	context.Context
	native     *legacyNormalizationNativeEvent
	field      int
	capacities map[*byte]int
}

func (observer *legacyNativeEventsHeapReviewObserver) observe(scalar *legacyNativeScalar) {
	if scalar != nil && scalar.text != nil && scalar.text.Len() > 0 {
		observer.capacities[unsafe.StringData(scalar.text.String())] = scalar.text.Cap()
	}
}

func (observer *legacyNativeEventsHeapReviewObserver) Err() error {
	if err := observer.Context.Err(); err != nil {
		return err
	}
	if observer.field < len(observer.native.object.events) {
		list := observer.native.object.events[observer.field]
		if list != nil && list.object != nil {
			observer.observe(list.object.scalar)
			for _, parser := range list.object.containers {
				if parser != nil {
					observer.observe(parser.scalar)
				}
			}
			for _, blocks := range list.object.blocks {
				if blocks != nil && blocks.object != nil {
					observer.observe(blocks.object.scalar)
				}
			}
		}
	}
	return observer.Context.Err()
}

func (observer *legacyNativeEventsHeapReviewObserver) capacity(test *testing.T, text string) int {
	test.Helper()
	capacity, found := observer.capacities[unsafe.StringData(text)]
	if !found || capacity <= len(text) {
		test.Fatal("live child string lacks an independently observed spare builder backing", len(text), capacity, found)
	}
	return capacity
}

func legacyNativeEventsHeapReviewAllocation(count, width int) int64 {
	if count == 0 || width == 0 {
		return 0
	}
	return 64 + 2*int64(count)*int64(width)
}

func TestLegacyNativeEventsHeapReviewBuilderCapacityAndBoxedPayload(test *testing.T) {
	largeText := strings.Repeat("v", (64<<10)+129)
	for _, fixture := range []struct {
		name  string
		child string
	}{
		{name: "boxed-map", child: `{"kind":"tool_result","output":{"payload":"kept"}}`},
		{name: "large-block", child: `{"kind":"message","role":"user","blocks":[{"type":"text","text":` + strconv.Quote(largeText) + `}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := `{"kind":"compaction","replacement":[` + fixture.child + `]}`
			decoded := legacyNativeCanonicalOwnerStart(test, raw)
			field := legacyEventNativeFields.match("replacement")
			if field < 0 {
				test.Fatal("replacement metadata is missing")
			}
			observer := &legacyNativeEventsHeapReviewObserver{
				Context: context.Background(), native: decoded.native, field: field, capacities: make(map[*byte]int),
			}
			var event CIREvent
			complete := false
			for step := 0; step < 128; step++ {
				var err error
				event, complete, err = decoded.step(test, observer, 0)
				if err != nil {
					test.Fatal("actual native child decode failed", step, err)
				}
				if complete {
					break
				}
			}
			if !complete || len(event.Replacement) != 1 || field >= len(decoded.native.object.events) {
				test.Fatal("actual event list did not complete within its finite allowance")
			}
			list := decoded.native.object.events[field]
			if list == nil || list.records.length() != 1 {
				test.Fatal("actual child did not use the native event-list record")
			}
			allocation := legacyNativeEventsHeapReviewAllocation
			child := event.Replacement[0]
			var payload, excess int64
			switch fixture.name {
			case "boxed-map":
				output, valid := child.Output.(map[string]any)
				if !valid || child.Kind != EventToolResult || len(output) != 1 || output["payload"] != "kept" {
					test.Fatal("boxed payload fixture changed its typed value")
				}
				for key, value := range output {
					text := value.(string)
					keyCapacity, textCapacity := observer.capacity(test, key), observer.capacity(test, text)
					payload = allocation(len("tool_result"), 1) + allocation(1, int(unsafe.Sizeof(output))) +
						allocation(1, 64) + allocation(8, int(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))+8) +
						allocation(keyCapacity, 1) + allocation(1, int(unsafe.Sizeof(text))) + allocation(textCapacity, 1)
					excess = 2 * int64(keyCapacity-len(key)+textCapacity-len(text))
				}
			case "large-block":
				if child.Kind != EventMessage || child.Role != RoleUser || len(child.Blocks) != 1 ||
					child.Blocks[0].Type != "text" || child.Blocks[0].Text != largeText {
					test.Fatal("large block fixture changed its typed value")
				}
				capacity := observer.capacity(test, child.Blocks[0].Text)
				payload = allocation(len("message"), 1) + allocation(len("user"), 1) +
					allocation(cap(child.Blocks), int(unsafe.Sizeof(ContentBlock{}))) + allocation(len("text"), 1) + allocation(capacity, 1)
				excess = 2 * int64(capacity-len(largeText))
			}
			if record := list.records.at(0); record.heap.err != nil || record.heap.bytes != payload || record.excess != excess ||
				list.payload.err != nil || list.payload.bytes != payload || list.excess != excess {
				test.Fatal("child commit lost builder capacity, removed a boxed value, or retained inline field storage", payload, excess, *record, list.payload, list.excess)
			}
			wantList := allocation(1, int(unsafe.Sizeof(event.Replacement))) +
				allocation(cap(event.Replacement), int(unsafe.Sizeof(CIREvent{}))) + payload
			if list.estimate.err != nil || list.estimate.bytes != wantList {
				test.Fatal("event backing and child payload are not charged exactly once", list.estimate, wantList)
			}
			wantOwner := allocation(1, int(unsafe.Sizeof(event.Kind))) + allocation(len("compaction"), 1) + wantList
			owner, err := decoded.native.takeCanonicalOwner(context.Background())
			if err != nil || owner == nil {
				test.Fatal("measured child owner handoff failed", err)
			}
			defer owner.release()
			if owner.heap.err != nil || owner.heap.bytes != wantOwner || !reflect.ValueOf(decoded.native.decoded).IsZero() ||
				decoded.native.object.events != nil || unsafe.SliceData(owner.event.Replacement) != unsafe.SliceData(event.Replacement) {
				test.Fatal("owner handoff lost independently measured child backing or retained cache aliases", owner.heap, wantOwner)
			}
			test.Logf("payload=%d observed builder excess=%d owner=%d", payload, excess, wantOwner)
		})
	}
}
