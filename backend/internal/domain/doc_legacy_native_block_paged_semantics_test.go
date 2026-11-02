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

func legacyNativeBlockPagedSemanticsSeed(count int) string {
	blocks := make([]string, count)
	for ordinal := range blocks {
		blocks[ordinal] = fmt.Sprintf(`{"type":"type-%03d","text":"old-%03d"}`, ordinal, ordinal)
	}
	return `[` + strings.Join(blocks, ",") + `]`
}

func legacyNativeBlockPagedSemanticsValues(test *testing.T, actual *legacyNativeBlockValues, expected []ContentBlock) {
	test.Helper()
	if (actual == nil) != (expected == nil) || actual.length() != len(expected) {
		test.Fatal("public block pages changed native nil/empty or logical length", actual.length(), len(expected))
	}
	if actual == nil {
		return
	}
	if actual.at(-1) != nil || actual.at(len(expected)) != nil || actual.records.length() < len(expected) || actual.records.length() > cap(expected) {
		test.Fatal("public block pages exposed a hidden slot or lost their native backing history", actual.records.length(), len(expected), cap(expected))
	}
	backing := expected[:cap(expected)]
	for ordinal := 0; ordinal < actual.records.length(); ordinal++ {
		record := actual.records.at(ordinal)
		if record == nil || record.block != backing[ordinal] {
			test.Fatal("public block page changed a native merged or hidden slot", ordinal, record, backing[ordinal])
		}
		if ordinal < len(expected) {
			if actual.at(ordinal) != record {
				test.Fatal("public block page omitted a visible native slot", ordinal)
			}
		} else if actual.at(ordinal) != nil {
			test.Fatal("public block page made its shortened tail visible", ordinal)
		}
	}
}

func legacyNativeBlockPagedSemanticsEvent(test *testing.T, actual CIREvent, blocks *legacyNativeBlockValues, replacement *legacyNativeEventValues, expected CIREvent) {
	test.Helper()
	if actual.Blocks != nil || actual.Replacement != nil {
		test.Fatal("public block semantics bypassed the native paged owner")
	}
	legacyNativeBlockPagedSemanticsValues(test, blocks, expected.Blocks)
	children := expected.Replacement
	expected.Blocks, expected.Replacement = nil, nil
	if !reflect.DeepEqual(actual, expected) || (replacement == nil) != (children == nil) {
		test.Fatal("public block pages changed event fields, presence, or replacement reset semantics", actual, expected)
	}
	if replacement == nil {
		return
	}
	if replacement.length() != len(children) {
		test.Fatal("public block semantics changed the visible replacement length", replacement.length(), len(children))
	}
	for ordinal, child := range children {
		value := replacement.at(ordinal)
		if value == nil {
			test.Fatal("public block semantics lost a replacement child", ordinal)
		}
		legacyNativeBlockPagedSemanticsEvent(test, value.event, value.blocks, value.replacement, child)
	}
}

func legacyNativeBlockPagedSemanticsPause(test *testing.T, work *LegacyNormalizationPreparation, storage *legacyNormalizationPreparationStorage, input *legacySourceProofBuffer, expected CIREvent) *legacyNativeCanonicalOwner {
	test.Helper()
	for quantum := 0; quantum < 8192; quantum++ {
		if work.phase == legacyNormalizationStage {
			for _, pending := range work.pending {
				encoding := pending.Encoding
				if encoding == nil || encoding.nativeOwner == nil || encoding.nativeOwner.event.ID != expected.ID {
					continue
				}
				owner := encoding.nativeOwner
				if work.decoder == nil || work.decoder.nativeCache == nil || work.decoder.nativeCache.object == nil ||
					!work.decoder.nativeCache.object.pagedEvents || encoding.preparation == nil {
					test.Fatal("block semantics fixture bypassed the default public native preparation")
				}
				legacyNativeBlockPagedSemanticsEvent(test, owner.event, owner.blocks, owner.replacement, expected)
				if expected.Kind == EventMessage && owner.blocks != nil {
					fields, ok := encoding.root.(map[string]any)
					if !ok || fields["blocks"] != owner.blocks {
						test.Fatal("public block projection substituted a typed array for its actual merged pages")
					}
				}
				return owner
			}
		}
		if complete, err := legacyTypedNumberPreparationStep(test, work, storage, input); err != nil || complete {
			test.Fatal("public preparation terminated before its selected block owner", expected.ID, complete, err)
		}
	}
	test.Fatal("public preparation did not reach its selected block owner", expected.ID)
	return nil
}

