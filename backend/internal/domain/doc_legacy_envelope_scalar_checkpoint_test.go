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

func legacyEnvelopeScalarCheckpointFixture(test testing.TB, envelope CIREnvelope, name string, model int) *LegacyEnvelopeEncoding {
	test.Helper()
	encoding := legacyEnvelopeEncodingFixture(test, envelope)
	for steps := 0; steps < 1<<14; steps++ {
		if encoding.scalar != nil {
			if model < 0 && encoding.phase == legacyEnvelopeEncodingFieldNext && encoding.fields[encoding.field-1].name == name ||
				model >= 0 && encoding.phase == legacyEnvelopeEncodingModelNext && encoding.fields[encoding.field].name == name && encoding.model == model+1 {
				return encoding
			}
		}
		if _, complete, err := encoding.Step(context.Background()); err != nil || complete {
			test.Fatal("envelope did not reach the requested scalar", name, model, err)
		}
	}
	test.Fatal("envelope scalar fixture exceeded its step bound")
	return nil
}

func TestLegacyEnvelopeScalarCheckpointEveryEnvelopeStep(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	large := strings.Repeat("a", 2*LegacyCanonicalEncodingBytes-1)
	escaped := strings.Repeat("🙂<&>\u2028\u2029\"\\\x00\xff", LegacyCanonicalEncodingBytes/8)
	envelopes := []CIREnvelope{
		{},
		{SourceModels: []string{}},
		{CIRVersion: "2", SourceProvider: ProviderKind("provider"), Fidelity: FidelityTier("tier"), Cwd: large},
		{ContextTokens: maximum, OutputTokens: -maximum - 1, CompactionCount: -19},
		{Cwd: escaped, SourceModels: []string{"", "same", "same", large, escaped, "last"}},
		{Cwd: strings.Repeat("a", LegacyCanonicalEncodingBytes-3) + "🙂\xff" + large},
	}
	seen := make(map[string]bool)
	for index, envelope := range envelopes {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			encoding := legacyEnvelopeEncodingFixture(test, envelope)
			want, err := canonicalJSON(envelope)
			if err != nil {
				test.Fatal(err)
			}
			var output []byte
			for steps := 0; steps < 1<<14; steps++ {
				before := *encoding
				var frameBefore legacyCanonicalEncodingFrame
				if before.scalar != nil {
					frameBefore = *before.scalar.frames.at(0)
				}
				state, err := legacyEnvelopeScalarControlFor(encoding)
				if err != nil || (state == nil) != (before.scalar == nil) {
					test.Fatal("checkpoint rejected an actual envelope Step state", steps, err)
				}
				if !reflect.DeepEqual(before, *encoding) || before.scalar != nil && !reflect.DeepEqual(frameBefore, *before.scalar.frames.at(0)) {
					test.Fatal("checkpoint advanced the live envelope scalar")
				}
				var wire *legacyEnvelopeScalarCheckpointControl
				if state != nil {
					body, err := json.Marshal(state)
					if err != nil || len(body) > 96 {
						test.Fatal("scalar control contains more than bounded metadata", len(body), err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(body, &fields); err != nil || len(fields) != 3 || fields["kind"] == nil || fields["phase"] == nil || fields["position"] == nil {
						test.Fatal("scalar control includes payload data", string(body), err)
					}
					if err := json.Unmarshal(body, &wire); err != nil {
						test.Fatal(err)
					}
				}
				restored, err := restoreLegacyEnvelopeScalar(context.Background(), encoding, wire)
				if err != nil || (restored == nil) != (state == nil) {
					test.Fatal("restore rejected an actual envelope Step state", steps, err)
				}
				if !reflect.DeepEqual(before, *encoding) || before.scalar != nil && !reflect.DeepEqual(frameBefore, *before.scalar.frames.at(0)) {
					test.Fatal("restore consumed the live scalar or changed the parent borrow ledger")
				}
				if restored != nil {
					if restored == before.scalar || restored.frames.at(0) == before.scalar.frames.at(0) ||
						!reflect.DeepEqual(restored, before.scalar) || restored.preparation != nil || restored.nativeOwner != nil || restored.inspectWork != 2 {
						test.Fatal("restore did not rebuild the exact leaf without replay")
					}
					retained, err := restored.RetainedBytes()
					originalRetained, originalErr := before.scalar.RetainedBytes()
					if err != nil || originalErr != nil || retained != originalRetained {
						test.Fatal("restore changed scalar retained accounting", retained, originalRetained, err, originalErr)
					}
					if text, ok := restored.root.(string); ok && len(text) != 0 && unsafe.StringData(text) != unsafe.StringData(before.scalar.root.(string)) {
						test.Fatal("restore copied the actual envelope string")
					}
					seen[fmt.Sprintf("kind-%d", state.Kind)] = true
					if _, ok := restored.root.(int); ok {
						seen["integer"] = true
					}
					if state.Kind == 's' {
						seen["field-string"] = seen["field-string"] || encoding.phase == legacyEnvelopeEncodingFieldNext
						seen["model-string"] = seen["model-string"] || encoding.phase == legacyEnvelopeEncodingModelNext
						seen["pending-string"] = seen["pending-string"] || encoding.pending != nil
						seen["closing-quote"] = seen["closing-quote"] || state.Position == len(restored.root.(string))
					}
				}
				encoding.scalar = restored
				chunk, complete, err := encoding.Step(context.Background())
				if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
					test.Fatal("restored envelope Step failed", steps, err)
				}
				output = append(output, chunk...)
				if complete {
					if !bytes.Equal(output, want) {
						test.Fatal("checkpoint at every Step changed canonical envelope output")
					}
					return
				}
			}
			test.Fatal("restored envelope exceeded its step bound")
		})
	}
	for _, stage := range []string{"kind-0", "kind-115", "integer", "field-string", "model-string", "pending-string", "closing-quote"} {
		if !seen[stage] {
			test.Fatal("actual envelope checkpoint coverage missed a scalar stage", stage)
		}
	}
}

