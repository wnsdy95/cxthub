package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const legacyNativeCheckpointModelsW79Budget int64 = 16 << 20
const legacyNativeCheckpointModelsW79Steps = 2048

type legacyNativeCheckpointModelsW79Work interface {
	step(context.Context) (bool, error)
	retainedBytes() (int64, error)
	close()
}

type legacyNativeCheckpointModelsW79Writer interface {
	legacyNativeCheckpointModelsW79Work
	take(context.Context) (legacyNativeCheckpointRef, error)
}

type legacyNativeCheckpointModelsW79Restore interface {
	legacyNativeCheckpointModelsW79Work
	take(context.Context) (*json.UnmarshalTypeError, error)
}

type legacyNativeCheckpointModelsW79Store struct {
	store   legacyNativeCheckpointIO
	objects map[ContentHash][]byte
	reads   int
	writes  int
	quantum int
}

func legacyNativeCheckpointModelsW79Storage(test *testing.T) *legacyNativeCheckpointModelsW79Store {
	test.Helper()
	store, objects := legacyNativeCheckpointStoreFixture(test)
	return &legacyNativeCheckpointModelsW79Store{store: store, objects: objects}
}

func (storage *legacyNativeCheckpointModelsW79Store) io() legacyNativeCheckpointIO {
	return legacyNativeCheckpointIO{
		Put: func(ctx context.Context, hash ContentHash, body []byte) error {
			storage.writes++
			storage.quantum += len(body)
			if len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes {
				return ErrIntegrity
			}
			return storage.store.Put(ctx, hash, body)
		},
		Read: func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
			storage.reads++
			storage.quantum += length
			if length <= 0 || length > LegacyCanonicalEncodingBytes {
				return nil, ErrIntegrity
			}
			return storage.store.Read(ctx, hash, length)
		},
	}
}

func legacyNativeCheckpointModelsW79Released(test *testing.T, work legacyNativeCheckpointModelsW79Work) {
	test.Helper()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatalf("terminal work retained=%d error=%v", retained, err)
	}
	work.close()
	work.close()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatalf("closed work retained=%d error=%v", retained, err)
	}
}

func legacyNativeCheckpointModelsW79Drain(test *testing.T, work legacyNativeCheckpointModelsW79Work, storage *legacyNativeCheckpointModelsW79Store) {
	test.Helper()
	for attempt := 0; attempt < legacyNativeCheckpointModelsW79Steps; attempt++ {
		storage.quantum = 0
		ctx, cancel := context.WithCancel(context.Background())
		ready, err := work.step(ctx)
		cancel()
		retained, heapErr := work.retainedBytes()
		if err != nil || heapErr != nil || retained < 0 || retained > legacyNativeCheckpointModelsW79Budget || storage.quantum > 512<<10 {
			test.Fatalf("step=%d ready=%v retained=%d IO=%d error=%v heap=%v", attempt, ready, retained, storage.quantum, err, heapErr)
		}
		if ready {
			return
		}
	}
	test.Fatal("error transport exceeded finite step schedule")
}

func legacyNativeCheckpointModelsW79Capture(test *testing.T, semantic *json.UnmarshalTypeError, sourceBytes int, storage *legacyNativeCheckpointModelsW79Store) legacyNativeCheckpointRef {
	test.Helper()
	before := *semantic
	ctx, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeCheckpointTypeErrorWriter(ctx, semantic, storage.io(), sourceBytes, legacyNativeCheckpointModelsW79Budget)
	cancel()
	if err != nil {
		test.Fatal("capture constructor", err)
	}
	defer work.close()
	legacyNativeCheckpointModelsW79Drain(test, work, storage)
	ref, err := work.take(context.Background())
	if err != nil || !ref.valid() || *semantic != before {
		test.Fatal("capture lost immutable caller error or root", ref, err)
	}
	legacyNativeCheckpointModelsW79Released(test, work)
	return ref
}

func legacyNativeCheckpointModelsW79Recover(test *testing.T, ref legacyNativeCheckpointRef, sourceBytes int, storage *legacyNativeCheckpointModelsW79Store) *json.UnmarshalTypeError {
	test.Helper()
	reads, writes := storage.reads, storage.writes
	ctx, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeCheckpointTypeErrorRestore(ctx, ref, storage.io(), sourceBytes, legacyNativeCheckpointModelsW79Budget)
	cancel()
	if err != nil {
		test.Fatal("restore constructor", err)
	}
	defer work.close()
	legacyNativeCheckpointModelsW79Drain(test, work, storage)
	semantic, err := work.take(context.Background())
	if err != nil || semantic == nil || storage.reads <= reads || storage.writes != writes {
		test.Fatal("restore did not freshly read immutable error state", semantic, err, storage.reads-reads, storage.writes-writes)
	}
	legacyNativeCheckpointModelsW79Released(test, work)
	return semantic
}

func legacyNativeCheckpointModelsW79Equal(test *testing.T, actual, expected *json.UnmarshalTypeError) {
	test.Helper()
	if actual == nil || expected == nil || actual == expected || *actual != *expected || actual.Error() != expected.Error() {
		test.Fatalf("error identity/fields changed: got=%+v want=%+v", actual, expected)
	}
	var classified *json.UnmarshalTypeError
	if !errors.As(error(actual), &classified) || classified != actual || reflect.TypeOf(error(actual)) != reflect.TypeOf(error(expected)) {
		test.Fatal("restored error changed its concrete semantic class")
	}
}

func legacyNativeCheckpointModelsW79Sticky(test *testing.T, work legacyNativeCheckpointModelsW79Work, failure error, take func() error) {
	test.Helper()
	if failure == nil {
		test.Fatal("missing expected failure")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if ready, err := work.step(context.Background()); ready || err != failure {
			test.Fatal("failure was not sticky", ready, err, failure)
		}
		if err := take(); err != failure {
			test.Fatal("take did not preserve sticky failure", err, failure)
		}
		legacyNativeCheckpointModelsW79Released(test, work)
	}
}

