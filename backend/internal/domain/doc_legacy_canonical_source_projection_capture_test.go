package domain

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

func legacyCanonicalProjectionCaptureCheckpoint(test *testing.T, stage *LegacyNormalizationPreparation, source *legacySourceProofBuffer) *legacyCanonicalSourceCheckpoint {
	test.Helper()
	retained, err := stage.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(grant.release)
	var metadata docRangeHeaderHeapEstimate
	metadata.allocation(1, int64(unsafe.Sizeof(*grant)))
	budget := legacyDecoderCheckpointBudget - retained - metadata.bytes
	if metadata.err != nil || budget <= 0 {
		test.Fatal("projection capture has no headroom within the original aggregate budget", retained, metadata)
	}
	store, _ := legacyNativeCheckpointStoreFixture(test)
	work, err := newLegacyCanonicalSourceCheckpoint(context.Background(), stage.pending[0].Encoding, stage.mutation,
		HashContent(source.body), len(source.body), store, budget)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(work.close)
	return work
}

func legacyCanonicalProjectionCaptureStage(test *testing.T) (*LegacyNormalizationPreparation, *legacySourceProofBuffer) {
	test.Helper()
	message := `{"kind":"message","seq":7,"blocks":[{"type":"text","text":"same"}],"provider_metadata":{"turn_id":"same"}}`
	event := `{"kind":"compaction","seq":7,"provider_metadata":{},"replacement_complete":true,"replacement":[` + message + `,` + message + `,` +
		`{"kind":"reasoning","seq":7,"locked":{"provider":"codex","scheme":"opaque","blob":"same"}},` +
		`{"kind":"tool_call","seq":7,"input":null},` +
		`{"kind":"tool_call","seq":7,"input":{}},` +
		`{"kind":"tool_result","seq":7,"output":{"kind":"turn","seq":7}},` +
		`{"kind":"compaction","seq":2,"replacement_complete":true,"replacement":[` +
		`{"kind":"tool_call","seq":7,"input":null},` +
		`{"kind":"reasoning","seq":7,"locked":{"provider":"codex","scheme":"opaque","blob":"same"}}]},` +
		`{"kind":"tool_call","seq":99,"input":{"items":[` + strings.Repeat("null,", 5000) + `null]}}]}`
	stage, source := legacyCanonicalInitialCheckpointStage(test, event+`,{"kind":"turn","seq":9000}`)
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		if complete, err := stage.Step(context.Background()); err != nil || complete || stage.next != 0 {
			test.Fatal("actual stage passed the unfinished preparation", complete, err)
		}
		preparation := stage.pending[0].Encoding.preparation
		if preparation == nil {
			test.Fatal("actual preparation did not yield in the tail input")
		}
		for index := 0; index < preparation.frames.length(); index++ {
			frame := preparation.frames.at(index)
			if frame.kind == 'a' && frame.length == 5001 && frame.position > 0 && frame.position < frame.length {
				return stage, source
			}
		}
	}
	test.Fatal("actual preparation never reached its tail input")
	return nil, nil
}

func legacyCanonicalProjectionCaptureAll(test *testing.T, work *legacyCanonicalSourceCheckpoint) []legacyCanonicalSourceProjectionCapture {
	test.Helper()
	root, err := work.captureSourceProjection(context.Background(), legacyCanonicalProjectionRootPosition, legacyCanonicalProjectionRootPosition)
	if err != nil {
		test.Fatal(err)
	}
	captures := []legacyCanonicalSourceProjectionCapture{root}
	for keysPosition := 0; keysPosition < work.encoding.mapKeys.length(); keysPosition++ {
		keys := work.encoding.mapKeys.at(keysPosition)
		if _, valid := keys.source.(*legacyCanonicalReplacement); !valid {
			continue
		}
		for fieldPosition := 0; fieldPosition < keys.fields.length(); fieldPosition++ {
			capture, err := work.captureSourceProjection(context.Background(), keysPosition, fieldPosition)
			if err != nil {
				test.Fatal(err)
			}
			captures = append(captures, capture)
		}
	}
	return captures
}

