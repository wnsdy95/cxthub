package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func legacyNativePagedSemanticsEvent(test *testing.T, actual CIREvent, values *legacyNativeEventValues, blocks *legacyNativeBlockValues, expected CIREvent) {
	test.Helper()
	if len(actual.Replacement) != 0 || cap(actual.Replacement) != 0 {
		test.Fatal("public paged event retained a typed replacement array")
	}
	children := expected.Replacement
	if actual.Blocks != nil {
		test.Fatal("public paged event retained a typed block array")
	}
	legacyNativeBlockPagedSemanticsValues(test, blocks, expected.Blocks)
	actual.Replacement, expected.Replacement, expected.Blocks = nil, nil, nil
	if !reflect.DeepEqual(actual, expected) || (values == nil) != (children == nil) {
		test.Fatalf("paged event changed typed fields, exact presence, or null/empty semantics: got=%#v want=%#v", actual, expected)
	}
	if values == nil {
		return
	}
	if values.length() != len(children) || values.records.length() < len(children) || values.at(-1) != nil || values.at(len(children)) != nil {
		test.Fatal("paged replacement exposed its hidden tail or changed its logical length", values.length(), values.records.length(), len(children))
	}
	for ordinal, child := range children {
		value := values.at(ordinal)
		if value == nil {
			test.Fatal("paged replacement omitted a visible child", ordinal)
		}
		legacyNativePagedSemanticsEvent(test, value.event, value.replacement, value.blocks, child)
	}
}

func legacyNativePagedSemanticsPause(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, input *legacySourceProofBuffer, expected CIREvent) (*LegacyCanonicalEncoding, *legacyNativeEventValues) {
	test.Helper()
	for quantum := 0; quantum < 8192; quantum++ {
		if work.phase == legacyNormalizationStage {
			for _, pending := range work.pending {
				encoding := pending.Encoding
				if encoding == nil || encoding.nativeOwner == nil || encoding.nativeOwner.event.ID != expected.ID {
					continue
				}
				owner := encoding.nativeOwner
				if expected.Kind != EventCompaction || expected.fieldPresence&eventFieldReplacement == 0 || encoding.preparation == nil ||
					work.decoder == nil || work.decoder.nativeCache == nil || work.decoder.nativeCache.object == nil || !work.decoder.nativeCache.object.pagedEvents {
					test.Fatal("fixture bypassed the public default paged compaction path or its pending preparation")
				}
				legacyNativePagedSemanticsEvent(test, owner.event, owner.replacement, owner.blocks, expected)
				fields, ok := encoding.root.(map[string]any)
				if !ok {
					test.Fatal("public paged owner did not project its actual root event")
				}
				field, present := fields["replacement"]
				if owner.replacement == nil {
					if present {
						test.Fatal("null replacement became an emitted empty boundary")
					}
				} else {
					wrapper, ok := field.(*legacyCanonicalReplacement)
					if !present || !ok || wrapper == nil || wrapper.native != owner.replacement || wrapper.events != nil {
						test.Fatal("public projection substituted a typed array or dummy fragment for the actual page values")
					}
				}
				return encoding, owner.replacement
			}
		}
		if complete, err := legacyTypedNumberPreparationStep(test, work, storage, input); err != nil || complete {
			test.Fatal("public preparation terminated before its selected paged root", expected.ID, complete, err)
		}
	}
	test.Fatal("public preparation did not reach its selected paged root", expected.ID)
	return nil, nil
}

func legacyNativePagedSemanticsDocument(test *testing.T, raw []byte) CIRDocument {
	test.Helper()
	var document CIRDocument
	if err := json.Unmarshal(raw, &document); err != nil || document.Envelope.CIRVersion != "2" || len(document.Events) == 0 {
		test.Fatal("paged semantics fixture lost its typed document, explicit v2 envelope, or root replacement", err)
	}
	return document
}