func legacyNativeCheckpointModelsW79RejectRestore(test *testing.T, ref legacyNativeCheckpointRef, store legacyNativeCheckpointIO, sourceBytes int, budget int64, expected error) {
	test.Helper()
	work, err := newLegacyNativeCheckpointTypeErrorRestore(context.Background(), ref, store, sourceBytes, budget)
	if err != nil {
		if expected != nil && !errors.Is(err, expected) {
			test.Fatal("unexpected constructor rejection", err, expected)
		}
		if work != nil {
			legacyNativeCheckpointModelsW79Released(test, work)
		}
		return
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeCheckpointModelsW79Steps; attempt++ {
		ready, failure := work.step(context.Background())
		if failure != nil {
			if expected != nil && !errors.Is(failure, expected) {
				test.Fatal("unexpected restore rejection", failure, expected)
			}
			legacyNativeCheckpointModelsW79Sticky(test, work, failure, func() error {
				semantic, takeErr := work.take(context.Background())
				if semantic != nil {
					test.Fatal("failed restore exposed an error allocation")
				}
				return takeErr
			})
			return
		}
		if ready {
			test.Fatal("invalid error transport became ready")
		}
	}
	test.Fatal("invalid error transport did not terminate")
}

func TestLegacyNativeCheckpointModelsW79Binding(test *testing.T) {
	schemas := make(map[ContentHash]uint8)
	models := []struct {
		identity uint8
		model    reflect.Type
	}{
		{identity: 1, model: reflect.TypeFor[CIREnvelope]()},
		{identity: 2, model: reflect.TypeFor[wire]()},
		{identity: 3, model: reflect.TypeFor[ContentBlock]()},
		{identity: 4, model: reflect.TypeFor[LockedBlob]()},
		{identity: 5, model: reflect.TypeFor[ProviderMetadata]()},
	}
	for _, fixture := range models {
		test.Run(fixture.model.Name(), func(test *testing.T) {
			registered := legacyNativeCheckpointModelMetadata(fixture.identity)
			if registered == nil || registered.model != fixture.model || registered.err != nil {
				test.Fatal("closed model ID changed concrete type", fixture.identity, registered)
			}
			schema, err := legacyNativeCheckpointModelSchema(registered)
			if err != nil || ValidateContentHash(schema) != nil || schemas[schema] != 0 {
				test.Fatal("closed model schema invalid or shared by different models", schema, err)
			}
			schemas[schema] = fixture.identity
			fresh := newLegacyNativeObjectFields(fixture.model)
			copied := *registered
			copied.fields = append([]legacyNativeObjectField(nil), registered.fields...)
			for _, metadata := range []*legacyNativeObjectFields{registered, &fresh, &copied} {
				identity, err := legacyNativeCheckpointModel(metadata)
				if err != nil || identity != fixture.identity || !reflect.DeepEqual(*metadata, *registered) {
					test.Fatal("valid independently owned metadata rejected", identity, err)
				}
				if copiedSchema, err := legacyNativeCheckpointModelSchema(metadata); err != nil || copiedSchema != schema {
					test.Fatal("schema depends on metadata pointer/backing identity", copiedSchema, schema, err)
				}
			}
			mutations := []struct {
				name   string
				change func(*legacyNativeObjectFields)
			}{
				{name: "nil-concrete-type", change: func(metadata *legacyNativeObjectFields) { metadata.model = nil }},
				{name: "metadata-error", change: func(metadata *legacyNativeObjectFields) { metadata.err = ErrIntegrity }},
				{name: "key-too-small", change: func(metadata *legacyNativeObjectFields) { metadata.keyBytes-- }},
				{name: "key-too-large", change: func(metadata *legacyNativeObjectFields) { metadata.keyBytes++ }},
				{name: "raw-too-small", change: func(metadata *legacyNativeObjectFields) { metadata.rawBytes-- }},
				{name: "raw-too-large", change: func(metadata *legacyNativeObjectFields) { metadata.rawBytes++ }},
				{name: "missing-field", change: func(metadata *legacyNativeObjectFields) { metadata.fields = metadata.fields[:len(metadata.fields)-1] }},
				{name: "extra-field", change: func(metadata *legacyNativeObjectFields) {
					metadata.fields = append(metadata.fields, metadata.fields[0])
				}},
				{name: "field-order", change: func(metadata *legacyNativeObjectFields) {
					metadata.fields[0], metadata.fields[1] = metadata.fields[1], metadata.fields[0]
				}},
			}
			for _, mutation := range mutations {
				test.Run(mutation.name, func(test *testing.T) {
					metadata := *registered
					metadata.fields = append([]legacyNativeObjectField(nil), registered.fields...)
					mutation.change(&metadata)
					if identity, err := legacyNativeCheckpointModel(&metadata); identity != 0 || !errors.Is(err, ErrIntegrity) {
						test.Fatal("changed schema accepted", identity, err)
					}
					if changedSchema, err := legacyNativeCheckpointModelSchema(&metadata); changedSchema != "" || !errors.Is(err, ErrIntegrity) {
						test.Fatal("invalid metadata received schema authority", changedSchema, err)
					}
				})
			}
			fieldMutations := []struct {
				name   string
				change func(*legacyNativeObjectField)
			}{
				{name: "name", change: func(field *legacyNativeObjectField) { field.name += "_other" }},
				{name: "prefix", change: func(field *legacyNativeObjectField) { field.prefix += " " }},
				{name: "index", change: func(field *legacyNativeObjectField) { field.index++ }},
				{name: "scalar-string", change: func(field *legacyNativeObjectField) { field.scalarString = !field.scalarString }},
				{name: "interface-string", change: func(field *legacyNativeObjectField) { field.interfaceString = !field.interfaceString }},
				{name: "string-list", change: func(field *legacyNativeObjectField) { field.stringList = !field.stringList }},
				{name: "block-list", change: func(field *legacyNativeObjectField) { field.blockList = !field.blockList }},
				{name: "event-list", change: func(field *legacyNativeObjectField) { field.eventList = !field.eventList }},
				{name: "fixed-scalar", change: func(field *legacyNativeObjectField) { field.fixedScalar = !field.fixedScalar }},
				{name: "nested-object", change: func(field *legacyNativeObjectField) { field.nestedObject = !field.nestedObject }},
				{name: "generic-map", change: func(field *legacyNativeObjectField) { field.genericMap = !field.genericMap }},
				{name: "typed-number", change: func(field *legacyNativeObjectField) { field.typedNumber = !field.typedNumber }},
			}
			for fieldIndex := range registered.fields {
				for _, mutation := range fieldMutations {
					test.Run(fmt.Sprintf("field-%d-%s", fieldIndex, mutation.name), func(test *testing.T) {
						metadata := *registered
						metadata.fields = append([]legacyNativeObjectField(nil), registered.fields...)
						mutation.change(&metadata.fields[fieldIndex])
						if identity, err := legacyNativeCheckpointModel(&metadata); identity != 0 || !errors.Is(err, ErrIntegrity) {
							test.Fatal("changed field descriptor accepted", identity, err)
						}
						if changedSchema, err := legacyNativeCheckpointModelSchema(&metadata); changedSchema != "" || !errors.Is(err, ErrIntegrity) {
							test.Fatal("changed field flag/name/index received schema authority", changedSchema, err)
						}
					})
				}
			}
			if !reflect.DeepEqual(newLegacyNativeObjectFields(fixture.model), *registered) {
				test.Fatal("oracle mutated global model metadata")
			}
		})
	}
	if identity, err := legacyNativeCheckpointModel(nil); identity != 0 || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil metadata accepted", identity, err)
	}
	if schema, err := legacyNativeCheckpointModelSchema(nil); schema != "" || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil metadata received schema authority", schema, err)
	}
	for identity := 0; identity <= 255; identity++ {
		if identity >= 1 && identity <= 5 {
			continue
		}
		if metadata := legacyNativeCheckpointModelMetadata(uint8(identity)); metadata != nil {
			test.Fatal("unknown model ID resolved", identity, metadata)
		}
	}
	test.Run("same-name-different-type", func(test *testing.T) {
		type ContentBlock struct {
			Type string `json:"type"`
			Text bool   `json:"text"`
		}
		metadata := newLegacyNativeObjectFields(reflect.TypeFor[ContentBlock]())
		if metadata.model.Name() != legacyContentBlockNativeFields.model.Name() {
			test.Fatal("same-name fixture lost its collision")
		}
		if identity, err := legacyNativeCheckpointModel(&metadata); identity != 0 || !errors.Is(err, ErrIntegrity) {
			test.Fatal("type-name-derived reconstruction accepted", identity, err)
		}
		metadata.fields = append([]legacyNativeObjectField(nil), legacyContentBlockNativeFields.fields...)
		metadata.keyBytes, metadata.rawBytes = legacyContentBlockNativeFields.keyBytes, legacyContentBlockNativeFields.rawBytes
		if identity, err := legacyNativeCheckpointModel(&metadata); identity != 0 || !errors.Is(err, ErrIntegrity) {
			test.Fatal("forged registered schema hid an unknown concrete type", identity, err)
		}
		if schema, err := legacyNativeCheckpointModelSchema(&metadata); schema != "" || !errors.Is(err, ErrIntegrity) {
			test.Fatal("same-name unknown type received schema authority", schema, err)
		}
	})
	for _, model := range []reflect.Type{reflect.TypeFor[CIREvent](), reflect.TypeFor[*ContentBlock](), reflect.TypeFor[struct{}](), reflect.TypeFor[string]()} {
		metadata := newLegacyNativeObjectFields(model)
		if identity, err := legacyNativeCheckpointModel(&metadata); identity != 0 || !errors.Is(err, ErrIntegrity) {
			test.Fatal("unregistered concrete model accepted", model, identity, err)
		}
	}
	test.Run("actual-leaf-root-schema-before-dependencies", legacyNativeCheckpointModelsW79SchemaGuards)
}

