package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

func LegacyReplacementOrderEvents(count int, prefix string) []CIREvent {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	sequences := [...]int{maximum, 0, minimum, -1, maximum - 1, minimum + 1, 1, -2}
	events := make([]CIREvent, count)
	for ordinal := range events {
		marker := fmt.Sprintf("%s-%03d", prefix, count-ordinal)
		events[ordinal] = CIREvent{
			Kind: EventMessage, ID: marker, Seq: sequences[ordinal%len(sequences)], Role: RoleAssistant,
			Blocks: []ContentBlock{{Type: "text", Text: marker + " <&> \ud55c\uad6d🙂"}},
		}
	}
	return events
}

func LegacyReplacementOrderPayload(marker string, count int) map[string]any {
	payload := make(map[string]any, count)
	for ordinal := 0; ordinal < count; ordinal++ {
		payload[fmt.Sprintf("key-%02d", count-ordinal)] = map[string]any{
			"z": marker,
			"a": []any{nil, map[string]any{"marker": marker, "ordinal": ordinal}},
		}
	}
	return payload
}

func LegacyReplacementOrderBoundedNext(test *testing.T, encoding *LegacyCanonicalEncoding) ([]byte, bool) {
	test.Helper()
	preparation := encoding.preparation
	beforeInspect, beforeKeys := encoding.inspectWork, encoding.keyWork
	beforeNumber, beforePosition := encoding.numbers.next, encoding.numbers.position
	chunk, complete, err := encoding.Next(context.Background())
	if err != nil {
		test.Fatal("replacement canonical Next failed", err)
	}
	if len(chunk) > 64<<10 || cap(chunk) > 64<<10 || encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > 64<<10 ||
		encoding.inspectWork < beforeInspect || encoding.inspectWork-beforeInspect > 1024 ||
		encoding.keyWork < beforeKeys || encoding.keyWork-beforeKeys > 1024 {
		test.Fatal("replacement Next escaped the existing byte or action quantum", len(chunk), cap(chunk), encoding.lastStorageBytes,
			beforeInspect, encoding.inspectWork, beforeKeys, encoding.keyWork)
	}
	if preparation != nil {
		if len(chunk) != 0 || complete || preparation.lastActions <= 0 || preparation.lastActions > 1024 ||
			preparation.lastBytes < 0 || preparation.lastBytes > 64<<10 || preparation.lastStorageBytes < 0 ||
			preparation.lastStorageBytes > preparation.lastBytes || encoding.lastStorageBytes != preparation.lastStorageBytes ||
			encoding.inspectWork-beforeInspect != uint64(preparation.lastActions) {
			test.Fatal("replacement preparation exposed output or lost bounded progress", len(chunk), complete,
				preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes)
		}
	}
	if !complete && len(chunk) == 0 && encoding.inspectWork == beforeInspect && encoding.keyWork == beforeKeys &&
		encoding.numbers.next == beforeNumber && encoding.numbers.position == beforePosition {
		test.Fatal("replacement Next yielded without preparation, ordering, number, or output progress")
	}
	return chunk, complete
}

func LegacyReplacementOrderField(test *testing.T, keys *legacyCanonicalEncodingKeys, name string) *legacyCanonicalKeyField {
	test.Helper()
	if keys == nil {
		test.Fatal("replacement child directory is missing", name)
	}
	for position := 0; position < keys.fields.length(); position++ {
		field := keys.fields.at(position)
		if field != nil && field.key == name {
			return field
		}
	}
	test.Fatal("replacement child directory omitted its field", name)
	return nil
}

