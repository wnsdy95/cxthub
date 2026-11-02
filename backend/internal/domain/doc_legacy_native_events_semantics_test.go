package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func legacyNativeEventsSemanticsOracle(test *testing.T, raw []byte, rejection string) ([]byte, VerifiedSessionDoc) {
	test.Helper()
	var document CIRDocument
	decodeErr := json.Unmarshal(raw, &document)
	var canonical []byte
	var canonicalErr error
	if decodeErr == nil {
		canonical, canonicalErr = CanonicalBytes(document)
	}
	var typeErr *json.UnmarshalTypeError
	switch rejection {
	case "typed":
		if !errors.As(decodeErr, &typeErr) {
			test.Fatal("fixture did not retain its original typed JSON rejection", decodeErr)
		}
	case "semantic":
		if decodeErr != nil || canonicalErr == nil || errors.Is(canonicalErr, ErrUnsupportedCIRVersion) {
			test.Fatal("fixture did not reach its original union/presence rejection", decodeErr, canonicalErr)
		}
	case "version":
		if decodeErr != nil || !errors.Is(canonicalErr, ErrUnsupportedCIRVersion) {
			test.Fatal("fixture did not retain its original version rejection", decodeErr, canonicalErr)
		}
	case "":
		if decodeErr != nil || canonicalErr != nil || !json.Valid(canonical) {
			test.Fatal("original json.Unmarshal and CanonicalBytes rejected the accepted fixture", decodeErr, canonicalErr)
		}
	default:
		test.Fatal("unknown expected rejection class", rejection)
	}
	oracle, original, oracleErr := legacyNormalizationPreparationOracle(raw)
	if rejection != "" {
		if !errors.Is(oracleErr, ErrIntegrity) || oracle != nil || original.Valid() {
			test.Fatal("original public preparation oracle accepted a rejected fixture", rejection, oracleErr)
		}
		return nil, VerifiedSessionDoc{}
	}
	if oracleErr != nil || !original.Valid() || !bytes.Equal(oracle, canonical) {
		test.Fatal("original preparation oracle changed accepted canonical bytes", oracleErr)
	}
	return canonical, original
}

func legacyNativeEventsSemanticsFinish(test *testing.T, raw, canonical []byte, original VerifiedSessionDoc, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, input *legacySourceProofBuffer) {
	test.Helper()
	for quantum := 0; quantum < 8192; quantum++ {
		complete, err := legacyTypedNumberPreparationStep(test, work, storage, input)
		if err != nil {
			if canonical != nil || !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) || errors.Is(err, context.Canceled) ||
				len(storage.slots[3]) != 0 || work.files[3].size != 0 {
				test.Fatal("public preparation changed rejection class or exposed rejected output", err)
			}
			legacyTypedNumberPreparationInputEOF(test, input)
			reads, scratchReads, resets := len(input.reads), len(storage.reads), storage.resets
			legacyNormalizationPreparationFailed(test, work, err, ErrIntegrity)
			if len(input.reads) != reads || len(storage.reads) != scratchReads || storage.resets != resets || !bytes.Equal(input.body, raw) {
				test.Fatal("sticky rejection performed I/O, reset scratch, or changed original bytes")
			}
			return
		}
		if !complete {
			continue
		}
		if canonical == nil || !bytes.Equal(input.body, raw) || sha256.Sum256(storage.slots[3]) != sha256.Sum256(canonical) {
			test.Fatal("public preparation accepted a rejection or changed original/canonical SHA bytes")
		}
		legacyTypedNumberPreparationInputEOF(test, input)
		legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
		return
	}
	test.Fatal("public native event-list semantics exceeded the finite preparation allowance")
}

func legacyNativeEventsSemanticsNested(tail string) string {
	dense := legacyCanonicalReplacementBudgetDense(1025)
	return `{"kind":"compaction","id":"outer","replacement":[{"kind":"compaction","id":"inner","replacement":` +
		dense[:len(dense)-1] + `,` + tail + `]}]}`
}

