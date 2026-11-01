package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyEnvelopeEncodingFixture(test testing.TB, envelope CIREnvelope) *LegacyEnvelopeEncoding {
	test.Helper()
	var heap docRangeHeaderHeapEstimate
	if err := legacyNativeValueHeapWalk(context.Background(), reflect.ValueOf(envelope), 0, false, &heap); err != nil {
		test.Fatal(err)
	}
	encoding, err := newLegacyEnvelopeEncoding(context.Background(), envelope, heap)
	if err != nil {
		test.Fatal(err)
	}
	return encoding
}

func collectLegacyEnvelopeEncoding(test testing.TB, encoding *LegacyEnvelopeEncoding) ([]byte, int) {
	test.Helper()
	var result []byte
	for steps := 1; steps <= 1<<20; steps++ {
		before := fmt.Sprintf("%d/%d/%d/%p/%d", encoding.phase, encoding.field, encoding.model, encoding.scalar, len(encoding.pending))
		chunk, complete, err := encoding.Step(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			test.Fatal("envelope output exceeds the canonical quantum", len(chunk), cap(chunk))
		}
		if _, err := encoding.RetainedBytes(); err != nil {
			test.Fatal("invalid live retained estimate", err)
		}
		result = append(result, chunk...)
		if complete {
			if !reflect.DeepEqual(encoding.envelope, CIREnvelope{}) || encoding.fields != nil ||
				encoding.scalar != nil || encoding.pending != nil || encoding.envelopeHeap.bytes != 0 {
				test.Fatal("completed encoding retains input or work buffers")
			}
			return result, steps
		}
		after := fmt.Sprintf("%d/%d/%d/%p/%d", encoding.phase, encoding.field, encoding.model, encoding.scalar, len(encoding.pending))
		if len(chunk) == 0 && before == after {
			test.Fatal("empty Step made no structural or scalar progress")
		}
	}
	test.Fatal("envelope encoding did not terminate")
	return nil, 0
}

func assertLegacyEnvelopeCanonicalBytes(test testing.TB, envelope CIREnvelope) {
	test.Helper()
	want, err := canonicalJSON(envelope)
	if err != nil {
		test.Fatal(err)
	}
	original := envelope
	original.SourceModels = append([]string(nil), envelope.SourceModels...)
	if envelope.SourceModels != nil && len(envelope.SourceModels) == 0 {
		original.SourceModels = []string{}
	}
	encoding := legacyEnvelopeEncodingFixture(test, envelope)
	got, _ := collectLegacyEnvelopeEncoding(test, encoding)
	if !bytes.Equal(got, want) {
		test.Fatalf("envelope canonical bytes differ: got=%q want=%q", got, want)
	}
	if !reflect.DeepEqual(envelope, original) {
		test.Fatal("encoding changed the typed envelope")
	}
	chunk, complete, err := encoding.Step(context.Background())
	if err != nil || !complete || len(chunk) != 0 {
		test.Fatal("completed encoding changed on retry", len(chunk), complete, err)
	}
	var empty docRangeHeaderHeapEstimate
	empty.allocation(1, int64(unsafe.Sizeof(*encoding)))
	if retained, err := encoding.RetainedBytes(); err != nil || retained != empty.bytes {
		test.Fatal("completed encoding retains more than its inline state", retained, empty.bytes, err)
	}
}

func TestLegacyEnvelopeEncodingCanonicalTypedParity(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	envelopes := []CIREnvelope{
		{},
		{SourceModels: []string{}},
		{SourceModels: []string{"", "same", "same", "last"}},
		{ContextTokens: maximum, OutputTokens: -maximum - 1, CompactionCount: -1},
		{ContextTokens: -maximum - 1, OutputTokens: maximum, CompactionCount: -maximum - 1},
		{ContextTokens: -1, OutputTokens: 1, CompactionCount: maximum},
	}
	for _, text := range []string{
		"v2", "\"\\/\b\f\n\r\t\x00\x01\x1f<&>\u2028\u2029한국🙂\ufffd",
		"\xff\x80\xc0\xaf\xed\xa0\x80\xf4\x90\x80\x80", `\ufffd\\ufffd`,
	} {
		var envelope CIREnvelope
		value := reflect.ValueOf(&envelope).Elem()
		for index := 0; index < value.NumField(); index++ {
			member := value.Field(index)
			switch member.Kind() {
			case reflect.String:
				member.SetString(text)
			case reflect.Int:
				member.SetInt(int64(index + 1))
			case reflect.Slice:
				member.Set(reflect.ValueOf([]string{text, "", text, "last"}))
			default:
				test.Fatalf("update the parity fixture for native envelope type %s", member.Type())
			}
		}
		envelopes = append(envelopes, envelope)
	}
	for index, envelope := range envelopes {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			assertLegacyEnvelopeCanonicalBytes(test, envelope)
		})
	}
}

