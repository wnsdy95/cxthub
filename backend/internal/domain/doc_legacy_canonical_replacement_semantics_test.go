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

func LegacyReplacementSemanticsOwner(test *testing.T, work *LegacyNormalizationPreparation, id string) *LegacyCanonicalEncoding {
	test.Helper()
	for _, event := range work.pending {
		encoding := event.Encoding
		if encoding == nil {
			continue
		}
		fields, ok := encoding.root.(map[string]any)
		if !ok || id != "" && fields["id"] != id {
			continue
		}
		replacement, ok := fields["replacement"].(*legacyCanonicalReplacement)
		if !ok || replacement == nil || replacement.length() < 0 {
			continue
		}
		if encoding.nativeOwner == nil || encoding.preparation == nil || work.written != 0 {
			test.Fatal("public replacement bypassed its actual native owner or pending preparation")
		}
		if replacement.events != nil || replacement.native == nil || replacement.native != encoding.nativeOwner.replacement ||
			work.decoder == nil || work.decoder.nativeCache == nil || !work.decoder.nativeCache.object.pagedEvents {
			test.Fatal("public replacement bypassed actual paged native ownership")
		}
		return encoding
	}
	test.Fatal("public preparation did not retain the requested native replacement", id)
	return nil
}

func LegacyReplacementSemanticsCheck(test *testing.T, raw []byte, expectedEvents string, native bool) {
	test.Helper()
	canonical, original, oracleErr := legacyNormalizationPreparationOracle(raw)
	if expectedEvents == "" {
		if !errors.Is(oracleErr, ErrIntegrity) || canonical != nil || original.Valid() {
			test.Fatal("independent typed oracle accepted the rejection fixture", oracleErr)
		}
	} else if oracleErr != nil || !original.Valid() {
		test.Fatal("independent typed oracle rejected the accepted fixture", oracleErr)
	}
	work, storage, input := legacyTypedNumberPreparationStart(test, raw)
	if native {
		legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
		LegacyReplacementSemanticsOwner(test, work, "")
	}
	_, err := legacyNormalizationPreparationDrain(test, context.Background(), work)
	legacyTypedNumberPreparationInputEOF(test, input)
	if expectedEvents == "" {
		if errors.Is(err, ErrPreparedDocRangeObject) || len(storage.slots[3]) != 0 || work.files[3].size != 0 {
			test.Fatal("invalid replacement published output or became a private-source failure", err)
		}
		legacyNormalizationPreparationFailed(test, work, err, ErrIntegrity)
		return
	}
	if err != nil {
		test.Fatal("public replacement normalization rejected an accepted union", err)
	}
	legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
	var document struct {
		Events json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(storage.slots[3], &document); err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(document.Events, []byte(expectedEvents)) || sha256.Sum256(storage.slots[3]) != sha256.Sum256(canonical) {
		test.Fatalf("replacement union bytes or SHA changed: got=%s want=%s", document.Events, expectedEvents)
	}
}

func TestLegacyReplacementSemanticsNestedUnionCanonical(test *testing.T) {
	const children = `{"seq":0,"kind":"turn","role":"developer","provider_metadata":{}},` +
		`{"seq":1,"kind":"message","role":"assistant","blocks":[{"text":"한🙂<&>\n","type":"text"}],` +
		`"compact_summary":true,"agent_message":true,"agent_author":"","agent_recipient":"/root",` +
		`"locked":{"provider":"codex","scheme":"encrypted_content","blob":"ENC<&>"},` +
		`"provider_metadata":{"turn_id":"old","create_time":"-0.0012500E+003","ignored":1},"provider_metadata":{"turn_id":"new"}},` +
		`{"seq":2,"kind":"tool_call","call_id":"call","tool_name":"read","provider_tool_name":"Read","input":null,"status":"done"},` +
		`{"seq":3,"kind":"tool_result","call_id":"call","output":[{"z":2,"a":null},false],"is_error":true},` +
		`{"seq":4,"kind":"reasoning","locked":{"provider":"claude","scheme":"signature","blob":"SIG"},"redacted_summary":"summary","cross_replayable":true},` +
		`{"seq":5,"kind":"compaction","replacement":[{"seq":0,"kind":"message","blocks":null}],"replacement_complete":true},` +
		`{"seq":6,"kind":"compaction","replacement":null,"replacement_complete":true,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"STATE"}}`
	const expectedChildren = `{"kind":"turn","provider_metadata":{},"role":"developer","seq":0},` +
		`{"agent_author":"","agent_message":true,"agent_recipient":"/root","blocks":[{"text":"한🙂\u003c\u0026\u003e\n","type":"text"}],` +
		`"compact_summary":true,"kind":"message","locked":{"blob":"ENC\u003c\u0026\u003e","provider":"codex","scheme":"encrypted_content"},` +
		`"provider_metadata":{"create_time":-0.0012500E+003,"turn_id":"new"},"role":"assistant","seq":1},` +
		`{"call_id":"call","input":{},"kind":"tool_call","provider_tool_name":"Read","seq":2,"status":"done","tool_name":"read"},` +
		`{"call_id":"call","is_error":true,"kind":"tool_result","output":[{"a":null,"z":2},false],"seq":3},` +
		`{"cross_replayable":false,"kind":"reasoning","locked":{"blob":"SIG","provider":"claude","scheme":"signature"},"redacted_summary":"summary","seq":4},` +
		`{"kind":"compaction","replacement":[{"blocks":[],"kind":"message","role":"","seq":0}],"replacement_complete":true,"seq":5},` +
		`{"kind":"compaction","locked":{"blob":"STATE","provider":"codex","scheme":"encrypted_content"},"seq":6}`
	raw := []byte(`{"events":[{"seq":17,"kind":"compaction","replacement":[` + children + `]}],"envelope":{"cir_version":"2"}}`)
	expected := `[{"kind":"compaction","replacement":[` + expectedChildren + `],"replacement_complete":false,"seq":17}]`
	LegacyReplacementSemanticsCheck(test, raw, expected, true)
}

func TestLegacyReplacementSemanticsNullEmptyLockedProvider(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		child    string
		expected string
	}{
		{
			name:     "empty-incomplete-boundary",
			child:    `{"kind":"compaction","replacement":[]}`,
			expected: `{"kind":"compaction","replacement":[],"replacement_complete":false,"seq":0}`,
		},
		{
			name:     "empty-complete-boundary",
			child:    `{"kind":"compaction","replacement":[],"replacement_complete":true}`,
			expected: `{"kind":"compaction","replacement":[],"replacement_complete":true,"seq":0}`,
		},
		{
			name:     "absent-boundary-locked-state",
			child:    `{"kind":"compaction","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`,
			expected: `{"kind":"compaction","locked":{"blob":"opaque","provider":"codex","scheme":"encrypted_content"},"seq":0}`,
		},
		{
			name:     "null-boundary-is-not-empty-boundary",
			child:    `{"kind":"compaction","replacement":null,"replacement_complete":false,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`,
			expected: `{"kind":"compaction","locked":{"blob":"opaque","provider":"codex","scheme":"encrypted_content"},"seq":0}`,
		},
		{
			name:     "present-false-agent-message-with-null-blocks",
			child:    `{"kind":"message","agent_message":false,"blocks":null}`,
			expected: `{"agent_message":false,"blocks":[],"kind":"message","role":"","seq":0}`,
		},
		{
			name:     "empty-blocks-and-empty-provider",
			child:    `{"kind":"message","blocks":[],"provider_metadata":{}}`,
			expected: `{"blocks":[],"kind":"message","provider_metadata":{},"role":"","seq":0}`,
		},
		{
			name:     "provider-number-keeps-precision",
			child:    `{"kind":"turn","provider_metadata":{"create_time":1787683260.123456789,"turn_id":"provider-id","extra":"not-emitted"}}`,
			expected: `{"kind":"turn","provider_metadata":{"create_time":1787683260.123456789,"turn_id":"provider-id"},"role":"","seq":0}`,
		},
		{
			name:     "null-tool-result-is-empty-string",
			child:    `{"kind":"tool_result","output":null,"is_error":false}`,
			expected: `{"call_id":"","kind":"tool_result","output":"","seq":0}`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[` + fixture.child + `]}]}`)
			expected := `[{"kind":"compaction","replacement":[` + fixture.expected + `],"replacement_complete":false,"seq":0}]`
			LegacyReplacementSemanticsCheck(test, raw, expected, true)
		})
	}
}

func TestLegacyReplacementSemanticsInvalidNestedChild(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		child    string
		deferred bool
	}{
		{name: "unknown-union", child: `{"kind":"unknown"}`, deferred: true},
		{name: "turn-with-null-replacement", child: `{"kind":"turn","replacement":null}`, deferred: true},
		{name: "turn-with-present-false-agent", child: `{"kind":"turn","agent_message":false}`, deferred: true},
		{name: "message-with-empty-author", child: `{"kind":"message","agent_author":""}`, deferred: true},
		{name: "message-with-locked-without-agent", child: `{"kind":"message","locked":{}}`, deferred: true},
		{name: "null-provider", child: `{"kind":"turn","provider_metadata":null}`, deferred: true},
		{name: "null-locked", child: `{"kind":"reasoning","locked":null}`, deferred: true},
		{name: "null-boundary-without-state", child: `{"kind":"compaction","replacement":null}`, deferred: true},
		{name: "empty-boundary-and-locked", child: `{"kind":"compaction","replacement":[],"locked":{}}`, deferred: true},
		{name: "wrong-provider-type", child: `{"kind":"turn","provider_metadata":{"turn_id":7}}`},
		{name: "wrong-block-type", child: `{"kind":"message","blocks":[{"text":7}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[` +
				`{"kind":"compaction","replacement":[{"kind":"turn","role":"user"},` + fixture.child + `]}]}]}`)
			LegacyReplacementSemanticsCheck(test, raw, "", fixture.deferred)
		})
	}
}

func TestLegacyReplacementSemanticsVersionAndLateEnvelope(test *testing.T) {
	const event = `{"kind":"compaction","replacement":[{"kind":"message","agent_message":false,"blocks":[]}]}`
	const expected = `[{"kind":"compaction","replacement":[{"agent_message":false,"blocks":[],"kind":"message","role":"","seq":0}],"replacement_complete":false,"seq":0}]`
	for _, fixture := range []struct {
		name   string
		prefix string
		suffix string
		accept bool
	}{
		{name: "v2", prefix: `{"envelope":{"cir_version":"2"},`, accept: true},
		{name: "v1", prefix: `{"envelope":{"cir_version":"1"},`},
		{name: "implicit-v1", prefix: `{`},
		{name: "unsupported-version", prefix: `{"envelope":{"cir_version":"99"},`},
		{name: "late-upgrade", prefix: `{"envelope":{"cir_version":"1"},`, suffix: `,"envelope":{"cir_version":"2"}`, accept: true},
		{name: "late-downgrade", prefix: `{"envelope":{"cir_version":"2"},`, suffix: `,"envelope":{"cir_version":"1"}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			wanted := ""
			if fixture.accept {
				wanted = expected
			}
			LegacyReplacementSemanticsCheck(test, []byte(fixture.prefix+`"events":[`+event+`]`+fixture.suffix+`}`), wanted, true)
		})
	}
}

func TestLegacyReplacementSemanticsLateTypedErrorSurvivesEventsReset(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		child string
	}{
		{name: "provider-type", child: `{"kind":"turn","provider_metadata":{"turn_id":7}}`},
		{name: "provider-number", child: `{"kind":"turn","provider_metadata":{"create_time":"not-a-number"}}`},
		{name: "locked-type", child: `{"kind":"reasoning","locked":{"blob":7}}`},
		{name: "block-type", child: `{"kind":"message","blocks":[{"text":7}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[` +
				`{"kind":"turn","agent_message":false},{"kind":"compaction","replacement":[` + fixture.child + `]}]}],` +
				`"events":[{"kind":"turn","id":"cannot-repair-wire-error"}]}`)
			LegacyReplacementSemanticsCheck(test, raw, "", false)
		})
	}
}

