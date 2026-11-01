package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyCanonicalSourceKeysCheckpoint(test *testing.T, stage *LegacyNormalizationPreparation, source *legacySourceProofBuffer, store legacyNativeCheckpointIO) (*legacyNormalizationCapture, legacyNativeCheckpointRef) {
	test.Helper()
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(grant.release)
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(context.Background(), stage.pending[0].Encoding, stage.mutation, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	legacyDecoderCheckpointDrain(test, checkpoint, legacyDecoderCheckpointBudget)
	root, err := checkpoint.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	return grant, root
}

func legacyCanonicalSourceKeysRestore(test *testing.T, restore *legacyCanonicalInitialRestore) (map[legacyCanonicalSourceKeyReference]string, map[legacyCanonicalSourceReference]map[string]any, *LegacyCanonicalEncoding) {
	test.Helper()
	bindings := make(map[legacyCanonicalSourceKeyReference]string)
	sources := make(map[legacyCanonicalSourceReference]map[string]any)
	bind := func(ctx context.Context, reference legacyCanonicalSourceKeyReference, key string) error {
		if err := reference.validate(); err != nil {
			return err
		}
		if _, exists := bindings[reference]; exists {
			test.Fatal("source key was emitted twice", reference)
		}
		retained, err := restore.retainedBytes()
		if err != nil {
			return err
		}
		if err := legacyCanonicalPreparedValueBudget(retained, int64(len(bindings)+1)*1024, legacyDecoderCheckpointBudget); err != nil {
			return err
		}
		frame := restore.child.fields.graph.read.frame
		if frame.phase != legacyNativeCheckpointGraphRestoreValue || frame.pending.value != nil || frame.position != reference.Member ||
			frame.member.Key.Text != reference.Text || unsafe.StringData(frame.key.value.(string)) != unsafe.StringData(key) {
			test.Fatal("binding did not borrow the just-decoded member key")
		}
		if len(key) > LegacyCanonicalEncodingBytes {
			before, err := restore.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			allocations := testing.AllocsPerRun(1, func() {
				current, borrowed, found, err := restore.sourceKey(ctx)
				if err != nil || !found || current != reference || unsafe.StringData(borrowed) != unsafe.StringData(key) {
					test.Fatal("large source key lost its descriptor or backing", err)
				}
			})
			after, err := restore.retainedBytes()
			if err != nil || before != after || allocations != 0 {
				test.Fatal("source key boundary allocated or retained a key copy", allocations, before, after, err)
			}
		}
		bindings[reference] = key
		return nil
	}
	for steps := 0; steps < legacyDecoderCheckpointSteps; steps++ {
		before := len(bindings)
		ready, err := restore.stepSourceKeys(context.Background(), bind)
		if err != nil || len(bindings)-before > 1 {
			test.Fatal("source key restore failed or escaped its member quantum", err)
		}
		if restore.sourceGraph() != nil {
			for reference := range bindings {
				if reference.Source.Record != restore.child.current.recordRow || sources[reference.Source] != nil {
					continue
				}
				value, found, err := restore.resolveSourceReference(context.Background(), reference.Source)
				if err != nil || !found {
					test.Fatal("key owner did not resolve from the same source graph", err)
				}
				sources[reference.Source] = value.(map[string]any)
			}
		}
		if ready {
			encoding, err := restore.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			for reference, key := range bindings {
				owner := sources[reference.Source]
				found := false
				for stored := range owner {
					if stored == key {
						found = true
						if unsafe.StringData(stored) != unsafe.StringData(key) {
							test.Fatal("raw map copied the borrowed member key")
						}
					}
				}
				if !found {
					test.Fatal("bound key is absent from its restored owner", reference)
				}
			}
			return bindings, sources, encoding
		}
	}
	test.Fatal("source key restore exceeded the existing step allowance")
	return nil, nil, nil
}

func TestLegacyCanonicalSourceKeysRecordAndSiblingIdentity(test *testing.T) {
	var children []string
	for index := 0; index < 24; index++ {
		children = append(children, fmt.Sprintf(`{"kind":"tool_call","seq":%d,"input":{"left":{"same":1},"right":{"same":1}}}`, index+1))
	}
	nested := `{"kind":"compaction","seq":50,"replacement_complete":true,"replacement":[` + children[0] + `,` + children[1] + `]}`
	event := `{"kind":"compaction","seq":100,"replacement_complete":true,"replacement":[` + strings.Join(children, ",") + `,` + nested + `]}`
	stage, source := legacyCanonicalInitialCheckpointStage(test, event)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	_, root := legacyCanonicalSourceKeysCheckpoint(test, stage, source, store)
	reads := 0
	read := store.Read
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		reads++
		return read(ctx, hash, length)
	}
	restore, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	bindings, _, encoding := legacyCanonicalSourceKeysRestore(test, restore)
	defer encoding.release()
	if len(bindings) != 26*4 {
		test.Fatal("raw source keys were skipped", len(bindings))
	}
	byRecord := make(map[uint64][]legacyCanonicalSourceKeyReference)
	byDescriptor := make(map[legacyCanonicalSourceKeyReference]legacyCanonicalSourceKeyReference)
	repeatedGraph := false
	var descriptor legacyNativeCheckpointText
	lastRecord := uint64(0)
	for reference, key := range bindings {
		lastRecord = max(lastRecord, reference.Source.Record)
		if key != "same" {
			continue
		}
		if descriptor != (legacyNativeCheckpointText{}) && descriptor != reference.Text {
			test.Fatal("equal source keys produced different text descriptors")
		}
		descriptor = reference.Text
		byRecord[reference.Source.Record] = append(byRecord[reference.Source.Record], reference)
		unscoped := reference
		unscoped.Source.Record = 0
		if previous, found := byDescriptor[unscoped]; found {
			repeatedGraph = true
			if previous.Source.Record == reference.Source.Record || unsafe.StringData(bindings[previous]) == unsafe.StringData(key) {
				test.Fatal("identical serialized graphs conflated sibling records")
			}
		}
		byDescriptor[unscoped] = reference
	}
	if len(byRecord) != 26 || lastRecord < legacyNativeCheckpointPageItems || !repeatedGraph {
		test.Fatal("fixture missed replacement nesting or a record page")
	}
	for _, siblings := range byRecord {
		if len(siblings) != 2 || siblings[0].Source.Graph != siblings[1].Source.Graph ||
			siblings[0].Source.Body == siblings[1].Source.Body || siblings[0].Source.Value.Reference == siblings[1].Source.Value.Reference ||
			unsafe.StringData(bindings[siblings[0]]) == unsafe.StringData(bindings[siblings[1]]) {
			test.Fatal("equal sibling keys lost their distinct source identity", siblings)
		}
	}
	boundReads := reads
	reads = 0
	plain, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer plain.close()
	legacyDecoderCheckpointDrain(test, plain, legacyDecoderCheckpointBudget)
	plainEncoding, err := plain.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer plainEncoding.release()
	if reads != boundReads || !reflect.DeepEqual(plainEncoding.nativeOwner.event, encoding.nativeOwner.event) ||
		!reflect.DeepEqual(plainEncoding.nativeOwner.replacement, encoding.nativeOwner.replacement) {
		test.Fatal("source key binding changed restoration or reread raw objects", reads, boundReads)
	}
}