func legacyNativeCheckpointModelsW79SchemaGuards(test *testing.T) {
	for _, fixture := range []struct {
		model uint8
		raw   string
	}{
		{model: 3, raw: `{"type":"text","text":"payload"}`},
		{model: 4, raw: `{"provider":"codex","scheme":"encrypted_content","blob":"payload"}`},
		{model: 5, raw: `{"turn_id":"turn","create_time":23}`},
	} {
		test.Run(fmt.Sprint(fixture.model), func(test *testing.T) {
			request, source := legacyNativeAnyW78FullSource(test, fixture.raw)
			metadata := legacyNativeCheckpointModelMetadata(fixture.model)
			native, err := newLegacyNativeObject(reflect.New(metadata.model), metadata, 1, false)
			if err != nil {
				test.Fatal(err)
			}
			native.fieldLimit = 1
			native.start(0, len(fixture.raw))
			var scanner legacyJSONValueScanner
			input := &legacyNormalizationInput{ctx: context.Background(), source: source}
			if ready, err := native.step(input.ctx, input, &scanner); ready || err != nil || input.failure != nil || native.phase == legacyNativeObjectDone {
				test.Fatal("leaf schema fixture did not reach a real pending object", ready, err, input.failure, native.phase)
			}
			storage := legacyNativeCheckpointModelsW79Storage(test)
			capture, err := newLegacyNativeObjectCheckpoint(context.Background(), native, &scanner, request.Binding, source, storage.io(), legacyNativeCheckpointModelsW79Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer capture.close()
			legacyNativeCheckpointModelsW79Drain(test, capture, storage)
			ref, err := capture.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeCheckpointModelsW79Released(test, capture)
			var state legacyNativeObjectCheckpointControl
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), ref, &state); err != nil {
				test.Fatal(err)
			}
			schema, err := legacyNativeCheckpointModelSchema(metadata)
			if err != nil || state.Schema != schema {
				test.Fatal("captured leaf did not bind exact model schema", state.Schema, schema, err)
			}
			_, fresh := legacyNativeAnyW78FullSource(test, fixture.raw)
			if source.authority == fresh.authority {
				test.Fatal("schema restore fixture reused old source authority")
			}
			otherModel := uint8(3)
			if fixture.model == otherModel {
				otherModel = 4
			}
			otherSchema, err := legacyNativeCheckpointModelSchema(legacyNativeCheckpointModelMetadata(otherModel))
			if err != nil {
				test.Fatal(err)
			}
			for _, mutation := range []struct {
				name          string
				change        func(*legacyNativeObjectCheckpointControl)
				expectedModel uint8
			}{
				{name: "missing-schema", expectedModel: fixture.model, change: func(control *legacyNativeObjectCheckpointControl) { control.Schema = "" }},
				{name: "invalid-schema", expectedModel: fixture.model, change: func(control *legacyNativeObjectCheckpointControl) { control.Schema = "not-a-content-hash" }},
				{name: "different-schema", expectedModel: fixture.model, change: func(control *legacyNativeObjectCheckpointControl) { control.Schema = otherSchema }},
				{name: "different-layout-hash", expectedModel: fixture.model, change: func(control *legacyNativeObjectCheckpointControl) {
					control.Schema = HashContent([]byte("changed JSON tag/field layout"))
				}},
				{name: "wrong-expected-model", expectedModel: otherModel, change: func(*legacyNativeObjectCheckpointControl) {}},
				{name: "unknown-expected-model", expectedModel: 255, change: func(*legacyNativeObjectCheckpointControl) {}},
				{name: "envelope-not-leaf", expectedModel: 1, change: func(*legacyNativeObjectCheckpointControl) {}},
				{name: "event-not-leaf", expectedModel: 2, change: func(*legacyNativeObjectCheckpointControl) {}},
				{name: "changed-model", expectedModel: fixture.model, change: func(control *legacyNativeObjectCheckpointControl) { control.Model = otherModel }},
			} {
				test.Run(mutation.name, func(test *testing.T) {
					changed := state
					mutation.change(&changed)
					changedRef, err := legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), changed)
					if err != nil {
						test.Fatal(err)
					}
					reads, writes := storage.reads, storage.writes
					work, failure := newLegacyNativeObjectCheckpointRestore(context.Background(), changedRef, mutation.expectedModel, storage.io(), fresh, request.Binding, legacyNativeCheckpointModelsW79Budget)
					if failure != nil {
						if !errors.Is(failure, ErrIntegrity) {
							test.Fatal(failure)
						}
						if work != nil {
							legacyNativeCheckpointModelsW79Released(test, work)
						}
					} else {
						defer work.close()
						for attempt := 0; attempt < legacyNativeCheckpointModelsW79Steps; attempt++ {
							ready, err := work.step(context.Background())
							if err != nil {
								failure = err
								break
							}
							if ready {
								test.Fatal("wrong schema/model became ready")
							}
						}
						if !errors.Is(failure, ErrIntegrity) {
							test.Fatal("schema rejection did not terminate", failure)
						}
						legacyNativeCheckpointModelsW79Sticky(test, work, failure, func() error {
							result, err := work.take(context.Background())
							if result.Object != nil || result.Source.Read != nil {
								test.Fatal("schema failure exposed restored native/authority")
							}
							return err
						})
					}
					if storage.reads-reads > 1 || storage.writes != writes {
						test.Fatal("schema/model rejection loaded dependent objects", storage.reads-reads, storage.writes-writes)
					}
				})
			}
		})
	}
}