func legacyNativeEventsSemanticsPause(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, input *legacySourceProofBuffer) (*legacyNormalizationNativeEvent, *legacyNativeEventList) {
	test.Helper()
	for quantum := 0; quantum < 8192; quantum++ {
		if work.phase == legacyNormalizationDecode && work.decoder != nil && work.decoder.native != nil {
			native := work.decoder.native
			object := native.object
			if object.eventActive && object.field >= 0 && object.field < len(object.events) {
				root := object.events[object.field]
				if root != nil && root.cursor != nil && root.cursor != root && root.cursor.parent == root &&
					root.cursor.count > 0 && root.cursor.count < 1025 {
					if root.count != 0 || native.decoded.Replacement != nil || root.child.Replacement != nil || native.canonicalReady ||
						len(work.pending) != 0 || work.count != 0 || work.written != 0 ||
						work.files[0].size != 0 || work.files[1].size != 0 || work.files[3].size != 0 || len(storage.slots[3]) != 0 {
						test.Fatal("partially decoded nested children escaped before their parent event completed")
					}
					legacyNormalizationPreparationPending(test, work)
					legacyNativeCanonicalOwnerReject(test, native, ErrIntegrity)
					return native, root
				}
			}
		}
		if complete, err := legacyTypedNumberPreparationStep(test, work, storage, input); err != nil || complete {
			test.Fatal("public preparation terminated before its actual nested parent/cursor pause", complete, err)
		}
	}
	test.Fatal("actual public preparation never retained a partially decoded nested child")
	return nil, nil
}