func TestLegacyCanonicalSourceProjectionCaptureUnvisitedRoot(test *testing.T) {
	ctx := context.Background()
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":7,"provider_metadata":{},"input":null}`)
	work := legacyCanonicalProjectionCaptureCheckpoint(test, stage, source)
	capture, err := work.captureSourceProjection(ctx, legacyCanonicalProjectionRootPosition, legacyCanonicalProjectionRootPosition)
	if err != nil || capture.record != work.child.source.records.at(0) || work.encoding.mapKeys.length() != 0 {
		test.Fatal("unvisited root required preparation or a provenance registry", err)
	}
	for attempt := 0; work.sourceRecord() == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
		if ready, err := work.step(ctx); err != nil || ready {
			test.Fatal("source traversal skipped the unvisited root boundary", ready, err)
		}
	}
	for _, member := range []struct {
		value any
		slot  legacyCanonicalProjectionSlot
	}{
		{value: work.encoding.root, slot: legacyCanonicalProjectionRoot},
		{value: capture.root["provider_metadata"], slot: legacyCanonicalProjectionMetadata},
		{value: capture.root["input"], slot: legacyCanonicalProjectionEmptyInput},
	} {
		reference, found, err := capture.reference(ctx, member.value)
		if err != nil || !found || reference.Source.Record != 1 || reference.Slot != member.slot {
			test.Fatal("empty synthetic maps lost their distinct root slots", reference, found, err)
		}
	}
	if !legacyCanonicalInitialState(work.encoding) {
		test.Fatal("root capture advanced canonical preparation")
	}
}

func TestLegacyCanonicalSourceProjectionCaptureLiveOccurrences(test *testing.T) {
	ctx := context.Background()
	stage, source := legacyCanonicalProjectionCaptureStage(test)
	encoding := stage.pending[0].Encoding
	before := *encoding
	retained, err := encoding.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	reads, written := len(source.reads), stage.written
	work := legacyCanonicalProjectionCaptureCheckpoint(test, stage, source)
	captures := legacyCanonicalProjectionCaptureAll(test, work)
	if len(captures) != 11 {
		test.Fatal("fixture did not retain every nested projection", len(captures))
	}
	byRecord := make(map[*legacyNativeEventValue]legacyCanonicalSourceProjectionCapture)
	for _, capture := range captures {
		if _, exists := byRecord[capture.record]; exists {
			test.Fatal("distinct projected occurrences collapsed to one native record")
		}
		byRecord[capture.record] = capture
		if _, found, err := capture.reference(ctx, capture.root); found || !errors.Is(err, ErrIntegrity) {
			test.Fatal("projection bound before a complete source record boundary", found, err)
		}
	}
	seen := make(map[*legacyNativeEventValue]legacyCanonicalSourceRecordReference)
	slots := make(map[legacyCanonicalProjectionSlot]int)
	active := make([]any, 0, encoding.mapKeys.length()+encoding.preparation.frames.length())
	for index := 0; index < encoding.mapKeys.length(); index++ {
		active = append(active, encoding.mapKeys.at(index).source)
	}
	for index := 0; index < encoding.preparation.frames.length(); index++ {
		active = append(active, encoding.preparation.frames.at(index).value)
	}
	activeMatches := 0
	ready := false
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
		ready, err = work.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if work.sourceRecord() == nil {
			continue
		}
		sourceReference, record, err := work.sourceRecordReference(ctx)
		if err != nil {
			test.Fatal(err)
		}
		capture, exists := byRecord[record]
		if !exists || seen[record].Record != 0 {
			test.Fatal("source boundary is missing its exact live projection or repeated")
		}
		seen[record] = sourceReference
		members := []struct {
			value any
			slot  legacyCanonicalProjectionSlot
		}{
			{value: capture.root, slot: legacyCanonicalProjectionRoot},
			{value: capture.root["provider_metadata"], slot: legacyCanonicalProjectionMetadata},
			{value: capture.root["locked"], slot: legacyCanonicalProjectionLocked},
			{value: capture.root["blocks"], slot: legacyCanonicalProjectionBlocks},
			{value: capture.root["replacement"], slot: legacyCanonicalProjectionReplacement},
		}
		if record.event.Kind == EventToolCall && record.event.Input == nil {
			members = append(members, struct {
				value any
				slot  legacyCanonicalProjectionSlot
			}{value: capture.root["input"], slot: legacyCanonicalProjectionEmptyInput})
		}
		for _, member := range members {
			if member.value == nil {
				continue
			}
			reference, found, err := capture.reference(ctx, member.value)
			if err != nil || !found || reference.Source != sourceReference || reference.Slot != member.slot {
				test.Fatal("live slot did not bind its actual occurrence", member.slot, reference, found, err)
			}
			slots[member.slot]++
		}
		for _, value := range active {
			if capture.slot(value) != "" {
				reference, found, err := capture.reference(ctx, value)
				if err != nil || !found || reference.Source != sourceReference {
					test.Fatal("active frame or map-key source lost its live projection", found, err)
				}
				activeMatches++
			}
		}
		for _, other := range captures {
			if other.record != record {
				if _, found, err := other.reference(ctx, other.root); err != nil || found {
					test.Fatal("another occurrence bound by equal fields or sequence", found, err)
				}
			}
		}
		copyRoot := make(map[string]any, len(capture.root))
		for key, value := range capture.root {
			copyRoot[key] = value
		}
		for _, value := range []any{copyRoot, map[string]any{}, record.event.Input, record.event.Output, nil, (*legacyCanonicalReplacement)(nil), (*legacyNativeBlockValues)(nil)} {
			if _, found, err := capture.reference(ctx, value); err != nil || found {
				test.Fatal("content-equal or borrowed value became a synthetic projection", found, err)
			}
		}
		if replacement, valid := capture.root["replacement"].(*legacyCanonicalReplacement); valid {
			copyReplacement := *replacement
			if _, found, err := capture.reference(ctx, &copyReplacement); err != nil || found {
				test.Fatal("a different wrapper sharing native children was accepted", found, err)
			}
		}
		if blocks, valid := capture.root["blocks"].(*legacyNativeBlockValues); valid {
			copyBlocks := *blocks
			if _, found, err := capture.reference(ctx, &copyBlocks); err != nil || found {
				test.Fatal("content-equal blocks were accepted", found, err)
			}
		}
	}
	if !ready || len(seen) != len(captures) || activeMatches < len(captures) {
		test.Fatal("capture did not cover the live preparation", ready, len(seen), activeMatches)
	}
	for _, slot := range []legacyCanonicalProjectionSlot{legacyCanonicalProjectionRoot, legacyCanonicalProjectionMetadata,
		legacyCanonicalProjectionLocked, legacyCanonicalProjectionEmptyInput, legacyCanonicalProjectionBlocks, legacyCanonicalProjectionReplacement} {
		if slots[slot] == 0 {
			test.Fatal("fixture missed a projection slot", slot)
		}
	}
	first, second := seen[encoding.nativeOwner.replacement.at(0)], seen[encoding.nativeOwner.replacement.at(1)]
	if first.Record == 0 || first.Record == second.Record || first.Fields != second.Fields {
		test.Fatal("equal native siblings were not distinguished by occurrence", first, second)
	}
	after, err := encoding.RetainedBytes()
	if err != nil || retained != after || !reflect.DeepEqual(before, *encoding) || len(source.reads) != reads || stage.written != written ||
		!legacyCanonicalInitialState(stage.pending[1].Encoding) {
		test.Fatal("reverse identification changed preparation, retention, output, or source reads", err)
	}
	if _, err := stage.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("capture did not preserve the caller freeze", err)
	}
	if _, err := work.take(ctx); err != nil {
		test.Fatal(err)
	}
	if _, found, err := captures[0].reference(ctx, captures[0].root); found || !errors.Is(err, ErrIntegrity) {
		test.Fatal("closed capture remained usable", found, err)
	}
}

