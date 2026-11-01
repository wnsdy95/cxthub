package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

const legacyCanonicalNativeRejectionTarget = `{"kind":"tool_call","seq":91,"id":"private-inspect-target","input":{"value":[true,null,3]}}`

const legacyCanonicalNativeRejectionSiblings = `{"kind":"turn","seq":7,"id":"tie-first"},` +
	`{"kind":"message","seq":-9,"id":"earliest","role":"user","blocks":[{"type":"text","text":"<&>한🙂"}]},` +
	`{"kind":"turn","seq":7,"id":"tie-second"},` +
	`{"kind":"tool_result","seq":0,"id":"middle","output":{"nested":[false,null,{"value":0.5}]}}`

func legacyCanonicalNativeRejectionInjectInspectFailure(test *testing.T, work *LegacyNormalizationPreparation) error {
	test.Helper()
	if work.phase != legacyNormalizationStage || work.next != 0 || len(work.pending) == 0 ||
		work.count != 0 || work.written != 0 || work.files[0].size != 0 || work.files[1].size != 0 {
		test.Fatal("private inspection injection did not precede event bytes and order records")
	}
	encoding := work.pending[0].Encoding
	if encoding == nil || encoding.nativeOwner == nil || encoding.preparation == nil ||
		encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 {
		test.Fatal("private inspection injection requires a real, unvisited native-owner encoding")
	}
	if retained, err := encoding.RetainedBytes(); err != nil || retained <= 0 {
		test.Fatal("real native owner lost its initial retained-byte accounting", retained, err)
	}
	projected, ok := encoding.root.(map[string]any)
	if !ok {
		test.Fatal("real native owner did not expose its canonical event projection")
	}
	payload, ok := projected["input"].(map[string]any)
	if !ok {
		test.Fatal("real native event did not retain its decoded input map")
	}
	if _, exists := payload["value"]; !exists {
		test.Fatal("private fault target is absent from the real native input map")
	}
	test.Log("PRIVATE FAULT INJECTION: replace a decoded input value with an unsupported Go struct after real native-owner construction; this is not a raw JSON reproducer")
	payload["value"] = struct{}{}
	preparation := encoding.preparation
	legacyNormalizationPreparationPending(test, work)
	if complete, err := work.Step(context.Background()); complete || err != nil {
		test.Fatal("deferred semantic failure became terminal before remaining events or source validation", complete, err)
	}
	failure := encoding.err
	if !errors.Is(failure, ErrValidation) || encoding.nativeOwner != nil || encoding.preparation != nil ||
		encoding.inspectWork == 0 || encoding.inspectWork > legacyCanonicalEncodingActions ||
		preparation.lastActions <= 0 || preparation.lastActions > legacyCanonicalEncodingActions ||
		preparation.lastBytes < 0 || preparation.lastBytes > LegacyCanonicalEncodingBytes {
		test.Fatal("private input mutation did not exercise and release bounded deferred inspection", encoding.inspectWork, failure)
	}
	if body, complete, err := encoding.Next(context.Background()); len(body) != 0 || complete || !errors.Is(err, failure) {
		test.Fatal("injected private inspection failure was not sticky and zero-valued", len(body), complete, err)
	}
	if work.decoder == nil || !work.decoder.invalid || work.decoder.phase == legacyNormalizationReject ||
		work.count != 0 || work.written != 0 || work.hashState != nil || work.active != (legacyNormalizationRecord{}) ||
		work.files[0].size != 0 || work.files[1].size != 0 || work.next != 1 ||
		work.phase == legacyNormalizationStage && work.pending[0] != (LegacyNormalizedEvent{}) {
		test.Fatal("failed pending event acquired bytes or a record, or prematurely rejected the decoder", work.count, work.files[0].size, work.files[1].size)
	}
	legacyNormalizationPreparationPending(test, work)
	return failure
}

