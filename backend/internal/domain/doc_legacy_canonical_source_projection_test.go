package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func legacyCanonicalProjectionSource(test *testing.T, event string) (*LegacyNormalizationPreparation, *legacySourceProofBuffer, legacyNativeCheckpointIO, legacyNativeCheckpointRef, []legacyCanonicalSourceRecordReference) {
	test.Helper()
	ctx := context.Background()
	stage, source := legacyCanonicalInitialCheckpointStage(test, event)
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(ctx, stage.pending[0].Encoding, stage.mutation, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	var references []legacyCanonicalSourceRecordReference
	ready := false
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
		ready, err = checkpoint.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if checkpoint.sourceRecord() != nil {
			reference, _, err := checkpoint.sourceRecordReference(ctx)
			if err != nil {
				test.Fatal(err)
			}
			references = append(references, reference)
		}
	}
	if !ready {
		test.Fatal("projection source capture exceeded existing step bound")
	}
	root, err := checkpoint.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	return stage, source, store, root, references
}

func TestLegacyCanonicalSourceProjectionSlotsAndTransfer(test *testing.T) {
	ctx := context.Background()
	event := `{"kind":"compaction","seq":7,"replacement_complete":true,"provider_metadata":{"turn_id":"root","create_time":1.25},"replacement":[` +
		`{"kind":"message","seq":7,"blocks":null,"provider_metadata":{}},` +
		`{"kind":"message","seq":7,"blocks":[{"type":"text","text":"same"}]},` +
		`{"kind":"message","seq":7,"blocks":[{"type":"text","text":"same"}]},` +
		`{"kind":"reasoning","seq":7,"locked":{"provider":"codex","scheme":"opaque","blob":"same"}},` +
		`{"kind":"tool_call","seq":7,"input":null},` +
		`{"kind":"tool_call","seq":7,"input":{}},` +
		`{"kind":"tool_result","seq":7,"output":{"items":[{},true]}},` +
		`{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":[]}]}`
	stage, source, store, root, references := legacyCanonicalProjectionSource(test, event)
	reads := len(source.reads)
	restore, err := newLegacyCanonicalInitialRestore(ctx, root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	var projections []*legacyCanonicalSourceProjection
	var rootValue any
	var rootProjection *legacyCanonicalSourceProjection
	wantedSlots := []map[legacyCanonicalProjectionSlot]bool{
		{legacyCanonicalProjectionMetadata: true, legacyCanonicalProjectionBlocks: true},
		{legacyCanonicalProjectionBlocks: true},
		{legacyCanonicalProjectionBlocks: true},
		{legacyCanonicalProjectionLocked: true},
		{legacyCanonicalProjectionEmptyInput: true},
		{},
		{},
		{legacyCanonicalProjectionReplacement: true},
		{legacyCanonicalProjectionMetadata: true, legacyCanonicalProjectionReplacement: true},
	}
	ready, position := false, 0
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
		ready, err = restore.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if restore.sourceRecord() == nil {
			continue
		}
		if position >= len(references) {
			test.Fatal("projection restore repeated a source occurrence")
		}
		reference := references[position]
		position++
		projection, found, err := restore.projectSourceRecord(ctx, reference)
		if err != nil || !found {
			test.Fatal("projection did not bind the current source record", found, err)
		}
		projections = append(projections, projection)
		defer projection.close()
		for _, slot := range []legacyCanonicalProjectionSlot{legacyCanonicalProjectionRoot, legacyCanonicalProjectionMetadata,
			legacyCanonicalProjectionLocked, legacyCanonicalProjectionEmptyInput, legacyCanonicalProjectionBlocks, legacyCanonicalProjectionReplacement} {
			descriptor := legacyCanonicalProjectionReference{Source: reference, Slot: slot}
			wire, err := json.Marshal(descriptor)
			if err != nil {
				test.Fatal(err)
			}
			descriptor = legacyCanonicalProjectionReference{}
			if err := json.Unmarshal(wire, &descriptor); err != nil {
				test.Fatal(err)
			}
			value, err := projection.resolve(ctx, descriptor)
			key := map[legacyCanonicalProjectionSlot]string{legacyCanonicalProjectionMetadata: "provider_metadata",
				legacyCanonicalProjectionLocked: "locked", legacyCanonicalProjectionEmptyInput: "input",
				legacyCanonicalProjectionBlocks: "blocks", legacyCanonicalProjectionReplacement: "replacement"}[slot]
			expected := slot == legacyCanonicalProjectionRoot || wantedSlots[position-1][slot]
			if !expected {
				if !errors.Is(err, ErrIntegrity) {
					test.Fatal("absent slot was accepted", slot, err)
				}
				continue
			}
			if err != nil {
				test.Fatal(err)
			}
			member := any(projection.root)
			if slot != legacyCanonicalProjectionRoot {
				member = projection.root[key]
			}
			if member == nil || reflect.ValueOf(value).Pointer() != reflect.ValueOf(member).Pointer() {
				test.Fatal("projection resolved a different member", slot)
			}
			again, err := projection.resolve(ctx, descriptor)
			if err != nil || reflect.ValueOf(value).Pointer() != reflect.ValueOf(again).Pointer() {
				test.Fatal("projection slot was reconstructed twice", slot, err)
			}
			if slot == legacyCanonicalProjectionRoot && reference.Record == 1 {
				rootValue, rootProjection = value, projection
			}
		}
		if reference.Record == 1 {
			repeated, found, err := restore.projectSourceRecord(ctx, reference)
			if err != nil || !found || repeated != projection {
				test.Fatal("root source boundary was materialized twice", err)
			}
		}
	}
	if !ready || position != len(references) || len(projections) != 9 || rootValue == nil {
		test.Fatal("projection restoration did not complete every occurrence", ready, position, len(projections))
	}
	encoding, err := restore.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer encoding.release()
	if !rootProjection.closed || reflect.ValueOf(encoding.root).Pointer() != reflect.ValueOf(rootValue).Pointer() ||
		reflect.ValueOf(encoding.preparation.frames.at(0).value).Pointer() != reflect.ValueOf(rootValue).Pointer() {
		test.Fatal("initial restore replaced the already bound root instead of transferring it")
	}
	for index, projection := range projections[:len(projections)-1] {
		if projection.reference.Record == 1 || projection.root == nil {
			test.Fatal("child projection was consumed with the root", index)
		}
		if index > 0 && reflect.ValueOf(projection.root).Pointer() == reflect.ValueOf(projections[index-1].root).Pointer() {
			test.Fatal("equal sibling projections were conflated")
		}
		if _, err := projection.resolve(ctx, legacyCanonicalProjectionReference{Source: references[len(references)-1], Slot: legacyCanonicalProjectionRoot}); !errors.Is(err, ErrIntegrity) {
			test.Fatal("a child projection accepted another source record", err)
		}
	}
	if projections[1].reference.Fields != projections[2].reference.Fields || projections[1].reference.Record == projections[2].reference.Record {
		test.Fatal("fixture does not cover equal fields on distinct occurrences")
	}
	if _, err := projections[4].resolve(ctx, legacyCanonicalProjectionReference{Source: references[4], Slot: legacyCanonicalProjectionEmptyInput}); err != nil {
		test.Fatal("nil input did not retain its synthetic empty object", err)
	}
	if _, err := projections[5].resolve(ctx, legacyCanonicalProjectionReference{Source: references[5], Slot: legacyCanonicalProjectionEmptyInput}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("borrowed empty input was misclassified as synthetic", err)
	}
	heap := encoding.nativeOwner.heap
	heap.include(legacyCanonicalNativeProjectionHeap(encoding.nativeOwner.event, encoding.root.(map[string]any)))
	if encoding.rootHeap != heap || len(source.reads) != reads {
		test.Fatal("root transfer changed retention or reread the source")
	}
	want := legacyCanonicalNativeOwnerReviewDrain(test, stage.pending[0].Encoding)
	got := legacyCanonicalNativeOwnerReviewDrain(test, encoding)
	if !bytes.Equal(got, want) || HashContent(got) != HashContent(want) {
		test.Fatal("projection root transfer changed canonical output")
	}
}