func TestLegacyNativeEventsSemanticsPublicChildTypesAndPresence(test *testing.T) {
	for _, fixture := range []struct {
		name      string
		child     string
		rejection string
	}{
		{name: "number-child", child: `7`, rejection: "typed"},
		{name: "string-child", child: `"turn"`, rejection: "typed"},
		{name: "boolean-child", child: `true`, rejection: "typed"},
		{name: "array-child", child: `[]`, rejection: "typed"},
		{name: "null-child-is-a-fresh-zero-event", child: `null`, rejection: "semantic"},
		{name: "empty-child-is-not-prior-child", child: `{}`, rejection: "semantic"},
		{name: "unknown-union", child: `{"kind":"unknown"}`, rejection: "semantic"},
		{name: "sequence-type-overwrite-cannot-repair", child: `{"kind":"turn","seq":"bad","seq":1}`, rejection: "typed"},
		{name: "provider-type", child: `{"kind":"turn","provider_metadata":{"turn_id":7}}`, rejection: "typed"},
		{name: "locked-type", child: `{"kind":"reasoning","locked":{"blob":7}}`, rejection: "typed"},
		{name: "block-type", child: `{"kind":"message","blocks":[{"text":7}]}`, rejection: "typed"},
		{name: "replacement-type", child: `{"kind":"compaction","replacement":false}`, rejection: "typed"},
		{name: "exact-null-agent-presence", child: `{"kind":"turn","agent_message":null}`, rejection: "semantic"},
		{name: "folded-null-agent-without-exact-presence", child: `{"kind":"turn","AGENT_MESSAGE":null}`},
		{name: "exact-null-provider-presence", child: `{"kind":"turn","provider_metadata":null}`, rejection: "semantic"},
		{name: "folded-null-provider-without-exact-presence", child: `{"kind":"turn","PROVIDER_METADATA":null}`},
		{name: "exact-null-replacement-presence", child: `{"kind":"turn","replacement":null}`, rejection: "semantic"},
		{name: "folded-null-replacement-without-exact-presence", child: `{"kind":"turn","REPLACEMENT":null}`},
		{name: "exact-null-locked-presence", child: `{"kind":"reasoning","locked":null}`, rejection: "semantic"},
		{name: "folded-null-locked-without-exact-presence", child: `{"kind":"reasoning","LOCKED":null}`},
		{name: "exact-null-author-without-agent", child: `{"kind":"message","agent_author":null}`, rejection: "semantic"},
		{name: "folded-null-author-without-exact-presence", child: `{"kind":"message","AGENT_AUTHOR":null}`},
		{name: "folded-null-does-not-erase-exact-presence", child: `{"kind":"turn","agent_message":null,"AGENT_MESSAGE":null}`, rejection: "semantic"},
		{name: "message-null-agent-preserves-false", child: `{"kind":"message","agent_message":null,"blocks":null}`},
		{name: "agent-null-author-preserves-empty", child: `{"kind":"message","agent_message":true,"agent_author":null,"agent_recipient":null}`},
		{name: "tool-input-null", child: `{"kind":"tool_call","input":null}`},
		{name: "tool-output-null", child: `{"kind":"tool_result","output":null}`},
		{name: "empty-boundary", child: `{"kind":"compaction","replacement":[]}`},
		{name: "null-boundary-without-state", child: `{"kind":"compaction","replacement":null}`, rejection: "semantic"},
		{name: "boundary-and-locked-union", child: `{"kind":"compaction","replacement":[],"locked":{}}`, rejection: "semantic"},
		{name: "null-boundary-with-state", child: `{"kind":"compaction","replacement":null,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", `{"kind":"compaction","replacement":[{"kind":"turn","seq":9,"role":"user"},`+fixture.child+`]}`)
			canonical, original := legacyNativeEventsSemanticsOracle(test, raw, fixture.rejection)
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
		})
	}
}

func TestLegacyNativeEventsSemanticsPublicOverwriteAndVersion(test *testing.T) {
	const survivor = `{"kind":"turn","id":"survivor","role":"user"}`
	for _, fixture := range []struct {
		name      string
		version   string
		event     string
		suffix    string
		rejection string
		count     int
	}{
		{name: "typed-child-cannot-be-reset-by-events-null", version: "2", event: `{"kind":"compaction","replacement":[7]}`, suffix: `,"events":null`, rejection: "typed"},
		{name: "typed-child-cannot-be-reset-by-folded-events", version: "2", event: `{"kind":"compaction","replacement":[{"kind":"turn","seq":false}]}`, suffix: `,"EVENTS":[` + survivor + `]`, rejection: "typed"},
		{name: "semantic-child-reset-by-events-null", version: "2", event: `{"kind":"compaction","replacement":[null]}`, suffix: `,"events":null`},
		{name: "semantic-child-reset-by-events-empty", version: "2", event: `{"kind":"compaction","replacement":[{"kind":"unknown"}]}`, suffix: `,"events":[]`},
		{name: "semantic-presence-reset-by-folded-events", version: "2", event: `{"kind":"compaction","replacement":[{"kind":"turn","provider_metadata":null}]}`, suffix: `,"EVENTS":[` + survivor + `]`, count: 1},
		{name: "discarded-v2-requirement-does-not-poison-v1", version: "1", event: `{"kind":"compaction","replacement":[{"kind":"message","agent_message":false}]}`, suffix: `,"events":[` + survivor + `]`, count: 1},
		{name: "discarded-union-and-v2-requirement", version: "1", event: `{"kind":"compaction","replacement":[{"kind":"turn","replacement":null}]}`, suffix: `,"events":null,"events":[` + survivor + `]`, count: 1},
		{name: "replacement-semantic-error-overwritten", version: "2", event: `{"kind":"compaction","replacement":[null],"replacement":[` + survivor + `]}`, count: 1},
		{name: "replacement-typed-error-not-overwritten", version: "2", event: `{"kind":"compaction","replacement":[false],"replacement":[` + survivor + `]}`, rejection: "typed"},
		{name: "replacement-shorter-child-does-not-inherit-agent-presence", version: "2", event: `{"kind":"compaction","replacement":[{"kind":"message","agent_message":true,"agent_author":"old"}],"replacement":[` + survivor + `]}`, count: 1},
		{name: "replacement-null-child-does-not-inherit-kind", version: "2", event: `{"kind":"compaction","replacement":[` + survivor + `],"replacement":[null]}`, rejection: "semantic"},
		{name: "v1-compaction-rejected", version: "1", event: `{"kind":"compaction","replacement":[]}`, rejection: "version"},
		{name: "unsupported-version", version: "99", event: `{"kind":"compaction","replacement":[]}`, rejection: "version"},
		{name: "late-envelope-upgrade", version: "1", event: `{"kind":"compaction","replacement":[{"kind":"message","agent_message":false}]}`, suffix: `,"envelope":{"cir_version":"2"}`, count: 1},
		{name: "late-envelope-downgrade", version: "2", event: `{"kind":"compaction","replacement":[{"kind":"message","agent_message":false}]}`, suffix: `,"envelope":{"cir_version":"1"}`, rejection: "version"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"` + fixture.version + `"},"events":[` + fixture.event + `]` + fixture.suffix + `}`)
			canonical, original := legacyNativeEventsSemanticsOracle(test, raw, fixture.rejection)
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
			if fixture.rejection == "" && (storage.resets[0] < 2 || work.count != fixture.count) {
				test.Fatal("events overwrite retained an earlier semantic error, v2 requirement, or event records", storage.resets, work.count)
			}
		})
	}
}

