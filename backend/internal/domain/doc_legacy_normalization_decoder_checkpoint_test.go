package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const legacyDecoderCheckpointBudget int64 = 64 << 20
const legacyDecoderCheckpointSteps = 65536

type legacyDecoderCheckpointFixture struct {
	name string
	raw  string
	cut  func(*LegacyNormalizationDecoder) bool
}

func legacyDecoderCheckpointFixtures() []legacyDecoderCheckpointFixture {
	envelope := `{"cir_version":"2","source_provider":"codex","fidelity":"full"}`
	event := `{"kind":"message","seq":3,"role":"user","provider_metadata":{"turn_id":"before-cut"},"blocks":[{"type":"text","text":"first"}]}`
	large := strings.Repeat("한🙂x", 4*ConversationManifestChunkBytes/8)
	return []legacyDecoderCheckpointFixture{
		{name: "active-envelope-scalar", raw: `{"envelope":{"cwd":"` + large + `","cir_version":"2","source_provider":"codex","fidelity":"full"},"events":[` + event + `]}`, cut: func(decoder *LegacyNormalizationDecoder) bool {
			return decoder.envelopeActive && decoder.envelopeNative.scalar != nil && decoder.envelopeNative.scalar.text != nil && decoder.envelopeNative.scalar.text.Len() > 0 && decoder.envelopeNative.scalar.position < decoder.envelopeNative.scalar.end
		}},
		{name: "active-event-child-scalar", raw: `{"envelope":` + envelope + `,"events":[` + event + `,{"kind":"message","seq":1,"role":"assistant","blocks":[{"type":"text","text":"` + large + `"}]}]}`, cut: func(decoder *LegacyNormalizationDecoder) bool {
			if decoder.native == nil || decoder.count != 1 {
				return false
			}
			child := legacyNativeEventW82List(decoder.native.object)
			return child != nil && child.object != nil && child.object.scalar != nil && child.object.scalar.text != nil && child.object.scalar.text.Len() > 0 && child.object.scalar.position < child.object.scalar.end
		}},
		{name: "outer-key-after-event-with-reset", raw: `{"envelope":` + envelope + `,"events":[` + event + `],"` + strings.Repeat("ignored-key", ConversationManifestChunkBytes/2) + `":{"ignored":true},"events":[{"kind":"message","seq":9,"role":"user","blocks":[{"type":"text","text":"after-reset"}]}]}`, cut: func(decoder *LegacyNormalizationDecoder) bool {
			return decoder.phase == legacyNormalizationKey && decoder.keyBytes == len(decoder.key)+1 && decoder.nativeCache != nil && decoder.native == nil && decoder.count == 1
		}},
		{name: "rejection-verification-progress", raw: `{"envelope":` + envelope + `,"events":[` + event + `,!` + strings.Repeat(" ", 2*ConversationManifestChunkBytes), cut: func(decoder *LegacyNormalizationDecoder) bool {
			return decoder.phase == legacyNormalizationReject && decoder.rejectPart > 0 && decoder.rejectPart < len(decoder.source.Parts)
		}},
	}
}

func legacyDecoderCheckpointDrain(test *testing.T, task interface {
	step(context.Context) (bool, error)
	retainedBytes() (int64, error)
}, budget int64) {
	test.Helper()
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		phase, childPhase, childEnd, position, depth, active := -1, -1, -1, -1, -1, false
		if restore, ok := task.(*legacyNormalizationDecoderCheckpointRestore); ok {
			phase, position, active = int(restore.phase), restore.normalizer.position, restore.control.EnvelopeActive
			if restore.child != nil {
				childPhase, childEnd, depth = int(restore.child.phase), restore.child.state.End, restore.child.state.ParentDepth
			}
		}
		complete, err := task.step(context.Background())
		if err != nil {
			test.Fatal("decoder checkpoint task failed", attempt, phase, childPhase, childEnd, position, depth, active, err)
		}
		retained, err := task.retainedBytes()
		if err != nil || retained < 0 || retained > budget {
			test.Fatal("decoder checkpoint escaped its retained budget", retained, err)
		}
		if complete {
			return
		}
	}
	test.Fatal("decoder checkpoint did not make bounded progress")
}

func legacyDecoderCheckpointConsume(test *testing.T, step LegacyNormalizationStep) []string {
	test.Helper()
	var trace []string
	if step.Reset {
		trace = append(trace, "reset")
	}
	for _, event := range step.Events {
		body, err := legacyNormalizationTestBytes(context.Background(), event)
		if err != nil {
			test.Fatal("actual emitted encoding failed", err)
		}
		trace = append(trace, fmt.Sprintf("event:%d:%s", event.Sequence, HashContent(body)))
	}
	return trace
}

