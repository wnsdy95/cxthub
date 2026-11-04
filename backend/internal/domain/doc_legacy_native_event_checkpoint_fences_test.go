package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

const legacyNativeEventW82FenceBudget int64 = 32 << 20
const legacyNativeEventW82FenceSteps = 65536

func legacyNativeEventW82FenceCopy(value *wire) *wire {
	if value == nil {
		return &wire{}
	}
	copyValue := *value
	if value.IsError != nil {
		boolean := *value.IsError
		copyValue.IsError = &boolean
	}
	if value.CrossReplayable != nil {
		if value.CrossReplayable == value.IsError {
			copyValue.CrossReplayable = copyValue.IsError
		} else {
			boolean := *value.CrossReplayable
			copyValue.CrossReplayable = &boolean
		}
	}
	return &copyValue
}

func legacyNativeEventW82FencePrepare(test *testing.T, raw string, initial *wire, stop func(*legacyNativeObject) bool) (legacyNativeAnyW78Request, *legacyNativeObject, *legacyJSONValueScanner, LegacyByteSource) {
	test.Helper()
	request, source := legacyNativeAnyW78FullSource(test, raw)
	request.Binding = HashContent([]byte("W82 actual Event root fences"))
	if len(request.Parts) > 4 {
		test.Fatal("Event fence fixture exceeded four source parts")
	}
	value := legacyNativeEventW82FenceCopy(initial)
	native, err := newLegacyNativeObject(reflect.ValueOf(value), &legacyEventNativeFields, 3, true)
	if err != nil {
		test.Fatal(err)
	}
	for index, field := range native.metadata.fields {
		current := native.destination.Elem().Field(field.index)
		if !current.IsZero() {
			native.heaps[index], err = legacyNativeValueHeap(context.Background(), current)
			if err != nil || native.heaps[index].err != nil || native.heaps[index].bytes <= 0 {
				test.Fatal("initial Event field lacks actual heap ownership", field.name, err)
			}
		}
	}
	native.start(0, source.Bytes)
	native.fieldLimit, native.deferTypeErrors = 1, true
	bundle := legacyNativeObjectRestored{Object: native, Source: source}
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		if stop != nil && stop(native) {
			return request, native, &bundle.Scanner, source
		}
		current, cancel := context.WithCancel(context.Background())
		var stepContext context.Context = current
		if stop != nil {
			stepContext = &legacyNativeAnyW78CutContext{Context: current, cancel: cancel, stop: func() bool { return stop(native) }}
		}
		complete, failure := legacyNativeEventW82Step(test, stepContext, &bundle, false, false)
		cancel()
		if failure != nil && !(errors.Is(failure, context.Canceled) && stop != nil && stop(native)) {
			test.Fatal("actual Event source failed before fence", failure)
		}
		if complete {
			if stop != nil && !stop(native) {
				test.Fatal("actual Event did not reach designated cut")
			}
			return request, native, &bundle.Scanner, source
		}
	}
	test.Fatal("actual Event cut exceeded finite source schedule")
	return request, native, &bundle.Scanner, source
}