func TestLegacyNativeEventsSemanticsPublicPartialChildAndLateFailure(test *testing.T) {
	for _, fixture := range []struct {
		name      string
		tail      string
		suffix    string
		rejection string
	}{
		{name: "accepted-resume", tail: `{"kind":"turn","id":"late-valid","seq":-99}`},
		{name: "late-typed-child", tail: `7`, rejection: "typed"},
		{name: "late-typed-field", tail: `{"kind":"message","blocks":[{"text":7}]}`, rejection: "typed"},
		{name: "late-null-child", tail: `null`, rejection: "semantic"},
		{name: "late-null-presence", tail: `{"kind":"turn","agent_message":null}`, rejection: "semantic"},
		{name: "late-semantic-reset", tail: `{"kind":"unknown"}`, suffix: `,"events":[{"kind":"turn","id":"survivor"}]`},
		{name: "late-typed-reset-still-rejects", tail: `false`, suffix: `,"events":[]`, rejection: "typed"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + legacyNativeEventsSemanticsNested(fixture.tail) + `]` + fixture.suffix + `}`)
			canonical, original := legacyNativeEventsSemanticsOracle(test, raw, fixture.rejection)
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			native, root := legacyNativeEventsSemanticsPause(test, work, storage, input)
			cursor := root.cursor
			position, count, phase := cursor.position, cursor.count, cursor.phase
			decoderPosition := work.decoder.position
			reads, scratchReads, resets := len(input.reads), len(storage.reads), storage.resets
			retained, retainedErr := work.RetainedBytes()
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			complete, cancelErr := work.Step(canceled)
			after, afterErr := work.RetainedBytes()
			if complete || !errors.Is(cancelErr, context.Canceled) || errors.Is(cancelErr, ErrIntegrity) ||
				retainedErr != nil || afterErr != nil || retained != after || work.terminal != nil ||
				work.decoder.native != native || root.cursor != cursor || cursor.parent != root ||
				cursor.position != position || cursor.count != count || cursor.phase != phase || work.decoder.position != decoderPosition ||
				len(input.reads) != reads || len(storage.reads) != scratchReads || storage.resets != resets {
				test.Fatal("pre-canceled public step changed the live native parent/cursor or consumed I/O", cancelErr)
			}
			legacyNormalizationPreparationPending(test, work)
			legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
		})
	}
}