func TestLegacyNativePagedSemanticsNestedSequenceStableTies(test *testing.T) {
	const count = 2*legacyCanonicalPageValues + 3
	maximum := int(^uint(0) >> 1)
	children := make([]string, count)
	wantIDs := make([]string, 0, count)
	wantIDs = append(wantIDs, "child-001")
	for ordinal := range children {
		sequence := 5
		if ordinal == 0 {
			sequence = maximum
		} else if ordinal == 1 {
			sequence = -maximum - 1
		} else {
			wantIDs = append(wantIDs, fmt.Sprintf("child-%03d", ordinal))
		}
		children[ordinal] = fmt.Sprintf(`{"kind":"turn","id":"child-%03d","seq":%d}`, ordinal, sequence)
	}
	wantIDs = append(wantIDs, "child-000")
	children[legacyCanonicalPageValues] = `{"kind":"compaction","id":"child-064","seq":5,"replacement":[` +
		`{"kind":"turn","id":"nested-tie-first","seq":9},` +
		`{"kind":"compaction","id":"nested-empty","seq":-3,"replacement":[]},` +
		`{"kind":"turn","id":"nested-tie-second","seq":9}]}`
	raw := legacyNativeEventDocument("2", `{"kind":"compaction","id":"root-order","replacement":[`+strings.Join(children, ",")+`]}`)
	document := legacyNativePagedSemanticsDocument(test, raw)
	canonical, original := legacyNativeEventsSemanticsOracle(test, raw, "")
	work, storage, input := legacyTypedNumberPreparationStart(test, raw)
	_, values := legacyNativePagedSemanticsPause(test, work, storage, input, document.Events[0])
	if values.records.pages != 3 || values.at(legacyCanonicalPageValues).replacement == nil {
		test.Fatal("sequence fixture did not cross two actual page boundaries with a nested replacement")
	}
	legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
	output := legacyNativePagedSemanticsDocument(test, storage.slots[3])
	actualIDs := make([]string, len(output.Events[0].Replacement))
	for ordinal, child := range output.Events[0].Replacement {
		actualIDs[ordinal] = child.ID
		if child.ID == "child-064" {
			var nestedIDs []string
			for _, nested := range child.Replacement {
				nestedIDs = append(nestedIDs, nested.ID)
			}
			if !reflect.DeepEqual(nestedIDs, []string{"nested-empty", "nested-tie-first", "nested-tie-second"}) || child.Replacement[0].Replacement == nil {
				test.Fatal("nested stable ties or the nested empty boundary changed", nestedIDs)
			}
		}
	}
	if !reflect.DeepEqual(actualIDs, wantIDs) {
		test.Fatal("canonical output sorted equal sequences by page or id instead of their original ordinal", actualIDs, wantIDs)
	}
}

func TestLegacyNativePagedSemanticsDuplicateArrayHiddenTail(test *testing.T) {
	const count = legacyCanonicalPageValues + 2
	old := make([]string, count)
	fresh := make([]string, count)
	for ordinal := range old {
		old[ordinal] = fmt.Sprintf(`{"kind":"compaction","id":"discarded-%03d","replacement":[{"kind":"message","agent_message":true,"agent_author":"old","blocks":[{"type":"text","text":"hidden"}]}]}`, ordinal)
		fresh[ordinal] = `{"kind":"turn"}`
	}
	fresh[legacyCanonicalPageValues] = `{"kind":"compaction","replacement":[]}`
	for _, fixture := range []struct {
		name      string
		suffix    string
		highwater int
	}{
		{name: "shorter-retains-nested-hidden-tail", suffix: `,"REPLACEMENT":[{"kind":"turn","id":"fresh"}]`, highwater: count},
		{name: "regrowth-resets-every-child-wire", suffix: `,"replacement":[{"kind":"turn","id":"fresh"}],"repla\u0063ement":[` + strings.Join(fresh, ",") + `]`, highwater: count},
		{name: "empty-drops-old-hidden-tail", suffix: `,"replacement":[],"replacement":[{"kind":"turn","id":"fresh"}]`, highwater: 1},
		{name: "null-drops-old-hidden-tail", suffix: `,"replacement":null,"replacement":[{"kind":"turn","id":"fresh"}]`, highwater: 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", `{"kind":"compaction","id":"root-duplicate","replacement":[`+strings.Join(old, ",")+`]`+fixture.suffix+`}`)
			document := legacyNativePagedSemanticsDocument(test, raw)
			canonical, original := legacyNativeEventsSemanticsOracle(test, raw, "")
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			_, values := legacyNativePagedSemanticsPause(test, work, storage, input, document.Events[0])
			if values.records.length() != fixture.highwater {
				test.Fatal("duplicate array changed its typed reset/highwater semantics", values.records.length(), fixture.highwater)
			}
			backing := document.Events[0].Replacement[:cap(document.Events[0].Replacement)]
			for ordinal := values.length(); ordinal < values.records.length(); ordinal++ {
				if ordinal >= len(backing) || values.records.at(ordinal) == nil {
					test.Fatal("fixture lost a known typed hidden-tail record", ordinal)
				}
				hidden := values.records.at(ordinal)
				legacyNativePagedSemanticsEvent(test, hidden.event, hidden.replacement, hidden.blocks, backing[ordinal])
			}
			legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
			if bytes.Contains(storage.slots[3], []byte("discarded-")) || bytes.Contains(storage.slots[3], []byte(`"agent_author":"old"`)) {
				test.Fatal("canonical output enumerated the hidden tail or inherited an overwritten child")
			}
		})
	}
}