func TestLegacyEnvelopeScalarCheckpointRejectsLiveCorruption(test *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*LegacyEnvelopeEncoding)
	}{
		{"root-value", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.root = "different" }},
		{"root-copy", func(encoding *LegacyEnvelopeEncoding) {
			encoding.scalar.root = strings.Clone(encoding.scalar.root.(string))
		}},
		{"root-named-string", func(encoding *LegacyEnvelopeEncoding) {
			encoding.scalar.root = ProviderKind(encoding.scalar.root.(string))
		}},
		{"root-number", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.root = json.Number("1") }},
		{"root-bool", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.root = true }},
		{"root-typed-nil", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.root = []string(nil) }},
		{"frame-value", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).value = "different" }},
		{"frame-copy", func(encoding *LegacyEnvelopeEncoding) {
			encoding.scalar.frames.at(0).value = strings.Clone(encoding.scalar.root.(string))
		}},
		{"frame-named-string", func(encoding *LegacyEnvelopeEncoding) {
			encoding.scalar.frames.at(0).value = FidelityTier(encoding.scalar.root.(string))
		}},
		{"frame-block", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).block.Text = "hidden" }},
		{"frame-keys", func(encoding *LegacyEnvelopeEncoding) {
			encoding.scalar.frames.at(0).fields.single = &legacyCanonicalKeyField{}
		}},
		{"frame-key-position", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).keysPosition = 1 }},
		{"frame-key-end", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).keysEnd = 1 }},
		{"frame-ordered", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).ordered = true }},
		{"frame-text", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).text = "hidden" }},
		{"frame-owned", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).ownedLiteral = true }},
		{"frame-kind", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).kind = 'a' }},
		{"frame-phase", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).phase = 1 }},
		{"frame-position", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.at(0).position = 1 }},
		{"string-text-copy", func(encoding *LegacyEnvelopeEncoding) {
			frame := encoding.scalar.frames.at(0)
			frame.kind, frame.text = 's', strings.Clone(encoding.scalar.root.(string))
		}},
		{"scalar-uninitialized", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.initialized = false }},
		{"scalar-complete", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.complete = true }},
		{"native-owner", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.nativeOwner = &legacyNativeCanonicalOwner{} }},
		{"preparation", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.preparation = &legacyCanonicalPreparation{} }},
		{"map-keys", func(encoding *LegacyEnvelopeEncoding) {
			encoding.scalar.mapKeys.single = &legacyCanonicalEncodingKeys{}
		}},
		{"key-heap", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.pendingKeysHeap.bytes = 1 }},
		{"key-order", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.order.count = 1 }},
		{"key-work", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.keyWork = 1 }},
		{"ordering", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.ordering = true }},
		{"numbers", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.numbers.next = 1 }},
		{"number-storage", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.numbers.pending.single = new(string) }},
		{"eager-numbers", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.eagerNumbers = true }},
		{"inspection-work", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.inspectWork++ }},
		{"storage-work", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.lastStorageBytes = 1 }},
		{"root-heap", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.rootHeap.bytes++ }},
		{"root-heap-error", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.rootHeap.err = ErrIntegrity }},
		{"missing-frame", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.clear() }},
		{"extra-frame", func(encoding *LegacyEnvelopeEncoding) { encoding.scalar.frames.count++ }},
		{"extra-frame-storage", func(encoding *LegacyEnvelopeEncoding) {
			encoding.scalar.frames.inline = new([legacyCanonicalInlineValues]legacyCanonicalEncodingFrame)
		}},
	}
	for _, mutation := range mutations {
		test.Run(mutation.name, func(test *testing.T) {
			encoding := legacyEnvelopeScalarCheckpointFixture(test, CIREnvelope{Cwd: strings.Repeat("bound", 32)}, "cwd", -1)
			mutation.mutate(encoding)
			if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("corrupt live scalar produced a control", err)
			}
		})
	}
}