func LegacyReplacementOrderDirectory(test *testing.T, encoding *LegacyCanonicalEncoding, wrapper *legacyCanonicalReplacement, directory int, original, expected []CIREvent) {
	test.Helper()
	if wrapper == nil || wrapper.events == nil || len(wrapper.events) != len(original) || cap(wrapper.events) != cap(original) ||
		!reflect.DeepEqual(wrapper.events, original) || len(original) != len(expected) || wrapper.order != (legacyCanonicalKeyOrder{}) {
		test.Fatal("replacement wrapper changed the original array or retained completed ordering state", directory)
	}
	if len(original) != 0 && &wrapper.events[0] != &original[0] {
		test.Fatal("replacement wrapper copied the source array", directory)
	}
	keys := encoding.mapKeys.at(directory)
	if keys == nil || !keys.sorted || keys.fields.length() != len(expected) || keys.endKeys <= directory || keys.endKeys > encoding.mapKeys.length() {
		test.Fatal("replacement directory lost its sorted fields or subtree interval", directory)
	}
	legacyCanonicalIndependentPageHeap(test, &keys.fields)
	if len(expected) == 1 && (keys.fields.single == nil || keys.fields.at(0) != keys.fields.single || keys.fields.inline != nil || keys.fields.root != nil) {
		test.Fatal("replacement fields lost their single-entry backing", len(expected))
	}
	if len(expected) > 1 && len(expected) <= 8 && (keys.fields.single != nil || keys.fields.inline == nil || keys.fields.root != nil) {
		test.Fatal("replacement fields lost their eight-entry inline boundary", len(expected))
	}
	if len(expected) > 8 && (keys.fields.single != nil || keys.fields.inline != nil || keys.fields.root == nil || keys.fields.pages != (len(expected)+63)/64) {
		test.Fatal("replacement fields lost their fixed 64-entry pages", len(expected), keys.fields.pages)
	}
	if len(expected) == 64 && keys.fields.depth != 0 || len(expected) == 65 && keys.fields.depth != 1 {
		test.Fatal("replacement page directory did not promote at 65 entries", len(expected), keys.fields.depth)
	}
	ordinals := make(map[string]int, len(original))
	for ordinal, event := range original {
		if _, duplicate := ordinals[event.ID]; duplicate {
			test.Fatal("replacement ordering fixture requires distinct ordinal markers", event.ID)
		}
		ordinals[event.ID] = ordinal
	}
	for position, event := range expected {
		field := keys.fields.at(position)
		if field == nil || len(field.key) != 32 || position != 0 && keys.fields.at(position-1).key >= field.key {
			test.Fatal("replacement synthetic keys are not strictly ordered 32-byte keys", directory, position)
		}
		ordinal, present := ordinals[event.ID]
		encodedOrdinal, err := strconv.ParseUint(field.key[16:], 16, 64)
		if !present || err != nil || encodedOrdinal != uint64(ordinal) {
			test.Fatal("replacement synthetic key lost the original ordinal", directory, position, event.ID, encodedOrdinal, ordinal, err)
		}
		projected, valid := field.value.(map[string]any)
		if !valid || projected["kind"] != event.Kind || projected["id"] != event.ID || projected["seq"] != event.Seq {
			test.Fatal("replacement fields differ from the independent stable event order", directory, position, event.ID)
		}
		end := keys.endKeys
		if position+1 < keys.fields.length() {
			end = keys.fields.at(position + 1).childKeys
		}
		child := encoding.mapKeys.at(field.childKeys)
		if field.childKeys <= directory || child == nil || child.fields.length() != len(projected) ||
			child.endKeys != end || child.endKeys <= field.childKeys || child.endKeys > keys.endKeys ||
			position == 0 && field.childKeys != directory+1 {
			test.Fatal("sorted replacement childKeys detached a child or crossed its sibling interval", directory, position, field.childKeys, end)
		}
		if event.Replacement != nil {
			nested, valid := projected["replacement"].(*legacyCanonicalReplacement)
			if !valid {
				test.Fatal("nested replacement lost its deferred wrapper", event.ID)
			}
			nestedField := LegacyReplacementOrderField(test, child, "replacement")
			if nestedField.value != nested || nestedField.childKeys <= field.childKeys || nestedField.childKeys >= child.endKeys {
				test.Fatal("nested replacement detached its childKeys from the owning event", event.ID)
			}
			LegacyReplacementOrderDirectory(test, encoding, nested, nestedField.childKeys, original[ordinal].Replacement, event.Replacement)
		}
	}
	if len(expected) == 0 && keys.endKeys != directory+1 {
		test.Fatal("empty replacement consumed a following sibling directory", directory, keys.endKeys)
	}
}

