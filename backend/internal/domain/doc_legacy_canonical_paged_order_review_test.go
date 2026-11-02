package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyCanonicalPagedOrderReviewPromotionYieldKeepsChildIntervals(test *testing.T) {
	frameBytes := int(unsafe.Sizeof(legacyCanonicalEncodingFrame{}))
	inlinePromotionBytes := int(unsafe.Sizeof([legacyCanonicalInlineValues]legacyCanonicalEncodingFrame{})) + frameBytes
	fillerBytes := LegacyCanonicalEncodingBytes - inlinePromotionBytes - (legacyCanonicalInlineValues+1)*frameBytes
	if fillerBytes <= 0 {
		test.Fatal("fixture cannot leave exactly one frame of source work before promotion", frameBytes)
	}
	filler := strings.Repeat("x", fillerBytes)
	prefix := strings.Repeat("k", LegacyCanonicalEncodingBytes+7)
	target := make(map[string]any, 9)
	for position := 0; position < 9; position++ {
		key := prefix + fmt.Sprintf("-%02d", 9-position)
		children := []map[string]any{
			nil,
			{"marker": key, "nested": map[string]any{"token": key}},
			{"hidden": map[string]any{"marker": key}},
		}
		target[key] = children[:2]
	}
	var nested any = target
	for depth := 0; depth < legacyCanonicalInlineValues-1; depth++ {
		nested = []any{nested}
	}
	backing := []any{filler, nested, map[string]any{"after": true}, map[string]any{"hidden-root": true}}
	value := backing[:3]
	want, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
	if err != nil {
		test.Fatal(err)
	}
	if encoding.frames.length() != 1 || encoding.frames.single == nil || encoding.frames.at(0) != encoding.frames.single ||
		encoding.frames.inline != nil || encoding.frames.root != nil {
		test.Fatal("initial traversal did not use its actual single-frame backing")
	}
	legacyCanonicalIndependentPageHeap(test, &encoding.frames)
	targetEnd := 1 + 2*len(target)
	if encoding.mapKeys.length() != targetEnd+1 || encoding.mapKeys.root == nil {
		test.Fatal("visible directory did not promote or included hidden capacity maps", encoding.mapKeys.length(), targetEnd+1)
	}
	keys := encoding.mapKeys.at(0)
	if keys == nil || keys.sorted || keys.endKeys != targetEnd || keys.fields.length() != len(target) || keys.fields.root == nil {
		test.Fatal("target lost its deferred ordering, promoted fields, or subtree endpoint")
	}
	for position := 0; position < keys.fields.length(); position++ {
		field := keys.fields.at(position)
		markerPosition := 1 + 2*position
		if field == nil || field.childKeys != markerPosition {
			test.Fatal("prepared field lost its exact child directory start", position, markerPosition)
		}
		children, valid := field.value.([]map[string]any)
		if !valid || len(children) != 2 || cap(children) != 3 || children[0] != nil || children[1]["marker"] != field.key {
			test.Fatal("prepared field detached its key from the visible typed slice", position)
		}
		marker := encoding.mapKeys.at(markerPosition)
		if marker == nil || !marker.sorted || marker.fields.length() != 2 || marker.endKeys != markerPosition+2 {
			test.Fatal("marker subtree interval crossed its hidden capacity map", position)
		}
		markerField, nestedField := marker.fields.at(0), marker.fields.at(1)
		if markerField == nil || nestedField == nil || markerField.key != "marker" || markerField.value != field.key ||
			markerField.childKeys != markerPosition+1 || nestedField.key != "nested" || nestedField.childKeys != markerPosition+1 {
			test.Fatal("tiny sorted child fields lost their leaf or nested interval", position)
		}
		leaf := encoding.mapKeys.at(markerPosition + 1)
		if leaf == nil || !leaf.sorted || leaf.fields.length() != 1 || leaf.endKeys != markerPosition+2 ||
			leaf.fields.single == nil || leaf.fields.at(0) != leaf.fields.single || leaf.fields.inline != nil || leaf.fields.root != nil {
			test.Fatal("nested map did not terminate before the next field interval", position)
		}
		token := leaf.fields.at(0)
		if token == nil || token.key != "token" || token.value != field.key || token.childKeys != markerPosition+2 {
			test.Fatal("nested token detached from its owning parent field", position)
		}
	}
	tail := encoding.mapKeys.at(targetEnd)
	if tail == nil || tail.endKeys != targetEnd+1 || tail.fields.length() != 1 ||
		tail.fields.single == nil || tail.fields.at(0) != tail.fields.single || tail.fields.inline != nil || tail.fields.root != nil {
		test.Fatal("following sibling did not begin at the target endpoint")
	}
	tailField := tail.fields.at(0)
	if tailField == nil || tailField.key != "after" || tailField.value != true || tailField.childKeys != targetEnd+1 {
		test.Fatal("following sibling was replaced by a hidden map directory")
	}
	first, complete, err := encoding.Next(context.Background())
	if err != nil || complete || len(first) > LegacyCanonicalEncodingBytes || cap(first) > LegacyCanonicalEncodingBytes {
		test.Fatal("promotion-blocked prefix violated the output quantum", len(first), cap(first), complete, err)
	}
	fillerJSON, err := json.Marshal(filler)
	if err != nil {
		test.Fatal(err)
	}
	wantPrefix := append([]byte{'['}, fillerJSON...)
	wantPrefix = append(wantPrefix, ',')
	wantPrefix = append(wantPrefix, strings.Repeat("[", legacyCanonicalInlineValues-1)...)
	if !bytes.Equal(first, wantPrefix) || encoding.frames.length() != legacyCanonicalInlineValues ||
		encoding.frames.single != nil || encoding.frames.inline == nil || encoding.frames.root != nil ||
		encoding.lastStorageBytes != inlinePromotionBytes || encoding.keyWork != 0 {
		test.Fatal("frame promotion did not yield before starting the target child")
	}
	for position := 0; position < encoding.frames.length(); position++ {
		frame := encoding.frames.at(position)
		wantPosition := 1
		if position == 0 {
			wantPosition = 2
		} else if position == legacyCanonicalInlineValues-1 {
			wantPosition = 0
		}
		if frame == nil || frame.kind != 'a' || frame.phase != 1 || frame.position != wantPosition || frame.keysPosition != 0 {
			test.Fatal("blocked push mutated a parent position or directory cursor", position, wantPosition)
		}
	}
	promotionBytes, err := encoding.frames.appendBytes()
	if err != nil || promotionBytes <= 0 || promotionBytes+frameBytes >= LegacyCanonicalEncodingBytes {
		test.Fatal("fixture did not isolate a bounded inline promotion", promotionBytes, frameBytes, err)
	}
	second, complete, err := encoding.Next(context.Background())
	if err != nil || complete || len(second) != 0 || cap(second) > LegacyCanonicalEncodingBytes || !encoding.ordering ||
		encoding.frames.length() != legacyCanonicalInlineValues+1 || encoding.frames.single != nil || encoding.frames.inline != nil || encoding.frames.root == nil ||
		encoding.lastStorageBytes != promotionBytes || encoding.keyWork == 0 || encoding.keyWork > legacyCanonicalEncodingActions {
		test.Fatal("resumed promotion did not share its bounded quantum with ordering", promotionBytes, encoding.lastStorageBytes, encoding.keyWork, err)
	}
	comparedBytes := 2 * encoding.order.compareOffset
	wantComparedBytes := (LegacyCanonicalEncodingBytes - promotionBytes - frameBytes) / 2 * 2
	if comparedBytes != wantComparedBytes {
		test.Fatal("comparison did not charge both key sides after frame storage and copy work", comparedBytes, wantComparedBytes)
	}
	parent := encoding.frames.at(legacyCanonicalInlineValues - 1)
	ordered := encoding.frames.at(legacyCanonicalInlineValues)
	if parent == nil || parent.position != 1 || parent.phase != 1 || ordered == nil || ordered.kind != 'o' ||
		ordered.keysPosition != 0 || ordered.keysEnd != targetEnd || ordered.position != 0 || ordered.phase != 0 || ordered.ordered {
		test.Fatal("inline promotion lost the parent update or introduced the wrong child interval")
	}
	got := bytes.Clone(first)
	for step := 0; step < 1024; step++ {
		beforeWork := encoding.keyWork
		chunk, done, err := encoding.Next(context.Background())
		if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes ||
			encoding.lastStorageBytes < 0 || encoding.lastStorageBytes > LegacyCanonicalEncodingBytes ||
			encoding.keyWork-beforeWork > legacyCanonicalEncodingActions {
			test.Fatal("continuation escaped a storage, output, or ordering quantum", step, len(chunk), cap(chunk), err)
		}
		if !done && len(chunk) == 0 && encoding.keyWork <= beforeWork {
			test.Fatal("continuation yielded without output or ordering progress", step)
		}
		got = append(got, chunk...)
		if !done {
			continue
		}
		if !bytes.Equal(got, want) || HashContent(got) != HashContent(want) {
			test.Fatal("reordered children or the following sibling changed canonical bytes or hash")
		}
		if encoding.root != nil || encoding.pendingKeysHeap.bytes != 0 ||
			encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) ||
			encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) {
			test.Fatal("completed continuation retained promoted traversal or child directories")
		}
		after, err := json.Marshal(value)
		if err != nil || !bytes.Equal(after, want) {
			test.Fatal("promotion or ordering mutated caller-owned values", err)
		}
		chunk, done, err = encoding.Next(context.Background())
		if err != nil || !done || len(chunk) != 0 {
			test.Fatal("completed continuation changed on retry", done, err)
		}
		return
	}
	test.Fatal("promotion and ordering counterexample exhausted its finite continuation allowance")
}