func TestLegacyEnvelopeScalarCheckpointRejectsEnvelopePositions(test *testing.T) {
	mutations := []struct {
		name   string
		model  bool
		mutate func(*LegacyEnvelopeEncoding)
	}{
		{"uninitialized", false, func(encoding *LegacyEnvelopeEncoding) { encoding.initialized = false }},
		{"complete", false, func(encoding *LegacyEnvelopeEncoding) { encoding.complete = true }},
		{"start-phase", false, func(encoding *LegacyEnvelopeEncoding) { encoding.phase = legacyEnvelopeEncodingStart }},
		{"value-phase", false, func(encoding *LegacyEnvelopeEncoding) { encoding.phase = legacyEnvelopeEncodingValue }},
		{"model-value-phase", false, func(encoding *LegacyEnvelopeEncoding) { encoding.phase = legacyEnvelopeEncodingModelValue }},
		{"unknown-phase", false, func(encoding *LegacyEnvelopeEncoding) { encoding.phase = 255 }},
		{"field-zero", false, func(encoding *LegacyEnvelopeEncoding) { encoding.field = 0 }},
		{"field-negative", false, func(encoding *LegacyEnvelopeEncoding) { encoding.field = -1 }},
		{"field-past-end", false, func(encoding *LegacyEnvelopeEncoding) { encoding.field = len(encoding.fields) + 1 }},
		{"field-index-negative", false, func(encoding *LegacyEnvelopeEncoding) { encoding.fields[encoding.field-1].index = -1 }},
		{"field-index-past-end", false, func(encoding *LegacyEnvelopeEncoding) {
			encoding.fields[encoding.field-1].index = reflect.TypeFor[CIREnvelope]().NumField()
		}},
		{"model-from-string", false, func(encoding *LegacyEnvelopeEncoding) {
			encoding.phase, encoding.model = legacyEnvelopeEncodingModelNext, 1
		}},
		{"model-zero", true, func(encoding *LegacyEnvelopeEncoding) { encoding.model = 0 }},
		{"model-negative", true, func(encoding *LegacyEnvelopeEncoding) { encoding.model = -1 }},
		{"model-past-end", true, func(encoding *LegacyEnvelopeEncoding) { encoding.model = len(encoding.envelope.SourceModels) + 1 }},
		{"model-field-negative", true, func(encoding *LegacyEnvelopeEncoding) { encoding.field = -1 }},
		{"model-field-past-end", true, func(encoding *LegacyEnvelopeEncoding) { encoding.field = len(encoding.fields) }},
		{"model-as-nonnil-slice", true, func(encoding *LegacyEnvelopeEncoding) {
			encoding.phase, encoding.field = legacyEnvelopeEncodingFieldNext, encoding.field+1
		}},
		{"model-source-lost", true, func(encoding *LegacyEnvelopeEncoding) { encoding.envelope.SourceModels = nil }},
	}
	for _, mutation := range mutations {
		test.Run(mutation.name, func(test *testing.T) {
			envelope := CIREnvelope{Cwd: "cwd", SourceModels: []string{"first", "second"}}
			name, model := "cwd", -1
			if mutation.model {
				name, model = "source_models", 1
			}
			encoding := legacyEnvelopeScalarCheckpointFixture(test, envelope, name, model)
			mutation.mutate(encoding)
			if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("checkpoint accepted an invalid envelope position", err)
			}
			if scalar, err := restoreLegacyEnvelopeScalar(context.Background(), encoding, &legacyEnvelopeScalarCheckpointControl{}); scalar != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("restore accepted an invalid envelope position", err)
			}
		})
	}
}

