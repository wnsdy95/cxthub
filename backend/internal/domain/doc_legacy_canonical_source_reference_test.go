package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyCanonicalSourceReferencesRestore(test *testing.T, restore *legacyCanonicalInitialRestore, references []legacyCanonicalSourceReference) []any {
	test.Helper()
	values := make([]any, len(references))
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		ready, err := restore.step(context.Background())
		if err != nil {
			test.Fatal("source reference restore failed", attempt, err)
		}
		retained, err := restore.retainedBytes()
		if err != nil || retained < 0 || retained > legacyDecoderCheckpointBudget {
			test.Fatal("source reference restore exceeded its budget", retained, err)
		}
		if restore.sourceGraph() != nil {
			for index, reference := range references {
				value, found, err := restore.resolveSourceReference(context.Background(), reference)
				if err != nil {
					test.Fatal("source reference could not resolve", reference.Record, err)
				}
				if found {
					if values[index] != nil {
						test.Fatal("source reference was resolved in more than one record", reference.Record)
					}
					values[index] = value
				}
			}
		}
		if ready {
			for index, value := range values {
				if value == nil {
					test.Fatal("source reference was not resolved", references[index].Record)
				}
			}
			return values
		}
	}
	test.Fatal("source reference restore did not make bounded progress")
	return nil
}

