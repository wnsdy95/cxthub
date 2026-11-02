package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLegacyCanonicalSourceNumbersBoundaryFences(test *testing.T) {
	ctx := context.Background()
	stage, source := legacyCanonicalSourceNumbersStage(test, strings.Repeat("7", 2*LegacyCanonicalEncodingBytes+17))
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	ioCalls := 0
	put, read := store.Put, store.Read
	store.Put = func(current context.Context, hash ContentHash, body []byte) error {
		ioCalls++
		return put(current, hash, body)
	}
	store.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		ioCalls++
		return read(current, hash, length)
	}
	binding := HashContent(source.body)
	encoding := stage.pending[0].Encoding
	numbersBefore := encoding.numbers
	writer, err := newLegacyCanonicalSourceNumbersCheckpoint(ctx, &encoding.numbers, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer writer.close()
	legacyDecoderCheckpointDrain(test, writer, legacyDecoderCheckpointBudget)
	numberState, texts, err := writer.takeWithTexts(ctx)
	if err != nil {
		test.Fatal(err)
	}
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(ctx, encoding, stage.mutation, binding, len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	for attempt := 0; checkpoint.sourceGraph() == nil || !checkpoint.child.fields.state.HasCreateTime; attempt++ {
		if attempt == legacyDecoderCheckpointSteps {
			test.Fatal("source did not reach a metadata graph boundary")
		}
		if ready, err := checkpoint.step(ctx); err != nil || ready {
			test.Fatal("source skipped its metadata boundary", ready, err)
		}
	}
	beforeIO := ioCalls
	reference, found, err := checkpoint.sourceNumberReference(ctx, &texts)
	if err != nil || !found || ioCalls != beforeIO {
		test.Fatal("source binding reread or rehashed the long numeric body", found, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := checkpoint.sourceNumberReference(canceled, &texts); !errors.Is(err, context.Canceled) {
		test.Fatal("source binding ignored cancellation", err)
	}
	budget := checkpoint.budget
	checkpoint.budget = 1
	if _, _, err := checkpoint.sourceNumberReference(ctx, &texts); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("source binding ignored its current budget", err)
	}
	checkpoint.budget = budget
	used := encoding.numbers.proven.root.used
	encoding.numbers.proven.root.used = legacyCanonicalNumberBucketValues + 1
	if _, _, err := checkpoint.sourceNumberReference(ctx, &texts); !errors.Is(err, ErrIntegrity) {
		test.Fatal("source binding treated malformed registry as an absent number", err)
	}
	encoding.numbers.proven.root.used = used
	capture := stage.mutation.capture.Swap(nil)
	_, _, captureErr := checkpoint.sourceNumberReference(ctx, &texts)
	stage.mutation.capture.Store(capture)
	if !errors.Is(captureErr, ErrConflict) {
		test.Fatal("source binding accepted an expired capture", captureErr)
	}
	savedText := *texts.values.at(reference.Ordinal)
	texts.values.at(reference.Ordinal).text.Tree.Ref.Hash = HashContent([]byte("wrong number text"))
	if _, _, err := checkpoint.sourceNumberReference(ctx, &texts); !errors.Is(err, ErrIntegrity) {
		test.Fatal("source binding accepted a mismatched prepared Text ref", err)
	}
	*texts.values.at(reference.Ordinal) = savedText
	if encoding.numbers != numbersBefore || ioCalls != beforeIO {
		test.Fatal("source binding fences mutated numbers or performed IO")
	}
	legacyDecoderCheckpointDrain(test, checkpoint, legacyDecoderCheckpointBudget)
	root, err := checkpoint.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := checkpoint.sourceNumberReference(ctx, &texts); !errors.Is(err, ErrIntegrity) {
		test.Fatal("source binding reused a released graph", err)
	}
	reader, err := newLegacyCanonicalSourceNumbersRestore(ctx, numberState, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer reader.close()
	legacyDecoderCheckpointDrain(test, reader, legacyDecoderCheckpointBudget)
	numbers, _, restoredTexts, err := reader.takeWithTexts(ctx)
	if err != nil {
		test.Fatal(err)
	}
	restore, err := newLegacyCanonicalInitialRestore(ctx, root, binding, len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	for attempt := 0; restore.sourceGraph() == nil || !restore.child.fields.state.HasCreateTime; attempt++ {
		if attempt == legacyDecoderCheckpointSteps {
			test.Fatal("restore did not reach a metadata graph boundary")
		}
		if ready, err := restore.step(ctx); err != nil || ready {
			test.Fatal("restore skipped its metadata boundary", ready, err)
		}
	}
	beforeIO = ioCalls
	graph := restore.sourceGraph()
	original := graph.values[legacyCanonicalSourceNumberRoot]
	assertUnchanged := func(test *testing.T) {
		test.Helper()
		current := graph.values[legacyCanonicalSourceNumberRoot]
		if legacyCanonicalNumberKey(current.value.(string)) != legacyCanonicalNumberKey(original.value.(string)) || current.capacity != original.capacity ||
			current.heap != original.heap || current.excess != original.excess || ioCalls != beforeIO {
			test.Fatal("failed binding changed the graph or performed numeric body IO")
		}
	}
	for _, fixture := range []struct {
		name string
		edit func(*legacyCanonicalSourceNumberReference)
	}{
		{name: "zero-row", edit: func(value *legacyCanonicalSourceNumberReference) { value.Record = 0 }},
		{name: "graph-hash", edit: func(value *legacyCanonicalSourceNumberReference) {
			value.Graph.Hash = HashContent([]byte("wrong graph"))
		}},
		{name: "graph-bytes", edit: func(value *legacyCanonicalSourceNumberReference) { value.Graph.Bytes++ }},
		{name: "ordinal", edit: func(value *legacyCanonicalSourceNumberReference) { value.Ordinal = numbers.pending.length() }},
		{name: "text-hash", edit: func(value *legacyCanonicalSourceNumberReference) {
			value.Text.Tree.Ref.Hash = HashContent([]byte("wrong text"))
		}},
		{name: "text-bytes", edit: func(value *legacyCanonicalSourceNumberReference) { value.Text.Bytes++ }},
		{name: "text-tree", edit: func(value *legacyCanonicalSourceNumberReference) { value.Text.Tree.Height++ }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			invalid := reference
			fixture.edit(&invalid)
			if _, err := restore.resolveSourceNumberReference(ctx, invalid, &numbers, &restoredTexts); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid number reference was accepted", err)
			}
			assertUnchanged(test)
		})
	}
	other := reference
	other.Record++
	if found, err := restore.resolveSourceNumberReference(ctx, other, &numbers, &restoredTexts); err != nil || found {
		test.Fatal("number reference bound the wrong source row", found, err)
	}
	if _, err := restore.resolveSourceNumberReference(canceled, reference, &numbers, &restoredTexts); !errors.Is(err, context.Canceled) {
		test.Fatal("number resolution ignored cancellation", err)
	}
	budget = restore.budget
	restore.budget = 1
	if _, err := restore.resolveSourceNumberReference(ctx, reference, &numbers, &restoredTexts); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("number resolution ignored its current budget", err)
	}
	restore.budget = budget
	graphBudget := graph.budget
	graph.budget = 1
	if _, err := restore.resolveSourceNumberReference(ctx, reference, &numbers, &restoredTexts); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("number resolution ignored the graph budget", err)
	}
	graph.budget = graphBudget
	rootText := graph.state.Roots[legacyCanonicalSourceNumberRoot].Text
	graph.state.Roots[legacyCanonicalSourceNumberRoot].Text.Tree.Ref.Hash = HashContent([]byte("wrong raw descriptor"))
	if _, err := restore.resolveSourceNumberReference(ctx, reference, &numbers, &restoredTexts); !errors.Is(err, ErrIntegrity) {
		test.Fatal("number resolution ignored the raw graph Text descriptor", err)
	}
	graph.state.Roots[legacyCanonicalSourceNumberRoot].Text = rootText
	assertUnchanged(test)
	if found, err := restore.resolveSourceNumberReference(ctx, reference, &numbers, &restoredTexts); err != nil || !found || ioCalls != beforeIO {
		test.Fatal("valid number resolution failed or performed body IO", found, err)
	}
	if legacyCanonicalNumberKey(graph.values[legacyCanonicalSourceNumberRoot].value.(string)) != legacyCanonicalNumberKey(*numbers.pending.at(reference.Ordinal)) {
		test.Fatal("number resolution did not replace the root before RecordFields")
	}
	restore.close()
	if _, err := restore.resolveSourceNumberReference(ctx, reference, &numbers, &restoredTexts); !errors.Is(err, ErrIntegrity) {
		test.Fatal("number resolution reused a released graph", err)
	}
}