func TestLegacyEnvelopeEncodingNativeDuplicateAndNullParity(test *testing.T) {
	for index, raw := range []string{
		`null`, `{}`, `{"source_models":null}`, `{"source_models":[]}`,
		`{"source_models":["first","same","same"],"source_models":["last","last"]}`,
		`{"source_models":["old","tail"],"source_models":[null,"new","new"]}`,
		`{"source_models":["old","tail"],"SOURCE_MODELS":null,"Source_Models":["again",null]}`,
		`{"source_model":"old","SOURCE_MODEL":null,"source_models":["kept"],"source_models":null}`,
		`{"context_tokens":7,"CONTEXT_TOKENS":null,"output_tokens":0,"compaction_count":-7}`,
		`{"source_models":["\ud800","\udfff","\ud83d\ude42"],"ignored":{"source_models":["wrong"]}}`,
	} {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			var envelope CIREnvelope
			if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
				test.Fatal(err)
			}
			assertLegacyEnvelopeCanonicalBytes(test, envelope)
		})
	}
}

func TestLegacyEnvelopeEncodingGiantStringsAndModels(test *testing.T) {
	for _, text := range []string{
		strings.Repeat("a", 3*LegacyCanonicalEncodingBytes+17),
		strings.Repeat("🙂<&>\u2028\u2029\"\\\x00\xff", LegacyCanonicalEncodingBytes/4),
		strings.Repeat("a", LegacyCanonicalEncodingBytes-3) + "🙂\xff" + strings.Repeat("b", LegacyCanonicalEncodingBytes+1),
	} {
		envelope := CIREnvelope{CIRVersion: "2", Cwd: text, SourceModels: []string{"same", text, "same", text}}
		assertLegacyEnvelopeCanonicalBytes(test, envelope)
	}
	models := make([]string, 2049)
	for index := range models {
		models[index] = fmt.Sprintf("model-%d", index%17)
	}
	assertLegacyEnvelopeCanonicalBytes(test, CIREnvelope{SourceModels: models})
}

func TestLegacyEnvelopeEncodingSharedQuantumAndOwnedOutput(test *testing.T) {
	text := strings.Repeat("a", LegacyCanonicalEncodingBytes-3) + "🙂\xff\\ufffd" + strings.Repeat("<&>\xff", LegacyCanonicalEncodingBytes)
	envelope := CIREnvelope{Cwd: text, SourceModels: []string{text, text}}
	encoding := legacyEnvelopeEncodingFixture(test, envelope)
	var chunks, snapshots [][]byte
	var got []byte
	for steps := 0; ; steps++ {
		if steps > 1<<16 {
			test.Fatal("envelope encoding did not terminate")
		}
		scalar, pending := encoding.scalar, encoding.pending
		position, sourceLength := 0, 0
		if scalar != nil {
			if source, ok := scalar.root.(string); ok {
				sourceLength = len(source)
				position = scalar.frames.at(0).position
			}
		}
		chunk, complete, err := encoding.Step(context.Background())
		if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			test.Fatal("invalid envelope output quantum", len(chunk), cap(chunk), err)
		}
		if scalar != nil && pending == nil {
			next := sourceLength
			if scalar.frames.length() != 0 {
				next = scalar.frames.at(0).position
			}
			if next-position > LegacyCanonicalEncodingBytes || len(pending) != 0 || len(chunk) != 0 {
				test.Fatal("scalar input and postprocessing did not yield independently", next-position, len(chunk))
			}
		}
		if pending != nil && (len(pending) > LegacyCanonicalEncodingBytes || encoding.scalar != scalar) {
			test.Fatal("postprocessing shared a quantum with more scalar input")
		}
		if pending != nil && scalar != nil && scalar.frames.length() != 0 && scalar.frames.at(0).position != position {
			test.Fatal("postprocessing also advanced the scalar source")
		}
		if len(chunk) != 0 {
			chunks = append(chunks, chunk)
			snapshots = append(snapshots, bytes.Clone(chunk))
			got = append(got, chunk...)
		}
		if complete {
			break
		}
	}
	want, err := canonicalJSON(envelope)
	if err != nil || !bytes.Equal(got, want) {
		test.Fatal("quantum boundaries changed complete canonical bytes", err)
	}
	for index := range chunks {
		if !bytes.Equal(chunks[index], snapshots[index]) {
			test.Fatal("later steps overwrote an owned chunk")
		}
	}
	for index := range chunks {
		if !bytes.Equal(chunks[index], snapshots[index]) {
			test.Fatal("output chunks share mutable backing storage")
		}
		chunks[index][0] ^= 0xff
	}
}

