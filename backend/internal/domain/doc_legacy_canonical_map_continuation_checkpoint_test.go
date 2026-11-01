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

type legacyCanonicalMapContinuationTestCodec struct {
	values        []any
	refs          []legacyNativeCheckpointRef
	indices       map[ContentHash]int
	keys          map[legacyNativeCheckpointText]string
	keyObjects    map[ContentHash][]byte
	encoded       int
	decoded       int
	decodedKeys   int
	ownerBytes    int64
	callbackBytes int64
	budget        int64
	encodeError   error
	decodeError   error
	keyError      error
	checkError    error
	onEncode      func()
	onDecode      func()
	onDecodeKey   func()
}

func (codec *legacyCanonicalMapContinuationTestCodec) encode(ctx context.Context, value any) (legacyNativeCheckpointRef, error) {
	if err := ctx.Err(); err != nil {
		return legacyNativeCheckpointRef{}, err
	}
	if codec.encodeError != nil {
		return legacyNativeCheckpointRef{}, codec.encodeError
	}
	if codec.encoded >= len(codec.values) || !reflect.DeepEqual(value, codec.values[codec.encoded]) {
		return legacyNativeCheckpointRef{}, ErrIntegrity
	}
	ref := codec.refs[codec.encoded]
	codec.encoded++
	if codec.onEncode != nil {
		codec.onEncode()
	}
	return ref, nil
}

func (codec *legacyCanonicalMapContinuationTestCodec) decode(ctx context.Context, ref legacyNativeCheckpointRef) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if codec.decodeError != nil {
		return nil, codec.decodeError
	}
	index, found := codec.indices[ref.Hash]
	if !found || ref != codec.refs[index] {
		return nil, ErrIntegrity
	}
	codec.decoded++
	if codec.onDecode != nil {
		codec.onDecode()
	}
	return codec.values[index], nil
}

func (codec *legacyCanonicalMapContinuationTestCodec) check(ctx context.Context, helperBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if codec.checkError != nil {
		return codec.checkError
	}
	return legacyCanonicalPreparedValueBudget(codec.ownerBytes+codec.callbackBytes, helperBytes, codec.budget)
}

func (codec *legacyCanonicalMapContinuationTestCodec) decodeKey(ctx context.Context, text legacyNativeCheckpointText) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if codec.keyError != nil {
		return "", codec.keyError
	}
	key, found := codec.keys[text]
	if !found {
		return "", ErrIntegrity
	}
	codec.decodedKeys++
	if codec.onDecodeKey != nil {
		codec.onDecodeKey()
	}
	return key, nil
}

func legacyCanonicalMapContinuationCodecFixture(test *testing.T, fields []legacyCanonicalKeyField) *legacyCanonicalMapContinuationTestCodec {
	test.Helper()
	store, keyObjects := legacyNativeCheckpointStoreFixture(test)
	codec := &legacyCanonicalMapContinuationTestCodec{indices: make(map[ContentHash]int), keys: make(map[legacyNativeCheckpointText]string),
		keyObjects: keyObjects, budget: legacyDecoderCheckpointBudget}
	for index, field := range fields {
		ref := legacyNativeCheckpointRef{Hash: HashContent([]byte(fmt.Sprintf("map-suffix-value-%d", index))), Bytes: 8}
		codec.values = append(codec.values, field.value)
		codec.refs = append(codec.refs, ref)
		codec.indices[ref.Hash] = index
		key := field.key
		writer, err := newLegacyNativeCheckpointTextWriter(context.Background(), len(key), func(ctx context.Context, offset, length int) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []byte(key[offset : offset+length]), nil
		}, store)
		if err != nil {
			test.Fatal(err)
		}
		for steps := 0; steps < legacyDecoderCheckpointSteps && !writer.ready; steps++ {
			if _, err := writer.step(context.Background()); err != nil {
				test.Fatal(err)
			}
		}
		text, err := writer.take(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		codec.keys[text] = key
		codec.ownerBytes += int64(len(key))
	}
	codec.callbackBytes = int64(len(fields))*1024 + int64(len(keyObjects))*256
	for _, body := range keyObjects {
		codec.callbackBytes += int64(cap(body))
	}
	return codec
}