type legacyCanonicalSourceKeysTestCodec struct {
	source     legacyCanonicalSourceReference
	members    map[legacyNativeCheckpointText]legacyCanonicalSourceKeyReference
	bindings   map[legacyCanonicalSourceKeyReference]string
	store      legacyNativeCheckpointIO
	ownerBytes int64
	decoded    int
}

func (codec *legacyCanonicalSourceKeysTestCodec) encode(ctx context.Context, value any) (legacyNativeCheckpointRef, error) {
	return legacyNativeCheckpointWriteJSON(ctx, codec.store, value)
}

func (codec *legacyCanonicalSourceKeysTestCodec) decode(ctx context.Context, ref legacyNativeCheckpointRef) (any, error) {
	var value any
	err := legacyNativeCheckpointReadJSON(ctx, codec.store, ref, &value)
	return value, err
}

func (codec *legacyCanonicalSourceKeysTestCodec) decodeKey(ctx context.Context, text legacyNativeCheckpointText) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	reference, found := codec.members[text]
	if !found || reference.Source != codec.source || reference.Text != text {
		return "", ErrIntegrity
	}
	key, found := codec.bindings[reference]
	if !found {
		return "", ErrIntegrity
	}
	codec.decoded++
	return key, nil
}

func (codec *legacyCanonicalSourceKeysTestCodec) check(ctx context.Context, helperBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return legacyCanonicalPreparedValueBudget(codec.ownerBytes+int64(len(codec.bindings))*1024, helperBytes, legacyDecoderCheckpointBudget)
}

