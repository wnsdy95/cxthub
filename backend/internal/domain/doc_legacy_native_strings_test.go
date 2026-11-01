package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeStringsTestHeap(test *testing.T, values []string) docRangeHeaderHeapEstimate {
	test.Helper()
	estimate, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(values))
	if err != nil {
		test.Fatal(err)
	}
	return estimate
}

func legacyNativeStringsTestStep(test *testing.T, list *legacyNativeStringList, source LegacyByteSource) bool {
	test.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	input := &legacyNormalizationInput{ctx: ctx, source: source}
	complete, err := list.step(ctx, input)
	cancel()
	if err != nil || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded {
		test.Fatal("string list exceeded its shared step or authenticated read budget", err, input.workBytes, input.loads)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return complete
}

func legacyNativeStringsTestDecode(test *testing.T, prior []string, heap docRangeHeaderHeapEstimate, raw string) (*legacyNativeStringList, int) {
	test.Helper()
	list, err := newLegacyNativeStringList(prior, heap, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	return legacyNativeStringsTestDrain(test, list, heap, raw)
}

func legacyNativeStringsTestDrain(test *testing.T, list *legacyNativeStringList, heap docRangeHeaderHeapEstimate, raw string) (*legacyNativeStringList, int) {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+64*parts; stepIndex++ {
		complete := legacyNativeStringsTestStep(test, list, source)
		retained := heap
		if complete {
			retained = list.heap()
		}
		list.retained(&retained)
		if retained.err != nil || !complete && retained.bytes < heap.bytes || retained.bytes < list.heap().bytes {
			test.Fatal("string list omitted retained input values or decoded storage", retained, list.heap())
		}
		if complete {
			minimum := legacyNativeStringsTestHeap(test, list.values())
			if list.heap().err != nil || list.heap().bytes < minimum.bytes {
				test.Fatal("string list heap omitted backing capacity or hidden values", list.heap(), minimum)
			}
			return list, stepIndex + 1
		}
	}
	test.Fatal("string list exhausted its per-part progress allowance")
	return nil, 0
}

func TestLegacyNativeStringListNullTailsMatchJSONUnmarshal(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		backing []string
		length  int
		arrays  []string
	}{
		{
			name:    "hidden-tail-survives-shorter-duplicates",
			backing: []string{"first", "second", "hidden-third", "hidden-fourth", "hidden-fifth", "hidden-sixth"},
			length:  2,
			arrays:  []string{`["fresh"]`, `[null,null,null,null,null,null]`, `["latest",null]`, `[null,null,null,null,null,null]`},
		},
		{
			name:    "zero-length-with-live-capacity",
			backing: []string{"hidden-first", "hidden-second", "hidden-third"},
			arrays:  []string{`[null,null]`, `["fresh"]`, `[null,null,null,null,null]`, `[null]`, `[null,null,null,null,null]`},
		},
		{
			name:    "empty-array-replaces-backing",
			backing: []string{"old-first", "old-tail"},
			length:  1,
			arrays:  []string{`[]`, `[null,null]`, `[]`, `null`, `[]`},
		},
		{
			name:    "whole-null-replaces-backing",
			backing: []string{"old-first", "old-tail"},
			length:  1,
			arrays:  []string{`null`, `[null,null]`, `["fresh",null,"tail"]`, `[null]`, `[null,null,null]`},
		},
		{
			name:   "nil-and-empty-remain-distinct",
			arrays: []string{`null`, `[]`, `[null]`, `[]`, `["",null]`, `null`},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var got, want []string
			if fixture.backing != nil {
				got = append([]string(nil), fixture.backing...)
				want = append([]string(nil), fixture.backing...)
				got, want = got[:fixture.length], want[:fixture.length]
			}
			heap := legacyNativeStringsTestHeap(test, got)
			var list *legacyNativeStringList
			for _, raw := range fixture.arrays {
				prior := got
				if err := json.Unmarshal([]byte(raw), &want); err != nil {
					test.Fatal(err)
				}
				if list == nil {
					list, _ = legacyNativeStringsTestDecode(test, got, heap, raw)
				} else {
					if err := list.start(0, len(raw)); err != nil {
						test.Fatal(err)
					}
					legacyNativeStringsTestDrain(test, list, heap, raw)
				}
				got, heap = list.values(), list.heap()
				if !reflect.DeepEqual(got, want) {
					test.Fatal("duplicate string array changed native null semantics", raw, got, want)
				}
				if len(got) > 0 && len(got) <= cap(prior) && &got[:cap(got)][0] != &prior[:cap(prior)][0] {
					test.Fatal("string array discarded reusable hidden backing", raw)
				}
				if raw == `[]` && (got == nil || cap(got) != 0) {
					test.Fatal("empty string array retained old capacity or became null")
				}
			}
		})
	}
}