func TestLegacyCanonicalSourceProjectionCapturePartialReplacement(test *testing.T) {
	for _, phase := range []uint8{0, 1} {
		test.Run(strconv.Itoa(int(phase)), func(test *testing.T) {
			var stage *LegacyNormalizationPreparation
			var source *legacySourceProofBuffer
			frameIndex := -1
			if phase == 1 {
				stage, source, frameIndex = legacyCanonicalReplacementCheckpointFixture(test)
			} else {
				event := `{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":[` +
					strings.Repeat(`{"kind":"turn","seq":7},`, 1199) + `{"kind":"turn","seq":7}]}`
				stage, source = legacyCanonicalInitialCheckpointStage(test, event)
				for attempt := 0; attempt < legacyDecoderCheckpointSteps && frameIndex < 0; attempt++ {
					if complete, err := stage.Step(context.Background()); err != nil || complete || stage.pending[0].Encoding.preparation == nil {
						test.Fatal("replacement did not yield during projection production", complete, err)
					}
					encoding := stage.pending[0].Encoding
					for index := 0; index < encoding.preparation.frames.length(); index++ {
						frame := encoding.preparation.frames.at(index)
						if frame.kind == 'r' && frame.phase == 0 && frame.position > 0 && frame.position < frame.count {
							frameIndex = index
						}
					}
				}
			}
			if frameIndex < 0 {
				test.Fatal("replacement boundary was not reached")
			}
			work := legacyCanonicalProjectionCaptureCheckpoint(test, stage, source)
			frame := work.encoding.preparation.frames.at(frameIndex)
			keys := work.encoding.mapKeys.at(frame.mapPosition)
			replacement := keys.source.(*legacyCanonicalReplacement)
			for _, position := range []int{0, keys.fields.length() / 2, keys.fields.length() - 1} {
				field := keys.fields.at(position)
				ordinal, err := strconv.ParseUint(field.key[16:], 16, 64)
				if err != nil {
					test.Fatal(err)
				}
				capture, err := work.captureSourceProjection(context.Background(), frame.mapPosition, position)
				if err != nil || capture.record != replacement.native.at(int(ordinal)) ||
					reflect.ValueOf(capture.root).Pointer() != reflect.ValueOf(field.value).Pointer() {
					test.Fatal("partial generation or sorting lost ordinal provenance", phase, position, ordinal, err)
				}
			}
			if phase == 0 {
				if _, err := work.captureSourceProjection(context.Background(), frame.mapPosition, keys.fields.length()); !errors.Is(err, ErrIntegrity) {
					test.Fatal("an unproduced projection was invented", err)
				}
			}
		})
	}
}