func TestLegacyCanonicalSourceNumbersOptionalLookup(test *testing.T) {
	var registry legacyCanonicalNumberRegistry
	text := strings.Clone("12345")
	identity := legacyCanonicalNumberKey(text)
	if entry, err := registry.lookupOptional(identity); err != nil || entry != nil {
		test.Fatal("empty registry did not report absence", entry, err)
	}
	if _, err := registry.insert(identity); err != nil {
		test.Fatal(err)
	}
	for _, missing := range []string{strings.Clone(text), text[:len(text)-1]} {
		if entry, err := registry.lookupOptional(legacyCanonicalNumberKey(missing)); err != nil || entry != nil {
			test.Fatal("optional lookup ignored exact pointer and length", err)
		}
	}
	if entry, err := registry.lookupOptional(identity); err != nil || entry == nil || entry.identity != identity {
		test.Fatal("optional lookup lost a registered identity", err)
	}
	registry.root.used = legacyCanonicalNumberBucketValues + 1
	if _, err := registry.lookupOptional(identity); !errors.Is(err, ErrIntegrity) {
		test.Fatal("malformed leaf was reported as absence", err)
	}
	registry.root.used = 1
	registry.nodes = 0
	if _, err := registry.lookupOptional(identity); !errors.Is(err, ErrIntegrity) {
		test.Fatal("malformed registry shape was reported as absence", err)
	}
}