func LegacyReplacementOrderCheck(test *testing.T, fixture CIREvent) {
	test.Helper()
	raw, err := json.Marshal(fixture)
	if err != nil || len(raw) > 2<<20 {
		test.Fatal("replacement order fixture failed to marshal within the existing input ceiling", len(raw), err)
	}
	var reference CIREvent
	if err := json.Unmarshal(raw, &reference); err != nil {
		test.Fatal("original Go event decoder rejected the replacement fixture", err)
	}
	normalized := canonicalEvents([]CIREvent{reference})[0]
	want := legacyCanonicalNativePreparationOracle(test, normalized)
	goCanonical, err := canonicalJSON(normalized)
	if err != nil || !bytes.Equal(want, goCanonical) || HashContent(want) != HashContent(goCanonical) {
		test.Fatal("original stable projection oracle differs from Go canonical bytes or SHA", err)
	}
	decoded := legacyNativeCanonicalOwnerStart(test, string(raw))
	event := decoded.drain(test)
	assertUnchanged := func() {
		test.Helper()
		if !bytes.Equal(decoded.buffer.body, raw) || !reflect.DeepEqual(event, reference) {
			test.Fatal("native handoff, replacement preparation, or streaming mutated the original source")
		}
	}
	assertUnchanged()
	owner, err := decoded.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil {
		test.Fatal("actual native replacement event could not transfer its owner", err)
	}
	defer owner.release()
	encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
	if err != nil || encoding == nil {
		test.Fatal("owner-backed replacement canonical construction failed", err)
	}
	defer encoding.release()
	if encoding.nativeOwner != owner || encoding.preparation == nil || encoding.inspectWork != 0 || encoding.mapKeys.length() != 0 {
		test.Fatal("replacement construction bypassed the actual pending native path")
	}
	root, valid := encoding.root.(map[string]any)
	if !valid {
		test.Fatal("native replacement root is not an event projection")
	}
	wrapper, valid := root["replacement"].(*legacyCanonicalReplacement)
	if !valid || wrapper == nil || !reflect.DeepEqual(wrapper.events, event.Replacement) ||
		len(event.Replacement) != 0 && &wrapper.events[0] != &event.Replacement[0] {
		test.Fatal("native replacement root sorted or copied the original array before Next")
	}
	for step := 0; encoding.preparation != nil; step++ {
		if step == 1<<16 {
			test.Fatal("replacement preparation exhausted its finite Next allowance")
		}
		LegacyReplacementOrderBoundedNext(test, encoding)
		assertUnchanged()
	}
	legacyCanonicalIndependentPageHeap(test, &encoding.mapKeys)
	rootKeys := encoding.mapKeys.at(0)
	if rootKeys == nil || rootKeys.endKeys != encoding.mapKeys.length() {
		test.Fatal("replacement root lost the complete prepared directory interval")
	}
	replacementField := LegacyReplacementOrderField(test, rootKeys, "replacement")
	if replacementField.value != wrapper {
		test.Fatal("prepared replacement field no longer owns the original wrapper")
	}
	LegacyReplacementOrderDirectory(test, encoding, wrapper, replacementField.childKeys, event.Replacement, normalized.Replacement)
	var actual []byte
	for step := 0; step < 1<<16; step++ {
		chunk, complete := LegacyReplacementOrderBoundedNext(test, encoding)
		actual = append(actual, chunk...)
		assertUnchanged()
		if !complete {
			continue
		}
		if !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) || HashContent(actual) != HashContent(goCanonical) {
			test.Fatalf("native replacement order changed canonical bytes or SHA: got=%s want=%s", actual, want)
		}
		if encoding.root != nil || encoding.nativeOwner != nil || encoding.preparation != nil ||
			encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) || encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) {
			test.Fatal("completed replacement order stream retained traversal or owner state")
		}
		chunk, complete = LegacyReplacementOrderBoundedNext(test, encoding)
		if len(chunk) != 0 || !complete {
			test.Fatal("replacement canonical EOF changed on retry", len(chunk), complete)
		}
		assertUnchanged()
		return
	}
	test.Fatal("replacement output exhausted its finite Next allowance")
}

func TestLegacyReplacementOrderSignedSequenceAndPageBoundaries(test *testing.T) {
	for _, count := range []int{0, 1, 7, 8, 9, 63, 64, 65} {
		test.Run(fmt.Sprintf("children-%d", count), func(test *testing.T) {
			LegacyReplacementOrderCheck(test, CIREvent{
				Kind: EventCompaction, ID: "signed-root", Seq: 17, ReplacementComplete: true,
				Replacement: LegacyReplacementOrderEvents(count, "signed"),
			})
		})
	}
}

func TestLegacyReplacementOrderStableOriginalOrdinal(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, sequence := range []int{-maximum - 1, -1, 0, maximum} {
		for _, count := range []int{8, 64, 65, 129} {
			test.Run(fmt.Sprintf("seq-%d/children-%d", sequence, count), func(test *testing.T) {
				events := LegacyReplacementOrderEvents(count, "reverse-id")
				for ordinal := range events {
					events[ordinal].Seq = sequence
				}
				LegacyReplacementOrderCheck(test, CIREvent{
					Kind: EventCompaction, ID: "stable-root", Seq: 1, Replacement: events,
				})
			})
		}
	}
}

func TestLegacyReplacementOrderNestedChildKeysAcrossPages(test *testing.T) {
	branches := LegacyReplacementOrderEvents(9, "branch")
	counts := [...]int{8, 64, 65}
	for ordinal := range branches {
		branch := &branches[ordinal]
		children := LegacyReplacementOrderEvents(counts[ordinal%len(counts)], branch.ID+"-child")
		for childOrdinal := range children {
			if childOrdinal != 0 && childOrdinal != 7 && childOrdinal != 8 && childOrdinal != 63 && childOrdinal != 64 {
				continue
			}
			child := &children[childOrdinal]
			switch childOrdinal % 3 {
			case 0:
				child.Kind, child.Role, child.Blocks = EventToolCall, "", nil
				child.CallID, child.ToolName = child.ID, "ordered-input"
				child.Input = LegacyReplacementOrderPayload(child.ID, 9)
			case 1:
				child.Kind, child.Role, child.Blocks = EventToolResult, "", nil
				child.CallID = child.ID
				child.Output = []any{LegacyReplacementOrderPayload(child.ID, 8), map[string]any{"after": child.ID}}
			case 2:
				child.Kind, child.Role, child.Blocks = EventCompaction, "", nil
				child.Replacement = LegacyReplacementOrderEvents(8, child.ID+"-grandchild")
			}
		}
		branch.Kind, branch.Role, branch.Blocks = EventCompaction, "", nil
		branch.Replacement, branch.ReplacementComplete = children, ordinal%2 == 0
		branch.ProviderMetadata = &ProviderMetadata{TurnID: branch.ID + "-before"}
	}
	LegacyReplacementOrderCheck(test, CIREvent{
		Kind: EventCompaction, ID: "nested-root", Seq: -17, ReplacementComplete: true, Replacement: branches,
		ProviderMetadata: &ProviderMetadata{TurnID: "root-before"},
	})
}