func TestLegacyEnvelopeScalarCheckpointRejectsControlCorruption(test *testing.T) {
	encoding := legacyEnvelopeScalarCheckpointFixture(test, CIREnvelope{Cwd: "bound"}, "cwd", -1)
	states := []legacyEnvelopeScalarCheckpointControl{
		{Kind: 'a'}, {Kind: 255}, {Kind: 'l'}, {Phase: 1}, {Position: 1}, {Position: -1},
		{Kind: 's', Phase: 2}, {Kind: 's', Position: 1}, {Kind: 's', Phase: 1, Position: -1},
		{Kind: 's', Phase: 1, Position: len("bound") + 1}, {Kind: 's', Phase: 1, Position: int(^uint(0) >> 1)},
	}
	for index, state := range states {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			if scalar, err := restoreLegacyEnvelopeScalar(context.Background(), encoding, &state); scalar != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("corrupt scalar control was restored", state, err)
			}
		})
	}
	for _, state := range []legacyEnvelopeScalarCheckpointControl{{}, {Kind: 's'}, {Kind: 's', Phase: 1}, {Kind: 's', Phase: 1, Position: len("bound")}} {
		scalar, err := restoreLegacyEnvelopeScalar(context.Background(), encoding, &state)
		if err != nil || scalar == nil {
			test.Fatal("valid string cursor boundary was rejected", state, err)
		}
		probe := *encoding
		probe.scalar = scalar
		got, err := legacyEnvelopeScalarControlFor(&probe)
		if err != nil || got == nil || *got != state {
			test.Fatal("valid string cursor boundary did not round-trip", state, got, err)
		}
	}
}

