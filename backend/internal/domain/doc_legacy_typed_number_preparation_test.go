package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyTypedNumberPreparationStart(test *testing.T, raw []byte) (*LegacyNormalizationPreparation, *legacyNormalizationPreparationStorage, *legacySourceProofBuffer) {
	test.Helper()
	if ConversationManifestChunkBytes != 512<<10 || LegacyCanonicalEncodingBytes != 64<<10 ||
		legacyNormalizationBufferBytes != 64<<10 || LegacyNormalizationStepEvents != 64 || LegacyNormalizationSlots != 4 {
		test.Fatal("typed Number preparation changed the existing source, encoding, buffer, or batch limits")
	}
	storage := &legacyNormalizationPreparationStorage{test: test, borrowed: make([]byte, 512<<10)}
	storage.appendHook = func(slot int, body []byte) error {
		if len(body) > 64<<10 {
			test.Fatal("typed Number preparation bypassed the original scratch append quantum", slot, len(body))
		}
		return nil
	}
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	source := legacyNormalizationPreparationInput(test, input, true)
	constructorContext, cancel := context.WithCancel(context.Background())
	work, err := NewLegacyNormalizationPreparation(constructorContext, source, storage)
	cancel()
	if err != nil || work == nil || len(input.reads) != 0 {
		test.Fatal("typed Number preparation failed or eagerly loaded its source", err, len(input.reads))
	}
	for slot := range storage.slots {
		if storage.resets[slot] != 1 || len(storage.slots[slot]) != 0 {
			test.Fatal("typed Number preparation did not reset its actual scratch slots", slot)
		}
	}
	for ordinal := range source.Parts {
		source.Parts[ordinal].Hash = HashContent([]byte("caller replaced typed Number source descriptor"))
	}
	legacyNormalizationPreparationPending(test, work)
	return work, storage, input
}

func legacyTypedNumberPreparationStep(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, input *legacySourceProofBuffer) (bool, error) {
	test.Helper()
	legacyNormalizationPreparationPending(test, work)
	decoder := work.decoder
	position := 0
	if decoder != nil {
		position = decoder.position
	}
	phase, pending, written := work.phase, len(work.pending), work.files[0].size
	reads := len(input.reads)
	stepContext, cancel := context.WithCancel(context.Background())
	complete, err := work.Step(stepContext)
	cancel()
	loads := 0
	for _, read := range input.reads[reads:] {
		if read[0] == len(input.body) && (read[1] == 0 || read[1] == 1) {
			continue
		}
		if read[0] < 0 || read[0] >= len(input.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(input.body)-read[0]) {
			test.Fatal("typed Number preparation bypassed authenticated full-part reads", read)
		}
		loads++
	}
	if loads > 2 || decoder != nil && (decoder.position < position || decoder.position-position > 512<<10) {
		test.Fatal("typed Number preparation exceeded the original two-load or 512 KiB decoder quantum", loads)
	}
	if phase == legacyNormalizationStage && pending == 1 && work.files[0].size-written > 64<<10 {
		test.Fatal("typed Number staging exceeded the original 64 KiB encoding quantum", work.files[0].size-written)
	}
	clear(storage.borrowed)
	if err != nil {
		if complete {
			test.Fatal("failed typed Number preparation exposed completion", err)
		}
		legacyNormalizationPreparationPending(test, work)
		return false, err
	}
	retained, retainedErr := work.RetainedBytes()
	if retainedErr != nil || retained <= 0 {
		test.Fatal("typed Number preparation lost retained ownership accounting", retained, retainedErr)
	}
	if work.decoder != nil {
		decoderBytes, decoderErr := work.decoder.RetainedBytes()
		if decoderErr != nil || decoderBytes <= 0 || retained < decoderBytes {
			test.Fatal("preparation stopped charging its actual decoder owner", retained, decoderBytes, decoderErr)
		}
		if work.decoder.native == nil && decoderBytes >= 64<<10 {
			test.Fatal("released typed Number decoder retained payload beyond the original 64 KiB gate", decoderBytes)
		}
	}
	withoutValues := *work
	withoutValues.decoder, withoutValues.pending = nil, nil
	quantumBytes, quantumErr := withoutValues.RetainedBytes()
	if quantumErr != nil || quantumBytes <= 0 || quantumBytes >= 2<<20 {
		test.Fatal("typed Number preparation retained scratch storage beyond the original 2 MiB quantum gate", quantumBytes, quantumErr)
	}
	if len(work.pending) == 0 && (work.decoder == nil || work.decoder.native == nil) && retained >= 2<<20 {
		test.Fatal("typed Number preparation retained released values or source-owned storage", retained)
	}
	if !complete {
		legacyNormalizationPreparationPending(test, work)
	}
	return complete, nil
}