func legacyNativeEventW82FenceCapture(test *testing.T, native *legacyNativeObject, scanner *legacyJSONValueScanner, request legacyNativeAnyW78Request, source LegacyByteSource, storage *legacyNativeAnyW78CAS) legacyNativeAnyW78Request {
	test.Helper()
	before := legacyNativeEventW82FenceCopy(native.destination.Interface().(*wire))
	frame, presence, phase, position := *scanner, native.presence, native.phase, native.position
	reads := 0
	current := source
	current.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		reads++
		return source.Read(ctx, offset, length)
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpoint(constructor, native, scanner, request.Binding, current, storage.io(), legacyNativeEventW82FenceBudget)
	cancel()
	if err != nil {
		state := legacyNativeObjectCheckpointControlFor(native, legacyNativeCheckpointEventModel, request.Binding, source.Bytes)
		test.Fatal("actual Event capture constructor", err, "control", state.validate(legacyNativeCheckpointEventModel, request.Binding, source.Bytes, false), "scanner", legacyNativeObjectCheckpointScanner(state, scanner), "phase", native.phase, "range", native.position, native.valueStart, native.valueEnd, native.end, "scalar", native.scalar != nil)
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		phase, field := work.phase, work.field
		ready, failure := work.step(context.Background())
		if failure != nil {
			test.Fatal("Event capture phase/field", phase, field, failure)
		}
		if ready {
			break
		}
		if attempt == legacyNativeEventW82FenceSteps-1 {
			test.Fatal("Event capture exceeded finite schedule")
		}
	}
	request.Root, err = work.take(context.Background())
	if err != nil || !request.Root.valid() {
		test.Fatal("actual Event capture did not publish a root", err)
	}
	legacyNativeAnyW78Released(test, work)
	if reads != 0 || !reflect.DeepEqual(before, native.destination.Interface().(*wire)) || frame != *scanner || presence != native.presence || phase != native.phase || position != native.position {
		test.Fatal("Event capture replayed RAW or mutated borrowed native state", reads)
	}
	if storage.inputWrites != 0 {
		test.Fatal("Event capture rewrote immutable original source CAS")
	}
	return request
}

func legacyNativeEventW82FenceRestore(test *testing.T, request legacyNativeAnyW78Request, storage *legacyNativeAnyW78CAS) legacyNativeObjectRestored {
	test.Helper()
	current := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	constructor, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeObjectCheckpointRestore(constructor, request.Root, legacyNativeCheckpointEventModel, storage.io(), current, request.Binding, legacyNativeEventW82FenceBudget)
	cancel()
	if err != nil {
		test.Fatal("actual Event restore constructor", err)
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		phase, field := work.phase, work.field
		blockPhase := uint8(255)
		if block, ok := work.blockTask.(*legacyNativeObjectBlockCheckpointRestore); ok {
			blockPhase = block.phase
		}
		ready, failure := work.step(context.Background())
		if failure != nil {
			test.Fatal("Event restore phase/field/block", phase, field, blockPhase, failure)
		}
		if ready {
			break
		}
		if attempt == legacyNativeEventW82FenceSteps-1 {
			test.Fatal("Event restore exceeded finite schedule")
		}
	}
	restored, err := work.take(context.Background())
	if err != nil || restored.Object == nil || restored.Object.metadata != &legacyEventNativeFields || restored.Object.destination.Elem().Type() != reflect.TypeFor[wire]() || restored.Source.authority == 0 || restored.Source.authority != current.authority {
		test.Fatal("Event restore lost actual wire model or current source grant", err)
	}
	legacyNativeAnyW78Released(test, work)
	return restored
}

func legacyNativeEventW82FenceReject(test *testing.T, ref legacyNativeCheckpointRef, source LegacyByteSource, binding ContentHash, model uint8, storage legacyNativeCheckpointIO, budget int64) error {
	test.Helper()
	work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), ref, model, storage, source, binding, budget)
	if err != nil {
		if work != nil {
			legacyNativeAnyW78Released(test, work)
			test.Fatal("rejected Event constructor returned owned work")
		}
		return err
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
		ready, failure := work.step(context.Background())
		if failure != nil {
			legacyNativeObjectW79Failed[legacyNativeObjectRestored](test, work, failure)
			return failure
		}
		if ready {
			test.Fatal("invalid actual Event checkpoint quietly became ready")
		}
	}
	test.Fatal("invalid Event checkpoint exceeded finite rejection schedule")
	return nil
}