func TestLegacyEnvelopeScalarCheckpointOwnedLiterals(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, number := range []int{maximum, -maximum - 1, -42} {
		test.Run(fmt.Sprint(number), func(test *testing.T) {
			encoding := legacyEnvelopeScalarCheckpointFixture(test, CIREnvelope{ContextTokens: number}, "context_tokens", -1)
			legacyEnvelopeScalarCheckpointAssertLiteral(test, encoding)
			for _, wrong := range []any{int64(number), float64(number), json.Number(fmt.Sprint(number)), nil} {
				original := encoding.scalar.root
				encoding.scalar.root = wrong
				if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, ErrIntegrity) {
					test.Fatal("integer root lost its exact type or value", err)
				}
				encoding.scalar.root = original
			}
		})
	}
	encoding := legacyEnvelopeEncodingFixture(test, CIREnvelope{SourceModels: []string{"seed"}})
	for steps := 0; ; steps++ {
		if steps > 256 {
			test.Fatal("nil slice fixture did not reach its value")
		}
		if encoding.phase == legacyEnvelopeEncodingValue && encoding.fields[encoding.field].name == "source_models" {
			encoding.envelope.SourceModels = nil
			if _, complete, err := encoding.Step(context.Background()); err != nil || complete || encoding.scalar == nil || encoding.scalar.root != nil {
				test.Fatal("actual envelope nil slice did not introduce a nil scalar", err)
			}
			break
		}
		if _, complete, err := encoding.Step(context.Background()); err != nil || complete {
			test.Fatal("nil slice fixture completed before its value", err)
		}
	}
	legacyEnvelopeScalarCheckpointAssertLiteral(test, encoding)
	encoding.scalar.root = []string(nil)
	if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("typed nil was accepted as the actual nil scalar root", err)
	}
}

func legacyEnvelopeScalarCheckpointAssertLiteral(test testing.TB, encoding *LegacyEnvelopeEncoding) {
	test.Helper()
	initial, err := legacyEnvelopeScalarControlFor(encoding)
	if err != nil || initial == nil || *initial != (legacyEnvelopeScalarCheckpointControl{}) {
		test.Fatal("actual envelope literal did not start as a leaf frame", err)
	}
	frame := encoding.scalar.frames.at(0)
	if err := encoding.scalar.introduce(frame); err != nil {
		test.Fatal(err)
	}
	literal := frame.text
	state, err := legacyEnvelopeScalarControlFor(encoding)
	if err != nil || state == nil || state.Kind != 'l' || !frame.ownedLiteral {
		test.Fatal("actual scalar introduction did not produce an owned literal", err)
	}
	for position := 0; position < len(literal); position++ {
		control := legacyEnvelopeScalarCheckpointControl{Kind: 'l', Position: position}
		scalar, err := restoreLegacyEnvelopeScalar(context.Background(), encoding, &control)
		if err != nil || scalar == nil || scalar.frames.at(0).text != literal || !scalar.frames.at(0).ownedLiteral || scalar.inspectWork != 2 {
			test.Fatal("restore did not derive the actual integer or null literal", control, err)
		}
		probe := *encoding
		probe.scalar = scalar
		got, err := legacyEnvelopeScalarControlFor(&probe)
		if err != nil || got == nil || *got != control {
			test.Fatal("literal cursor did not round-trip", control, got, err)
		}
		if chunk, complete, err := probe.Step(context.Background()); err != nil || complete || len(chunk) != 0 || probe.scalar != nil || string(probe.pending) != literal[position:] {
			test.Fatal("literal restore replayed consumed output", control, err)
		}
		if chunk, complete, err := probe.Step(context.Background()); err != nil || complete || string(chunk) != literal[position:] {
			test.Fatal("literal suffix was not emitted through the envelope Step", control, err)
		}
	}
	for _, control := range []legacyEnvelopeScalarCheckpointControl{
		{Kind: 's'}, {Kind: 'l', Phase: 1}, {Kind: 'l', Position: -1},
		{Kind: 'l', Position: len(literal)}, {Kind: 'l', Position: len(literal) + 1},
	} {
		if scalar, err := restoreLegacyEnvelopeScalar(context.Background(), encoding, &control); scalar != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid literal cursor was restored", control, err)
		}
	}
	frame.text = "false"
	if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("owned literal text was not checked against the envelope", err)
	}
	frame.text, frame.ownedLiteral = literal, false
	if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("unowned integer or null literal was accepted", err)
	}
	frame.ownedLiteral = true
}

