package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyCanonicalSourceRecordsBindCompleteOccurrences(test *testing.T) {
	ctx := context.Background()
	child := `{"kind":"message","seq":7,"role":"user","blocks":[{"type":"text","text":"same"}],"provider_metadata":{"turn_id":"same","create_time":1.25}}`
	nested := `{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":[` + child + `,{"kind":"reasoning","seq":7,"locked":{"provider":"codex","scheme":"opaque","blob":"same"}}]}`
	event := `{"kind":"compaction","seq":7,"replacement_complete":true,"replacement":[` + strings.Repeat(child+",", 24) + nested + `,{"kind":"tool_call","seq":7,"input":null},{"kind":"tool_result","seq":7,"output":{"items":[{}]}}]}`
	stage, source := legacyCanonicalInitialCheckpointStage(test, event+`,{"kind":"turn","seq":9000}`)
	encoding := stage.pending[0].Encoding
	owner := encoding.nativeOwner
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(ctx, encoding, stage.mutation, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	paths := make(map[*legacyNativeEventValue][]int)
	var visit func(*legacyNativeEventValues, []int)
	visit = func(values *legacyNativeEventValues, path []int) {
		if values == nil {
			return
		}
		for index := 0; index < values.records.length(); index++ {
			record := values.records.at(index)
			current := append(append([]int{}, path...), index)
			paths[record] = current
			visit(record.replacement, current)
		}
	}
	visit(owner.replacement, nil)
	var references []legacyCanonicalSourceRecordReference
	var positions [][]int
	seen := make(map[uint64]bool)
	ready, reads := false, len(source.reads)
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
		ready, err = checkpoint.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if checkpoint.sourceRecord() == nil {
			continue
		}
		reference, record, err := checkpoint.sourceRecordReference(ctx)
		if err != nil || record == nil || seen[reference.Record] {
			test.Fatal("complete source occurrence was missing or repeated", reference, err)
		}
		path, exists := paths[record]
		if reference.Record == 1 {
			if exists || record.replacement != owner.replacement {
				test.Fatal("root occurrence lost its native children")
			}
		} else if !exists {
			test.Fatal("record boundary did not borrow the actual source record")
		}
		seen[reference.Record] = true
		references = append(references, reference)
		positions = append(positions, path)
	}
	if !ready || len(references) != len(paths)+1 || len(references) != 30 || len(source.reads) != reads || !legacyCanonicalInitialState(encoding) ||
		references[0].Fields != references[1].Fields || references[0].Record == references[1].Record {
		test.Fatal("source occurrence binding conflated equal records, replayed input, or advanced preparation", len(references), len(paths))
	}
	if _, err := stage.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("source occurrence capture released caller grant", err)
	}
	root, err := checkpoint.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	wire, err := json.Marshal(references)
	if err != nil {
		test.Fatal(err)
	}
	references = nil
	if err := json.Unmarshal(wire, &references); err != nil {
		test.Fatal(err)
	}
	restore, err := newLegacyCanonicalInitialRestore(ctx, root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	values := make([]legacyNativeEventValue, len(references))
	resolved := make([]bool, len(references))
	ready = false
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
		ready, err = restore.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if restore.sourceRecord() == nil {
			continue
		}
		for index, reference := range references {
			value, found, err := restore.resolveSourceRecord(ctx, reference)
			if err != nil {
				test.Fatal("cannot bind complete restored occurrence", reference, err)
			}
			if found {
				if resolved[index] {
					test.Fatal("record resolved twice", reference.Record)
				}
				values[index], resolved[index] = value, true
			}
		}
	}
	if !ready {
		test.Fatal("record restore exceeded original finite steps")
	}
	restored, err := restore.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer restored.release()
	for index, path := range positions {
		expected := legacyNativeEventValue{event: restored.nativeOwner.event, replacement: restored.nativeOwner.replacement, blocks: restored.nativeOwner.blocks}
		for _, ordinal := range path {
			expected = *expected.replacement.records.at(ordinal)
		}
		value := values[index]
		if !resolved[index] || !reflect.DeepEqual(value.event, expected.event) || value.blocks != expected.blocks || value.replacement != expected.replacement ||
			!legacyNativeCheckpointGoGraphSetSame(value.event.Input, expected.event.Input) || !legacyNativeCheckpointGoGraphSetSame(value.event.Output, expected.event.Output) {
			test.Fatal("record binding returned another occurrence or incomplete native children", index, path)
		}
	}
	if values[0].blocks == values[1].blocks || values[0].blocks == owner.replacement.at(0).blocks || len(source.reads) != reads || !legacyCanonicalInitialState(stage.pending[1].Encoding) {
		test.Fatal("record restore reused old objects, conflated equal siblings, or consumed input")
	}
	test.Logf("complete occurrences=%d; descriptor bytes=%d; root record remains first row across nested traversal", len(references), len(wire))
}