func TestLegacyNativeCheckpointModelsW79ErrorTypes(test *testing.T) {
	types := []reflect.Type{
		reflect.TypeFor[string](), reflect.TypeFor[ProviderKind](), reflect.TypeFor[FidelityTier](),
		reflect.TypeFor[EventKind](), reflect.TypeFor[Role](), reflect.TypeFor[int](), reflect.TypeFor[bool](),
		reflect.TypeFor[any](), reflect.TypeFor[map[string]any](), reflect.TypeFor[[]ContentBlock](),
		reflect.TypeFor[[]CIREvent](), reflect.TypeFor[*bool](), reflect.TypeFor[*LockedBlob](),
		reflect.TypeFor[*ProviderMetadata](), reflect.TypeFor[*json.Number](), reflect.TypeFor[json.Number](),
		reflect.TypeFor[wire](), reflect.TypeFor[CIREnvelope](), reflect.TypeFor[ContentBlock](),
		reflect.TypeFor[LockedBlob](), reflect.TypeFor[ProviderMetadata](), reflect.TypeFor[float64](),
		reflect.TypeFor[[]string](), reflect.TypeFor[[]any](),
	}
	for index, model := range types {
		test.Run(fmt.Sprintf("type-%02d-%s", index+1, model), func(test *testing.T) {
			identity, err := legacyNativeCheckpointErrorTypeID(model)
			if err != nil || identity != uint8(index+1) {
				test.Fatal("closed type ID changed", identity, err)
			}
			decoded, err := legacyNativeCheckpointErrorType(identity)
			if err != nil || decoded != model {
				test.Fatal("closed error type changed concrete identity", decoded, model, err)
			}
			for _, offset := range []int64{0, 37, 2*4096 + (64 << 10)} {
				semantic := &json.UnmarshalTypeError{Value: "number 1e999", Type: model, Offset: offset, Struct: "Outer🙂", Field: "nested.필드"}
				storage := legacyNativeCheckpointModelsW79Storage(test)
				ref := legacyNativeCheckpointModelsW79Capture(test, semantic, 4096, storage)
				legacyNativeCheckpointModelsW79Equal(test, legacyNativeCheckpointModelsW79Recover(test, ref, 4096, storage), semantic)
			}
		})
	}
	for identity := 0; identity <= 255; identity++ {
		if identity >= 1 && identity <= len(types) {
			continue
		}
		if model, err := legacyNativeCheckpointErrorType(uint8(identity)); model != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("unknown error type ID accepted", identity, model, err)
		}
	}
	type ProviderKind string
	for _, model := range []reflect.Type{nil, reflect.TypeFor[ProviderKind](), reflect.TypeFor[int64](), reflect.TypeFor[*string](), reflect.TypeFor[map[string]string](), reflect.TypeFor[CIREvent](), reflect.TypeFor[chan int]()} {
		if identity, err := legacyNativeCheckpointErrorTypeID(model); identity != 0 || !errors.Is(err, ErrIntegrity) {
			test.Fatal("unknown/nil concrete error type accepted", model, identity, err)
		}
	}
}