func TestLegacyNativePagedSemanticsNullEmptyAndDiscardedInvalidTail(test *testing.T) {
	const locked = `,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"state"}`
	for _, fixture := range []struct {
		name      string
		members   string
		rejection string
	}{
		{name: "empty-boundary", members: `"replacement":[],"replacement_complete":false`},
		{name: "null-locked-state", members: `"replacement":null,"replacement_complete":true` + locked},
		{name: "array-then-null", members: `"replacement":[{"kind":"turn","id":"discarded"}],"replacement":null` + locked},
		{name: "null-then-empty", members: `"replacement":null,"replacement":[],"replacement_complete":true`},
		{name: "empty-then-null", members: `"replacement":[],"replacement":null` + locked},
		{name: "null-child-overwritten", members: `"replacement":[null],"replacement":[{"kind":"turn","id":"fresh"}]`},
		{name: "invalid-union-hidden-tail", members: `"replacement":[{"kind":"turn"},{"kind":"unknown"}],"replacement":[{"kind":"turn","id":"fresh"}]`},
		{name: "invalid-nested-hidden-tail", members: `"replacement":[{"kind":"turn"},{"kind":"compaction","replacement":[{"kind":"turn","agent_message":false}]}],"replacement":[{"kind":"turn","id":"fresh"}]`},
		{name: "nested-duplicate-reset", members: `"replacement":[{"kind":"compaction","replacement":[null],"replacement":null,"replacement":[]}]`},
		{name: "null-child-does-not-inherit-kind", members: `"replacement":[{"kind":"turn","id":"old"}],"replacement":[null]`, rejection: "semantic"},
		{name: "empty-child-does-not-inherit-agent", members: `"replacement":[{"kind":"message","agent_message":true,"agent_author":"old"}],"replacement":[{}]`, rejection: "semantic"},
		{name: "nested-null-child-does-not-inherit-kind", members: `"replacement":[{"kind":"compaction","replacement":[{"kind":"turn"}],"replacement":[null]}]`, rejection: "semantic"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", `{"kind":"compaction","id":"root-null-empty",`+fixture.members+`}`)
			document := legacyNativePagedSemanticsDocument(test, raw)
			canonical, original := legacyNativeEventsSemanticsOracle(test, raw, fixture.rejection)
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			legacyNativePagedSemanticsPause(test, work, storage, input, document.Events[0])
			legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
		})
	}
}

func TestLegacyNativePagedSemanticsEventsResetReusesPublicCache(test *testing.T) {
	const staged = `{"kind":"compaction","id":"discarded-staged","replacement":[{"kind":"turn"}]}`
	const invalid = `{"kind":"compaction","id":"discarded-invalid","replacement":[{"kind":"compaction","replacement":[{"kind":"turn","replacement":null}]}]}`
	const selected = `{"kind":"compaction","id":"selected","replacement":[{"kind":"turn","id":"only"}]}`
	for _, reset := range []string{"null", "[]"} {
		test.Run(reset, func(test *testing.T) {
			resets := 4
			if reset == "null" {
				resets = 3
			}
			raw := []byte(`{"envelope":{"cir_version":"2"},"events":[` + staged + `,` + invalid + `],"events":` + reset + `,"EVENTS":[` + selected + `]}`)
			var initial CIREvent
			if err := json.Unmarshal([]byte(invalid), &initial); err != nil {
				test.Fatal(err)
			}
			document := legacyNativePagedSemanticsDocument(test, raw)
			canonical, original := legacyNativeEventsSemanticsOracle(test, raw, "")
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			rejected, _ := legacyNativePagedSemanticsPause(test, work, storage, input, initial)
			cache := work.decoder.nativeCache
			legacyNormalizationPreparationAdvance(test, work, func() bool {
				return work.phase == legacyNormalizationDecode && work.decoder != nil && work.decoder.invalid
			})
			if rejected.err == nil || rejected.root != nil || work.count != 1 || work.files[0].size == 0 || len(storage.slots[3]) != 0 {
				test.Fatal("reset fixture did not actually stage and then reject its earlier nested replacement")
			}
			_, values := legacyNativePagedSemanticsPause(test, work, storage, input, document.Events[0])
			if work.decoder.nativeCache != cache || work.decoder.invalid || work.count != 0 || values.records.length() != 1 ||
				storage.resets[0] != resets || storage.resets[1] != resets || bytes.Contains(storage.slots[0], []byte("discarded-")) {
				test.Fatal("events reset retained rejected pages, staged bytes, invalid state, or replaced the public native cache", storage.resets)
			}
			legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
			if work.count != 1 || bytes.Contains(storage.slots[3], []byte("discarded-")) {
				test.Fatal("events reset published old paged history")
			}
		})
	}
}

