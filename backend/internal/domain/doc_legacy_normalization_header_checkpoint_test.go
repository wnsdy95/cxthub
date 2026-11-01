package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyHeaderCheckpointFixtures() []legacyDecoderCheckpointFixture {
	unit := strings.Repeat("checkpoint/", 1024) + "\ud55c🙂<&>\\\"\\\\\\n\\u2028\\u2029/"
	large := strings.Repeat(unit, ConversationManifestChunkBytes/len(unit)+1)
	raw := `{"envelope":{"cir_version":"2","source_provider":"codex","fidelity":"full","cwd":"` + large +
		`","source_models":["discarded","old-tail"],"source_models":["same","` + large +
		`","same","tail"]},"events":[{"kind":"message","seq":3,"role":"user","blocks":[{"type":"text","text":"before-header"}]},{"kind":"turn","seq":1,"role":"assistant"}]}`
	initial := func(decoder *LegacyNormalizationDecoder) bool {
		header := decoder.finalizing
		return decoder.phase == legacyNormalizationFinalizing && header != nil && header.encoding != nil &&
			header.encoding.phase == legacyEnvelopeEncodingStart && header.encoding.scalar == nil && header.encoding.pending == nil &&
			header.length == 0 && header.position == 0 && !header.copying && header.first == nil && header.header == nil && header.output == nil
	}
	return []legacyDecoderCheckpointFixture{
		{name: "source-model-scalar-with-pending-utf8", raw: raw, cut: func(decoder *LegacyNormalizationDecoder) bool {
			header := decoder.finalizing
			if decoder.phase != legacyNormalizationFinalizing || header == nil || header.encoding == nil {
				return false
			}
			encoding := header.encoding
			if encoding.field >= len(encoding.fields) || encoding.fields[encoding.field].name != "source_models" || encoding.model != 2 ||
				encoding.scalar == nil || len(encoding.pending) == 0 || encoding.scalar.frames.length() != 1 {
				return false
			}
			frame := encoding.scalar.frames.at(0)
			return frame.kind == 's' && frame.position > 0 && frame.position < len(frame.text)
		}},
		{name: "copying-chunks-to-header", raw: raw, cut: func(decoder *LegacyNormalizationDecoder) bool {
			header := decoder.finalizing
			return decoder.phase == legacyNormalizationFinalizing && header != nil && header.encoding == nil && !header.copying &&
				header.first != nil && header.position > 0 && header.position < header.length
		}},
		{name: "copying-header-to-output", raw: raw, cut: func(decoder *LegacyNormalizationDecoder) bool {
			header := decoder.finalizing
			return decoder.phase == legacyNormalizationFinalizing && header != nil && header.encoding == nil && header.copying &&
				header.first == nil && header.position > 0 && header.position < header.length && len(header.output) == header.length
		}},
		{name: "empty-envelope-initial", raw: `{"envelope":{},"events":[]}`, cut: initial},
		{name: "missing-envelope-initial", raw: `{"events":[{"kind":"turn","seq":4,"role":"user"}]}`, cut: initial},
	}
}

func TestLegacyNormalizationHeaderCheckpointActualContinuation(test *testing.T) {
	for _, fixture := range legacyHeaderCheckpointFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			var document CIRDocument
			if err := json.Unmarshal([]byte(fixture.raw), &document); err != nil {
				test.Fatal("header fixture is not an original typed document", err)
			}
			wantHeader, err := canonicalJSON(document.Envelope)
			if err != nil {
				test.Fatal("original envelope cannot be canonically encoded", err)
			}
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			store, _ := legacyNativeCheckpointStoreFixture(test)
			binding := HashContent(buffer.body)
			reads := len(buffer.reads)
			root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
			if len(buffer.reads) != reads {
				test.Fatal("header capture reread original source")
			}
			checkpointing := true
			source := buffer.input()
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				if checkpointing {
					test.Fatal("header restore or recapture attempted an original source read", offset, length)
				}
				return buffer.Read(ctx, offset, length)
			}
			assertProgress := func(restored *LegacyNormalizationDecoder) {
				test.Helper()
				if !fixture.cut(restored) || restored.phase != decoder.phase || restored.position != decoder.position ||
					restored.count != decoder.count || restored.requiresV2 != decoder.requiresV2 || restored.invalid != decoder.invalid {
					test.Fatal("restored actual decoder lost its header cut or consumer state")
				}
				want, got := decoder.finalizing, restored.finalizing
				if got.length != want.length || got.position != want.position || got.copying != want.copying ||
					!bytes.Equal(got.header, want.header) || !bytes.Equal(got.output, want.output) {
					test.Fatal("header restore changed staged bytes or copy progress", got.length, want.length, got.position, want.position)
				}
				if (got.encoding == nil) != (want.encoding == nil) {
					test.Fatal("header restore changed encoding ownership")
				}
				if want.encoding != nil {
					actual, original := got.encoding, want.encoding
					if actual.phase != original.phase || actual.field != original.field || actual.model != original.model ||
						!bytes.Equal(actual.pending, original.pending) || !reflect.DeepEqual(actual.envelope, original.envelope) ||
						(actual.scalar == nil) != (original.scalar == nil) {
						test.Fatal("header restore changed envelope, model cursor, scalar ownership, or pending output")
					}
					if original.scalar != nil {
						if actual.scalar.frames.length() != original.scalar.frames.length() ||
							actual.scalar.frames.at(0).position != original.scalar.frames.at(0).position ||
							actual.scalar.frames.at(0).text != original.scalar.frames.at(0).text {
							test.Fatal("header restore replayed or skipped part of the active string")
						}
					}
				}
			}
			restored := legacyDecoderCheckpointRestore(test, root, binding, source, store)
			assertProgress(restored)
			second := legacyDecoderCheckpointCapture(test, restored, binding, store)
			again := legacyDecoderCheckpointRestore(test, second, binding, source, store)
			assertProgress(again)
			if len(buffer.reads) != reads {
				test.Fatal("header checkpoint round trip replayed original input")
			}
			checkpointing = false
			original := legacyDecoderCheckpointContinue(test, decoder)
			wantTrace := []string{fmt.Sprintf("header:%d:%s", len(document.Events), HashContent(wantHeader))}
			if !reflect.DeepEqual(original, wantTrace) || !bytes.Equal(decoder.header, wantHeader) || decoder.count != len(document.Events) {
				test.Fatal("uninterrupted actual decoder changed original header or count", original, wantTrace)
			}
			for _, candidate := range []*LegacyNormalizationDecoder{restored, again} {
				if got := legacyDecoderCheckpointContinue(test, candidate); !reflect.DeepEqual(got, original) ||
					!bytes.Equal(candidate.header, decoder.header) || candidate.count != decoder.count {
					test.Fatal("restored or recaptured actual decoder changed header continuation", got, original)
				}
			}
		})
	}
}

func TestLegacyNormalizationHeaderCheckpointNewProcess(test *testing.T) {
	legacyDecoderCheckpointNewProcesses(test, legacyHeaderCheckpointFixtures())
}