func legacyNativeCheckpointModelsW79Deferred(test *testing.T, raw string, destination any) *json.UnmarshalTypeError {
	test.Helper()
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(destination).Elem())
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 1, false)
	if err != nil {
		test.Fatal(err)
	}
	native.deferTypeErrors, native.fieldLimit = true, 1
	native.start(0, len(raw))
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := buffer.input()
	var scanner legacyJSONValueScanner
	var remembered *json.UnmarshalTypeError
	pending := false
	for attempt := 0; attempt < 128; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		ready, failure := native.step(ctx, input, &scanner)
		cancel()
		if input.failure != nil {
			test.Fatal("deferred fixture source failure", input.failure)
		}
		if remembered != nil && native.typeError != remembered {
			test.Fatal("later native field replaced first deferred type error")
		}
		if native.typeError != nil {
			remembered = native.typeError
			pending = pending || native.phase != legacyNativeObjectDone
		}
		if failure != nil {
			var semantic *json.UnmarshalTypeError
			if ready || !pending || native.phase != legacyNativeObjectDone || !errors.As(failure, &semantic) || semantic != remembered {
				test.Fatal("fixture did not execute a real deferred native error", ready, pending, native.phase, failure)
			}
			return semantic
		}
		if ready {
			test.Fatal("deferred native error disappeared")
		}
	}
	test.Fatal("deferred fixture exceeded finite native schedule")
	return nil
}

func TestLegacyNativeCheckpointModelsW79DeferredErrors(test *testing.T) {
	fixtures := []struct {
		name  string
		model reflect.Type
		raw   string
	}{
		{name: "block-number-first-array-later", model: reflect.TypeFor[ContentBlock](), raw: `{"text":17,"type":"text","text":[],"type":"later"}`},
		{name: "locked-enum-bool", model: reflect.TypeFor[LockedBlob](), raw: `{"provider":false,"scheme":[],"blob":"kept","scheme":"signature"}`},
		{name: "provider-typed-number-bool", model: reflect.TypeFor[ProviderMetadata](), raw: `{"create_time":true,"turn_id":"kept","create_time":23}`},
		{name: "int-string", model: reflect.TypeFor[CIREnvelope](), raw: `{"context_tokens":"bad","source_model":"kept"}`},
		{name: "string-list-object", model: reflect.TypeFor[CIREnvelope](), raw: `{"source_models":{},"source_model":"kept"}`},
		{name: "fidelity-array", model: reflect.TypeFor[CIREnvelope](), raw: `{"fidelity":[],"source_model":"kept"}`},
		{name: "event-kind-object", model: reflect.TypeFor[wire](), raw: `{"kind":{},"id":"kept"}`},
		{name: "role-array", model: reflect.TypeFor[wire](), raw: `{"role":[],"id":"kept"}`},
		{name: "fixed-bool-string", model: reflect.TypeFor[wire](), raw: `{"compact_summary":"bad","id":"kept"}`},
		{name: "pointer-bool-array", model: reflect.TypeFor[wire](), raw: `{"is_error":[],"id":"kept"}`},
		{name: "block-list-object", model: reflect.TypeFor[wire](), raw: `{"blocks":{},"id":"kept"}`},
		{name: "event-list-object", model: reflect.TypeFor[wire](), raw: `{"replacement":{},"id":"kept"}`},
		{name: "locked-pointer-array", model: reflect.TypeFor[wire](), raw: `{"locked":[],"id":"kept"}`},
		{name: "provider-pointer-array", model: reflect.TypeFor[wire](), raw: `{"provider_metadata":[],"id":"kept"}`},
		{name: "map-array", model: reflect.TypeFor[wire](), raw: `{"input":[],"id":"kept"}`},
		{name: "any-number-overflow", model: reflect.TypeFor[wire](), raw: `{"output":1e999,"id":"kept"}`},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			expected, actual := reflect.New(fixture.model), reflect.New(fixture.model)
			var standard *json.UnmarshalTypeError
			if failure := json.Unmarshal([]byte(fixture.raw), expected.Interface()); !errors.As(failure, &standard) {
				test.Fatal("independent JSON decoder did not produce real type error", failure)
			}
			deferred := legacyNativeCheckpointModelsW79Deferred(test, fixture.raw, actual.Interface())
			if !reflect.DeepEqual(actual.Interface(), expected.Interface()) {
				test.Fatalf("native deferred continuation changed later values: got=%#v want=%#v", actual.Interface(), expected.Interface())
			}
			for _, semantic := range []*json.UnmarshalTypeError{standard, deferred} {
				storage := legacyNativeCheckpointModelsW79Storage(test)
				ref := legacyNativeCheckpointModelsW79Capture(test, semantic, len(fixture.raw), storage)
				legacyNativeCheckpointModelsW79Equal(test, legacyNativeCheckpointModelsW79Recover(test, ref, len(fixture.raw), storage), semantic)
			}
		})
	}
	test.Run("large-real-number-value", func(test *testing.T) {
		raw := `{"context_tokens":` + strings.Repeat("9", 3*LegacyCanonicalEncodingBytes+257) + `,"source_model":"later"}`
		var destination CIREnvelope
		var semantic *json.UnmarshalTypeError
		if failure := json.Unmarshal([]byte(raw), &destination); !errors.As(failure, &semantic) || destination.SourceModel != "later" || len(semantic.Value) <= 3*LegacyCanonicalEncodingBytes {
			test.Fatal("large real deferred error fixture changed", failure)
		}
		storage := legacyNativeCheckpointModelsW79Storage(test)
		ref := legacyNativeCheckpointModelsW79Capture(test, semantic, len(raw), storage)
		if storage.writes < 4 {
			test.Fatal("large Value was not divided into bounded immutable objects")
		}
		legacyNativeCheckpointModelsW79Equal(test, legacyNativeCheckpointModelsW79Recover(test, ref, len(raw), storage), semantic)
		test.Run("native-value-budget-before-body-read", func(test *testing.T) {
			reads := storage.reads
			legacyNativeCheckpointModelsW79RejectRestore(test, ref, storage.io(), len(raw), 300<<10, ErrDocumentResourceLimit)
			if storage.reads-reads != 1 {
				test.Fatal("large native Value was not admitted before reading dependent text", storage.reads-reads)
			}
		})
		for hash, body := range storage.objects {
			if len(body) > LegacyCanonicalEncodingBytes || HashContent(body) != hash {
				test.Fatal("large Value escaped checkpoint object/hash bound", len(body), hash)
			}
		}
	})
	for _, semantic := range []*json.UnmarshalTypeError{
		{Value: "", Type: reflect.TypeFor[string](), Offset: 0},
		{Value: "string\x00\n\"\\🙂", Type: reflect.TypeFor[bool](), Offset: 1, Struct: strings.Repeat("界", 1365) + "x", Field: strings.Repeat("f", 4096)},
	} {
		storage := legacyNativeCheckpointModelsW79Storage(test)
		ref := legacyNativeCheckpointModelsW79Capture(test, semantic, 4096, storage)
		legacyNativeCheckpointModelsW79Equal(test, legacyNativeCheckpointModelsW79Recover(test, ref, 4096, storage), semantic)
	}
}

