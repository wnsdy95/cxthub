package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyCanonicalSourceNumbersSaved struct {
	Root       legacyNativeCheckpointRef
	Numbers    legacyCanonicalPreparedNumberState
	References []legacyCanonicalSourceNumberReference
}

func legacyCanonicalSourceNumbersStage(test *testing.T, token string) (*LegacyNormalizationPreparation, *legacySourceProofBuffer) {
	test.Helper()
	rawToken := token
	if rawToken == "" {
		rawToken = "123456789"
	}
	metadata := `,"provider_metadata":{"create_time":` + rawToken + `}`
	events := `{"kind":"compaction","seq":10,"replacement_complete":true,"replacement":[` +
		`{"kind":"turn","seq":1` + metadata + `},` +
		`{"kind":"turn","seq":2` + metadata + `},` +
		`{"kind":"tool_call","seq":3,"input":{"number":17}` + metadata + `},` +
		`{"kind":"tool_result","seq":4,"output":[` + strings.Repeat("1,", 5000) + `1]},` +
		`{"kind":"turn","seq":5` + metadata + `}]}`
	stage, source := legacyCanonicalInitialCheckpointStage(test, events)
	encoding := stage.pending[0].Encoding
	records := encoding.nativeOwner.replacement
	if token == "" {
		for _, index := range []int{0, 1, 2, 4} {
			number := records.records.at(index).event.ProviderMetadata.CreateTime
			*number = (*number)[:0]
		}
	}
	first := records.records.at(0).event.ProviderMetadata.CreateTime
	second := records.records.at(1).event.ProviderMetadata.CreateTime
	*second = *first
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		if _, complete, err := encoding.Next(context.Background()); err != nil || complete || encoding.preparation == nil {
			test.Fatal("fixture passed its preparing cursor", complete, err)
		}
		if encoding.numbers.pending.length() != 2 {
			continue
		}
		third := records.records.at(2).event.ProviderMetadata.CreateTime
		unvisited := records.records.at(4).event.ProviderMetadata.CreateTime
		if legacyCanonicalNumberKey(string(*first)) != legacyCanonicalNumberKey(string(*second)) ||
			legacyCanonicalNumberKey(string(*first)) == legacyCanonicalNumberKey(string(*third)) ||
			legacyCanonicalNumberKey(string(*third)) == legacyCanonicalNumberKey(string(*unvisited)) {
			test.Fatal("fixture lost shared, distinct, or unvisited identities")
		}
		if _, err := encoding.numbers.index(string(*unvisited)); !errors.Is(err, ErrIntegrity) {
			test.Fatal("equal-text unvisited number was registered", err)
		}
		if _, ok := records.records.at(2).event.Input["number"].(float64); !ok {
			test.Fatal("fixture replaced real raw graph numbers with json.Number")
		}
		return stage, source
	}
	test.Fatal("fixture did not register its visited metadata numbers")
	return nil, nil
}