func TestLegacyNativeBlockPagedSemanticsDuplicateSlotMergeAndHiddenTail(test *testing.T) {
	const count = 2*legacyCanonicalPageValues + 3
	seed := legacyNativeBlockPagedSemanticsSeed(count)
	regrowth := make([]string, count)
	for ordinal := range regrowth {
		switch ordinal % 3 {
		case 0:
			regrowth[ordinal] = `null`
		case 1:
			regrowth[ordinal] = `{}`
		default:
			regrowth[ordinal] = `{"type":null,"text":null}`
		}
	}
	regrowth[legacyCanonicalPageValues-1] = `{"TYPE":"boundary-type","text":null}`
	regrowth[legacyCanonicalPageValues] = `{"type":null,"TEXT":""}`
	regrowth[legacyCanonicalPageValues+1] = `{"type":"","te\u0078t":null}`
	regrowth[2*legacyCanonicalPageValues] = `{"text":"new-page-tail"}`
	shorter := `,"BLOCKS":[{"type":null,"TEXT":"new-head"}]`
	for _, fixture := range []struct {
		name      string
		suffix    string
		highwater int
	}{
		{name: "shorter-merges-head-and-hides-two-pages", suffix: shorter, highwater: count},
		{name: "null-element-keeps-prior-slot", suffix: `,"blocks":[null]`, highwater: count},
		{name: "null-fields-keep-both-strings", suffix: `,"blocks":[{"type":null,"text":null}]`, highwater: count},
		{name: "regrowth-reexposes-merged-hidden-tail", suffix: shorter + `,"bloc\u006bs":[` + strings.Join(regrowth, ",") + `]`, highwater: count},
		{name: "empty-strings-clear-both-fields", suffix: `,"blocks":[{"type":"","text":""}],"blocks":[null]`, highwater: count},
		{name: "empty-array-releases-hidden-tail", suffix: shorter + `,"blocks":[]`},
		{name: "null-array-releases-hidden-tail", suffix: shorter + `,"blocks":null`},
		{name: "empty-then-regrowth-is-fresh", suffix: shorter + `,"blocks":[],"blocks":[null,{}]`, highwater: 2},
		{name: "null-then-regrowth-is-fresh", suffix: shorter + `,"blocks":null,"blocks":[{},null]`, highwater: 2},
	} {
		for _, nested := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/nested=%t", fixture.name, nested), func(test *testing.T) {
				event := `{"kind":"message","id":"merged-blocks","seq":7,"role":"user","blocks":` + seed + fixture.suffix + `}`
				if nested {
					event = `{"kind":"compaction","id":"nested-blocks","replacement":[{"kind":"compaction","replacement":[` + event + `]}]}`
				}
				raw := legacyNativeEventDocument("2", event)
				document := legacyNativePagedSemanticsDocument(test, raw)
				canonical, original := legacyNativeEventsSemanticsOracle(test, raw, "")
				work, storage, input := legacyTypedNumberPreparationStart(test, raw)
				owner := legacyNativeBlockPagedSemanticsPause(test, work, storage, input, document.Events[0])
				blocks := owner.blocks
				if nested {
					blocks = owner.replacement.at(0).replacement.at(0).blocks
				}
				if blocks == nil {
					if fixture.highwater != 0 {
						test.Fatal("duplicate block list lost its retained history")
					}
				} else if blocks.records.length() != fixture.highwater {
					test.Fatal("duplicate block list reset or retained the wrong hidden tail", blocks.records.length(), fixture.highwater)
				}
				legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
				if fixture.highwater < count && bytes.Contains(storage.slots[3], []byte("old-")) {
					test.Fatal("empty or null block reset resurrected discarded strings")
				}
			})
		}
	}
}