func TestLegacyReplacementSemanticsDiscardedInvalidArrayRecovers(test *testing.T) {
	const earlier = `{"kind":"compaction","id":"discarded-valid","replacement":[{"kind":"turn","id":"discarded-body"}]},` +
		`{"kind":"compaction","id":"discarded-invalid","replacement":[{"kind":"compaction","replacement":[{"kind":"turn","agent_message":false}]}]}`
	for _, fixture := range []struct {
		name     string
		selected string
		resets   int
		count    int
	}{
		{name: "selected-null", selected: `,"events":null`, resets: 3},
		{name: "selected-empty", selected: `,"events":[]`, resets: 3},
		{
			name:     "multiple-resets-clear-invalid-and-v2-requirement",
			selected: `,"events":[{"kind":"compaction","id":"discarded-again","replacement":[]}],"events":null,"events":[{"kind":"turn","id":"survivor","role":"user"}]`,
			resets:   4,
			count:    1,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"1"},"events":[` + earlier + `]` + fixture.selected + `}`)
			canonical, original, err := legacyNormalizationPreparationOracle(raw)
			if err != nil || !original.Valid() {
				test.Fatal("independent oracle did not recover a discarded semantic error", err)
			}
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
			invalid := LegacyReplacementSemanticsOwner(test, work, "discarded-invalid")
			legacyNormalizationPreparationAdvance(test, work, func() bool {
				return work.phase == legacyNormalizationDecode && work.decoder != nil && work.decoder.invalid
			})
			if invalid.err == nil || invalid.root != nil || work.count != 1 || work.files[0].size == 0 || work.files[3].size != 0 {
				test.Fatal("fixture did not stage earlier private bytes and actually reject its deferred child")
			}
			if _, err := legacyNormalizationPreparationDrain(test, context.Background(), work); err != nil {
				test.Fatal("discarded replacement error survived a later events reset", err)
			}
			if storage.resets[0] != fixture.resets || storage.resets[1] != fixture.resets+1 || work.count != fixture.count ||
				bytes.Contains(storage.slots[0], []byte("discarded-")) || bytes.Contains(storage.slots[3], []byte("discarded-")) {
				test.Fatal("events reset kept rejected history, index records, or the old v2 requirement", storage.resets, work.count)
			}
			legacyTypedNumberPreparationInputEOF(test, input)
			legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
			if sha256.Sum256(storage.slots[3]) != sha256.Sum256(canonical) {
				test.Fatal("events reset changed the original canonical SHA")
			}
		})
	}
}

func TestLegacyReplacementSemanticsSourceAndCancellationPriority(test *testing.T) {
	const events = `{"kind":"compaction","id":"staged","replacement":[]},` +
		`{"kind":"compaction","id":"invalid","replacement":[{"kind":"turn","replacement":null}]}`
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + events + `],"ignored":"` + strings.Repeat("x", 2*(512<<10)+17) + `"}`)
	if canonical, original, err := legacyNormalizationPreparationOracle(raw); !errors.Is(err, ErrIntegrity) || canonical != nil || original.Valid() {
		test.Fatal("priority fixture is not rejected by the original typed oracle", err)
	}
	for _, mode := range []string{
		"document-error", "part-hash", "part-read", "eof-zero-error", "eof-one-extra", "eof-one-joined",
		"cancel-part-error", "cancel-part-success", "cancel-eof-zero", "cancel-eof-one",
	} {
		test.Run(mode, func(test *testing.T) {
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			storage := &legacyNormalizationPreparationStorage{test: test, borrowed: make([]byte, 512<<10)}
			source := legacyNormalizationPreparationInput(test, input, true)
			read := source.Read
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			armed, injected := false, false
			source.Read = func(readContext context.Context, offset, length int) ([]byte, error) {
				body, err := read(readContext, offset, length)
				if !armed {
					return body, err
				}
				part := offset == 2*(512<<10) && length > 0
				zeroEOF, oneEOF := offset == len(raw) && length == 0, offset == len(raw) && length == 1
				switch {
				case mode == "part-read" && part, mode == "eof-zero-error" && zeroEOF:
					injected = true
					return nil, io.ErrClosedPipe
				case mode == "eof-one-extra" && oneEOF:
					injected = true
					return []byte("x"), nil
				case mode == "eof-one-joined" && oneEOF:
					injected = true
					return nil, errors.Join(io.EOF, ErrPreparedDocRangeObject, io.ErrClosedPipe)
				case mode == "cancel-part-error" && part, mode == "cancel-eof-zero" && zeroEOF:
					injected = true
					cancel()
					return nil, io.ErrClosedPipe
				case mode == "cancel-part-success" && part:
					injected = true
					cancel()
					return body, err
				case mode == "cancel-eof-one" && oneEOF:
					injected = true
					cancel()
					return []byte("x"), nil
				}
				return body, err
			}
			work, err := NewLegacyNormalizationPreparation(context.Background(), source, storage)
			if err != nil {
				test.Fatal(err)
			}
			legacyNormalizationPreparationAdvance(test, work, func() bool { return work.phase == legacyNormalizationStage })
			invalid := LegacyReplacementSemanticsOwner(test, work, "invalid")
			legacyNormalizationPreparationAdvance(test, work, func() bool {
				return work.phase == legacyNormalizationDecode && work.decoder != nil && work.decoder.invalid
			})
			if invalid.err == nil || work.count != 1 || work.files[0].size == 0 {
				test.Fatal("source fault did not follow an actual deferred rejection and earlier private staging")
			}
			armed = true
			if mode == "part-hash" {
				input.body[len(input.body)-3] ^= 1
				injected = true
			}
			_, err = legacyNormalizationPreparationDrain(test, ctx, work)
			want := ErrPreparedDocRangeObject
			if mode == "document-error" {
				want = ErrIntegrity
			} else if strings.HasPrefix(mode, "cancel-") {
				want = context.Canceled
			}
			if mode != "document-error" && !injected || len(storage.slots[3]) != 0 || work.files[3].size != 0 {
				test.Fatal("priority fault was not exercised or rejected output became visible", mode, err)
			}
			if want == context.Canceled && (errors.Is(err, ErrPreparedDocRangeObject) || errors.Is(err, io.ErrClosedPipe)) ||
				want == ErrIntegrity && errors.Is(err, ErrPreparedDocRangeObject) {
				test.Fatal("cancellation, private-source, and semantic rejection priority changed", mode, err)
			}
			if (mode == "part-read" || mode == "eof-zero-error" || mode == "eof-one-joined") && !errors.Is(err, io.ErrClosedPipe) {
				test.Fatal("private-source rejection discarded its underlying I/O error", mode, err)
			}
			legacyNormalizationPreparationFailed(test, work, err, want)
			if mode == "document-error" || strings.HasPrefix(mode, "eof-one-") {
				legacyTypedNumberPreparationInputEOF(test, input)
			}
			reads, scratchReads := len(input.reads), len(storage.reads)
			canceled, stop := context.WithCancel(context.Background())
			stop()
			if complete, cancelErr := work.Step(canceled); complete || !errors.Is(cancelErr, context.Canceled) ||
				len(input.reads) != reads || len(storage.reads) != scratchReads {
				test.Fatal("pre-canceled retry lost priority over a sticky failure or performed I/O", cancelErr)
			}
			legacyNormalizationPreparationPending(test, work)
		})
	}
}