func TestLegacyCanonicalSourceReferencesRecordScopes(test *testing.T) {
	var children []string
	for index := 0; index < 24; index++ {
		blocks := ""
		if index%3 == 0 {
			blocks = `,"blocks":[{"type":"text","text":"not part of the raw graph"}]`
		}
		children = append(children, fmt.Sprintf(`{"kind":"tool_call","seq":%d,"input":{"items":[{"value":1}]}%s}`, index+1, blocks))
	}
	nested := `{"kind":"compaction","seq":50,"replacement_complete":true,"replacement":[` + children[0] + `,` + children[1] + `]}`
	event := `{"kind":"compaction","seq":100,"replacement_complete":true,"replacement":[` + strings.Join(children, ",") + `,` + nested + `]}`
	stage, source := legacyCanonicalInitialCheckpointStage(test, event+`,{"kind":"turn","seq":200}`)
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(context.Background(), stage.pending[0].Encoding, stage.mutation, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	var references []legacyCanonicalSourceReference
	var graph legacyNativeCheckpointRef
	ready := false
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
		ready, err = checkpoint.step(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if checkpoint.sourceGraph() == nil || checkpoint.child.current.record.event.Input == nil {
			continue
		}
		input := checkpoint.child.current.record.event.Input
		reference, found, err := checkpoint.sourceReference(context.Background(), input["items"])
		if err != nil || !found {
			test.Fatal("raw nested source was not captured", found, err)
		}
		if len(references) > 0 && (reference.Graph != graph || reference.Record <= references[len(references)-1].Record) {
			test.Fatal("identical raw graphs did not preserve distinct record positions", reference, graph)
		}
		graph = reference.Graph
		references = append(references, reference)
		if _, found, err := checkpoint.sourceReference(context.Background(), map[string]any{"items": input["items"]}); err != nil || found {
			test.Fatal("equal projection was confused with a raw source owner", found, err)
		}
	}
	if !ready || len(references) != 26 || references[len(references)-1].Record < legacyNativeCheckpointPageItems {
		test.Fatal("fixture did not cross a record page and nested replacement", ready, len(references))
	}
	root, err := checkpoint.take(context.Background())
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
	restore, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	values := legacyCanonicalSourceReferencesRestore(test, restore, references)
	encoding, err := restore.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer encoding.release()
	replacement := encoding.nativeOwner.replacement
	for index, value := range values {
		var record *legacyNativeEventValue
		if index < 24 {
			record = replacement.records.at(index)
		} else {
			record = replacement.records.at(24).replacement.records.at(index - 24)
		}
		if !legacyNativeCheckpointGoGraphSetSame(value, record.event.Input["items"]) ||
			index > 0 && legacyNativeCheckpointGoGraphSetSame(value, values[index-1]) {
			test.Fatal("reference returned a copy or conflated equal sibling records", index)
		}
	}
}

func TestLegacyCanonicalSourceReferencesRejectInvalidBinding(test *testing.T) {
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":{"items":[1,2]}}`)
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	store, _ := legacyNativeCheckpointStoreFixture(test)
	ioCalls := 0
	put, read := store.Put, store.Read
	store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		ioCalls++
		return put(ctx, hash, body)
	}
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		ioCalls++
		return read(ctx, hash, length)
	}
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(context.Background(), stage.pending[0].Encoding, stage.mutation, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	for attempt := 0; checkpoint.sourceGraph() == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
		if ready, err := checkpoint.step(context.Background()); err != nil || ready {
			test.Fatal("writer skipped the live raw graph boundary", ready, err)
		}
	}
	input := stage.pending[0].Encoding.nativeOwner.event.Input
	before := ioCalls
	reference, found, err := checkpoint.sourceReference(context.Background(), input["items"])
	if err != nil || !found || ioCalls != before {
		test.Fatal("binding reread or rewrote the original", found, err, before, ioCalls)
	}
	budget := checkpoint.budget
	checkpoint.budget = 1
	if _, _, err := checkpoint.sourceReference(context.Background(), input); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("binding bypassed the current memory budget", err)
	}
	checkpoint.budget = budget
	currentCapture := stage.mutation.capture.Swap(nil)
	_, _, staleErr := checkpoint.sourceReference(context.Background(), input)
	stage.mutation.capture.Store(currentCapture)
	if !errors.Is(staleErr, ErrConflict) {
		test.Fatal("binding reused a stale capture grant", staleErr)
	}
	original := checkpoint.encoding.nativeOwner
	replaced := *original
	checkpoint.encoding.nativeOwner = &replaced
	_, _, staleErr = checkpoint.sourceReference(context.Background(), input)
	checkpoint.encoding.nativeOwner = original
	if !errors.Is(staleErr, ErrConflict) {
		test.Fatal("binding accepted a replacement source owner", staleErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := checkpoint.sourceReference(ctx, input); !errors.Is(err, context.Canceled) {
		test.Fatal("binding ignored cancellation", err)
	}
	legacyDecoderCheckpointDrain(test, checkpoint, legacyDecoderCheckpointBudget)
	root, err := checkpoint.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := checkpoint.sourceReference(context.Background(), input); !errors.Is(err, ErrIntegrity) {
		test.Fatal("binding reused a released graph", err)
	}
	restore, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	for attempt := 0; restore.sourceGraph() == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
		if ready, err := restore.step(context.Background()); err != nil || ready {
			test.Fatal("restore skipped the live raw graph boundary", ready, err)
		}
	}
	before = ioCalls
	value, found, err := restore.resolveSourceReference(context.Background(), reference)
	if err != nil || !found || ioCalls != before || !reflect.DeepEqual(value, input["items"]) {
		test.Fatal("resolve reread the original or changed its value", found, err, before, ioCalls)
	}
	for _, change := range []struct {
		name string
		edit func(*legacyCanonicalSourceReference)
	}{
		{"graph", func(value *legacyCanonicalSourceReference) { value.Graph.Hash = HashContent([]byte("different graph")) }},
		{"body", func(value *legacyCanonicalSourceReference) { value.Body.Hash = HashContent([]byte("different owner")) }},
		{"shape", func(value *legacyCanonicalSourceReference) { value.Value.Length-- }},
		{"identity", func(value *legacyCanonicalSourceReference) { value.Value.Reference = legacyNativeCheckpointNodes }},
		{"zero-record", func(value *legacyCanonicalSourceReference) { value.Record = 0 }},
	} {
		test.Run(change.name, func(test *testing.T) {
			invalid := reference
			change.edit(&invalid)
			if _, _, err := restore.resolveSourceReference(context.Background(), invalid); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid reference was accepted", invalid, err)
			}
		})
	}
	other := reference
	other.Record++
	if _, found, err := restore.resolveSourceReference(context.Background(), other); err != nil || found {
		test.Fatal("record identity was ignored", found, err)
	}
	if _, _, err := restore.resolveSourceReference(ctx, reference); !errors.Is(err, context.Canceled) {
		test.Fatal("resolve ignored cancellation", err)
	}
	budget = restore.budget
	restore.budget = 1
	if _, _, err := restore.resolveSourceReference(context.Background(), reference); !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("resolve bypassed the current budget", err)
	}
	restore.budget = budget
	if ioCalls != before {
		test.Fatal("reference validation performed extra I/O", before, ioCalls)
	}
	restore.close()
	if _, _, err := restore.resolveSourceReference(context.Background(), reference); !errors.Is(err, ErrIntegrity) {
		test.Fatal("resolve reused a released registry", err)
	}
}