func TestLegacyEnvelopeEncodingTransfersHeapLedgerWithoutModelPreparation(test *testing.T) {
	models := make([]string, 2, 19)
	models[0], models[1] = strings.Repeat("m", 4*LegacyCanonicalEncodingBytes), "duplicate"
	models[:cap(models)][18] = strings.Repeat("hidden", 4096)
	envelope := CIREnvelope{Cwd: models[0], SourceModels: models}
	encoding := legacyEnvelopeEncodingFixture(test, envelope)
	if &encoding.envelope.SourceModels[0] != &models[0] || encoding.scalar != nil || encoding.pending != nil {
		test.Fatal("constructor cloned or prepared the model array")
	}
	minimum := int64(cap(models))*int64(unsafe.Sizeof("")) + int64(2*len(models[0])+len(models[:cap(models)][18]))
	retained, err := encoding.RetainedBytes()
	if err != nil || retained < minimum || retained < encoding.envelopeHeap.bytes {
		test.Fatal("transferred input capacity or hidden references are uncharged", retained, minimum, err)
	}
	extendedHeap := encoding.envelopeHeap
	extendedHeap.bytes += 4096
	base, err := newLegacyEnvelopeEncoding(context.Background(), envelope, extendedHeap)
	if err != nil {
		test.Fatal(err)
	}
	baseRetained, err := base.RetainedBytes()
	if err != nil || baseRetained-retained != 4096 {
		test.Fatal("input ledger was not included exactly once", retained, baseRetained, err)
	}
	for encoding.pending == nil || len(encoding.pending) < 4096 {
		if _, complete, err := encoding.Step(context.Background()); err != nil || complete {
			test.Fatal("large scalar fixture did not yield", err)
		}
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*encoding)))
	expected.include(encoding.envelopeHeap)
	expected.allocation(int64(cap(encoding.fields)), int64(unsafe.Sizeof(legacyEnvelopeEncodingField{})))
	for _, field := range encoding.fields {
		expected.allocation(int64(len(field.name)), 1)
		expected.allocation(int64(cap(field.key)), 1)
	}
	expected.allocation(int64(cap(encoding.pending)), 1)
	if encoding.scalar == nil {
		test.Fatal("large scalar was eagerly completed")
	}
	scalarRetained, err := encoding.scalar.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	expected.include(docRangeHeaderHeapEstimate{bytes: scalarRetained - encoding.scalarBorrowed.bytes})
	if retained, err := encoding.RetainedBytes(); err != nil || retained != expected.bytes {
		test.Fatal("pending owned output or scalar metadata was miscounted", retained, expected.bytes, err)
	}
	var pendingHeap docRangeHeaderHeapEstimate
	pendingHeap.allocation(int64(cap(encoding.pending)), 1)
	beforeReturn, err := encoding.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := encoding.Step(context.Background()); err != nil {
		test.Fatal(err)
	}
	afterReturn, err := encoding.RetainedBytes()
	if err != nil || encoding.pending != nil || beforeReturn-afterReturn != pendingHeap.bytes {
		test.Fatal("returned chunk remains owned by the helper")
	}
	collectLegacyEnvelopeEncoding(test, encoding)
	if models[:cap(models)][18] != strings.Repeat("hidden", 4096) {
		test.Fatal("hidden model references were changed")
	}
}

func TestLegacyEnvelopeEncodingFreshContexts(test *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	encoding, err := newLegacyEnvelopeEncoding(base, CIREnvelope{}, docRangeHeaderHeapEstimate{})
	cancel()
	if err != nil {
		test.Fatal(err)
	}
	var got []byte
	for steps := 0; ; steps++ {
		if steps > 1024 {
			test.Fatal("small envelope did not terminate")
		}
		ctx, cancel := context.WithCancel(context.Background())
		chunk, complete, err := encoding.Step(ctx)
		cancel()
		if err != nil {
			test.Fatal("an earlier constructor or Step context was retained", err)
		}
		got = append(got, chunk...)
		if complete {
			break
		}
	}
	want, err := canonicalJSON(CIREnvelope{})
	if err != nil || !bytes.Equal(got, want) {
		test.Fatal("fresh contexts changed canonical bytes", err)
	}
}