func TestLegacyCanonicalSourceRecordFences(test *testing.T) {
	ctx := context.Background()
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":7,"input":{"same":{}}}`)
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
	if _, _, err := checkpoint.sourceRecordReference(ctx); !errors.Is(err, ErrIntegrity) {
		test.Fatal("record exposed before its native children", err)
	}
	for attempt := 0; checkpoint.sourceRecord() == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
		if ready, err := checkpoint.step(ctx); err != nil || ready {
			test.Fatal("source complete-record boundary skipped", ready, err)
		}
	}
	reference, _, err := checkpoint.sourceRecordReference(ctx)
	if err != nil {
		test.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := checkpoint.sourceRecordReference(canceled); !errors.Is(err, context.Canceled) {
		test.Fatal("record binding ignored cancellation", err)
	}
	budget := checkpoint.budget
	checkpoint.budget = 1
	if _, _, err := checkpoint.sourceRecordReference(ctx); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("record binding ignored retained budget", err)
	}
	checkpoint.budget = budget
	ready := false
	for attempt := 0; !ready && attempt < legacyDecoderCheckpointSteps; attempt++ {
		ready, err = checkpoint.step(ctx)
		if err != nil {
			test.Fatal(err)
		}
	}
	root, err := checkpoint.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := checkpoint.sourceRecordReference(ctx); !errors.Is(err, ErrIntegrity) {
		test.Fatal("released source boundary remained readable", err)
	}
	restore, err := newLegacyCanonicalInitialRestore(ctx, root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	for attempt := 0; restore.sourceRecord() == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
		if ready, err := restore.step(ctx); err != nil || ready {
			test.Fatal("restore skipped the complete record", ready, err)
		}
	}
	for _, scenario := range []string{"zero-row", "large-row", "wrong-row", "missing-fields", "wrong-fields", "wrong-length", "cancel", "budget"} {
		test.Run(scenario, func(test *testing.T) {
			candidate, current, want := reference, ctx, ErrIntegrity
			originalBudget := restore.budget
			defer func() { restore.budget = originalBudget }()
			switch scenario {
			case "zero-row":
				candidate.Record = 0
			case "large-row":
				candidate.Record = 8*uint64(MaxFinalizedDocBytes) + 1
			case "wrong-row":
				candidate.Record++
				want = nil
			case "missing-fields":
				candidate.Fields = legacyNativeCheckpointRef{}
			case "wrong-fields":
				candidate.Fields.Hash = HashContent([]byte("other"))
			case "wrong-length":
				candidate.Fields.Bytes++
			case "cancel":
				current, want = canceled, context.Canceled
			case "budget":
				restore.budget, want = 1, ErrDocumentResourceLimit
			}
			if _, found, err := restore.resolveSourceRecord(current, candidate); found || !errors.Is(err, want) {
				test.Fatal("record reference bypassed scope or validation", found, err, want)
			}
		})
	}
}