func TestLegacyNativePagedSemanticsRejectionSourcePriority(test *testing.T) {
	for _, rejection := range []struct {
		name    string
		members string
	}{
		{name: "typed", members: `"replacement":[false],"replacement":[{"kind":"turn","id":"cannot-repair"}]`},
		{name: "semantic", members: `"replacement":[{"kind":"turn","replacement":null}]`},
	} {
		raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","id":"root-rejected",` + rejection.members +
			`}],"events":[],"ignored":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}`)
		if rejection.name == "semantic" {
			raw = bytes.Replace(raw, []byte(`,"events":[]`), nil, 1)
		}
		legacyNativeEventsSemanticsOracle(test, raw, rejection.name)
		for _, fault := range []string{"document", "late-hash", "one-eof-io", "cancel-one-eof"} {
			test.Run(rejection.name+"/"+fault, func(test *testing.T) {
				input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
				storage := &legacyNormalizationPreparationStorage{test: test, borrowed: make([]byte, 512<<10)}
				source := legacyNormalizationPreparationInput(test, input, true)
				read := source.Read
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				armed, injected := false, false
				source.Read = func(readContext context.Context, offset, length int) ([]byte, error) {
					body, err := read(readContext, offset, length)
					if !armed || injected {
						return body, err
					}
					if fault == "late-hash" && offset == 2*ConversationManifestChunkBytes && length > 0 {
						injected = true
						changed := bytes.Clone(body)
						changed[0] ^= 1
						return changed, nil
					}
					if offset == len(raw) && length == 1 {
						switch fault {
						case "one-eof-io":
							injected = true
							return nil, io.ErrClosedPipe
						case "cancel-one-eof":
							injected = true
							cancel()
							return nil, io.ErrClosedPipe
						}
					}
					return body, err
				}
				work, err := NewLegacyNormalizationPreparation(context.Background(), source, storage)
				if err != nil {
					test.Fatal(err)
				}
				legacyNormalizationPreparationAdvance(test, work, func() bool {
					return work.phase == legacyNormalizationDecode && work.decoder != nil &&
						(work.decoder.phase == legacyNormalizationReject || work.decoder.invalid)
				})
				if work.decoder.nativeCache == nil || work.decoder.nativeCache.object == nil || !work.decoder.nativeCache.object.pagedEvents ||
					(work.decoder.phase == legacyNormalizationReject) != (rejection.name == "typed") {
					test.Fatal("rejected fixture bypassed paged decoding or confused typed and deferred union errors")
				}
				armed = true
				_, err = legacyNormalizationPreparationDrain(test, ctx, work)
				private, canceled := fault == "late-hash" || fault == "one-eof-io", fault == "cancel-one-eof"
				want := error(ErrIntegrity)
				if private {
					want = ErrPreparedDocRangeObject
				} else if canceled {
					want = context.Canceled
				}
				if !errors.Is(err, want) || errors.Is(err, ErrPreparedDocRangeObject) != private || errors.Is(err, context.Canceled) != canceled ||
					errors.Is(err, ErrIntegrity) != !canceled || injected != (fault != "document") ||
					len(storage.slots[3]) != 0 || work.files[3].size != 0 || !bytes.Equal(input.body, raw) || HashContent(input.body) != HashContent(raw) {
					test.Fatal("paged rejection changed source/cancel/error priority, exposed output, or mutated original bytes/hash", err)
				}
				if fault == "one-eof-io" && !errors.Is(err, io.ErrClosedPipe) || canceled && errors.Is(err, io.ErrClosedPipe) {
					test.Fatal("paged rejection discarded private I/O or joined it into cancellation", err)
				}
				if fault != "late-hash" {
					legacyTypedNumberPreparationInputEOF(test, input)
				}
				if !canceled {
					reads, scratchReads, resets := len(input.reads), len(storage.reads), storage.resets
					legacyNormalizationPreparationFailed(test, work, err, want)
					if len(input.reads) != reads || len(storage.reads) != scratchReads || storage.resets != resets {
						test.Fatal("sticky paged rejection resumed source reads or scratch resets")
					}
				}
			})
		}
	}
}

func TestLegacyNativePagedSemanticsInternalTypedCompatibility(test *testing.T) {
	result := legacyNativeEventsParityDecode(test, `{"kind":"compaction","replacement":[{"kind":"turn","id":"typed"}],"replacement":[{"kind":"turn","id":"fresh"}]}`)
	object := result.fixture.native.object
	index := legacyEventNativeFields.match("replacement")
	if object.pagedEvents || index < 0 || index >= len(object.events) || object.events[index] == nil ||
		object.events[index].paged || object.events[index].values != nil || len(result.event.Replacement) != 1 {
		test.Fatal("internal typed constructor was silently changed into the public paged consumer")
	}
	legacyNativeEventsParityCanonical(test, result)
}