func TestLegacyNativeStringListGiantStringsMatchJSONUnmarshal(test *testing.T) {
	var value strings.Builder
	value.WriteString(strings.Repeat("x", LegacyCanonicalEncodingBytes-5))
	value.WriteString(`\uD83D\uDE42\uD800x\uDC00\"\\\/\b\f\n\r\t\u0000`)
	value.WriteString(strings.Repeat("y", 2*ConversationManifestChunkBytes))
	value.WriteString("한🙂\xff\xc0\x80\xed\xa0\x80\xf0\x9f")
	value.WriteString(`\uD800`)
	raw := `["` + value.String() + `",null,"",null]`
	got := []string{"old", "kept", "replaced", "hidden-tail", "still-hidden"}[:2]
	want := append([]string(nil), got[:cap(got)]...)
	want = want[:len(got)]
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		test.Fatal(err)
	}
	list, steps := legacyNativeStringsTestDecode(test, got, legacyNativeStringsTestHeap(test, got), raw)
	if steps <= 1 || !reflect.DeepEqual(list.values(), want) || list.values()[:cap(list.values())][4] != "still-hidden" {
		test.Fatal("giant string list changed Unicode or hidden null tails", steps)
	}
	var allocation docRangeHeaderHeapEstimate
	allocation.allocation(int64(len(want[0])), 1)
	if list.payload.bytes < allocation.bytes || list.capacities[0] < len(want[0]) {
		test.Fatal("completed giant string storage was not charged", list.payload, allocation)
	}
	heap := list.heap()
	if err := list.start(0, len(`[null,null,null,null,null]`)); err != nil {
		test.Fatal(err)
	}
	next, _ := legacyNativeStringsTestDrain(test, list, heap, `[null,null,null,null,null]`)
	if err := json.Unmarshal([]byte(`[null,null,null,null,null]`), &want); err != nil || !reflect.DeepEqual(next.values(), want) {
		test.Fatal("duplicate array lost completed scalar or hidden tail", err)
	}
}