type legacyNativeCheckpointModelsW79Member struct {
	name  string
	value json.RawMessage
}

func legacyNativeCheckpointModelsW79Members(test *testing.T, body []byte) []legacyNativeCheckpointModelsW79Member {
	test.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		test.Fatal("error root is not explicit object metadata", token, err)
	}
	var members []legacyNativeCheckpointModelsW79Member
	for decoder.More() {
		token, err := decoder.Token()
		name, valid := token.(string)
		if err != nil || !valid {
			test.Fatal("invalid root member", token, err)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			test.Fatal(err)
		}
		members = append(members, legacyNativeCheckpointModelsW79Member{name: name, value: value})
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		test.Fatal("error root closing delimiter", token, err)
	}
	return members
}

func legacyNativeCheckpointModelsW79Body(test *testing.T, members []legacyNativeCheckpointModelsW79Member) []byte {
	test.Helper()
	var body bytes.Buffer
	body.WriteByte('{')
	for index, member := range members {
		if index > 0 {
			body.WriteByte(',')
		}
		name, err := json.Marshal(member.name)
		if err != nil {
			test.Fatal(err)
		}
		body.Write(name)
		body.WriteByte(':')
		body.Write(member.value)
	}
	body.WriteByte('}')
	return body.Bytes()
}