func TestLegacyNativeEventCheckpointW82PrimitivePresence(test *testing.T) {
	shared, separate := false, false
	fixtures := []struct {
		name    string
		raw     string
		initial *wire
		cut     bool
	}{
		{name: "zero-uninitialized", raw: `{}`},
		{name: "initialized-false-and-true", raw: `{"agent_message":false,"compact_summary":true,"replacement_complete":false,"is_error":false,"cross_replayable":true,"output":""}`},
		{name: "boolean-null-retention", raw: `{"agent_message":null,"compact_summary":null,"replacement_complete":null,"is_error":null,"cross_replayable":null}`, initial: &wire{AgentMessage: true, CompactSummary: true, ReplacementComplete: true, IsError: &shared, CrossReplayable: &shared, fieldPresence: eventFieldLocked}},
		{name: "initial-owned-values", raw: `{}`, initial: &wire{Kind: EventToolResult, Seq: 17, AgentMessage: true, Output: "prior", IsError: &shared, CrossReplayable: &separate, fieldPresence: eventFieldLocked}},
		{name: "strings-enums-integer-output", raw: `{"kind":"tool_result","seq":-7,"id":"id","ts":"later","role":"assistant","agent_author":"","agent_recipient":"agent","call_id":"call","tool_name":"tool","provider_tool_name":"provider","status":"ok","redacted_summary":"summary","output":"kept"}`, initial: &wire{fieldPresence: eventFieldLocked}},
		{name: "dynamic-string-null", raw: `{"output":"prior","output":null}`},
		{name: "pointer-reuse-after-recapture", raw: `{"is_error":true,"is_error":false,"cross_replayable":true}`, initial: &wire{IsError: &shared, CrossReplayable: &separate, fieldPresence: eventFieldLocked}, cut: true},
		{name: "cross-field-alias-after-recapture", raw: `{"is_error":true,"cross_replayable":false}`, initial: &wire{IsError: &shared, CrossReplayable: &shared, fieldPresence: eventFieldLocked}, cut: true},
		{name: "pointer-null-after-recapture", raw: `{"is_error":true,"is_error":null,"cross_replayable":null}`, initial: &wire{IsError: &shared, CrossReplayable: &shared}, cut: true},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			var stop func(*legacyNativeObject) bool
			if fixture.cut {
				stop = func(native *legacyNativeObject) bool {
					return native.phase == legacyNativeObjectAfter && native.field == native.metadata.match("is_error")
				}
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, fixture.raw, fixture.initial, stop)
			storage.preload(test, request)
			before := legacyNativeEventW82FenceCopy(native.destination.Interface().(*wire))
			request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
			var state legacyNativeObjectCheckpointControl
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil || state.Version != 3 || state.Model != legacyNativeCheckpointEventModel || state.Presence != native.presence || state.DestinationPresence != before.fieldPresence || len(state.Fields) != len(legacyEventNativeFields.fields) {
				test.Fatal("Event root lost version-three wire/presence schema", err)
			}
			for index, row := range state.Fields {
				if row.Index != index || row.Initialized != (native.heaps[index].bytes != 0) {
					test.Fatal("Event row lost actual initialized-field ownership", index)
				}
			}
			pointer, alias := legacyEventNativeFields.match("is_error"), legacyEventNativeFields.match("cross_replayable")
			var expectedAlias uint8
			if before.IsError != nil && before.IsError == before.CrossReplayable {
				expectedAlias = uint8(pointer + 1)
			}
			if state.Fields[alias].BooleanAlias != expectedAlias {
				test.Fatal("Event row lost exact prior-field bool alias descriptor")
			}
			restored := legacyNativeEventW82FenceRestore(test, request, storage)
			value := restored.Object.destination.Interface().(*wire)
			beforeScanner, beforeError := scanner.checkpoint(context.Background())
			afterScanner, afterError := restored.Scanner.checkpoint(context.Background())
			if beforeError != nil || afterError != nil || !reflect.DeepEqual(before, value) || !bytes.Equal(beforeScanner, afterScanner) || restored.Object.presence != native.presence || value.fieldPresence != before.fieldPresence || restored.Object.trackPresence != native.trackPresence {
				test.Fatal("Event restore changed values, scanner continuation, or distinct presence states", beforeError, afterError)
			}
			if value.IsError != nil && value.IsError == native.destination.Interface().(*wire).IsError || value.CrossReplayable != nil && value.CrossReplayable == native.destination.Interface().(*wire).CrossReplayable {
				test.Fatal("restored bool pointer borrowed original destination storage")
			}
			if before.IsError != nil && before.CrossReplayable != nil && (before.IsError == before.CrossReplayable) != (value.IsError == value.CrossReplayable) {
				test.Fatal("Event restore changed cross-field bool pointer aliases")
			}
			for index, heap := range restored.Object.heaps {
				if heap.err != nil || heap.bytes < 0 || (heap.bytes != 0) != state.Fields[index].Initialized {
					test.Fatal("Event restore changed initialized ownership", index, heap.err)
				}
			}
			request = legacyNativeEventW82FenceCapture(test, restored.Object, &restored.Scanner, request, restored.Source, storage)
			recaptured := legacyNativeEventW82FenceRestore(test, request, storage)
			if !reflect.DeepEqual(value, recaptured.Object.destination.Interface()) || recaptured.Scanner != restored.Scanner || recaptured.Object.presence != restored.Object.presence {
				test.Fatal("immediate Event recapture lost accepted state before first native step")
			}
			prior := recaptured.Object.destination.Interface().(*wire).IsError
			for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
				current, cancel := context.WithCancel(context.Background())
				complete, failure := legacyNativeObjectW79Step(test, current, recaptured.Object, &recaptured.Scanner, recaptured.Source, legacyNativeNumberDiagnosticsFull, false)
				cancel()
				if failure != nil {
					test.Fatal("restored Event continuation failed", failure)
				}
				if complete {
					break
				}
				if attempt == legacyNativeEventW82FenceSteps-1 {
					test.Fatal("restored Event continuation exceeded finite schedule")
				}
			}
			expected := legacyNativeEventW82FenceCopy(fixture.initial)
			if err := json.Unmarshal([]byte(fixture.raw), expected); err != nil {
				test.Fatal(err)
			}
			actual := recaptured.Object.destination.Interface().(*wire)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(fixture.raw), &fields); err != nil {
				test.Fatal(err)
			}
			if !reflect.DeepEqual(expected, actual) || recaptured.Object.presence != eventPresence(fields) {
				test.Fatal("continued Event differs from independent encoding/json or exact native presence")
			}
			if fixture.cut && actual.IsError != nil && actual.IsError != prior {
				test.Fatal("duplicate bool update replaced a reusable restored pointer")
			}
			legacyNativeEnvelopeW80Immutable(test, storage, request, fixture.raw)
		})
	}
}