func legacyCanonicalNativeRejectionDrainPending(test *testing.T, work *LegacyNormalizationPreparation) {
	test.Helper()
	for quantum := 0; quantum < 256; quantum++ {
		legacyNormalizationPreparationPending(test, work)
		if work.phase != legacyNormalizationStage {
			if work.phase != legacyNormalizationDecode || work.pending != nil || work.written != 0 || work.hashState != nil {
				test.Fatal("pending event rejection did not return cleanly to decoding", work.phase, work.written)
			}
			return
		}
		encoding := work.pending[work.next].Encoding
		preparing := encoding != nil && encoding.preparation != nil
		var inspected uint64
		if encoding != nil {
			inspected = encoding.inspectWork
		}
		pending := make([]*LegacyCanonicalEncoding, 0, len(work.pending)-work.next)
		prior := make([]uint64, 0, cap(pending))
		for _, event := range work.pending[work.next:] {
			if event.Encoding != nil && event.Encoding.preparation != nil {
				pending = append(pending, event.Encoding)
				prior = append(prior, event.Encoding.inspectWork)
			}
		}
		bodyBytes, records, count := work.files[0].size, work.files[1].size, work.count
		if complete, err := work.Step(context.Background()); complete || err != nil {
			test.Fatal("a failed event prevented valid pending siblings from progressing", complete, err)
		}
		prepared := 0
		for index, candidate := range pending {
			if candidate.inspectWork < prior[index] || candidate.inspectWork-prior[index] > legacyCanonicalEncodingActions {
				test.Fatal("one inspection exceeded its original action quantum", prior[index], candidate.inspectWork)
			}
			if candidate.inspectWork != prior[index] {
				prepared++
			}
		}
		if prepared*LegacyCanonicalEncodingBytes+work.files[0].size-bodyBytes > ConversationManifestChunkBytes {
			test.Fatal("pending sibling preparation and output exceeded the shared stage budget", prepared, work.files[0].size-bodyBytes)
		}
		if preparing && encoding.preparation != nil &&
			(work.files[0].size != bodyBytes || work.files[1].size != records || work.count != count) {
			test.Fatal("still-active inspection emitted bytes or committed a record", inspected, encoding.inspectWork)
		}
	}
	test.Fatal("pending siblings exceeded the finite rejection fixture allowance")
}

func legacyCanonicalNativeRejectionRecords(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, want []CIREvent) {
	test.Helper()
	body := append(bytes.Clone(storage.slots[0]), work.files[0].buffer...)
	records := append(bytes.Clone(storage.slots[1]), work.files[1].buffer...)
	if work.count != len(want) || len(records) != len(want)*legacyNormalizationRecordBytes ||
		work.files[1].size != len(records) || work.files[0].size != len(body) {
		test.Fatal("failed pending event changed the valid sibling record count", work.count, len(records), len(want))
	}
	position := uint64(0)
	for ordinal, expected := range want {
		record := records[ordinal*legacyNormalizationRecordBytes : (ordinal+1)*legacyNormalizationRecordBytes]
		sequence := int64(binary.BigEndian.Uint64(record[:8]))
		offset, length := binary.BigEndian.Uint64(record[16:24]), binary.BigEndian.Uint64(record[24:32])
		if sequence != int64(expected.Seq) || binary.BigEndian.Uint64(record[8:16]) != uint64(ordinal) ||
			offset != position || offset > uint64(len(body)) || length == 0 || length > uint64(len(body))-offset {
			test.Fatal("rejection changed a sibling sequence, stable ordinal, or contiguous body range", ordinal, sequence, offset, length)
		}
		raw := body[offset : offset+length]
		digest := sha256.Sum256(raw)
		var event CIREvent
		if err := json.Unmarshal(raw, &event); err != nil || event.Seq != expected.Seq || event.ID != expected.ID || !bytes.Equal(record[32:], digest[:]) {
			test.Fatal("rejection substituted a sibling body or its original digest", ordinal, event.Seq, event.ID, err)
		}
		position += length
	}
	if position != uint64(len(body)) || bytes.Contains(body, []byte("private-inspect-target")) {
		test.Fatal("failed inspection retained staged bytes without a valid sibling record", position, len(body))
	}
}

func legacyCanonicalNativeRejectionCheckSource(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, canonical []byte) {
	test.Helper()
	source, err := work.Source()
	if err != nil || source.Read == nil || source.Bytes != len(canonical) || !bytes.Equal(storage.slots[3], canonical) {
		test.Fatal("completed overwrite changed the independent canonical bytes or source", source.Bytes, err)
	}
	var actual []byte
	for ordinal, part := range source.Parts {
		body, err := source.Read(context.Background(), ordinal*ConversationManifestChunkBytes, int(part.Bytes))
		if err != nil || len(body) != int(part.Bytes) || HashContent(body) != part.Hash {
			test.Fatal("completed overwrite exposed an unauthenticated output part", ordinal, err)
		}
		actual = append(actual, body...)
	}
	if !bytes.Equal(actual, canonical) {
		test.Fatal("completed overwrite exposed different canonical source bytes")
	}
}