func TestLegacyNativeStringListMigrationBuffersRemainCharged(test *testing.T) {
	raw := `[` + strings.Repeat(`"value",`, 20000) + `null]`
	list, err := newLegacyNativeStringList(nil, docRangeHeaderHeapEstimate{}, 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	migrating := false
	for stepIndex := 0; stepIndex < 256; stepIndex++ {
		complete := legacyNativeStringsTestStep(test, list, source)
		if list.previousOwned && list.copied < len(list.previous) {
			migrating = true
			var expected docRangeHeaderHeapEstimate
			expected.allocation(1, int64(unsafe.Sizeof(*list)))
			expected.allocation(int64(cap(list.capacities)), int64(unsafe.Sizeof(int(0))))
			expected.allocation(int64(cap(list.previousCaps)), int64(unsafe.Sizeof(int(0))))
			expected.include(list.added)
			expected.include(legacyNativeStringsBacking(cap(list.decoded)))
			expected.include(legacyNativeStringsBacking(cap(list.previous)))
			if list.scalar != nil {
				list.scalar.retained(&expected)
			}
			var retained docRangeHeaderHeapEstimate
			list.retained(&retained)
			if retained.err != nil || retained.bytes != expected.bytes {
				test.Fatal("bounded slice migration omitted a live buffer", retained, expected)
			}
		}
		if complete {
			var want []string
			if err := json.Unmarshal([]byte(raw), &want); err != nil || !reflect.DeepEqual(list.values(), want) || !migrating {
				test.Fatal("wide native string list lost values or never exposed bounded migration", err, migrating)
			}
			return
		}
	}
	test.Fatal("wide native string list exhausted its progress allowance")
}

func TestLegacyNativeStringListDuplicateReplacementReleasesCapacity(test *testing.T) {
	raw := `["` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `",null]`
	list, _ := legacyNativeStringsTestDecode(test, nil, docRangeHeaderHeapEstimate{}, raw)
	giantHeap := list.heap().bytes
	for _, next := range []string{`["short"]`, `[null,null]`, `[""]`, `[null,null]`, `[]`, `[null,null]`, `null`} {
		heap := list.heap()
		if err := list.start(0, len(next)); err != nil {
			test.Fatal(err)
		}
		legacyNativeStringsTestDrain(test, list, heap, next)
		var payload docRangeHeaderHeapEstimate
		for _, capacity := range list.capacities {
			payload.allocation(int64(capacity), 1)
		}
		var expected docRangeHeaderHeapEstimate
		expected.allocation(1, int64(unsafe.Sizeof(list.decoded)))
		expected.include(legacyNativeStringsBacking(cap(list.decoded)))
		expected.include(payload)
		if list.heap().bytes != expected.bytes || list.payload.bytes != payload.bytes || list.heap().bytes >= giantHeap || list.added.bytes != 0 {
			test.Fatal("duplicate replacement accumulated discarded backing or string capacity", next, list.heap(), expected)
		}
		if next == `[""]` && list.capacities[0] != 0 {
			test.Fatal("empty replacement kept the giant string allocation")
		}
	}
}

func TestLegacyNativeStringListHiddenScalarCapacitySurvivesDuplicates(test *testing.T) {
	raw := `["first","` + strings.Repeat("x", ConversationManifestChunkBytes+73) + `",null]`
	var want []string
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		test.Fatal(err)
	}
	list, _ := legacyNativeStringsTestDecode(test, nil, docRangeHeaderHeapEstimate{}, raw)
	capacity := list.capacities[1]
	for _, next := range []string{`["short"]`, `[null,null,null]`, `["head","",null]`} {
		heap := list.heap()
		if err := list.start(0, len(next)); err != nil {
			test.Fatal(err)
		}
		legacyNativeStringsTestDrain(test, list, heap, next)
		if err := json.Unmarshal([]byte(next), &want); err != nil || !reflect.DeepEqual(list.values(), want) {
			test.Fatal("hidden giant string changed across duplicate arrays", err)
		}
		if next != `["head","",null]` && list.capacities[1] != capacity {
			test.Fatal("shortened duplicate lost the hidden string allocation capacity")
		}
		if next == `["head","",null]` && list.capacities[1] != 0 {
			test.Fatal("hidden giant string capacity survived an empty overwrite")
		}
	}
}

func TestLegacyNativeStringListPreCanceledStepDoesNotRead(test *testing.T) {
	raw := `["` + strings.Repeat("x", 2*ConversationManifestChunkBytes) + `",null]`
	prior := []string{"old", "hidden"}[:1]
	list, err := newLegacyNativeStringList(prior, legacyNativeStringsTestHeap(test, prior), 0, len(raw))
	if err != nil {
		test.Fatal(err)
	}
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	if legacyNativeStringsTestStep(test, list, source) {
		test.Fatal("giant string completed in one bounded step")
	}
	var before docRangeHeaderHeapEstimate
	list.retained(&before)
	position, reads := list.position, len(buffer.reads)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	input := &legacyNormalizationInput{ctx: ctx, source: source}
	complete, err := list.step(ctx, input)
	var after docRangeHeaderHeapEstimate
	list.retained(&after)
	if complete || !errors.Is(err, context.Canceled) || len(buffer.reads) != reads || list.position != position || before.bytes != after.bytes {
		test.Fatal("pre-canceled string list changed retained state or read source", err, before, after)
	}
	for stepIndex := 0; stepIndex < 128; stepIndex++ {
		if legacyNativeStringsTestStep(test, list, source) {
			if list.values()[1] != "hidden" {
				test.Fatal("resumed string list discarded a hidden null tail")
			}
			return
		}
	}
	test.Fatal("resumed string list exhausted its progress allowance")
}