func legacyCanonicalMapContinuationCheckpointFixture(test *testing.T, count, consumed int, resumed bool) (*LegacyNormalizationPreparation, *legacyNormalizationCapture, []legacyCanonicalKeyField) {
	test.Helper()
	source := make(map[string]any, count)
	for index := 0; index < count; index++ {
		source[fmt.Sprintf("key-%04d", index)] = index
	}
	iterator := reflect.ValueOf(source).MapRange()
	for index := 0; index < consumed; index++ {
		if !iterator.Next() {
			test.Fatal("fixture consumed beyond its map")
		}
	}
	remaining := *iterator
	var fields []legacyCanonicalKeyField
	for remaining.Next() {
		fields = append(fields, legacyCanonicalKeyField{key: remaining.Key().String(), value: remaining.Value().Interface()})
	}
	frame := legacyCanonicalPreparationFrame{value: source, iterator: iterator, kind: 'o', count: count, position: consumed}
	if resumed {
		resume := &legacyCanonicalMapContinuation{position: consumed}
		for index := 0; index < consumed; index++ {
			if err := resume.fields.append(legacyCanonicalKeyField{key: "consumed-prefix-must-not-be-read", childKeys: -1}); err != nil {
				test.Fatal(err)
			}
		}
		for _, field := range fields {
			if err := resume.fields.append(field); err != nil {
				test.Fatal(err)
			}
		}
		frame.iterator, frame.mapResume = nil, resume
	}
	preparation := &legacyCanonicalPreparation{}
	if err := preparation.frames.append(frame); err != nil {
		test.Fatal(err)
	}
	stage := &LegacyNormalizationPreparation{mutation: &legacyNormalizationMutation{}, phase: legacyNormalizationStage,
		pending: []LegacyNormalizedEvent{{Encoding: &LegacyCanonicalEncoding{preparation: preparation}}}}
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(grant.release)
	return stage, grant, fields
}

func legacyCanonicalMapContinuationCheckpointDrain(test *testing.T, work *legacyCanonicalMapContinuationCheckpoint) legacyNativeCheckpointRef {
	test.Helper()
	for steps := 0; steps < legacyDecoderCheckpointSteps; steps++ {
		position := work.position
		ready, err := work.step(context.Background())
		if err != nil || work.position-position > 1 {
			test.Fatal("suffix writer failed or advanced more than one entry", err)
		}
		if ready {
			ref, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			return ref
		}
	}
	test.Fatal("suffix writer exceeded the existing step allowance")
	return legacyNativeCheckpointRef{}
}

func legacyCanonicalMapContinuationRestoreDrain(test *testing.T, work *legacyCanonicalMapContinuationRestore) legacyCanonicalMapContinuationRestored {
	test.Helper()
	for steps := 0; steps < legacyDecoderCheckpointSteps; steps++ {
		count := work.result.continuation.fields.length()
		ready, err := work.step(context.Background())
		if err != nil || work.result.continuation.fields.length()-count > 1 {
			test.Fatal("suffix restore failed or escaped its entry quantum", err)
		}
		if ready {
			result, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			return result
		}
	}
	test.Fatal("suffix restore exceeded the existing step allowance")
	return legacyCanonicalMapContinuationRestored{}
}