func TestLegacyCanonicalNativeRejectionPrivateInspectFailureSourcePrecedence(test *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + legacyCanonicalNativeRejectionTarget + `,` +
		legacyCanonicalNativeRejectionSiblings + `],"ignored":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}`)
	if _, err := legacyNormalizationOracle(raw); err != nil {
		test.Fatal("private inspection fixture is not valid real JSON before fault injection", err)
	}
	for _, fault := range []struct {
		name      string
		damage    string
		atEOF     bool
		eofLength int
		cancel    bool
	}{
		{name: "authenticated-semantic-rejection"},
		{name: "late-source-hash", damage: "hash"},
		{name: "late-short-source", damage: "short"},
		{name: "late-source-context", cancel: true},
		{name: "late-source-hash-context", damage: "hash", cancel: true},
		{name: "zero-private-eof", damage: "private-eof", atEOF: true},
		{name: "one-private-eof", damage: "private-eof", atEOF: true, eofLength: 1},
		{name: "one-missing-eof", damage: "missing-eof", atEOF: true, eofLength: 1},
		{name: "one-extra-byte", damage: "extra-byte", atEOF: true, eofLength: 1},
		{name: "zero-eof-context", atEOF: true, cancel: true},
		{name: "one-private-eof-context", damage: "private-eof", atEOF: true, eofLength: 1, cancel: true},
		{name: "one-missing-eof-context", damage: "missing-eof", atEOF: true, eofLength: 1, cancel: true},
	} {
		test.Run(fault.name, func(test *testing.T) {
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			source := legacyNormalizationPreparationInput(test, input, true)
			readSource := source.Read
			last := (len(raw) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
			privateFailure := errors.New("private source fault sentinel")
			var work *LegacyNormalizationPreparation
			var cancelStep context.CancelFunc
			var probes [2]int
			injected, triggered, lateReads, failedAt := false, false, 0, -1
			faulty := fault.damage != "" || fault.cancel
			source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
				body, err := readSource(ctx, offset, length)
				late := offset == last && length > 0
				atEOF := offset == len(raw) && (length == 0 || length == 1)
				if late || atEOF {
					if !injected || work == nil || work.decoder == nil || !work.decoder.invalid || work.count != 4 {
						test.Fatal("late source validation preceded rejection and valid sibling staging", injected, offset, length)
					}
					legacyNormalizationPreparationPending(test, work)
				}
				if late {
					lateReads++
				}
				if atEOF {
					probes[length]++
				}
				matches := fault.atEOF && atEOF && length == fault.eofLength || !fault.atEOF && late
				if !faulty || triggered || !matches {
					return body, err
				}
				triggered, failedAt = true, len(input.reads)
				if fault.cancel {
					cancelStep()
				}
				switch fault.damage {
				case "hash":
					corrupt := bytes.Clone(body)
					corrupt[0] ^= 1
					return corrupt, err
				case "short":
					return body[:len(body)-1], err
				case "private-eof":
					return nil, errors.Join(ErrPreparedDocRangeObject, privateFailure, io.EOF)
				case "missing-eof":
					return nil, nil
				case "extra-byte":
					return []byte{'!'}, nil
				default:
					return body, err
				}
			}
			storage := &legacyNormalizationPreparationStorage{test: test}
			var err error
			work, err = NewLegacyNormalizationPreparation(context.Background(), source, storage)
			if err != nil || len(input.reads) != 0 {
				test.Fatal("native rejection constructor read source bytes or failed", err, len(input.reads))
			}
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
			injectedFailure := legacyCanonicalNativeRejectionInjectInspectFailure(test, work)
			injected = true
			legacyCanonicalNativeRejectionDrainPending(test, work)
			legacyCanonicalNativeRejectionRecords(test, work, storage, []CIREvent{
				{Seq: 7, ID: "tie-first"}, {Seq: -9, ID: "earliest"}, {Seq: 7, ID: "tie-second"}, {Seq: 0, ID: "middle"},
			})
			for quantum := 0; quantum < 128+32*len(source.Parts); quantum++ {
				legacyNormalizationPreparationPending(test, work)
				stepContext, stop := context.WithCancel(context.Background())
				cancelStep = stop
				complete, err := work.Step(stepContext)
				stop()
				if complete || len(storage.slots[3]) != 0 {
					test.Fatal("rejected inspection published output before source and EOF validation", complete, err)
				}
				if err == nil {
					continue
				}
				private := faulty && !fault.cancel
				want := error(ErrIntegrity)
				if fault.cancel {
					want = context.Canceled
				} else if private {
					want = ErrPreparedDocRangeObject
				}
				if triggered != faulty || lateReads == 0 || faulty && len(input.reads) != failedAt ||
					!errors.Is(err, want) || errors.Is(err, ErrPreparedDocRangeObject) != private ||
					errors.Is(err, ErrIntegrity) != !fault.cancel || errors.Is(err, context.Canceled) != fault.cancel ||
					errors.Is(err, injectedFailure) || errors.Is(err, ErrValidation) ||
					errors.Is(err, privateFailure) != (fault.damage == "private-eof" && !fault.cancel) {
					test.Fatal("deferred inspection changed source, EOF, or context failure precedence", err, triggered, lateReads, len(input.reads), failedAt)
				}
				if !faulty && (probes[0] == 0 || probes[1] == 0) ||
					faulty && !fault.atEOF && probes != [2]int{} ||
					fault.atEOF && (probes[0] != 1 || probes[1] != fault.eofLength) {
					test.Fatal("deferred inspection skipped EOF validation or read beyond its failure", probes)
				}
				reads := len(input.reads)
				legacyNormalizationPreparationFailed(test, work, err, want)
				if len(input.reads) != reads || work.count != 4 || work.files[1].size != 4*legacyNormalizationRecordBytes {
					test.Fatal("terminal inspection rejection resumed reads or recorded the failed event")
				}
				return
			}
			test.Fatal("deferred inspection rejection exhausted its finite source-validation allowance")
		})
	}
}