func TestLegacyNativeBlockPagedSemanticsReplacementStartsFreshWire(test *testing.T) {
	const count = legacyCanonicalPageValues + 3
	seed := legacyNativeBlockPagedSemanticsSeed(count)
	raw := legacyNativeEventDocument("2", `{"kind":"compaction","id":"fresh-replacement","replacement":[`+
		`{"kind":"message","id":"discarded","agent_message":true,"agent_author":"old-author","blocks":`+seed+`}],`+
		`"replacement":[{"kind":"message","id":"fresh","blocks":[{"text":"fresh-text"}],"BLOCKS":[{"type":null,"text":null}]}]}`)
	document := legacyNativePagedSemanticsDocument(test, raw)
	canonical, original := legacyNativeEventsSemanticsOracle(test, raw, "")
	child := document.Events[0].Replacement[0]
	if child.AgentMessage || child.AgentAuthor != "" || !reflect.DeepEqual(child.Blocks, []ContentBlock{{Text: "fresh-text"}}) {
		test.Fatal("native oracle did not distinguish fresh replacement wires from merging block slots", child)
	}
	work, storage, input := legacyTypedNumberPreparationStart(test, raw)
	owner := legacyNativeBlockPagedSemanticsPause(test, work, storage, input, document.Events[0])
	if owner.replacement.at(0).blocks.records.length() != 1 {
		test.Fatal("fresh replacement wire inherited the previous child's block tail")
	}
	legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
}

func TestLegacyNativeBlockPagedSemanticsSecondEventCacheReset(test *testing.T) {
	const count = legacyCanonicalPageValues + 3
	seed := legacyNativeBlockPagedSemanticsSeed(count)
	for _, fixture := range []struct {
		name    string
		members string
	}{
		{name: "absent-blocks"},
		{name: "null-blocks", members: `,"blocks":null`},
		{name: "empty-blocks", members: `,"blocks":[]`},
		{name: "fresh-null-and-partial-slots", members: `,"blocks":[null,{},{"type":null,"text":"fresh"}]`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", `{"kind":"message","id":"first-blocks","seq":1,"role":"user","blocks":`+seed+
				`,"blocks":[{"text":"short-first"}]},{"kind":"message","id":"second-blocks","seq":2,"role":"assistant"`+fixture.members+`}`)
			document := legacyNativePagedSemanticsDocument(test, raw)
			canonical, original := legacyNativeEventsSemanticsOracle(test, raw, "")
			work, storage, input := legacyTypedNumberPreparationStart(test, raw)
			legacyNativeBlockPagedSemanticsPause(test, work, storage, input, document.Events[0])
			cache := work.decoder.nativeCache
			second := legacyNativeBlockPagedSemanticsPause(test, work, storage, input, document.Events[1])
			if work.decoder.nativeCache != cache || second.blocks != nil && second.blocks.records.length() != len(document.Events[1].Blocks) {
				test.Fatal("second event replaced the public native cache or inherited the first block highwater")
			}
			legacyNativeEventsSemanticsFinish(test, raw, canonical, original, work, storage, input)
			var output CIRDocument
			if err := json.Unmarshal(storage.slots[3], &output); err != nil || len(output.Events) != 2 {
				test.Fatal("second-event reset lost the canonical document", err)
			}
			for _, block := range output.Events[1].Blocks {
				if strings.Contains(block.Type, "type-") || strings.Contains(block.Text, "old-") || block.Text == "short-first" {
					test.Fatal("second event inherited a prior cached block slot", block)
				}
			}
		})
	}
}