func legacyCanonicalSourceNumbersSave(test *testing.T, stage *LegacyNormalizationPreparation, source *legacySourceProofBuffer, store legacyNativeCheckpointIO) legacyCanonicalSourceNumbersSaved {
	test.Helper()
	ctx := context.Background()
	encoding := stage.pending[0].Encoding
	before, preparation, numbers := *encoding, *encoding.preparation, encoding.numbers
	frames := make([]legacyCanonicalPreparationFrame, preparation.frames.length())
	for index := range frames {
		frames[index] = *preparation.frames.at(index)
	}
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	binding := HashContent(source.body)
	writer, err := newLegacyCanonicalSourceNumbersCheckpoint(ctx, &encoding.numbers, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer writer.close()
	legacyDecoderCheckpointDrain(test, writer, legacyDecoderCheckpointBudget)
	state, texts, err := writer.takeWithTexts(ctx)
	if err != nil || !texts.ready || texts.values.length() != numbers.pending.length() || writer.texts != (legacyCanonicalSourceNumberTexts{}) {
		test.Fatal("number checkpoint lost per-ordinal text refs", err)
	}
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(ctx, encoding, stage.mutation, binding, len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	saved := legacyCanonicalSourceNumbersSaved{Numbers: state}
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		ready, err := checkpoint.step(ctx)
		if err != nil {
			test.Fatal("actual source checkpoint failed", attempt, err)
		}
		if checkpoint.sourceGraph() != nil {
			reference, found, err := checkpoint.sourceNumberReference(ctx, &texts)
			if err != nil {
				test.Fatal("metadata source binding failed", err)
			}
			if found {
				saved.References = append(saved.References, reference)
			}
		}
		if ready {
			saved.Root, err = checkpoint.take(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(*encoding, before) || *encoding.preparation != preparation || encoding.numbers != numbers {
				test.Fatal("checkpoint advanced the preparing or numeric cursor")
			}
			for index, frame := range frames {
				if !reflect.DeepEqual(*preparation.frames.at(index), frame) {
					test.Fatal("checkpoint changed a live preparation frame", index)
				}
			}
			return saved
		}
	}
	test.Fatal("actual source checkpoint did not finish")
	return saved
}

func legacyCanonicalSourceNumbersLoad(test *testing.T, saved legacyCanonicalSourceNumbersSaved, source *legacySourceProofBuffer, store legacyNativeCheckpointIO) (*LegacyCanonicalEncoding, legacyCanonicalNumberValidation) {
	test.Helper()
	ctx := context.Background()
	binding := HashContent(source.body)
	reader, err := newLegacyCanonicalSourceNumbersRestore(ctx, saved.Numbers, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer reader.close()
	legacyDecoderCheckpointDrain(test, reader, legacyDecoderCheckpointBudget)
	numbers, heap, texts, err := reader.takeWithTexts(ctx)
	if err != nil || heap.err != nil || reader.texts != (legacyCanonicalSourceNumberTexts{}) {
		test.Fatal("number restore lost its ownership handoff", heap, err)
	}
	before := numbers
	restore, err := newLegacyCanonicalInitialRestore(ctx, saved.Root, binding, len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	resolved := make([]bool, len(saved.References))
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		ready, err := restore.step(ctx)
		if err != nil {
			test.Fatal("actual source restore failed", attempt, err)
		}
		if graph := restore.sourceGraph(); graph != nil {
			for index, reference := range saved.References {
				found, err := restore.resolveSourceNumberReference(ctx, reference, &numbers, &texts)
				if err != nil {
					test.Fatal("metadata source resolution failed", index, err)
				}
				if found {
					value := graph.values[legacyCanonicalSourceNumberRoot]
					if resolved[index] || value.capacity != texts.values.at(reference.Ordinal).capacity || value.validateOwnership() != nil ||
						value.excess != 2*int64(value.capacity-reference.Text.Bytes) {
						test.Fatal("binding repeated or lost numeric string capacity accounting", index)
					}
					resolved[index] = true
				}
			}
		}
		if ready {
			for index, found := range resolved {
				if !found {
					test.Fatal("source never consumed a number reference", index)
				}
			}
			if numbers != before {
				test.Fatal("source restore advanced the numeric cursor")
			}
			encoding, err := restore.take(ctx)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(encoding.release)
			return encoding, numbers
		}
	}
	test.Fatal("actual source restore did not finish")
	return nil, numbers
}

func TestLegacyCanonicalSourceNumbersPreparingRoundTrip(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "shared-distinct-and-unvisited", token: "123456789"},
		{name: "shared-distinct-and-unvisited-empty", token: ""},
		{name: "long-partial-number", token: strings.Repeat("7", 2*LegacyCanonicalEncodingBytes+17)},
		{name: "current-proof", token: strings.Repeat("7", LegacyCanonicalEncodingBytes)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			stage, source := legacyCanonicalSourceNumbersStage(test, fixture.token)
			original := &stage.pending[0].Encoding.numbers
			if _, err := original.step(context.Background()); err != nil {
				test.Fatal(err)
			}
			if len(fixture.token) >= LegacyCanonicalEncodingBytes && (original.next != 0 || original.position != LegacyCanonicalEncodingBytes || original.valid(*original.pending.at(0))) {
				test.Fatal("fixture missed a partial lexical or pending-proof boundary")
			}
			store, objects := legacyNativeCheckpointStoreFixture(test)
			sourceReads := len(source.reads)
			saved := legacyCanonicalSourceNumbersSave(test, stage, source, store)
			if len(saved.References) != 3 || saved.References[0].Ordinal != saved.References[1].Ordinal || saved.References[1].Ordinal == saved.References[2].Ordinal ||
				saved.References[0].Text != saved.References[2].Text || saved.References[0].Record == saved.References[1].Record {
				test.Fatal("bindings merged equal text, missed sharing, or included an unvisited value", saved.References)
			}
			wire, err := json.Marshal(saved)
			if err != nil {
				test.Fatal(err)
			}
			saved = legacyCanonicalSourceNumbersSaved{}
			if err := json.Unmarshal(wire, &saved); err != nil {
				test.Fatal(err)
			}
			currentIO := legacyNativeCheckpointIO{Read: func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				body, exists := objects[hash]
				if !exists || len(body) != length {
					return nil, ErrNotFound
				}
				return append([]byte(nil), body...), nil
			}}
			encoding, restored := legacyCanonicalSourceNumbersLoad(test, saved, source, currentIO)
			if restored.next != original.next || restored.position != original.position || restored.mode != original.mode || len(source.reads) != sourceReads {
				test.Fatal("roundtrip replayed parsing or original-source reads")
			}
			for index, reference := range saved.References {
				number := encoding.nativeOwner.replacement.records.at(index).event.ProviderMetadata.CreateTime
				ordinal, err := restored.index(string(*number))
				if err != nil || ordinal != reference.Ordinal || restored.valid(string(*number)) != (ordinal < original.next) {
					test.Fatal("RecordFields constructed CreateTime from a fresh identity", index, ordinal, err)
				}
			}
			unvisited := encoding.nativeOwner.replacement.records.at(4).event.ProviderMetadata.CreateTime
			if _, err := restored.index(string(*unvisited)); !errors.Is(err, ErrIntegrity) {
				test.Fatal("unvisited equal text acquired a registered identity", err)
			}
			for attempt := 0; attempt < 16; attempt++ {
				originalReady, originalErr := original.step(context.Background())
				restoredReady, restoredErr := restored.step(context.Background())
				if originalReady != restoredReady || originalErr != nil || restoredErr != nil || original.next != restored.next || original.position != restored.position || original.mode != restored.mode {
					test.Fatal("numeric continuation replayed or skipped work", attempt, originalErr, restoredErr)
				}
				if originalReady {
					return
				}
			}
			test.Fatal("numeric continuation did not finish")
		})
	}
}