func TestLegacyNativeCheckpointModelsW79ErrorTransportGuards(test *testing.T) {
	const sourceBytes = 4096
	semantic := &json.UnmarshalTypeError{Value: "number 23", Type: reflect.TypeFor[int](), Offset: 17, Struct: "oracle-struct", Field: "oracle.field"}
	storage := legacyNativeCheckpointModelsW79Storage(test)
	ref := legacyNativeCheckpointModelsW79Capture(test, semantic, sourceBytes, storage)
	root := bytes.Clone(storage.objects[ref.Hash])
	members := legacyNativeCheckpointModelsW79Members(test, root)
	if !bytes.Equal(root, legacyNativeCheckpointModelsW79Body(test, members)) {
		test.Fatal("ordered root member oracle changed canonical metadata")
	}
	roles := make(map[string]int)
	for index, member := range members {
		switch string(member.value) {
		case "1":
			roles["version"] = index
		case "6":
			roles["type"] = index
		case "17":
			roles["offset"] = index
		case "4096":
			roles["source"] = index
		case `"oracle-struct"`:
			roles["struct"] = index
		case `"oracle.field"`:
			roles["field"] = index
		default:
			if len(member.value) > 0 && member.value[0] == '{' {
				roles["value"] = index
			}
		}
	}
	for _, role := range []string{"version", "type", "offset", "source", "struct", "field", "value"} {
		if _, found := roles[role]; !found {
			test.Fatal("explicit error metadata missing frozen role", role)
		}
	}
	rejectBody := func(test *testing.T, body []byte) {
		test.Helper()
		changed, err := legacyNativeCheckpointWrite(context.Background(), storage.io(), body)
		if err != nil {
			test.Fatal(err)
		}
		reads, writes := storage.reads, storage.writes
		legacyNativeCheckpointModelsW79RejectRestore(test, changed, storage.io(), sourceBytes, legacyNativeCheckpointModelsW79Budget, ErrIntegrity)
		if storage.reads-reads > 1 || storage.writes != writes {
			test.Fatal("invalid root loaded dependent bodies or wrote restore state", storage.reads-reads, storage.writes-writes)
		}
	}
	for _, role := range []string{"version", "type", "offset", "source", "struct", "field", "value"} {
		test.Run("duplicate-"+role, func(test *testing.T) {
			duplicate := append([]legacyNativeCheckpointModelsW79Member(nil), members...)
			duplicate = append(duplicate, members[roles[role]])
			rejectBody(test, legacyNativeCheckpointModelsW79Body(test, duplicate))
		})
		test.Run("missing-"+role, func(test *testing.T) {
			missing := append([]legacyNativeCheckpointModelsW79Member(nil), members[:roles[role]]...)
			missing = append(missing, members[roles[role]+1:]...)
			rejectBody(test, legacyNativeCheckpointModelsW79Body(test, missing))
		})
	}
	mutations := []struct{ name, role, value string }{
		{name: "version-zero", role: "version", value: "0"},
		{name: "version-future", role: "version", value: "2"},
		{name: "type-zero", role: "type", value: "0"},
		{name: "type-unknown", role: "type", value: "25"},
		{name: "type-wide", role: "type", value: "256"},
		{name: "type-string", role: "type", value: `"int"`},
		{name: "source-mismatch", role: "source", value: "4097"},
		{name: "offset-negative", role: "offset", value: "-1"},
		{name: "offset-too-large", role: "offset", value: "73729"},
		{name: "offset-overflow", role: "offset", value: "9223372036854775808"},
		{name: "struct-null", role: "struct", value: "null"},
		{name: "field-null", role: "field", value: "null"},
		{name: "struct-too-large", role: "struct", value: `"` + strings.Repeat("s", 4097) + `"`},
		{name: "field-too-large", role: "field", value: `"` + strings.Repeat("f", 4097) + `"`},
		{name: "struct-invalid-utf8", role: "struct", value: "\"\xff\""},
		{name: "field-invalid-utf8", role: "field", value: "\"\xff\""},
		{name: "value-null", role: "value", value: "null"},
		{name: "value-array", role: "value", value: "[]"},
	}
	for _, mutation := range mutations {
		test.Run(mutation.name, func(test *testing.T) {
			changed := append([]legacyNativeCheckpointModelsW79Member(nil), members...)
			changed[roles[mutation.role]].value = json.RawMessage(mutation.value)
			rejectBody(test, legacyNativeCheckpointModelsW79Body(test, changed))
		})
	}
	var text legacyNativeCheckpointText
	if err := json.Unmarshal(members[roles["value"]].value, &text); err != nil || text.Bytes != len(semantic.Value) {
		test.Fatal("error Value is not bounded checkpoint Text", text, err)
	}
	for _, mutation := range []struct {
		name   string
		change func(*legacyNativeCheckpointText)
	}{
		{name: "text-negative", change: func(value *legacyNativeCheckpointText) { value.Bytes = -1 }},
		{name: "text-too-large", change: func(value *legacyNativeCheckpointText) {
			value.Bytes = int(legacyNativeValueCheckpointPayloadLimit()) + 1
		}},
		{name: "text-weight", change: func(value *legacyNativeCheckpointText) { value.Tree.Count++ }},
		{name: "text-height", change: func(value *legacyNativeCheckpointText) { value.Tree.Height = legacyNativeCheckpointTreeHeight + 1 }},
		{name: "text-ref-size", change: func(value *legacyNativeCheckpointText) { value.Tree.Ref.Bytes = LegacyCanonicalEncodingBytes + 1 }},
		{name: "text-ref-hash", change: func(value *legacyNativeCheckpointText) { value.Tree.Ref.Hash = "not-a-content-hash" }},
	} {
		test.Run(mutation.name, func(test *testing.T) {
			changedText := text
			mutation.change(&changedText)
			body, err := json.Marshal(changedText)
			if err != nil {
				test.Fatal(err)
			}
			changed := append([]legacyNativeCheckpointModelsW79Member(nil), members...)
			changed[roles["value"]].value = body
			rejectBody(test, legacyNativeCheckpointModelsW79Body(test, changed))
		})
	}
	unknown := append([]legacyNativeCheckpointModelsW79Member(nil), members...)
	unknown = append(unknown, legacyNativeCheckpointModelsW79Member{name: "unknown", value: json.RawMessage("true")})
	for _, body := range [][]byte{legacyNativeCheckpointModelsW79Body(test, unknown), append(bytes.Clone(root), '\n'), []byte("null"), []byte("{}"), []byte("[]"), append(bytes.Clone(root), root...)} {
		test.Run("noncanonical-"+string(HashContent(body)), func(test *testing.T) { rejectBody(test, body) })
	}
	var hashes []string
	for hash := range storage.objects {
		if hash == ref.Hash || hash == text.Tree.Ref.Hash {
			hashes = append(hashes, string(hash))
		}
	}
	sort.Strings(hashes)
	for _, storedHash := range hashes {
		for _, fault := range []string{"hash", "short", "long", "missing"} {
			test.Run("fresh-read-"+fault+"-"+storedHash, func(test *testing.T) {
				store := storage.io()
				read := store.Read
				store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
					if string(hash) != storedHash {
						return read(ctx, hash, length)
					}
					if fault == "missing" {
						return nil, ErrNotFound
					}
					body, err := read(ctx, hash, length)
					if err != nil {
						return nil, err
					}
					switch fault {
					case "hash":
						body[len(body)/2] ^= 1
					case "short":
						body = body[:len(body)-1]
					case "long":
						body = append(body, 0)
					}
					return body, nil
				}
				expected := ErrPreparedDocRangeObject
				if fault == "missing" {
					expected = ErrNotFound
				}
				legacyNativeCheckpointModelsW79RejectRestore(test, ref, store, sourceBytes, legacyNativeCheckpointModelsW79Budget, expected)
			})
		}
	}
	test.Run("fresh-source-size", func(test *testing.T) {
		legacyNativeCheckpointModelsW79RejectRestore(test, ref, storage.io(), sourceBytes+1, legacyNativeCheckpointModelsW79Budget, ErrIntegrity)
	})
	legacyNativeCheckpointModelsW79ConstructionGuards(test, semantic, ref, storage)
}