func TestLegacyNativeBlockPagedSemanticsRejectionSourceIOPriority(test *testing.T) {
	const count = 2*legacyCanonicalPageValues + 3
	seed := legacyNativeBlockPagedSemanticsSeed(count)
	for _, rejection := range []struct {
		name   string
		value  string
		member string
	}{
		{name: "typed-field", value: `{"type":null,"text":false,"text":"cannot-repair"}`},
		{name: "typed-element", value: `false`},
		{name: "semantic", member: `,"agent_author":null`},
	} {
		members := `"blocks":` + seed
		reset := ""
		class := "semantic"
		if rejection.value != "" {
			class = "typed"
			members += `,"blocks":[` + strings.Repeat(`null,`, legacyCanonicalPageValues) + rejection.value + `],"blocks":[]`
			reset = `,"events":[]`
		} else {
			members += `,"blocks":[null]` + rejection.member
		}
		raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"message","id":"rejected-blocks","role":"user",` + members +
			`}]` + reset + `,"ignored":"` + strings.Repeat("x", 2*ConversationManifestChunkBytes+73) + `"}`)
		legacyNativeEventsSemanticsOracle(test, raw, class)
		for _, fault := range []string{"document", "late-hash", "late-io", "one-eof-io", "cancel-one-eof"} {
			test.Run(rejection.name+"/"+fault, func(test *testing.T) {
				input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
				storage := &legacyNormalizationPreparationStorage{test: test, borrowed: make([]byte, 512<<10)}
				source := legacyNormalizationPreparationInput(test, input, true)
				read := source.Read
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				armed, injected := false, false
				failedAt := -1
				source.Read = func(readContext context.Context, offset, length int) ([]byte, error) {
					body, err := read(readContext, offset, length)
					if !armed || injected {
						return body, err
					}
					if offset == 2*ConversationManifestChunkBytes && length > 0 && (fault == "late-hash" || fault == "late-io") {
						injected, failedAt = true, len(input.reads)
						if fault == "late-io" {
							return nil, io.ErrClosedPipe
						}
						changed := bytes.Clone(body)
						changed[0] ^= 1
						return changed, nil
					}
					if offset == len(raw) && length == 1 && (fault == "one-eof-io" || fault == "cancel-one-eof") {
						injected, failedAt = true, len(input.reads)
						if fault == "cancel-one-eof" {
							cancel()
						}
						return nil, io.ErrClosedPipe
					}
					return body, err
				}
				work, err := NewLegacyNormalizationPreparation(context.Background(), source, storage)
				if err != nil || len(input.reads) != 0 {
					test.Fatal("block source-priority constructor failed or eagerly read its source", err)
				}
				observedPages := false
				for quantum := 0; quantum < 8192; quantum++ {
					if work.decoder != nil && work.decoder.native != nil {
						for _, list := range work.decoder.native.object.blocks {
							if list != nil && list.paged && list.data != nil && list.data.records.length() > legacyCanonicalPageValues {
								observedPages = true
								break
							}
						}
					}
					if work.phase == legacyNormalizationDecode && work.decoder != nil &&
						(class == "typed" && work.decoder.phase == legacyNormalizationReject || class == "semantic" && work.decoder.invalid) {
						armed = true
						break
					}
					if complete, err := legacyTypedNumberPreparationStep(test, work, storage, input); err != nil || complete {
						test.Fatal("block rejection became terminal before its source-priority drain", complete, err)
					}
				}
				if !armed || !observedPages || work.count != 0 || work.files[0].size != 0 || work.files[1].size != 0 || work.files[3].size != 0 {
					test.Fatal("block rejection did not traverse actual pages before remaining private", armed, observedPages)
				}
				_, err = legacyNormalizationPreparationDrain(test, ctx, work)
				private := fault == "late-hash" || fault == "late-io" || fault == "one-eof-io"
				canceled := fault == "cancel-one-eof"
				want := error(ErrIntegrity)
				if private {
					want = ErrPreparedDocRangeObject
				} else if canceled {
					want = context.Canceled
				}
				if !errors.Is(err, want) || errors.Is(err, ErrPreparedDocRangeObject) != private || errors.Is(err, context.Canceled) != canceled ||
					errors.Is(err, ErrIntegrity) != !canceled || injected != (fault != "document") || injected && len(input.reads) != failedAt ||
					len(storage.slots[3]) != 0 || work.files[3].size != 0 || !bytes.Equal(input.body, raw) || HashContent(input.body) != HashContent(raw) {
					test.Fatal("block rejection changed source/I/O/cancel priority or exposed or mutated bytes", err)
				}
				if errors.Is(err, io.ErrClosedPipe) != (fault == "late-io" || fault == "one-eof-io") {
					test.Fatal("block rejection lost its private I/O cause or joined it into cancellation", err)
				}
				if fault == "document" || fault == "one-eof-io" || canceled {
					legacyTypedNumberPreparationInputEOF(test, input)
				}
				reads, scratchReads, resets := len(input.reads), len(storage.reads), storage.resets
				legacyNormalizationPreparationFailed(test, work, err, want)
				if len(input.reads) != reads || len(storage.reads) != scratchReads || storage.resets != resets {
					test.Fatal("terminal block rejection resumed source I/O or scratch resets")
				}
			})
		}
	}
}