func TestLegacyCanonicalSourceKeysActualPreparingMapContinuation(test *testing.T) {
	input := map[string]any{"": nil, "\ud55c🙂": nil, strings.Repeat("a", LegacyCanonicalEncodingBytes-1) + strings.Repeat("\ud55c🙂", LegacyCanonicalEncodingBytes/4): nil}
	for index := 0; index < 1100; index++ {
		input[fmt.Sprintf("member-%04d", index)] = nil
	}
	body, err := json.Marshal(input)
	if err != nil {
		test.Fatal(err)
	}
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":`+string(body)+`}`)
	encoding := stage.pending[0].Encoding
	frameIndex := -1
	for steps := 0; steps < legacyDecoderCheckpointSteps && frameIndex < 0; steps++ {
		ready, err := stage.Step(context.Background())
		if err != nil || ready || stage.next != 0 || encoding.preparation == nil {
			test.Fatal("actual preparation skipped its map boundary", err)
		}
		for index := 0; index < encoding.preparation.frames.length(); index++ {
			frame := encoding.preparation.frames.at(index)
			if frame.kind == 'o' && frame.phase == 0 && frame.count == len(input) && frame.position > 0 && frame.position < frame.count {
				frameIndex = index
				break
			}
		}
	}
	if frameIndex < 0 {
		test.Fatal("actual preparation never yielded within the source map")
	}
	frame := encoding.preparation.frames.at(frameIndex)
	position, inspectWork, written := frame.position, encoding.inspectWork, stage.written
	store, objects := legacyNativeCheckpointStoreFixture(test)
	grant, root := legacyCanonicalSourceKeysCheckpoint(test, stage, source, store)
	rawObjects := make(map[ContentHash]bool, len(objects))
	for hash := range objects {
		rawObjects[hash] = true
	}
	restore, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	bindings, sources, restored := legacyCanonicalSourceKeysRestore(test, restore)
	defer restored.release()
	codec := &legacyCanonicalSourceKeysTestCodec{bindings: bindings, members: make(map[legacyNativeCheckpointText]legacyCanonicalSourceKeyReference), store: store}
	large, empty, unicode := false, false, false
	for reference, key := range bindings {
		if !legacyNativeCheckpointGoGraphSetSame(sources[reference.Source], restored.nativeOwner.event.Input) {
			test.Fatal("unexpected raw map owner in continuation fixture")
		}
		codec.source, codec.members[reference.Text] = reference.Source, reference
		large, empty, unicode = large || len(key) > LegacyCanonicalEncodingBytes, empty || key == "", unicode || key == "\ud55c🙂"
	}
	if !large || !empty || !unicode || len(bindings) != len(input) {
		test.Fatal("source key fixture missed segmented, empty, or Unicode keys")
	}
	codec.ownerBytes, err = stage.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	restoredBytes, err := restored.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	codec.ownerBytes += restoredBytes + 1024
	writer, err := newLegacyCanonicalMapContinuationCheckpoint(context.Background(), stage, frameIndex, grant, codec, HashContent(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	ref := legacyCanonicalMapContinuationCheckpointDrain(test, writer)
	read := store.Read
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		if rawObjects[hash] {
			test.Fatal("map continuation reread raw source or key objects", hash)
		}
		return read(ctx, hash, length)
	}
	codec.store = store
	continuation, err := newLegacyCanonicalMapContinuationRestore(context.Background(), ref, HashContent(source.body), frame.count, frame.position, grant, codec, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	result := legacyCanonicalMapContinuationRestoreDrain(test, continuation)
	if codec.decoded != frame.count-position || frame.position != position || encoding.inspectWork != inspectWork || stage.written != written {
		test.Fatal("source-backed suffix changed source progress or suffix geometry")
	}
	iterator := *frame.iterator
	for index := 0; index < result.continuation.fields.length(); index++ {
		field := result.continuation.fields.at(index)
		if !iterator.Next() || iterator.Key().String() != field.key || field.value != nil {
			test.Fatal("source-backed continuation changed suffix order or values", index)
		}
		for key := range restored.nativeOwner.event.Input {
			if key == field.key && unsafe.StringData(key) != unsafe.StringData(field.key) {
				test.Fatal("decodeKey returned a copy rather than the restored raw key")
			}
		}
	}
	if iterator.Next() {
		test.Fatal("source-backed continuation omitted members")
	}
	if _, err := stage.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("source binding released the parent's capture", err)
	}
	for reference, key := range bindings {
		if len(key) <= LegacyCanonicalEncodingBytes {
			continue
		}
		allocations := testing.AllocsPerRun(1, func() {
			borrowed, err := codec.decodeKey(context.Background(), reference.Text)
			if err != nil || unsafe.StringData(borrowed) != unsafe.StringData(key) {
				test.Fatal("large raw key lookup lost its backing", err)
			}
		})
		if allocations != 0 {
			test.Fatal("large raw key lookup allocated", allocations)
		}
	}
}

func TestLegacyCanonicalSourceKeysRejectCorruptTextBeforeBinding(test *testing.T) {
	key := "raw-key-must-be-verified-before-binding"
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":{"`+key+`":null}}`)
	store, objects := legacyNativeCheckpointStoreFixture(test)
	_, root := legacyCanonicalSourceKeysCheckpoint(test, stage, source, store)
	corrupted := false
	for hash, body := range objects {
		if bytes.Equal(body, []byte(key)) {
			changed := bytes.Clone(body)
			changed[0] ^= 1
			objects[hash], corrupted = changed, true
		}
	}
	if !corrupted {
		test.Fatal("fixture did not locate the actual raw key payload")
	}
	restore, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	calls := 0
	bind := func(context.Context, legacyCanonicalSourceKeyReference, string) error {
		calls++
		return nil
	}
	for steps := 0; steps < legacyDecoderCheckpointSteps; steps++ {
		ready, err := restore.stepSourceKeys(context.Background(), bind)
		if err != nil {
			if !errors.Is(err, ErrPreparedDocRangeObject) || calls != 0 {
				test.Fatal("unverified key reached the binding callback", err, calls)
			}
			if _, repeated := restore.stepSourceKeys(context.Background(), bind); repeated != err {
				test.Fatal("corrupt source failure was not sticky", repeated)
			}
			return
		}
		if ready {
			test.Fatal("corrupt source key was accepted")
		}
	}
	test.Fatal("corrupt key restore did not make bounded progress")
}

func TestLegacyCanonicalSourceKeysBindingFences(test *testing.T) {
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":{"source-key":null}}`)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	_, root := legacyCanonicalSourceKeysCheckpoint(test, stage, source, store)
	reads := 0
	read := store.Read
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		reads++
		return read(ctx, hash, length)
	}
	restore, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	var reference legacyCanonicalSourceKeyReference
	var key string
	for steps := 0; reference.Source.Record == 0 && steps < legacyDecoderCheckpointSteps; steps++ {
		ready, err := restore.stepSourceKeys(context.Background(), func(ctx context.Context, current legacyCanonicalSourceKeyReference, borrowed string) error {
			reference, key = current, borrowed
			return nil
		})
		if err != nil || ready {
			test.Fatal("restore skipped the verified key boundary", err)
		}
	}
	if reference.Source.Record == 0 {
		test.Fatal("no verified key boundary")
	}
	before := reads
	for _, change := range []struct {
		name string
		edit func(*legacyCanonicalSourceKeyReference)
	}{
		{"record", func(value *legacyCanonicalSourceKeyReference) { value.Source.Record = 0 }},
		{"graph", func(value *legacyCanonicalSourceKeyReference) { value.Source.Graph = legacyNativeCheckpointRef{} }},
		{"body", func(value *legacyCanonicalSourceKeyReference) { value.Source.Body = legacyNativeCheckpointRef{} }},
		{"array", func(value *legacyCanonicalSourceKeyReference) { value.Source.Value.Kind = legacyNativeCheckpointArray }},
		{"member", func(value *legacyCanonicalSourceKeyReference) { value.Member = value.Source.Value.Length }},
		{"text-length", func(value *legacyCanonicalSourceKeyReference) { value.Text.Bytes++ }},
		{"text-tree", func(value *legacyCanonicalSourceKeyReference) { value.Text.Tree.Ref = legacyNativeCheckpointRef{} }},
	} {
		test.Run(change.name, func(test *testing.T) {
			invalid := reference
			change.edit(&invalid)
			if err := invalid.validate(); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid source key identity was accepted", err)
			}
		})
	}
	for _, budget := range []*int64{&restore.budget, &restore.child.budget, &restore.child.fields.budget, &restore.child.fields.graph.budget, &restore.child.fields.graph.read.budget} {
		original := *budget
		*budget = 1
		_, _, found, err := restore.sourceKey(context.Background())
		*budget = original
		if !errors.Is(err, ErrDocumentResourceLimit) || found {
			test.Fatal("key binding bypassed an existing source budget", err)
		}
	}
	graph := restore.child.fields.graph
	originalBinding := graph.state.Binding
	graph.state.Binding = HashContent([]byte("different-source"))
	_, _, found, err := restore.sourceKey(context.Background())
	graph.state.Binding = originalBinding
	if !errors.Is(err, ErrIntegrity) || found {
		test.Fatal("key binding accepted a foreign graph", err)
	}
	frame := graph.read.frame
	frame.position++
	_, _, found, err = restore.sourceKey(context.Background())
	frame.position--
	if !errors.Is(err, ErrIntegrity) || found {
		test.Fatal("key binding accepted a foreign member position", err)
	}
	restore.child.current.recordRow++
	_, _, found, err = restore.sourceKey(context.Background())
	restore.child.current.recordRow--
	if !errors.Is(err, ErrIntegrity) || found {
		test.Fatal("key binding accepted a foreign record position", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, found, err := restore.sourceKey(ctx); !errors.Is(err, context.Canceled) || found {
		test.Fatal("key binding ignored cancellation", err)
	}
	current, borrowed, found, err := restore.sourceKey(context.Background())
	if err != nil || !found || current != reference || unsafe.StringData(borrowed) != unsafe.StringData(key) || reads != before {
		test.Fatal("source binding changed identity or reread its key", err, reads, before)
	}
	restore.close()
	if _, _, found, err := restore.sourceKey(context.Background()); !errors.Is(err, ErrIntegrity) || found {
		test.Fatal("released source still exposed a borrowed key", err)
	}
}

func TestLegacyCanonicalSourceKeysCallbackFailuresAreSticky(test *testing.T) {
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_call","seq":1,"input":{"source-key":null,"next-key":true}}`)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	_, root := legacyCanonicalSourceKeysCheckpoint(test, stage, source, store)
	callbackError := errors.New("parent descriptor index admission failed")
	for _, mode := range []string{"error", "cancel", "budget", "replace", "close", "nil", "pre-cancel"} {
		test.Run(mode, func(test *testing.T) {
			restore, err := newLegacyCanonicalInitialRestore(context.Background(), root, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			bind := func(context.Context, legacyCanonicalSourceKeyReference, string) error {
				calls++
				switch mode {
				case "error":
					return callbackError
				case "cancel":
					cancel()
				case "budget":
					restore.budget = 1
				case "replace":
					frame := restore.child.fields.graph.read.frame
					frame.key.value = strings.Clone(frame.key.value.(string))
				case "close":
					restore.close()
				default:
					test.Fatal("callback ran before its initial gate", mode)
				}
				return nil
			}
			want := callbackError
			switch mode {
			case "cancel", "pre-cancel":
				want = context.Canceled
			case "budget":
				want = ErrDocumentResourceLimit
			case "replace":
				want = ErrConflict
			case "close", "nil":
				want = ErrIntegrity
			}
			if mode == "nil" {
				bind = nil
			}
			if mode == "pre-cancel" {
				cancel()
			}
			for steps := 0; steps < legacyDecoderCheckpointSteps; steps++ {
				ready, err := restore.stepSourceKeys(ctx, bind)
				if err != nil {
					expectedCalls := 1
					if mode == "nil" || mode == "pre-cancel" {
						expectedCalls = 0
					}
					if !errors.Is(err, want) || calls != expectedCalls {
						test.Fatal("callback failure did not stop at its binding boundary", err, calls)
					}
					if _, repeated := restore.stepSourceKeys(context.Background(), bind); repeated != err || calls != expectedCalls {
						test.Fatal("callback failure was not sticky", repeated, calls)
					}
					if retained, err := restore.retainedBytes(); err != nil || retained != 0 {
						test.Fatal("failed source retained binding workspace", retained, err)
					}
					return
				}
				if ready {
					test.Fatal("source restore ignored a binding failure")
				}
			}
			test.Fatal("binding failure did not make bounded progress")
		})
	}
}