func TestLegacyCanonicalSourceProjectionFences(test *testing.T) {
	ctx := context.Background()
	_, source, store, root, references := legacyCanonicalProjectionSource(test, `{"kind":"tool_call","seq":7,"input":null}`)
	restore, err := newLegacyCanonicalInitialRestore(ctx, root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	for attempt := 0; restore.sourceRecord() == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
		if ready, err := restore.step(ctx); ready || err != nil {
			test.Fatal("source projection boundary skipped", ready, err)
		}
	}
	reference := references[0]
	before, err := restore.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	budget := restore.budget
	restore.budget = before
	if projection, _, err := restore.projectSourceRecord(ctx, reference); projection != nil || !errors.Is(err, ErrDocumentResourceLimit) || restore.projection != nil {
		test.Fatal("projection allocated before budget admission", err)
	}
	restore.budget = budget
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if projection, _, err := restore.projectSourceRecord(canceled, reference); projection != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("projection ignored cancellation", err)
	}
	projection, found, err := restore.projectSourceRecord(ctx, reference)
	if err != nil || !found {
		test.Fatal(err)
	}
	retained, err := projection.retainedBytes()
	after, outerErr := restore.retainedBytes()
	if err != nil || outerErr != nil || retained <= 0 || after-before != retained {
		test.Fatal("projection or source payload charged incorrectly", before, after, retained, err, outerErr)
	}
	for _, descriptor := range []legacyCanonicalProjectionReference{
		{Source: reference, Slot: "unknown"},
		{Source: reference, Slot: legacyCanonicalProjectionMetadata},
		{Source: reference, Slot: legacyCanonicalProjectionReplacement},
		{Slot: legacyCanonicalProjectionRoot},
	} {
		if _, err := projection.resolve(ctx, descriptor); !errors.Is(err, ErrIntegrity) {
			test.Fatal("projection accepted invalid or absent slot", descriptor.Slot, err)
		}
	}
	if _, err := projection.resolve(canceled, legacyCanonicalProjectionReference{Source: reference, Slot: legacyCanonicalProjectionRoot}); !errors.Is(err, context.Canceled) {
		test.Fatal("projection slot ignored cancellation", err)
	}
	restore.close()
	if !projection.closed || projection.root != nil || projection.record.event.Input != nil {
		test.Fatal("restore cleanup retained the projection or borrowed record")
	}
	if _, err := projection.resolve(ctx, legacyCanonicalProjectionReference{Source: reference, Slot: legacyCanonicalProjectionRoot}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("released projection remained usable", err)
	}
}