func TestLegacyCanonicalMapContinuationCheckpointSuffixOnlyRoundTrip(test *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, count := range []int{0, 1, 3, 65, 1100} {
			test.Run(fmt.Sprintf("resume-%t/count-%d", resumed, count), func(test *testing.T) {
				consumed := min(count/2, 7)
				stage, grant, fields := legacyCanonicalMapContinuationCheckpointFixture(test, count, consumed, resumed)
				codec := legacyCanonicalMapContinuationCodecFixture(test, fields)
				store, _ := legacyNativeCheckpointStoreFixture(test)
				binding := HashContent([]byte("suffix-only-source-occurrence"))
				work, err := newLegacyCanonicalMapContinuationCheckpoint(context.Background(), stage, 0, grant, codec, binding, store, legacyDecoderCheckpointBudget)
				if err != nil {
					test.Fatal(err)
				}
				ref := legacyCanonicalMapContinuationCheckpointDrain(test, work)
				frame := stage.pending[0].Encoding.preparation.frames.at(0)
				if frame.position != consumed || codec.encoded != len(fields) || frame.mapResume != nil && frame.mapResume.position != consumed {
					test.Fatal("writer advanced source state or visited consumed prefix")
				}
				restore, err := newLegacyCanonicalMapContinuationRestore(context.Background(), ref, binding, count, consumed, grant, codec, store, legacyDecoderCheckpointBudget)
				if err != nil {
					test.Fatal(err)
				}
				result := legacyCanonicalMapContinuationRestoreDrain(test, restore)
				if result.continuation.position != 0 || result.continuation.fields.length() != len(fields) || codec.decoded != len(fields) || codec.decodedKeys != len(fields) {
					test.Fatal("restore changed suffix geometry")
				}
				for index, field := range fields {
					if !reflect.DeepEqual(*result.continuation.fields.at(index), field) || unsafe.StringData(result.continuation.fields.at(index).key) != unsafe.StringData(field.key) {
						test.Fatal("suffix order or values changed", index)
					}
				}
			})
		}
	}
}

func TestLegacyCanonicalMapContinuationCheckpointSegmentedKeysBorrowedValues(test *testing.T) {
	stage, grant, _ := legacyCanonicalMapContinuationCheckpointFixture(test, 5, 0, true)
	frame := stage.pending[0].Encoding.preparation.frames.at(0)
	number := json.Number("UNVISITED_PAYLOAD-invalid-number")
	nested := map[string]any{"not-prepared": []any{number}}
	values := []any{nested, []any{nested}, number, &number, nil}
	keys := []string{"", "\ud55c🙂", strings.Repeat("a", LegacyCanonicalEncodingBytes-1) + strings.Repeat("🙂\ud55c", LegacyCanonicalEncodingBytes/2), "number-pointer", "nil"}
	var fields []legacyCanonicalKeyField
	for index, key := range keys {
		field := legacyCanonicalKeyField{key: key, value: values[index]}
		*frame.mapResume.fields.at(index) = field
		fields = append(fields, field)
	}
	codec := legacyCanonicalMapContinuationCodecFixture(test, fields)
	codec.ownerBytes = int64(len(keys[2])) + 4096
	store, objects := legacyNativeCheckpointStoreFixture(test)
	put, read := store.Put, store.Read
	stepBytes, maximumBytes := 0, 0
	store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		stepBytes += len(body)
		return put(ctx, hash, body)
	}
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		stepBytes += length
		return read(ctx, hash, length)
	}
	binding := HashContent([]byte("segmented-key-source"))
	work, err := newLegacyCanonicalMapContinuationCheckpoint(context.Background(), stage, 0, grant, codec, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	for steps := 0; steps < legacyDecoderCheckpointSteps && !work.ready; steps++ {
		stepBytes = 0
		if _, err := work.step(context.Background()); err != nil {
			test.Fatal(err)
		}
		maximumBytes = max(maximumBytes, stepBytes)
	}
	ref, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	for _, body := range objects {
		if bytes.Contains(body, []byte("UNVISITED_PAYLOAD")) {
			test.Fatal("suffix codec serialized an unvisited value")
		}
	}
	keyReads := 0
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		if _, keyObject := codec.keyObjects[hash]; keyObject {
			keyReads++
			return nil, ErrIntegrity
		}
		stepBytes += length
		return read(ctx, hash, length)
	}
	restore, err := newLegacyCanonicalMapContinuationRestore(context.Background(), ref, binding, 5, 0, grant, codec, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	for steps := 0; steps < legacyDecoderCheckpointSteps && !restore.ready; steps++ {
		stepBytes = 0
		if restore.phase == 2 && restore.entry.Key.Bytes == len(keys[2]) {
			before, err := restore.retainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			allocations := testing.AllocsPerRun(1, func() {
				restore.phase = 2
				if ready, err := restore.step(context.Background()); err != nil || ready {
					test.Fatal("borrowed key step failed", err)
				}
			})
			after, err := restore.retainedBytes()
			if err != nil || allocations != 0 || after != before || stepBytes != 0 || unsafe.StringData(restore.field.key) != unsafe.StringData(keys[2]) {
				test.Fatal("borrowed huge key allocated, read CAS, or lost identity", allocations, before, after, stepBytes, err)
			}
		}
		if _, err := restore.step(context.Background()); err != nil {
			test.Fatal(err)
		}
		maximumBytes = max(maximumBytes, stepBytes)
	}
	result, err := restore.take(context.Background())
	if err != nil || maximumBytes > legacyNativeAnyCheckpointScratch || keyReads != 0 {
		test.Fatal("segmentation or borrowed key resolution failed", err, maximumBytes, keyReads)
	}
	for index, field := range fields {
		got := result.continuation.fields.at(index)
		if len(got.key) != len(field.key) || unsafe.StringData(got.key) != unsafe.StringData(field.key) || !reflect.DeepEqual(got.value, field.value) {
			test.Fatal("segmented key or untouched value changed", index)
		}
	}
	if reflect.ValueOf(result.continuation.fields.at(0).value).Pointer() != reflect.ValueOf(nested).Pointer() ||
		result.continuation.fields.at(3).value != &number {
		test.Fatal("resolver payload was copied, replaced by counts, or numerically admitted")
	}
}