type legacyEnvelopeFinalizationContext struct {
	context.Context
	encoding *LegacyEnvelopeEncoding
}

func (ctx legacyEnvelopeFinalizationContext) Err() error {
	if ctx.encoding.complete {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestLegacyEnvelopeEncodingCancellationReleasesAllState(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if encoding, err := newLegacyEnvelopeEncoding(canceled, CIREnvelope{}, docRangeHeaderHeapEstimate{}); encoding != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("constructor ignored cancellation", err)
	}
	for _, stage := range []string{"before-step", "active-scalar", "pending-output", "finalization"} {
		test.Run(stage, func(test *testing.T) {
			encoding := legacyEnvelopeEncodingFixture(test, CIREnvelope{Cwd: strings.Repeat("<&>\xff", LegacyCanonicalEncodingBytes)})
			ctx := context.Context(canceled)
			if stage != "before-step" {
				for steps := 0; ; steps++ {
					if steps > 1024 {
						test.Fatal("cancellation fixture did not reach its stage")
					}
					ready := stage == "active-scalar" && encoding.scalar != nil ||
						stage == "pending-output" && len(encoding.pending) >= 4096 ||
						stage == "finalization" && encoding.phase == legacyEnvelopeEncodingFieldNext &&
							encoding.field == len(encoding.fields) && encoding.scalar == nil && encoding.pending == nil
					if ready {
						break
					}
					if _, complete, err := encoding.Step(context.Background()); err != nil || complete {
						test.Fatal("cancellation fixture completed early", err)
					}
				}
			}
			if stage == "pending-output" || stage == "active-scalar" {
				base, stop := context.WithCancel(context.Background())
				defer stop()
				checks := 0
				ctx = canonicalAdmissionContext{Context: base, check: func() {
					checks++
					if checks == 3 {
						stop()
					}
				}}
			}
			if stage == "finalization" {
				ctx = legacyEnvelopeFinalizationContext{Context: context.Background(), encoding: encoding}
			}
			chunk, complete, err := encoding.Step(ctx)
			if len(chunk) != 0 || complete || !errors.Is(err, context.Canceled) {
				test.Fatal("cancellation returned partial output or success", len(chunk), complete, err)
			}
			if !reflect.DeepEqual(encoding.envelope, CIREnvelope{}) || encoding.fields != nil || encoding.scalar != nil || encoding.pending != nil || encoding.envelopeHeap.bytes != 0 {
				test.Fatal("cancellation retained envelope or encoding storage")
			}
			if _, _, err := encoding.Step(context.Background()); !errors.Is(err, context.Canceled) {
				test.Fatal("failed work resumed with a fresh context", err)
			}
			if retained, err := encoding.RetainedBytes(); retained != 0 || !errors.Is(err, context.Canceled) {
				test.Fatal("failed work reports a usable retained estimate", retained, err)
			}
		})
	}
}

func TestLegacyEnvelopeEncodingInvalidReceiverAndLedger(test *testing.T) {
	for _, encoding := range []*LegacyEnvelopeEncoding{nil, {}} {
		if _, _, err := encoding.Step(context.Background()); !errors.Is(err, ErrValidation) {
			test.Fatal("invalid encoding receiver accepted", err)
		}
		if _, err := encoding.RetainedBytes(); !errors.Is(err, ErrValidation) {
			test.Fatal("invalid encoding retained estimate accepted", err)
		}
	}
	for _, heap := range []docRangeHeaderHeapEstimate{{bytes: -1}, {bytes: math.MaxInt64}, {err: ErrIntegrity}} {
		if encoding, err := newLegacyEnvelopeEncoding(context.Background(), CIREnvelope{}, heap); encoding != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid or overflowing input heap ledger accepted", err)
		}
	}
	if encoding, err := newLegacyEnvelopeEncoding(context.Background(), CIREnvelope{Cwd: "unaccounted"}, docRangeHeaderHeapEstimate{}); encoding != nil || !errors.Is(err, ErrIntegrity) {
		test.Fatal("obviously incomplete input heap ledger accepted", err)
	}
}