func TestLegacyNativeEventsSemanticsPublicLateFailureSourcePriority(test *testing.T) {
	for _, rejection := range []struct {
		name  string
		tail  string
		class string
	}{
		{name: "typed", tail: `{"kind":"turn","seq":false}`, class: "typed"},
		{name: "semantic", tail: `{"kind":"turn","provider_metadata":null}`, class: "semantic"},
	} {
		raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + legacyNativeEventsSemanticsNested(rejection.tail) +
			`],"ignored":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}`)
		legacyNativeEventsSemanticsOracle(test, raw, rejection.class)
		for _, fault := range []struct {
			name      string
			damage    string
			atEOF     bool
			eofLength int
			cancel    bool
		}{
			{name: "authenticated-document-error"},
			{name: "late-part-hash", damage: "hash"},
			{name: "late-part-short", damage: "short"},
			{name: "late-part-io", damage: "io"},
			{name: "zero-eof-io", damage: "io", atEOF: true},
			{name: "one-eof-joined-private-error", damage: "joined", atEOF: true, eofLength: 1},
			{name: "zero-eof-extra-byte", damage: "extra", atEOF: true},
			{name: "one-eof-missing-error", damage: "missing", atEOF: true, eofLength: 1},
			{name: "cancel-late-part-success", cancel: true},
			{name: "cancel-late-part-io", damage: "io", cancel: true},
			{name: "cancel-late-part-hash", damage: "hash", cancel: true},
			{name: "cancel-zero-eof-io", damage: "io", atEOF: true, cancel: true},
			{name: "cancel-one-eof-extra-byte", damage: "extra", atEOF: true, eofLength: 1, cancel: true},
		} {
			test.Run(rejection.name+"/"+fault.name, func(test *testing.T) {
				input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
				storage := &legacyNormalizationPreparationStorage{test: test, borrowed: make([]byte, 512<<10)}
				storage.appendHook = func(slot int, body []byte) error {
					if len(body) > LegacyCanonicalEncodingBytes {
						test.Fatal("source-priority fixture exceeded the original scratch append quantum", slot, len(body))
					}
					return nil
				}
				source := legacyNormalizationPreparationInput(test, input, true)
				read := source.Read
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				armed, triggered := false, false
				lastPart := (len(raw) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
				failedAt := -1
				var probes [2]int
				source.Read = func(readContext context.Context, offset, length int) ([]byte, error) {
					body, err := read(readContext, offset, length)
					if !armed {
						return body, err
					}
					if offset == len(raw) && (length == 0 || length == 1) {
						probes[length]++
					}
					matches := fault.atEOF && offset == len(raw) && length == fault.eofLength ||
						!fault.atEOF && offset == lastPart && length > 0
					if triggered || !matches || fault.damage == "" && !fault.cancel {
						return body, err
					}
					triggered, failedAt = true, len(input.reads)
					if fault.cancel {
						cancel()
					}
					switch fault.damage {
					case "hash":
						changed := bytes.Clone(body)
						changed[0] ^= 1
						return changed, nil
					case "short":
						return body[:len(body)-1], nil
					case "io":
						return nil, io.ErrClosedPipe
					case "joined":
						return nil, errors.Join(io.EOF, ErrPreparedDocRangeObject, io.ErrClosedPipe)
					case "extra":
						return []byte("x"), nil
					case "missing":
						return nil, nil
					default:
						return body, err
					}
				}
				work, err := NewLegacyNormalizationPreparation(context.Background(), source, storage)
				if err != nil || len(input.reads) != 0 {
					test.Fatal("source-priority constructor failed or eagerly read the source", err)
				}
				legacyNativeEventsSemanticsPause(test, work, storage, input)
				for quantum := 0; quantum < 8192; quantum++ {
					if work.phase == legacyNormalizationDecode && work.decoder != nil &&
						(rejection.class == "typed" && work.decoder.phase == legacyNormalizationReject ||
							rejection.class == "semantic" && work.decoder.invalid) {
						armed = true
						break
					}
					if complete, err := legacyTypedNumberPreparationStep(test, work, storage, input); err != nil || complete {
						test.Fatal("late child failure became terminal before its source/EOF priority checks", complete, err)
					}
				}
				if !armed || work.count != 0 || work.files[0].size != 0 || work.files[1].size != 0 || work.files[3].size != 0 {
					test.Fatal("late child rejection did not remain private before source fault injection")
				}
				if rejection.class == "semantic" && work.decoder.phase == legacyNormalizationReject {
					test.Fatal("deferred union failure was confused with a terminal typed decode error")
				}
				_, err = legacyNormalizationPreparationDrain(test, ctx, work)
				private := fault.damage != "" && !fault.cancel
				want := error(ErrIntegrity)
				if fault.cancel {
					want = context.Canceled
				} else if private {
					want = ErrPreparedDocRangeObject
				}
				if !errors.Is(err, want) || errors.Is(err, ErrPreparedDocRangeObject) != private ||
					errors.Is(err, context.Canceled) != fault.cancel || errors.Is(err, ErrIntegrity) != !fault.cancel ||
					triggered != (fault.damage != "" || fault.cancel) || triggered && len(input.reads) != failedAt ||
					len(storage.slots[3]) != 0 || work.files[3].size != 0 || !bytes.Equal(input.body, raw) {
					test.Fatal("late native child rejection lost source/EOF/cancel precedence or exposed output", err)
				}
				if !fault.cancel && (fault.damage == "io" || fault.damage == "joined") && !errors.Is(err, io.ErrClosedPipe) {
					test.Fatal("source-priority failure discarded the underlying private I/O error", err)
				}
				if fault.cancel && (errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF)) {
					test.Fatal("cancellation was joined with a lower-priority private/EOF failure", err)
				}
				if fault.atEOF {
					wantProbes := [2]int{1, fault.eofLength}
					if probes != wantProbes {
						test.Fatal("source EOF failure skipped a required probe or continued after failure", probes, wantProbes)
					}
				} else if triggered && probes != [2]int{} {
					test.Fatal("source part failure incorrectly continued to EOF probes", probes)
				}
				if !triggered {
					legacyTypedNumberPreparationInputEOF(test, input)
				}
				reads, scratchReads, resets := len(input.reads), len(storage.reads), storage.resets
				legacyNormalizationPreparationFailed(test, work, err, want)
				canceled, stop := context.WithCancel(context.Background())
				stop()
				if complete, cancelErr := work.Step(canceled); complete || !errors.Is(cancelErr, context.Canceled) ||
					errors.Is(cancelErr, ErrPreparedDocRangeObject) || len(input.reads) != reads ||
					len(storage.reads) != scratchReads || storage.resets != resets {
					test.Fatal("pre-canceled terminal retry resumed I/O or lost cancellation priority", cancelErr)
				}
				legacyNormalizationPreparationPending(test, work)
			})
		}
	}
}