func TestLegacyCanonicalNativeRejectionPrivateInspectFailureLaterEventsOverwrite(test *testing.T) {
	for _, replacement := range []struct {
		name   string
		fields string
		count  int
	}{
		{name: "null", fields: `"events":null`},
		{name: "empty-array", fields: `"events":[]`},
		{name: "selected-siblings", fields: `"events":[` + legacyCanonicalNativeRejectionSiblings + `]`, count: 4},
		{name: "folded-selected-siblings", fields: `"EvEnTs":[` + legacyCanonicalNativeRejectionSiblings + `]`, count: 4},
		{name: "escaped-folded-selected-siblings", fields: `"event\u017f":[` + legacyCanonicalNativeRejectionSiblings + `]`, count: 4},
		{name: "null-then-selected-siblings", fields: `"events":null,"events":[` + legacyCanonicalNativeRejectionSiblings + `]`, count: 4},
	} {
		test.Run(replacement.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + legacyCanonicalNativeRejectionTarget +
				`,{"kind":"turn","seq":-2,"id":"discarded-sibling"}],` + replacement.fields + `}`)
			canonical, err := legacyNormalizationOracle(raw)
			if err != nil {
				test.Fatal("real JSON overwrite control lost its independent acceptance", err)
			}
			work, storage := legacyNormalizationPreparationStart(test, raw, true)
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
			legacyCanonicalNativeRejectionInjectInspectFailure(test, work)
			legacyCanonicalNativeRejectionDrainPending(test, work)
			legacyCanonicalNativeRejectionRecords(test, work, storage, []CIREvent{{Seq: -2, ID: "discarded-sibling"}})
			resets := storage.resets[0]
			legacyNormalizationPreparationAdvance(test, work, func() bool { return storage.resets[0] > resets })
			if work.decoder == nil || work.decoder.invalid || work.decoder.phase == legacyNormalizationReject ||
				work.count != 0 || work.files[0].size != 0 || work.files[1].size != 0 {
				test.Fatal("later events overwrite did not clear semantic invalidity and discarded sibling history")
			}
			if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
				test.Fatal("discarded private inspection failure survived a later events overwrite", err)
			}
			if work.count != replacement.count || bytes.Contains(storage.slots[3], []byte("discarded-sibling")) ||
				bytes.Contains(storage.slots[3], []byte("private-inspect-target")) {
				test.Fatal("overwritten private inspection event or valid sibling leaked into the selected events", work.count)
			}
			legacyCanonicalNativeRejectionCheckSource(test, work, storage, canonical)
		})
	}
}