func legacyTypedNumberPreparationInputEOF(test *testing.T, input *legacySourceProofBuffer) {
	test.Helper()
	zeroEOF, oneEOF := false, false
	for _, read := range input.reads {
		if read[0] == len(input.body) {
			zeroEOF = zeroEOF || read[1] == 0
			oneEOF = oneEOF || read[1] == 1
		}
	}
	if !zeroEOF || !oneEOF {
		test.Fatal("typed Number preparation finished before both authenticated input EOF probes", zeroEOF, oneEOF)
	}
}

func TestLegacyTypedNumberPreparationNativeCanonicalAndOwnership(test *testing.T) {
	zeroes := strings.Repeat("0", (1<<20)+73)
	lexeme := "-0." + zeroes + "12500E+003"
	for _, fixture := range []struct {
		name   string
		value  string
		resume bool
	}{
		{name: "raw-number", value: lexeme, resume: true},
		{name: "escaped-quoted-number", value: `"\u002d0\u002e` + zeroes + `12500\u0045\u002b003"`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"events":[{"kind":"turn","seq":7,"role":"user","provider_metadata":{"turn_id":"retained","create_time":` + fixture.value + `},"PROVIDER_METADATA":{"turn_id":null}}],"envelope":{"cir_version":"2"}}`)
			canonical, original, err := legacyNormalizationPreparationOracle(raw)
			if err != nil || !original.Valid() || !bytes.Contains(canonical, []byte(`"create_time":`+lexeme)) {
				test.Fatal("whole native CIR oracle rejected or changed the provider Number lexeme", err)
			}
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			storage.readHook = func(slot, offset, length int) ([]byte, error, bool) {
				legacyNormalizationPreparationPending(test, work)
				return nil, nil, false
			}
			parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
			var encoding *LegacyCanonicalEncoding
			resumed, pendingEOF, complete := false, false, false
			for stepIndex := 0; stepIndex < 64+64*parts; stepIndex++ {
				if work.phase == legacyNormalizationStage && encoding == nil {
					if len(work.pending) != 1 || work.next != 0 || work.pending[0].Encoding == nil {
						test.Fatal("typed Number did not enter preparation through its actual pending encoder")
					}
					encoding = work.pending[0].Encoding
					fields, ok := encoding.root.(map[string]any)
					if !ok {
						test.Fatal("typed Number pending encoder lost its native canonical object")
					}
					metadata, ok := fields["provider_metadata"].(map[string]any)
					if !ok {
						test.Fatal("typed Number pending encoder lost provider metadata")
					}
					number, ok := metadata["create_time"].(*json.Number)
					if !ok || number == nil || number.String() != lexeme || metadata["turn_id"] != "retained" {
						test.Fatal("preparation converted the typed Number or changed whole-native provider merge semantics")
					}
					encodingBytes, encodingErr := encoding.RetainedBytes()
					retained, retainedErr := work.RetainedBytes()
					withoutPending := *work
					withoutPending.pending = nil
					baseBytes, baseErr := withoutPending.RetainedBytes()
					var pendingHeap docRangeHeaderHeapEstimate
					pendingHeap.allocation(int64(cap(work.pending)), int64(unsafe.Sizeof(LegacyNormalizedEvent{})))
					if encodingErr != nil || retainedErr != nil || baseErr != nil || pendingHeap.err != nil ||
						encodingBytes < 2*int64(len(lexeme)) || baseBytes >= 2<<20 ||
						retained-baseBytes != pendingHeap.bytes+encodingBytes {
						test.Fatal("preparation lost or double charged its live Number encoder instead of retaining the original quantum gate", retained, baseBytes, encodingBytes)
					}
					decoderBytes, decoderErr := work.decoder.RetainedBytes()
					if decoderErr != nil || decoderBytes <= 0 || decoderBytes >= 64<<10 {
						test.Fatal("canonical handoff left the typed Number in the decoder", decoderBytes, decoderErr)
					}
				}
				if fixture.resume && !resumed && work.phase == legacyNormalizationStage && work.written > 0 {
					before := *work
					retained, retainedErr := work.RetainedBytes()
					inputReads, scratchReads, resets := len(input.reads), len(storage.reads), storage.resets
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					finished, cancelErr := work.Step(ctx)
					after, afterErr := work.RetainedBytes()
					if finished || !errors.Is(cancelErr, context.Canceled) || retainedErr != nil || afterErr != nil || retained != after ||
						!reflect.DeepEqual(*work, before) || len(input.reads) != inputReads || len(storage.reads) != scratchReads || storage.resets != resets {
						test.Fatal("pre-canceled preparation mutated its live Number, scratch progress, or source reads", cancelErr)
					}
					legacyNormalizationPreparationPending(test, work)
					resumed = true
				}
				pendingEOF = pendingEOF || work.phase == legacyNormalizationVerify && work.verified == len(work.files[3].parts)
				complete, err = legacyTypedNumberPreparationStep(test, work, storage, input)
				if err != nil {
					test.Fatal("full preparation rejected a whole-native typed Number", err)
				}
				if complete {
					break
				}
			}
			if !complete || encoding == nil || !encoding.complete || encoding.root != nil || encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) ||
				work.decoder != nil || work.pending != nil || !pendingEOF || fixture.resume != resumed || len(work.files[3].parts) < 3 {
				test.Fatal("typed Number preparation did not release its owner and cross all output verification quanta", complete, pendingEOF, resumed)
			}
			retained, retainedErr := work.RetainedBytes()
			if retainedErr != nil || retained <= 0 || retained >= 2<<20 || len(storage.slots[3]) <= 1<<20 {
				test.Fatal("ready preparation retained source-owned canonical bytes or lost the original 2 MiB gate", retained, retainedErr)
			}
			legacyTypedNumberPreparationInputEOF(test, input)
			clear(input.body)
			storage.readHook = nil
			legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
		})
	}
}

func TestLegacyTypedNumberPreparationBadQuotedNumberRejectsSourceAtomically(test *testing.T) {
	badNumber := `"0` + strings.Repeat("1", (1<<20)+73) + `"`
	stagedID := strings.Repeat("p", (64<<10)+17)
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"turn","seq":-1,"role":"user","id":"` + stagedID + `"},{"kind":"turn","seq":7,"provider_metadata":{"create_time":` + badNumber + `,"create_time":1}}],"events":[]}`)
	canonical, original, oracleErr := legacyNormalizationPreparationOracle(raw)
	if !json.Valid(raw) || !errors.Is(oracleErr, ErrIntegrity) || canonical != nil || original.Valid() {
		test.Fatal("whole native CIR oracle repaired a bad quoted Number", oracleErr)
	}
	work, storage, input := legacyTypedNumberPreparationStart(test, raw)
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	var rejection error
	for stepIndex := 0; stepIndex < 64+64*parts; stepIndex++ {
		complete, err := legacyTypedNumberPreparationStep(test, work, storage, input)
		if complete {
			test.Fatal("bad quoted Number published a normalized source")
		}
		if err != nil {
			rejection = err
			break
		}
	}
	if !errors.Is(rejection, ErrIntegrity) || errors.Is(rejection, ErrPreparedDocRangeObject) ||
		len(storage.slots[0]) == 0 || work.count != 1 || len(storage.slots[3]) != 0 || work.files[3].size != 0 {
		test.Fatal("bad quoted Number repaired or published private staged events, or became a source failure", rejection, work.count)
	}
	legacyTypedNumberPreparationInputEOF(test, input)
	inputReads, scratchReads := len(input.reads), len(storage.reads)
	legacyNormalizationPreparationFailed(test, work, rejection, ErrIntegrity)
	if len(input.reads) != inputReads || len(storage.reads) != scratchReads {
		test.Fatal("terminal typed Number rejection resumed private source or scratch I/O")
	}
}
