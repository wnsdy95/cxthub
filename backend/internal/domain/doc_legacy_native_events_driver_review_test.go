package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeEventsL09DriverPreservesEmptyBlockDepth(test *testing.T) {
	const outerLayers = 4997
	for _, sample := range []struct {
		name     string
		block    string
		accepted bool
	}{
		{name: "empty-object-exceeds-depth", block: `{}`},
		{name: "null-does-not-open-object", block: `null`, accepted: true},
	} {
		test.Run(sample.name, func(test *testing.T) {
			raw := `{"kind":"compaction","replacement":[{"kind":"message","blocks":[` + sample.block + `]}]}`
			rooted := strings.Repeat(`{"kind":"compaction","replacement":[`, outerLayers) + raw + strings.Repeat(`]}`, outerLayers)
			if json.Valid(legacyNativeEventDocument("2", rooted)) != sample.accepted {
				test.Fatal("unchanged Go document depth oracle disagrees with the boundary fixture")
			}
			var expected CIREvent
			if err := json.Unmarshal([]byte(raw), &expected); err != nil {
				test.Fatal("relative event oracle rejected the boundary fixture", err)
			}
			fixture := legacyNativeCanonicalOwnerStart(test, raw)
			fixture.native.object.parentDepth = 3 + 2*outerLayers
			for range 8 {
				event, complete, err := fixture.step(test, context.Background(), 0)
				if string(fixture.buffer.body) != raw {
					test.Fatal("depth review mutated the authenticated source")
				}
				if err != nil {
					if sample.accepted || complete || !errors.Is(err, ErrIntegrity) {
						test.Fatal("actual event driver rejected a valid boundary or lost its depth error", complete, err)
					}
					return
				}
				if complete {
					if !sample.accepted || !reflect.DeepEqual(event, expected) {
						test.Fatal("actual event driver accepted an over-depth empty block or changed null semantics")
					}
					return
				}
			}
			test.Fatal("depth review exhausted its finite driver allowance")
		})
	}
}

func TestLegacyNativeEventsL09DriverBlockCallQuota(test *testing.T) {
	blockLimit := LegacyNormalizationStepEvents * len(legacyContentBlockNativeFields.fields)
	if blockLimit != 128 {
		test.Fatal("original block action limit changed", blockLimit)
	}
	raw := `{"kind":"compaction","replacement":[{"kind":"message","role":"user","blocks":[{"type":"text","text":""},{"type":"text",` +
		strings.Repeat(`"text":"",`, blockLimit) + `"text":""}]}]}`
	var expected CIREvent
	if err := json.Unmarshal([]byte(raw), &expected); err != nil {
		test.Fatal("original typed oracle rejected the duplicate-member fixture", err)
	}
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	rootLimit := fixture.native.object.fieldActionLimit()
	if rootLimit <= 2*blockLimit {
		test.Fatal("review fixture no longer has a wider root action allowance", rootLimit)
	}
	_, complete, err := fixture.step(test, context.Background(), rootLimit-5)
	if err != nil || complete || !fixture.native.object.eventActive {
		test.Fatal("actual driver did not park at the first block member boundary", complete, err)
	}
	object := fixture.native.object
	if object.field < 0 || object.field >= len(object.events) || object.events[object.field] == nil {
		test.Fatal("parked source has no actual event-list driver")
	}
	root := object.events[object.field]
	cursor := root.cursor
	if cursor == nil || cursor.object == nil || !cursor.object.blockActive {
		test.Fatal("actual event cursor did not retain its active block decoder")
	}
	object = cursor.object
	if object.field < 0 || object.field >= len(object.blocks) || object.blocks[object.field] == nil {
		test.Fatal("active event child lost its actual block list")
	}
	blocks := object.blocks[object.field]
	if blocks.count != 0 || blocks.object == nil || blocks.object.phase != legacyNativeObjectAfter || blocks.block.Type != "text" {
		test.Fatal("quota fixture did not stop immediately before the first block commit")
	}
	ctx := context.Background()
	input := &legacyNormalizationInput{
		ctx: ctx, source: fixture.source, eventsRunning: cursor, eventsFieldLimit: rootLimit,
	}
	complete, err = blocks.step(ctx, input)
	if err != nil || input.failure != nil || complete || input.fields <= 0 || input.fields > blockLimit || input.eventsPending != nil ||
		input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded {
		test.Fatal("one real block call escaped its original action quota inside the shared root quantum", complete, err, input.failure, input.fields, blockLimit, input.workBytes, input.loads)
	}
	workBytes, fields, loads := input.workBytes, input.fields, input.loads
	input.eventsRunning, input.eventsFieldLimit = nil, 0
	actual, complete, err := fixture.native.step(ctx, input, &fixture.frame)
	if err != nil || input.failure != nil || !complete || input.fields <= fields || input.fields > rootLimit ||
		input.workBytes < workBytes || input.workBytes > ConversationManifestChunkBytes || input.loads < loads || input.loads > 2 || input.unbounded ||
		input.eventsRunning != nil || input.eventsPending != nil {
		test.Fatal("local block yield did not re-enter within the same unreset root quantum", complete, err, input.failure, fields, input.fields, workBytes, input.workBytes, loads, input.loads)
	}
	if !reflect.DeepEqual(actual, expected) || string(fixture.buffer.body) != raw {
		test.Fatal("same-quantum block re-entry changed typed values or authenticated source bytes")
	}
}