func TestLegacyCanonicalMapContinuationCheckpointActualPreparationHelperHandoff(test *testing.T) {
	stage, storage, canonical, original, encoding, index := legacyCanonicalMapContinuationFixture(test)
	frame := encoding.preparation.frames.at(index)
	iterator := *frame.iterator
	var fields []legacyCanonicalKeyField
	for iterator.Next() {
		fields = append(fields, legacyCanonicalKeyField{key: iterator.Key().String(), value: iterator.Value().Interface()})
	}
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if grant != nil {
			grant.release()
		}
	})
	codec := legacyCanonicalMapContinuationCodecFixture(test, fields)
	codec.ownerBytes, err = stage.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	var captureHeap docRangeHeaderHeapEstimate
	captureHeap.allocation(1, int64(unsafe.Sizeof(*grant)))
	codec.ownerBytes += captureHeap.bytes
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent([]byte("actual-preparation-suffix-helper-not-whole-cursor"))
	position, inspectWork, written := frame.position, encoding.inspectWork, stage.written
	work, err := newLegacyCanonicalMapContinuationCheckpoint(context.Background(), stage, index, grant, codec, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	ref := legacyCanonicalMapContinuationCheckpointDrain(test, work)
	restore, err := newLegacyCanonicalMapContinuationRestore(context.Background(), ref, binding, frame.count, position, grant, codec, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	result := legacyCanonicalMapContinuationRestoreDrain(test, restore)
	if frame.position != position || encoding.inspectWork != inspectWork || stage.written != written {
		test.Fatal("suffix helper replayed preparation or output")
	}
	if _, err := stage.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("borrowed capture did not freeze the actual owner", err)
	}
	frame.iterator, frame.mapResume = nil, result.continuation
	grant.release()
	grant = nil
	complete := false
	for steps := 0; steps < legacyDecoderCheckpointSteps && !complete; steps++ {
		complete, err = stage.Step(context.Background())
		if err != nil {
			test.Fatal(err)
		}
	}
	if !complete {
		test.Fatal("actual preparation did not finish after suffix-only handoff")
	}
	legacyNormalizationPreparationCheck(test, stage, storage, canonical, original)
}