func TestLegacyEnvelopeScalarCheckpointAbsenceFailureAndCancellation(test *testing.T) {
	ctx := context.Background()
	if state, err := legacyEnvelopeScalarControlFor(nil); state != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil envelope receiver was accepted", err)
	}
	if scalar, err := restoreLegacyEnvelopeScalar(ctx, nil, &legacyEnvelopeScalarCheckpointControl{}); scalar != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("present scalar restored without an envelope", err)
	}
	complete := legacyEnvelopeEncodingFixture(test, CIREnvelope{})
	collectLegacyEnvelopeEncoding(test, complete)
	failed := legacyEnvelopeEncodingFixture(test, CIREnvelope{})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := failed.Step(canceled); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	for _, encoding := range []*LegacyEnvelopeEncoding{{}, legacyEnvelopeEncodingFixture(test, CIREnvelope{}), complete, failed} {
		if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || err != nil {
			test.Fatal("absent scalar produced state or failure", err)
		}
		if scalar, err := restoreLegacyEnvelopeScalar(ctx, encoding, nil); scalar != nil || err != nil {
			test.Fatal("absent control did not restore an absent scalar", err)
		}
	}
	if scalar, err := restoreLegacyEnvelopeScalar(ctx, nil, nil); scalar != nil || err != nil {
		test.Fatal("absent control inspected an unused envelope", err)
	}
	for _, encoding := range []*LegacyEnvelopeEncoding{{}, complete, failed} {
		want := error(ErrIntegrity)
		if encoding.err != nil {
			want = encoding.err
		}
		if scalar, err := restoreLegacyEnvelopeScalar(ctx, encoding, &legacyEnvelopeScalarCheckpointControl{}); scalar != nil || !errors.Is(err, want) {
			test.Fatal("present control accepted an unusable envelope", err)
		}
	}
	encoding := legacyEnvelopeScalarCheckpointFixture(test, CIREnvelope{Cwd: strings.Repeat("x", LegacyCanonicalEncodingBytes)}, "cwd", -1)
	failure := errors.New("scalar checkpoint fixture failure")
	encoding.scalar.err = failure
	if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, failure) {
		test.Fatal("checkpoint did not preserve the scalar failure", err)
	}
	encoding.scalar.err = nil
	encoding.err = failure
	if state, err := legacyEnvelopeScalarControlFor(encoding); state != nil || !errors.Is(err, failure) {
		test.Fatal("checkpoint did not preserve the envelope failure", err)
	}
	if scalar, err := restoreLegacyEnvelopeScalar(ctx, encoding, &legacyEnvelopeScalarCheckpointControl{}); scalar != nil || !errors.Is(err, failure) {
		test.Fatal("restore did not preserve the envelope failure", err)
	}
	encoding.err = nil
	before, frameBefore := *encoding, *encoding.scalar.frames.at(0)
	for _, state := range []*legacyEnvelopeScalarCheckpointControl{nil, {}} {
		if scalar, err := restoreLegacyEnvelopeScalar(canceled, encoding, state); scalar != nil || !errors.Is(err, context.Canceled) {
			test.Fatal("restore ignored cancellation", err)
		}
	}
	for _, limit := range []int{2, 4, 6, 8} {
		base, stop := context.WithCancel(ctx)
		checks := 0
		during := canonicalAdmissionContext{Context: base, check: func() {
			checks++
			if checks == limit {
				stop()
			}
		}}
		scalar, err := restoreLegacyEnvelopeScalar(during, encoding, &legacyEnvelopeScalarCheckpointControl{})
		canceledDuringRestore := checks >= limit
		stop()
		if !canceledDuringRestore {
			if scalar == nil || err != nil {
				test.Fatal("restore failed before cancellation was requested", limit, checks, err)
			}
			scalar.release()
		} else if scalar != nil || !errors.Is(err, context.Canceled) {
			test.Fatal("restore ignored cancellation during leaf reconstruction", limit, checks, err)
		}
	}
	if !reflect.DeepEqual(before, *encoding) || !reflect.DeepEqual(frameBefore, *encoding.scalar.frames.at(0)) {
		test.Fatal("failed restore consumed live envelope state")
	}
}