func TestLegacyCanonicalNativeRejectionRealUnionFailureLaterEventsOverwrite(test *testing.T) {
	for _, invalid := range []struct {
		name  string
		event string
	}{
		{name: "unknown-kind", event: `{"kind":"unknown","seq":91}`},
		{name: "wrong-kind-presence", event: `{"kind":"turn","replacement":null}`},
		{name: "invalid-replacement-child", event: `{"kind":"compaction","replacement":[{}]}`},
	} {
		test.Run(invalid.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + invalid.event + `,` +
				legacyCanonicalNativeRejectionSiblings + `],"events":[` + legacyCanonicalNativeRejectionSiblings + `]}`)
			canonical, err := legacyNormalizationOracle(raw)
			if err != nil {
				test.Fatal("real union overwrite fixture lost original typed acceptance", err)
			}
			work, storage := legacyNormalizationPreparationStart(test, raw, true)
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
			legacyCanonicalNativeRejectionDrainPending(test, work)
			if work.decoder == nil || !work.decoder.invalid || work.decoder.phase == legacyNormalizationReject {
				test.Fatal("real union failure rejected before a later events field could overwrite it")
			}
			legacyCanonicalNativeRejectionRecords(test, work, storage, []CIREvent{
				{Seq: 7, ID: "tie-first"}, {Seq: -9, ID: "earliest"}, {Seq: 7, ID: "tie-second"}, {Seq: 0, ID: "middle"},
			})
			if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
				test.Fatal("later events field no longer clears the original union invalid flag", err)
			}
			legacyCanonicalNativeRejectionCheckSource(test, work, storage, canonical)
		})
	}
}

func TestLegacyCanonicalNativeRejectionPrivateInspectFailureLastPendingOverwrite(test *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + legacyCanonicalNativeRejectionTarget +
		`],"events":[` + legacyCanonicalNativeRejectionSiblings + `]}`)
	canonical, err := legacyNormalizationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	if len(work.pending) != 1 {
		test.Fatal("last-pending inspection fixture did not isolate its native owner", len(work.pending))
	}
	legacyCanonicalNativeRejectionInjectInspectFailure(test, work)
	if work.phase != legacyNormalizationDecode || work.pending != nil {
		test.Fatal("last failed pending event did not resume decoding for a later events overwrite", work.phase)
	}
	legacyCanonicalNativeRejectionDrainPending(test, work)
	legacyCanonicalNativeRejectionRecords(test, work, storage, nil)
	if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
		test.Fatal("last pending inspection failure prevented the later events overwrite", err)
	}
	if work.count != 4 {
		test.Fatal("last pending inspection failure changed the selected sibling count", work.count)
	}
	legacyCanonicalNativeRejectionCheckSource(test, work, storage, canonical)
}

func TestLegacyCanonicalNativeRejectionValidSiblingSequenceControl(test *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + legacyCanonicalNativeRejectionSiblings + `]}`)
	canonical, err := legacyNormalizationOracle(raw)
	if err != nil {
		test.Fatal(err)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
	legacyCanonicalNativeRejectionDrainPending(test, work)
	if work.decoder == nil || work.decoder.invalid {
		test.Fatal("valid sibling sequence was marked semantically invalid")
	}
	legacyCanonicalNativeRejectionRecords(test, work, storage, []CIREvent{
		{Seq: 7, ID: "tie-first"}, {Seq: -9, ID: "earliest"}, {Seq: 7, ID: "tie-second"}, {Seq: 0, ID: "middle"},
	})
	if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
		test.Fatal("valid siblings failed canonical normalization", err)
	}
	legacyCanonicalNativeRejectionCheckSource(test, work, storage, canonical)
	var document CIRDocument
	if err := json.Unmarshal(storage.slots[3], &document); err != nil || len(document.Events) != 4 {
		test.Fatal("valid sibling normalization changed document shape", err)
	}
	for ordinal, expected := range []string{"earliest", "middle", "tie-first", "tie-second"} {
		if document.Events[ordinal].ID != expected {
			test.Fatal("valid sibling normalization changed sequence sorting or stable ties", ordinal, document.Events[ordinal].ID)
		}
	}
}