func legacyNativeCheckpointModelsW79ConstructionGuards(test *testing.T, semantic *json.UnmarshalTypeError, ref legacyNativeCheckpointRef, storage *legacyNativeCheckpointModelsW79Store) {
	test.Helper()
	test.Run("writer-nil-put", func(test *testing.T) {
		work, err := newLegacyNativeCheckpointTypeErrorWriter(context.Background(), semantic, legacyNativeCheckpointIO{}, 4096, legacyNativeCheckpointModelsW79Budget)
		if !errors.Is(err, ErrIntegrity) {
			if work != nil {
				work.close()
			}
			test.Fatal("writer accepted nil Put", err)
		}
		if work != nil {
			legacyNativeCheckpointModelsW79Released(test, work)
		}
	})
	test.Run("restore-nil-read", func(test *testing.T) {
		legacyNativeCheckpointModelsW79RejectRestore(test, ref, legacyNativeCheckpointIO{}, 4096, legacyNativeCheckpointModelsW79Budget, ErrIntegrity)
	})
	test.Run("writer-put-sticky-release", func(test *testing.T) {
		failure := errors.New("W79 immutable checkpoint Put unavailable")
		store := storage.io()
		store.Put = func(context.Context, ContentHash, []byte) error { return failure }
		work, err := newLegacyNativeCheckpointTypeErrorWriter(context.Background(), semantic, store, 4096, legacyNativeCheckpointModelsW79Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		for attempt := 0; attempt < legacyNativeCheckpointModelsW79Steps; attempt++ {
			ready, err := work.step(context.Background())
			if err != nil {
				if err != failure {
					test.Fatal("writer changed storage failure identity", err)
				}
				legacyNativeCheckpointModelsW79Sticky(test, work, failure, func() error {
					saved, err := work.take(context.Background())
					if saved != (legacyNativeCheckpointRef{}) {
						test.Fatal("failed Put exposed checkpoint root")
					}
					return err
				})
				return
			}
			if ready {
				test.Fatal("failed Put became ready")
			}
		}
		test.Fatal("failed Put did not terminate")
	})
	for _, fixture := range []struct {
		name        string
		semantic    *json.UnmarshalTypeError
		sourceBytes int
		budget      int64
		expected    error
	}{
		{name: "nil-error", sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "nil-type", semantic: &json.UnmarshalTypeError{Value: "object"}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "unknown-type", semantic: &json.UnmarshalTypeError{Value: "object", Type: reflect.TypeFor[complex128]()}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "negative-offset", semantic: &json.UnmarshalTypeError{Type: reflect.TypeFor[int](), Offset: -1}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "offset-limit", semantic: &json.UnmarshalTypeError{Type: reflect.TypeFor[int](), Offset: 73729}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "struct-limit", semantic: &json.UnmarshalTypeError{Type: reflect.TypeFor[int](), Struct: strings.Repeat("s", 4097)}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "field-limit", semantic: &json.UnmarshalTypeError{Type: reflect.TypeFor[int](), Field: strings.Repeat("f", 4097)}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "struct-utf8", semantic: &json.UnmarshalTypeError{Type: reflect.TypeFor[int](), Struct: "\xff"}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "field-utf8", semantic: &json.UnmarshalTypeError{Type: reflect.TypeFor[int](), Field: "\xff"}, sourceBytes: 4096, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "source-negative", semantic: semantic, sourceBytes: -1, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "source-over-limit", semantic: semantic, sourceBytes: MaxFinalizedDocBytes + 1, budget: legacyNativeCheckpointModelsW79Budget, expected: ErrIntegrity},
		{name: "small-budget", semantic: semantic, sourceBytes: 4096, budget: 1, expected: ErrDocumentResourceLimit},
	} {
		test.Run("writer-constructor-"+fixture.name, func(test *testing.T) {
			writes := storage.writes
			work, err := newLegacyNativeCheckpointTypeErrorWriter(context.Background(), fixture.semantic, storage.io(), fixture.sourceBytes, fixture.budget)
			if !errors.Is(err, fixture.expected) || storage.writes != writes {
				if work != nil {
					work.close()
				}
				test.Fatal("invalid writer constructor admitted work/IO", err, fixture.expected, storage.writes-writes)
			}
			if work != nil {
				legacyNativeCheckpointModelsW79Released(test, work)
			}
		})
	}
	test.Run("restore-small-budget-before-read", func(test *testing.T) {
		reads := storage.reads
		legacyNativeCheckpointModelsW79RejectRestore(test, ref, storage.io(), 4096, 1, ErrDocumentResourceLimit)
		if storage.reads != reads {
			test.Fatal("restore read before minimal heap admission")
		}
	})
	for _, operation := range []string{"writer", "restore"} {
		for _, phase := range []string{"constructor", "step", "io", "take"} {
			test.Run(operation+"-cancel-"+phase, func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				store := storage.io()
				if phase == "constructor" {
					cancel()
				}
				if phase == "io" {
					put, read := store.Put, store.Read
					store.Put = func(current context.Context, hash ContentHash, body []byte) error {
						err := put(current, hash, body)
						cancel()
						return err
					}
					store.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
						body, err := read(current, hash, length)
						cancel()
						return body, err
					}
				}
				var work legacyNativeCheckpointModelsW79Work
				var take func() error
				var constructorErr error
				if operation == "writer" {
					current, err := newLegacyNativeCheckpointTypeErrorWriter(ctx, semantic, store, 4096, legacyNativeCheckpointModelsW79Budget)
					constructorErr = err
					if current != nil {
						work = current
						take = func() error {
							saved, failure := current.take(context.Background())
							if saved != (legacyNativeCheckpointRef{}) {
								test.Fatal("canceled writer exposed root")
							}
							return failure
						}
					}
				} else {
					current, err := newLegacyNativeCheckpointTypeErrorRestore(ctx, ref, store, 4096, legacyNativeCheckpointModelsW79Budget)
					constructorErr = err
					if current != nil {
						work = current
						take = func() error {
							value, failure := current.take(context.Background())
							if value != nil {
								test.Fatal("canceled restore exposed native error")
							}
							return failure
						}
					}
				}
				if phase == "constructor" {
					if !errors.Is(constructorErr, context.Canceled) {
						test.Fatal("constructor ignored cancellation", constructorErr)
					}
					if work != nil {
						legacyNativeCheckpointModelsW79Released(test, work)
					}
					return
				}
				if constructorErr != nil || work == nil {
					test.Fatal("valid cancellation fixture rejected", constructorErr)
				}
				defer work.close()
				if phase == "take" {
					legacyNativeCheckpointModelsW79Drain(test, work, storage)
					cancel()
					var failure error
					switch current := work.(type) {
					case legacyNativeCheckpointModelsW79Writer:
						saved, err := current.take(ctx)
						failure = err
						if saved != (legacyNativeCheckpointRef{}) {
							test.Fatal("canceled take exposed root")
						}
					case legacyNativeCheckpointModelsW79Restore:
						value, err := current.take(ctx)
						failure = err
						if value != nil {
							test.Fatal("canceled take exposed error")
						}
					}
					if !errors.Is(failure, context.Canceled) {
						test.Fatal("take ignored cancellation", failure)
					}
					legacyNativeCheckpointModelsW79Sticky(test, work, failure, take)
					return
				}
				if phase == "step" {
					if ready, err := work.step(context.Background()); ready || err != nil {
						test.Fatal("cancellation fixture did not enter active work", ready, err)
					}
					cancel()
				}
				for attempt := 0; attempt < legacyNativeCheckpointModelsW79Steps; attempt++ {
					ready, failure := work.step(ctx)
					if failure != nil {
						if !errors.Is(failure, context.Canceled) {
							test.Fatal("cancellation changed error class", failure)
						}
						legacyNativeCheckpointModelsW79Sticky(test, work, failure, take)
						return
					}
					if ready {
						test.Fatal("canceled work became ready")
					}
				}
				test.Fatal("canceled work did not terminate")
			})
		}
	}
}