func TestLegacyNativeEventCheckpointW82Fences(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"kind":"message","seq":7,"agent_message":false,"compact_summary":true,"replacement_complete":false,"is_error":false,"cross_replayable":true,"output":"kept"}`, &wire{fieldPresence: eventFieldLocked}, nil)
	storage.preload(test, request)
	request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
	body, err := storage.io().Read(context.Background(), request.Root.Hash, request.Root.Bytes)
	if err != nil {
		test.Fatal(err)
	}
	names := []string{"binding", "model", "unknown-model", "schema", "version", "destination-presence", "parser-presence", "untracked-presence", "cursor", "phase", "field", "missing-scanner", "missing-field", "duplicate-row", "boolean-integer", "boolean-dynamic", "boolean-block", "boolean-absent", "boolean-alias", "pointer-uninitialized", "nil-pointer-boolean", "alias-self", "alias-forward", "alias-wrong-type", "alias-value", "alias-absent", "dynamic-tag", "dynamic-absent", "dynamic-nil-payload", "string-boolean", "aggregate-present", "missing-block", "block-flag", "unknown-key", "duplicate-key", "fields-33", "root-hash", "root-short", "root-ref-hash", "root-ref-size", "no-authority", "source-size", "source-part-size", "source-part-hash", "source-read"}
	for _, name := range names {
		test.Run(name, func(test *testing.T) {
			ref, current, binding, model := request.Root, source, request.Binding, legacyNativeCheckpointEventModel
			var state legacyNativeObjectCheckpointControl
			if err := json.Unmarshal(body, &state); err != nil {
				test.Fatal(err)
			}
			changed := true
			boolean := legacyEventNativeFields.match("agent_message")
			pointer := legacyEventNativeFields.match("is_error")
			alias := legacyEventNativeFields.match("cross_replayable")
			output := legacyEventNativeFields.match("output")
			switch name {
			case "binding":
				binding, changed = HashContent([]byte("different Event job")), false
			case "model":
				model, changed = legacyNativeCheckpointBlockModel, false
			case "unknown-model":
				model, changed = 255, false
			case "schema":
				state.Schema = HashContent([]byte("different actual wire metadata"))
			case "version":
				state.Version = 2
			case "destination-presence":
				state.DestinationPresence = 1 << 15
			case "parser-presence":
				state.Presence = 1 << 15
			case "untracked-presence":
				state.TrackPresence = false
			case "cursor":
				state.Position = state.End + 1
			case "phase":
				state.Phase = 255
			case "field":
				state.Field = len(legacyEventNativeFields.fields)
			case "missing-scanner":
				state.Scanner = legacyNativeCheckpointRef{}
			case "missing-field":
				state.Fields = state.Fields[:len(state.Fields)-1]
			case "duplicate-row":
				state.Fields[1].Index = 0
			case "boolean-integer":
				state.Fields[boolean].Integer = 1
			case "boolean-dynamic":
				state.Fields[boolean].Dynamic = 1
			case "boolean-block":
				state.Fields[boolean].Block = state.Fields[legacyEventNativeFields.match("blocks")].Block
			case "boolean-absent":
				state.Fields[boolean].Present = false
			case "boolean-alias":
				state.Fields[boolean].BooleanAlias = 1
			case "pointer-uninitialized":
				state.Fields[pointer].Initialized = false
			case "nil-pointer-boolean":
				state.Fields[pointer].Present, state.Fields[pointer].Boolean = false, true
			case "alias-self":
				state.Fields[pointer].BooleanAlias = uint8(pointer + 1)
			case "alias-forward":
				state.Fields[pointer].BooleanAlias = uint8(alias + 1)
			case "alias-wrong-type":
				state.Fields[pointer].BooleanAlias = 1
			case "alias-value":
				state.Fields[alias].BooleanAlias = uint8(pointer + 1)
			case "alias-absent":
				state.Fields[pointer].Present = false
				state.Fields[alias].Boolean, state.Fields[alias].BooleanAlias = false, uint8(pointer+1)
			case "dynamic-tag":
				state.Fields[output].Dynamic = 2
			case "dynamic-absent":
				state.Fields[output].Present = false
			case "dynamic-nil-payload":
				state.Fields[output].Dynamic = 0
			case "string-boolean":
				state.Fields[0].Boolean = true
			case "aggregate-present":
				state.Fields[legacyEventNativeFields.match("input")].Present = true
			case "missing-block":
				state.Fields[legacyEventNativeFields.match("blocks")].Block = nil
			case "block-flag":
				state.BlockActive = true
			case "root-ref-hash":
				ref.Hash, changed = "bad", false
			case "root-ref-size":
				ref.Bytes, changed = 0, false
			case "no-authority":
				current.authority, changed = 0, false
			case "source-size":
				current.Bytes--
				changed = false
			case "source-part-size", "source-part-hash":
				current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
				if name == "source-part-size" {
					current.Parts[0].Bytes--
				} else {
					current.Parts[0].Hash = "bad"
				}
				changed = false
			case "source-read":
				current.Read, changed = nil, false
			case "unknown-key", "duplicate-key", "fields-33", "root-hash", "root-short":
				changed = false
			}
			altered := bytes.Clone(body)
			if changed {
				altered, err = json.Marshal(state)
				if err != nil {
					test.Fatal(err)
				}
			}
			if name == "unknown-key" || name == "duplicate-key" {
				addition := `,"unknown":0}`
				if name == "duplicate-key" {
					addition = `,"model":2}`
				}
				altered = append(altered[:len(altered)-1], addition...)
			}
			if name == "fields-33" {
				rows, failure := json.Marshal(state.Fields)
				if failure != nil {
					test.Fatal(failure)
				}
				needle := append([]byte(`"fields":`), rows...)
				if bytes.Count(altered, needle) != 1 {
					test.Fatal("ambiguous actual Event field table")
				}
				replacement := `"fields":[` + string(bytes.Repeat([]byte(`{"i":0},`), 32)) + `{"i":0}]`
				altered = bytes.Replace(altered, needle, []byte(replacement), 1)
			}
			if !bytes.Equal(altered, body) {
				ref, err = legacyNativeCheckpointWrite(context.Background(), storage.io(), altered)
				if err != nil {
					test.Fatal(err)
				}
			}
			storageIO := storage.io()
			read := storageIO.Read
			dependentReads := 0
			storageIO.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
				if hash != ref.Hash {
					dependentReads++
					return nil, ErrConflict
				}
				value, failure := read(ctx, hash, length)
				if failure == nil && name == "root-hash" {
					value[0] ^= 1
				}
				if failure == nil && name == "root-short" {
					value = value[:len(value)-1]
				}
				return value, failure
			}
			failure := legacyNativeEventW82FenceReject(test, ref, current, binding, model, storageIO, legacyNativeEventW82FenceBudget)
			if !errors.Is(failure, ErrIntegrity) || dependentReads != 0 {
				test.Fatal("Event root fence failed after dependent storage read", failure, dependentReads)
			}
		})
	}
	test.Run("field-decoder-capacity-bound", func(test *testing.T) {
		for _, count := range []int{32, 33} {
			body := []byte(`[` + string(bytes.Repeat([]byte(`{"i":0},`), count-1)) + `{"i":0}]`)
			var fields legacyNativeObjectCheckpointFields
			err := json.Unmarshal(body, &fields)
			if count == 32 && (err != nil || len(fields) != 32 || cap(fields) > 32) || count == 33 && (!errors.Is(err, ErrIntegrity) || len(fields) != 0) {
				test.Fatal("Event field decoder escaped closed 32-row allocation bound", count, len(fields), cap(fields), err)
			}
		}
	})
}

func TestLegacyNativeEventCheckpointW82AdmissionLifecycle(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"agent_message":true,"is_error":false,"output":"kept"}`, &wire{fieldPresence: eventFieldLocked}, nil)
	storage.preload(test, request)
	request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
	test.Run("capture", func(test *testing.T) {
		legacyNativeEnvelopeW80Lifecycle[legacyNativeCheckpointRef](test, storage, func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef], error) {
			work, err := newLegacyNativeObjectCheckpoint(ctx, native, scanner, request.Binding, source, current, budget)
			if err != nil {
				return nil, err
			}
			return work, nil
		})
	})
	test.Run("restore", func(test *testing.T) {
		legacyNativeEnvelopeW80Lifecycle[legacyNativeObjectRestored](test, storage, func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeObjectRestored], error) {
			work, err := newLegacyNativeObjectCheckpointRestore(ctx, request.Root, legacyNativeCheckpointEventModel, current, source, request.Binding, budget)
			if err != nil {
				return nil, err
			}
			return work, nil
		})
	})
	test.Run("restore-scratch-before-read", func(test *testing.T) {
		work, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Root, legacyNativeCheckpointEventModel, storage.io(), source, request.Binding, legacyNativeEventW82FenceBudget)
		if err != nil {
			test.Fatal(err)
		}
		retained, err := work.retainedBytes()
		legacyNativeAnyW78Released(test, work)
		if err != nil || retained <= 0 {
			test.Fatal("invalid Event restore baseline accounting", retained, err)
		}
		reads := storage.reads
		failure := legacyNativeEventW82FenceReject(test, request.Root, source, request.Binding, legacyNativeCheckpointEventModel, storage.io(), retained+legacyNativeAnyCheckpointScratch-1)
		if !errors.Is(failure, ErrDocumentResourceLimit) || storage.reads != reads {
			test.Fatal("Event restore read metadata without full transient scratch admission", failure)
		}
	})
	for _, name := range []string{"boolean", "pointer-value", "pointer-identity", "dynamic", "destination-presence", "native-presence", "metadata-model"} {
		test.Run("borrowed-guard-"+name, func(test *testing.T) {
			value := native.destination.Interface().(*wire)
			before, presence, metadata := legacyNativeEventW82FenceCopy(value), native.presence, native.metadata
			pointer := value.IsError
			defer func() {
				*value = *before
				value.IsError = pointer
				if pointer != nil {
					*pointer = *before.IsError
				}
				native.presence, native.metadata = presence, metadata
			}()
			work, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, storage.io(), legacyNativeEventW82FenceBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			switch name {
			case "boolean":
				value.AgentMessage = !value.AgentMessage
			case "pointer-value":
				*value.IsError = !*value.IsError
			case "pointer-identity":
				boolean := *value.IsError
				value.IsError = &boolean
			case "dynamic":
				value.Output = "changed"
			case "destination-presence":
				value.fieldPresence ^= eventFieldLocked
			case "native-presence":
				native.presence ^= eventFieldAgentAuthor
			case "metadata-model":
				changed := *metadata
				changed.model = reflect.TypeFor[CIREvent]()
				native.metadata = &changed
			}
			writes := storage.writes
			ready, failure := work.step(context.Background())
			if ready || !errors.Is(failure, ErrConflict) || storage.writes != writes {
				test.Fatal("capture accepted changed borrowed Event before IO", ready, failure)
			}
			legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
		})
	}
}