func TestLegacyCanonicalSourceProjectionCaptureFences(test *testing.T) {
	ctx := context.Background()
	stage, source := legacyCanonicalProjectionCaptureStage(test)
	work := legacyCanonicalProjectionCaptureCheckpoint(test, stage, source)
	captures := legacyCanonicalProjectionCaptureAll(test, work)
	capture := captures[1]
	keys := work.encoding.mapKeys.at(capture.keysPosition)
	field := keys.fields.at(capture.fieldPosition)
	for _, scenario := range []string{"cancel", "budget", "owner", "preparation", "freeze", "keys", "native", "mixed", "ordinal", "sequence", "field", "root"} {
		test.Run(scenario, func(test *testing.T) {
			request, want := ctx, error(ErrIntegrity)
			switch scenario {
			case "cancel":
				var cancel context.CancelFunc
				request, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "budget":
				budget := work.budget
				work.budget = 1
				defer func() { work.budget = budget }()
				want = ErrDocumentResourceLimit
			case "owner":
				owner := work.encoding.nativeOwner
				work.encoding.nativeOwner = &legacyNativeCanonicalOwner{}
				defer func() { work.encoding.nativeOwner = owner }()
				want = ErrConflict
			case "preparation":
				preparation := work.encoding.preparation
				work.encoding.preparation = nil
				defer func() { work.encoding.preparation = preparation }()
				want = ErrConflict
			case "freeze":
				stage.mutation.closed.Store(true)
				defer stage.mutation.closed.Store(false)
				want = ErrConflict
			case "keys":
				original := keys.source
				keys.source = capture.root
				defer func() { keys.source = original }()
			case "native":
				original := keys.source
				keys.source = &legacyCanonicalReplacement{events: []CIREvent{capture.record.event}}
				defer func() { keys.source = original }()
			case "mixed":
				replacement := keys.source.(*legacyCanonicalReplacement)
				replacement.events = []CIREvent{}
				defer func() { replacement.events = nil }()
			case "ordinal":
				original := field.key
				field.key = original[:16] + "ffffffffffffffff"
				defer func() { field.key = original }()
			case "sequence":
				original := field.key
				field.key = "0000000000000000" + original[16:]
				defer func() { field.key = original }()
			case "field":
				original := field.value
				field.value = true
				defer func() { field.value = original }()
			case "root":
				original := field.value
				field.value = map[string]any{}
				defer func() { field.value = original }()
				want = ErrConflict
			}
			if _, found, err := capture.reference(request, capture.root); found || !errors.Is(err, want) {
				test.Fatal("projection capture bypassed its source, budget, or identity fence", found, err, want)
			}
		})
	}
	for _, coordinates := range [][2]int{{-2, -1}, {-1, 0}, {work.encoding.mapKeys.length(), 0}, {capture.keysPosition, -1}, {capture.keysPosition, keys.fields.length()}} {
		if _, err := work.captureSourceProjection(ctx, coordinates[0], coordinates[1]); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid projection coordinates were accepted", coordinates, err)
		}
	}
	if _, _, err := (legacyCanonicalSourceProjectionCapture{}).reference(ctx, nil); !errors.Is(err, ErrIntegrity) {
		test.Fatal("uninitialized capture was accepted", err)
	}
}