func legacyDecoderCheckpointContinue(test *testing.T, decoder *LegacyNormalizationDecoder) []string {
	test.Helper()
	var trace []string
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		step, err := decoder.Step(context.Background())
		if err != nil {
			if !errors.Is(err, ErrIntegrity) {
				test.Fatal("unexpected continuation failure", err)
			}
			return append(trace, "integrity")
		}
		trace = append(trace, legacyDecoderCheckpointConsume(test, step)...)
		if step.Complete {
			return append(trace, fmt.Sprintf("header:%d:%s", step.Count, HashContent(step.Envelope)))
		}
	}
	test.Fatal("actual decoder continuation did not terminate")
	return nil
}

func legacyDecoderCheckpointCut(test *testing.T, fixture legacyDecoderCheckpointFixture) (*LegacyNormalizationDecoder, *legacySourceProofBuffer) {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(fixture.raw)}
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		step, err := decoder.Step(context.Background())
		if err != nil || step.Complete {
			test.Fatal("decoder finished before real successful-Step cut", fixture.name, decoder.phase, err)
		}
		legacyDecoderCheckpointConsume(test, step)
		if fixture.cut(decoder) {
			return decoder, buffer
		}
	}
	test.Fatal("actual decoder did not reach requested successful-Step cut", fixture.name)
	return nil, nil
}

func legacyDecoderCheckpointCapture(test *testing.T, decoder *LegacyNormalizationDecoder, binding ContentHash, store legacyNativeCheckpointIO) legacyNativeCheckpointRef {
	test.Helper()
	work, err := newLegacyNormalizationDecoderCheckpoint(context.Background(), decoder, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal("decoder capture construction", decoder.phase, err)
	}
	defer work.close()
	if _, err := decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("actual decoder escaped capture barrier", err)
	}
	legacyDecoderCheckpointDrain(test, work, legacyDecoderCheckpointBudget)
	root, err := work.take(context.Background())
	if err != nil || !root.valid() {
		test.Fatal("decoder capture did not hand off a valid root", err)
	}
	return root
}

func legacyDecoderCheckpointRestore(test *testing.T, root legacyNativeCheckpointRef, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO) *LegacyNormalizationDecoder {
	test.Helper()
	work, err := newLegacyNormalizationDecoderCheckpointRestore(context.Background(), root, store, source, binding, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal("decoder restore construction", err)
	}
	defer work.close()
	legacyDecoderCheckpointDrain(test, work, legacyDecoderCheckpointBudget)
	decoder, err := work.take(context.Background())
	if err != nil || decoder == nil {
		test.Fatal("decoder restore did not hand off actual decoder", err)
	}
	return decoder
}

func TestLegacyNormalizationDecoderCheckpointActualContinuation(test *testing.T) {
	for _, fixture := range legacyDecoderCheckpointFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			store, _ := legacyNativeCheckpointStoreFixture(test)
			binding := HashContent(buffer.body)
			reads := len(buffer.reads)
			root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
			if len(buffer.reads) != reads {
				test.Fatal("capture replayed original input")
			}
			restored := legacyDecoderCheckpointRestore(test, root, binding, buffer.input(), store)
			originalFrame, originalErr := decoder.frame.checkpoint(context.Background())
			restoredFrame, restoredErr := restored.frame.checkpoint(context.Background())
			if originalErr != nil || restoredErr != nil || !bytes.Equal(originalFrame, restoredFrame) || len(buffer.reads) != reads || restored.position != decoder.position || restored.phase != decoder.phase || restored.count != decoder.count || restored.requiresV2 != decoder.requiresV2 || restored.invalid != decoder.invalid || restored.key != decoder.key || restored.keyBytes != decoder.keyBytes || restored.rejectPart != decoder.rejectPart {
				test.Fatal("decoder state was replayed or changed during restore")
			}
			if decoder.native == nil && restored.nativeCache != nil {
				test.Fatal("empty event scratch cache became persisted semantic state")
			}
			second := legacyDecoderCheckpointCapture(test, restored, binding, store)
			again := legacyDecoderCheckpointRestore(test, second, binding, buffer.input(), store)
			original := legacyDecoderCheckpointContinue(test, decoder)
			if got := legacyDecoderCheckpointContinue(test, restored); !reflect.DeepEqual(got, original) {
				test.Fatal("restored actual decoder changed semantic continuation", got, original)
			}
			if got := legacyDecoderCheckpointContinue(test, again); !reflect.DeepEqual(got, original) {
				test.Fatal("second checkpoint did not close over needed state", got, original)
			}
		})
	}
}