func TestLegacyCanonicalSourceNumbersTextOwnershipFences(test *testing.T) {
	ctx := context.Background()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent([]byte("source number text ownership"))
	var numbers legacyCanonicalNumberValidation
	for ordinal := 0; ordinal < 65; ordinal++ {
		if err := numbers.prepare(strings.Clone("empty-number")[:0]); err != nil {
			test.Fatal(err)
		}
	}
	writer, err := newLegacyCanonicalSourceNumbersCheckpoint(ctx, &numbers, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer writer.close()
	legacyDecoderCheckpointDrain(test, writer, legacyDecoderCheckpointBudget)
	state, texts, err := writer.takeWithTexts(ctx)
	if err != nil || texts.values.length() != 65 {
		test.Fatal("text refs lost empty numeric ordinals across pages", err)
	}
	reader, err := newLegacyCanonicalSourceNumbersRestore(ctx, state, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer reader.close()
	legacyDecoderCheckpointDrain(test, reader, legacyDecoderCheckpointBudget)
	restored, numberHeap, restoredTexts, err := reader.takeWithTexts(ctx)
	if err != nil || numberHeap.err != nil || restoredTexts.values.length() != 65 {
		test.Fatal("text refs lost restored ownership", err)
	}
	var textHeap docRangeHeaderHeapEstimate
	restoredTexts.retained(&textHeap)
	if textHeap.err != nil || textHeap.bytes <= 0 || reader.texts != (legacyCanonicalSourceNumberTexts{}) || writer.texts != (legacyCanonicalSourceNumberTexts{}) {
		test.Fatal("text-ref ownership was uncharged or retained after take", textHeap)
	}
	for ordinal := 0; ordinal < 65; ordinal++ {
		text := restoredTexts.values.at(ordinal)
		if text == nil || text.text != (legacyNativeCheckpointText{}) || text.capacity < 1 || text.text != texts.values.at(ordinal).text {
			test.Fatal("empty Text ref lost its distinct nonzero backing", ordinal)
		}
		actual, err := restored.index(*restored.pending.at(ordinal))
		if err != nil || actual != ordinal {
			test.Fatal("empty restored identities were merged", ordinal, actual, err)
		}
	}
	for _, fault := range []string{"cancel", "budget"} {
		test.Run(fault, func(test *testing.T) {
			writer, err := newLegacyCanonicalSourceNumbersCheckpoint(ctx, &numbers, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer writer.close()
			reader, err := newLegacyCanonicalSourceNumbersRestore(ctx, state, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer reader.close()
			for attempt := 0; !writer.pending || reader.value == nil; attempt++ {
				if attempt == legacyDecoderCheckpointSteps {
					test.Fatal("numeric codecs missed the text-ref retention boundary")
				}
				if !writer.pending {
					if ready, err := writer.step(ctx); err != nil || ready {
						test.Fatal("writer skipped text retention", ready, err)
					}
				}
				if reader.value == nil {
					if ready, err := reader.step(ctx); err != nil || ready {
						test.Fatal("reader skipped text retention", ready, err)
					}
				}
			}
			current, expected := ctx, error(ErrDocumentResourceLimit)
			if fault == "cancel" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				current, expected = canceled, context.Canceled
			} else {
				writer.budget, reader.budget = 1, 1
			}
			for _, task := range []interface {
				step(context.Context) (bool, error)
				retainedBytes() (int64, error)
			}{writer, reader} {
				if _, err := task.step(current); !errors.Is(err, expected) {
					test.Fatal("text retention bypassed its cancellation or budget fence", err)
				}
				if retained, err := task.retainedBytes(); err != nil || retained != 0 {
					test.Fatal("failed text retention leaked owned refs", retained, err)
				}
				if _, err := task.step(ctx); !errors.Is(err, expected) {
					test.Fatal("text retention failure was not sticky", err)
				}
			}
			if writer.texts != (legacyCanonicalSourceNumberTexts{}) || reader.texts != (legacyCanonicalSourceNumberTexts{}) {
				test.Fatal("failed codecs retained their text-ref pages")
			}
		})
	}
}