func TestLegacyNativeEventCheckpointW82UnsupportedPopulated(test *testing.T) {
	test.Log("Initial untracked Input graphs and Replacement continuation remain unfinished; supported Any fields have positive W84 regression coverage")
	fixtures := []struct {
		name    string
		raw     string
		initial *wire
		active  bool
	}{
		{name: "replacement-events", raw: `{"replacement":[{"kind":"message","seq":0,"blocks":[{"type":"text","text":"child"}]}]}`},
		{name: "nil-aggregate-parser-cache", raw: `{"input":null,"replacement":null,"locked":null,"provider_metadata":null,"output":null,"is_error":null,"cross_replayable":null}`},
		{name: "empty-owned-replacement", raw: `{}`, initial: &wire{Replacement: []CIREvent{}}},
		{name: "active-replacement-parser", raw: `{"replacement":[{"kind":"message","seq":0}]}`, active: true},
		{name: "inactive-replacement-cache-null", raw: `{"replacement":[{"kind":"message","seq":0}],"replacement":null}`},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			var stop func(*legacyNativeObject) bool
			if fixture.active {
				stop = func(native *legacyNativeObject) bool {
					return native.containerActive || native.eventActive || native.nestedActive
				}
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, fixture.raw, fixture.initial, stop)
			if fixture.active && !stop(native) {
				test.Fatal("unsupported cache fixture did not retain a real active parser")
			}
			if fixture.active && (native.containerActive && (len(native.containers) <= native.field || native.containers[native.field] == nil) || native.eventActive && (len(native.events) <= native.field || native.events[native.field] == nil) || native.nestedActive && (len(native.nested) <= native.field || native.nested[native.field] == nil)) {
				test.Fatal("unsupported active flag lacks an actual native child")
			}
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			work, failure := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, storage.io(), legacyNativeEventW82FenceBudget)
			if failure != nil {
				if work != nil || !errors.Is(failure, ErrIntegrity) {
					test.Fatal("unfinished populated Event codec lost explicit integrity rejection", failure)
				}
				return
			}
			defer work.close()
			for attempt := 0; attempt < legacyNativeEventW82FenceSteps; attempt++ {
				ready, failure := work.step(context.Background())
				if failure != nil {
					if !errors.Is(failure, ErrIntegrity) {
						test.Fatal("unfinished Event codec returned an unrelated failure", failure)
					}
					legacyNativeObjectW79Failed[legacyNativeCheckpointRef](test, work, failure)
					return
				}
				if ready {
					test.Fatal("unfinished populated Event codec fabricated an empty ready checkpoint")
				}
			}
			test.Fatal("unfinished Event codec did not reject within finite schedule")
		})
	}
}
