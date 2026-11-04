package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

func assertLegacyCanonicalNumberPendingState(test testing.TB, encoding *LegacyCanonicalEncoding) {
	test.Helper()
	numbers := &encoding.numbers
	if numbers.next < 0 || numbers.next > numbers.pending.length() || numbers.proven.length() != numbers.pending.length() {
		test.Fatal("number queue and identity tracking disagree", numbers.next, numbers.pending.length(), numbers.proven.length())
	}
	for index := 0; index < numbers.pending.length(); index++ {
		pending := numbers.pending.at(index)
		if pending == nil {
			test.Fatal("pending number page omitted a registered identity", index)
		}
		text := *pending
		identity := legacyCanonicalNumberIdentity{start: unsafe.StringData(text), size: len(text)}
		proven, exists := numbers.proven.get(identity)
		if !exists || proven != (index < numbers.next) {
			test.Fatal("number identity was omitted or proven before validation", index, numbers.next, exists, proven)
		}
	}
	if numbers.position < 0 || numbers.next == numbers.pending.length() && numbers.position != 0 ||
		numbers.next < numbers.pending.length() && numbers.position > len(*numbers.pending.at(numbers.next)) {
		test.Fatal("number cursor escaped its pending text", numbers.next, numbers.position)
	}
	if numbers.position == 0 && numbers.mode != legacyJSONValueIdle {
		test.Fatal("completed number retained lexical state", numbers.mode)
	}
	frame := encoding.frames.at(0)
	if encoding.frames.length() != 1 || frame == nil || frame.kind != 0 || frame.position != 0 || encoding.ordering || encoding.keyWork != 0 {
		test.Fatal("canonical traversal started before numeric preparation finished")
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*encoding)))
	expected.include(encoding.rootHeap)
	expected.include(encoding.pendingKeysHeap)
	expected.include(legacyCanonicalIndependentNumberHeap(test, numbers))
	expected.include(legacyCanonicalIndependentPageHeap(test, &encoding.mapKeys))
	expected.include(legacyCanonicalIndependentPageHeap(test, &encoding.frames))
	retained, err := encoding.RetainedBytes()
	if err != nil || expected.err != nil || retained != expected.bytes {
		test.Fatal("pending number storage is omitted or its aliased text is charged twice", retained, expected.bytes, err)
	}
}

func assertLegacyCanonicalNumberPendingReleased(test testing.TB, encoding *LegacyCanonicalEncoding, terminal error) {
	test.Helper()
	numbers := &encoding.numbers
	if encoding.nativeOwner != nil || encoding.preparation != nil || encoding.root != nil || encoding.rootHeap.bytes != 0 || encoding.rootHeap.err != nil || encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) ||
		encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) || encoding.pendingKeysHeap.bytes != 0 || encoding.pendingKeysHeap.err != nil || encoding.ordering ||
		numbers.pending != (legacyCanonicalPages[string]{}) || numbers.proven != (legacyCanonicalNumberRegistry{}) || numbers.next != 0 || numbers.position != 0 || numbers.mode != legacyJSONValueIdle {
		test.Fatal("terminal canonical encoding retained numeric or traversal state")
	}
	retained, err := encoding.RetainedBytes()
	if terminal != nil {
		if retained != 0 || !errors.Is(err, terminal) {
			test.Fatal("failed encoding exposed a valid retained estimate", retained, err)
		}
	} else {
		var expected docRangeHeaderHeapEstimate
		expected.allocation(1, int64(unsafe.Sizeof(*encoding)))
		if err != nil || retained != expected.bytes {
			test.Fatal("completed encoding retained numeric tracking", retained, expected.bytes, err)
		}
	}
	chunk, complete, err := encoding.Next(context.Background())
	if len(chunk) != 0 || complete != (terminal == nil) || !errors.Is(err, terminal) {
		test.Fatal("terminal canonical encoding changed on retry", len(chunk), complete, err)
	}
}

func collectLegacyCanonicalNumberPending(test testing.TB, encoding *LegacyCanonicalEncoding) []byte {
	test.Helper()
	var offsets []int
	var output []byte
	var preparedKeyWork uint64
	for step := 0; step < 1<<12; step++ {
		wasPreparing := encoding.preparation != nil
		beforeInspectWork := encoding.inspectWork
		beforeNext, beforePosition := encoding.numbers.next, encoding.numbers.position
		validating := beforeNext < encoding.numbers.pending.length()
		if !wasPreparing && validating {
			assertLegacyCanonicalNumberPendingConsumerState(test, encoding, preparedKeyWork)
			if len(offsets) != encoding.numbers.pending.length()+1 {
				offsets = make([]int, encoding.numbers.pending.length()+1)
				for index := 0; index < encoding.numbers.pending.length(); index++ {
					offsets[index+1] = offsets[index] + len(*encoding.numbers.pending.at(index))
				}
			}
		}
		beforeKeyWork := encoding.keyWork
		chunk, complete, err := encoding.Next(context.Background())
		if err != nil || len(chunk) > 64<<10 || cap(chunk) > 64<<10 {
			test.Fatal("pending encoding failed or escaped its output quantum", len(chunk), cap(chunk), err)
		}
		if wasPreparing {
			if encoding.preparation == nil {
				preparedKeyWork = encoding.keyWork
			}
			if len(chunk) != 0 || complete {
				test.Fatal("native preparation exposed canonical output", len(chunk), complete)
			}
			if encoding.inspectWork <= beforeInspectWork {
				test.Fatal("native preparation made no inspection progress")
			}
			if encoding.inspectWork-beforeInspectWork > legacyCanonicalEncodingActions {
				test.Fatal("native preparation exceeded its action quantum")
			}
		} else if validating {
			if len(chunk) != 0 || complete {
				test.Fatal("numeric preparation exposed canonical output", len(chunk), complete)
			}
			assertLegacyCanonicalNumberPendingConsumerState(test, encoding, preparedKeyWork)
			afterNext, afterPosition := encoding.numbers.next, encoding.numbers.position
			if afterNext < beforeNext || afterNext-beforeNext > 1024 || afterNext == beforeNext && afterPosition <= beforePosition {
				test.Fatal("number preparation stalled or exceeded its item quantum", beforeNext, beforePosition, afterNext, afterPosition)
			}
			consumed := offsets[afterNext] + afterPosition - offsets[beforeNext] - beforePosition
			if consumed < 0 || consumed+(afterNext-beforeNext)*legacyCanonicalNumberProofBytes > 64<<10 {
				test.Fatal("number preparation reset or exceeded its shared character quantum", consumed)
			}
		} else if len(chunk) == 0 && !complete && encoding.keyWork <= beforeKeyWork {
			test.Fatal("canonical encoding made no numeric, output, or ordering progress")
		}
		output = append(output, chunk...)
		if complete {
			assertLegacyCanonicalNumberPendingReleased(test, encoding, nil)
			return output
		}
	}
	test.Fatal("pending canonical encoding did not terminate")
	return nil
}

func TestLegacyCanonicalNumberPendingLargeLexicalValues(test *testing.T) {
	for _, size := range []int{(64 << 10) + 17, (1 << 20) + 17} {
		for _, candidate := range []struct {
			name string
			text string
			mode legacyJSONValueMode
		}{
			{name: "integer", text: "1" + strings.Repeat("0", size), mode: legacyJSONValueInteger},
			{name: "fraction", text: "-0." + strings.Repeat("7", size), mode: legacyJSONValueFraction},
			{name: "exponent", text: "1E+" + strings.Repeat("2", size), mode: legacyJSONValueExponentDigits},
		} {
			for _, nested := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/%d/nested=%t", candidate.name, size, nested), func(test *testing.T) {
					number := json.Number(candidate.text)
					var value any = number
					if nested {
						value = map[string]any{"a": "<prefix>", "z": []any{true, []map[string]any{{"n": &number}}}}
					}
					want, err := json.Marshal(value)
					if err != nil {
						test.Fatal(err)
					}
					encoding, err := newLegacyCanonicalEncodingPending(context.Background(), value)
					if err != nil || encoding == nil {
						test.Fatal("large lexical number was rejected during registration", err)
					}
					if encoding.numbers.pending.length() != 1 || encoding.numbers.next != 0 || encoding.numbers.position != 0 || encoding.numbers.mode != legacyJSONValueIdle {
						test.Fatal("constructor validated numeric text instead of registering it")
					}
					if text := *encoding.numbers.pending.at(0); len(text) != len(candidate.text) || unsafe.StringData(text) != unsafe.StringData(candidate.text) {
						test.Fatal("number registration copied or truncated immutable text")
					}
					assertLegacyCanonicalNumberPendingState(test, encoding)
					chunk, complete, err := encoding.Next(context.Background())
					if err != nil || complete || len(chunk) != 0 || encoding.numbers.next != 0 || encoding.numbers.position != 64<<10 || encoding.numbers.mode != candidate.mode {
						test.Fatal("first number quantum did not preserve lexical progress", len(chunk), complete, encoding.numbers.next, encoding.numbers.position, encoding.numbers.mode, err)
					}
					assertLegacyCanonicalNumberPendingState(test, encoding)
					if got := collectLegacyCanonicalNumberPending(test, encoding); !bytes.Equal(got, want) {
						test.Fatal("pending number validation changed legacy canonical bytes")
					}
				})
			}
		}
	}
}

func TestLegacyCanonicalNumberPendingAllNumbersBeforeOutput(test *testing.T) {
	first := json.Number("1" + strings.Repeat("0", (64<<10)+16))
	second := json.Number("0." + strings.Repeat("7", (1<<20)+17))
	value := map[string]any{"a": "prefix must stay private", "z": []any{first, map[string]any{"n": &second}, json.Number("1e1000")}}
	encoding, err := newLegacyCanonicalEncodingPending(context.Background(), value)
	if err != nil || encoding == nil {
		test.Fatal(err)
	}
	if encoding.numbers.pending.length() != 3 {
		test.Fatal("nested numeric references were not all registered", encoding.numbers.pending.length())
	}
	for step := 0; step < 2; step++ {
		chunk, complete, err := encoding.Next(context.Background())
		if err != nil || complete || len(chunk) != 0 {
			test.Fatal("canonical prefix escaped before every number was proven", step, len(chunk), complete, err)
		}
		assertLegacyCanonicalNumberPendingState(test, encoding)
	}
	if encoding.numbers.next != 1 || encoding.numbers.position != (64<<10)-17-legacyCanonicalNumberProofBytes || encoding.numbers.mode != legacyJSONValueFraction {
		test.Fatal("character budget restarted when validation advanced to another number", encoding.numbers.next, encoding.numbers.position, encoding.numbers.mode)
	}
	want, err := json.Marshal(value)
	if err != nil || !bytes.Equal(collectLegacyCanonicalNumberPending(test, encoding), want) {
		test.Fatal("all-number preparation changed canonical bytes", err)
	}
}

func TestLegacyCanonicalNumberPendingItemQuantum(test *testing.T) {
	values := make([]any, 1027)
	for index := range values {
		values[index] = json.Number(strconv.Itoa(index + 1))
	}
	encoding, err := newLegacyCanonicalEncodingPending(context.Background(), values)
	if err != nil || encoding == nil {
		test.Fatal(err)
	}
	assertLegacyCanonicalNumberPendingState(test, encoding)
	chunk, complete, err := encoding.Next(context.Background())
	consumed := encoding.numbers.position
	for index := 0; index < encoding.numbers.next; index++ {
		consumed += len(*encoding.numbers.pending.at(index))
	}
	if err != nil || complete || len(chunk) != 0 || encoding.numbers.next <= 0 || encoding.numbers.next > 1024 ||
		consumed+encoding.numbers.next*legacyCanonicalNumberProofBytes > 64<<10 {
		test.Fatal("short numbers escaped the shared 1024-item preparation quantum", encoding.numbers.next, encoding.numbers.position, len(chunk), complete, err)
	}
	assertLegacyCanonicalNumberPendingState(test, encoding)
	want, err := json.Marshal(values)
	if err != nil || !bytes.Equal(collectLegacyCanonicalNumberPending(test, encoding), want) {
		test.Fatal("item-limited validation changed canonical bytes", err)
	}
}

type legacyCanonicalNumberPendingPreparationContext struct {
	context.Context
	checks int
}

func (ctx *legacyCanonicalNumberPendingPreparationContext) Err() error {
	ctx.checks++
	return ctx.Context.Err()
}

func TestLegacyCanonicalNumberPendingPreparationIndependentOfTextSize(test *testing.T) {
	for _, suffix := range []string{"", "x"} {
		test.Run(fmt.Sprintf("invalid=%t", suffix != ""), func(test *testing.T) {
			var referenceChecks, referenceLength int
			var referenceRoot, referenceRetained, referenceFrames, referencePending int64
			for index, size := range []int{(64 << 10) + 17, (1 << 20) + 17} {
				number := json.Number("1" + strings.Repeat("0", size) + suffix)
				ctx := &legacyCanonicalNumberPendingPreparationContext{Context: context.Background()}
				encoding, err := newLegacyCanonicalEncodingPending(ctx, number)
				if err != nil || encoding == nil {
					test.Fatal("constructor scanned the numeric tail", size, err)
				}
				assertLegacyCanonicalNumberPendingState(test, encoding)
				if encoding.numbers.pending.length() != 1 || encoding.numbers.next != 0 || encoding.numbers.position != 0 ||
					unsafe.StringData(*encoding.numbers.pending.at(0)) != unsafe.StringData(string(number)) {
					test.Fatal("first number preparation depended on its text size")
				}
				retained, err := encoding.RetainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				if index == 0 {
					referenceChecks, referenceLength = ctx.checks, len(number)
					referenceFrames = legacyCanonicalIndependentPageHeap(test, &encoding.frames).bytes
					referencePending = legacyCanonicalIndependentPageHeap(test, &encoding.numbers.pending).bytes
					referenceRoot, referenceRetained = encoding.rootHeap.bytes, retained
				} else if ctx.checks != referenceChecks || legacyCanonicalIndependentPageHeap(test, &encoding.frames).bytes != referenceFrames || legacyCanonicalIndependentPageHeap(test, &encoding.numbers.pending).bytes != referencePending ||
					encoding.rootHeap.bytes-referenceRoot != 2*int64(len(number)-referenceLength) || retained-referenceRetained != encoding.rootHeap.bytes-referenceRoot {
					test.Fatal("numeric text size changed preparation work or auxiliary retained storage", size, ctx.checks, referenceChecks)
				}
				chunk, complete, err := encoding.Next(context.Background())
				if err != nil || complete || len(chunk) != 0 || encoding.numbers.next != 0 || encoding.numbers.position != 64<<10 || encoding.numbers.mode != legacyJSONValueInteger {
					test.Fatal("first validation quantum depended on the unseen numeric tail", size, encoding.numbers.position, err)
				}
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				chunk, complete, err = encoding.Next(canceled)
				if len(chunk) != 0 || complete || !errors.Is(err, context.Canceled) {
					test.Fatal("partially validated number ignored cancellation", err)
				}
				assertLegacyCanonicalNumberPendingReleased(test, encoding, context.Canceled)
			}
		})
	}
}

func TestLegacyCanonicalNumberPendingInvalidAtomicAndEagerContract(test *testing.T) {
	integer := "1" + strings.Repeat("0", (64<<10)+17)
	for _, candidate := range []struct {
		name string
		text string
	}{
		{name: "late-byte", text: "1" + strings.Repeat("0", (1<<20)+17) + "x"},
		{name: "late-delimiter", text: integer + "]"},
		{name: "fraction-eof", text: integer + "."},
		{name: "exponent-eof", text: integer + "e"},
		{name: "exponent-sign-eof", text: integer + "E+"},
		{name: "minus-eof", text: "-"},
		{name: "leading-zero", text: "01"},
	} {
		for _, nested := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/nested=%t", candidate.name, nested), func(test *testing.T) {
				number := json.Number(candidate.text)
				var value any = number
				if nested {
					value = map[string]any{"a": "must not escape", "z": []any{json.Number("2"), &number}}
				}
				_, nativeErr := json.Marshal(number)
				if nativeErr == nil {
					test.Fatal("invalid-number fixture is valid")
				}
				if eager, err := newLegacyCanonicalEncoding(context.Background(), value); eager != nil || err == nil || err.Error() != nativeErr.Error() {
					test.Fatal("eager constructor did not preserve the immediate native numeric error")
				}
				encoding, err := newLegacyCanonicalEncodingPending(context.Background(), value)
				if err != nil || encoding == nil {
					test.Fatal("pending constructor eagerly rejected numeric text", err)
				}
				assertLegacyCanonicalNumberPendingState(test, encoding)
				for step := 0; step < 64; step++ {
					chunk, complete, err := encoding.Next(context.Background())
					if len(chunk) != 0 || complete {
						test.Fatal("invalid numeric suffix exposed partial or completed canonical bytes", step, len(chunk), complete)
					}
					if err != nil {
						if !errors.Is(err, ErrValidation) || step < len(candidate.text)/(64<<10)-1 {
							test.Fatal("numeric failure bypassed bounded validation", step, err)
						}
						assertLegacyCanonicalNumberPendingReleased(test, encoding, err)
						return
					}
					assertLegacyCanonicalNumberPendingState(test, encoding)
				}
				test.Fatal("invalid pending number did not terminate")
			})
		}
	}
}

type legacyCanonicalNumberPendingProgressContext struct {
	context.Context
	encoding         *LegacyCanonicalEncoding
	position         int
	afterNumber      bool
	canceled         bool
	observedPosition int
	observedNext     int
}

func (ctx *legacyCanonicalNumberPendingProgressContext) Err() error {
	if ctx.canceled {
		return context.Canceled
	}
	numbers := &ctx.encoding.numbers
	if ctx.position > 0 && numbers.position >= ctx.position || ctx.afterNumber && numbers.next > 0 {
		ctx.canceled = true
		ctx.observedPosition, ctx.observedNext = numbers.position, numbers.next
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestLegacyCanonicalNumberPendingCancellationAtomicAndTerminal(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if encoding, err := newLegacyCanonicalEncodingPending(canceled, json.Number("01")); encoding != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("pending constructor masked cancellation with numeric validation", err)
	}
	for _, stage := range []string{"before-validation", "during-validation", "after-number", "finalization"} {
		test.Run(stage, func(test *testing.T) {
			number := json.Number("1" + strings.Repeat("0", (1<<20)+17))
			if stage == "after-number" || stage == "finalization" {
				number = "12"
			}
			encoding, err := newLegacyCanonicalEncodingPending(context.Background(), map[string]any{"a": "private prefix", "z": &number})
			if err != nil || encoding == nil {
				test.Fatal(err)
			}
			ctx := context.Context(canceled)
			var progress *legacyCanonicalNumberPendingProgressContext
			switch stage {
			case "during-validation", "after-number":
				progress = &legacyCanonicalNumberPendingProgressContext{Context: context.Background(), encoding: encoding, afterNumber: stage == "after-number"}
				if stage == "during-validation" {
					progress.position = canonicalCheckBytes
				}
				ctx = progress
			case "finalization":
				chunk, complete, err := encoding.Next(context.Background())
				if err != nil || complete || len(chunk) != 0 || encoding.numbers.next != encoding.numbers.pending.length() {
					test.Fatal("finalization fixture did not finish numeric preparation privately", err)
				}
				ctx = legacyCanonicalFinalizationContext{Context: context.Background(), encoding: encoding}
			}
			chunk, complete, err := encoding.Next(ctx)
			if len(chunk) != 0 || complete || !errors.Is(err, context.Canceled) {
				test.Fatal("canceled pending encoding exposed output or lost cancellation", len(chunk), complete, err)
			}
			if progress != nil && (!progress.canceled || stage == "during-validation" && (progress.observedPosition <= 0 || progress.observedPosition > 64<<10) ||
				stage == "after-number" && progress.observedNext != 1) {
				test.Fatal("cancellation did not observe actual numeric progress", progress.observedPosition, progress.observedNext)
			}
			assertLegacyCanonicalNumberPendingReleased(test, encoding, context.Canceled)
		})
	}
}

func TestLegacyCanonicalNumberPendingChangedCanonicalPointeeRejectsUnprovenIdentity(test *testing.T) {
	for _, afterValidation := range []bool{false, true} {
		for _, sameBacking := range []bool{false, true} {
			test.Run(fmt.Sprintf("validated=%t/same-backing=%t", afterValidation, sameBacking), func(test *testing.T) {
				original := "1" + strings.Repeat("2", (64<<10)+17)
				number := json.Number(original)
				encoding, err := newLegacyCanonicalEncodingPending(context.Background(), map[string]any{"a": "private prefix", "z": &number})
				if err != nil || encoding == nil {
					test.Fatal(err)
				}
				if afterValidation {
					for step := 0; encoding.numbers.next < encoding.numbers.pending.length(); step++ {
						if step == 4 {
							test.Fatal("canonical pointee fixture did not finish numeric validation")
						}
						chunk, complete, err := encoding.Next(context.Background())
						if err != nil || complete || len(chunk) != 0 {
							test.Fatal("canonical pointee validation exposed output", err)
						}
					}
				}
				if sameBacking {
					number = json.Number(original[:len(original)-1])
				} else {
					number = json.Number(strings.Clone(original))
					if unsafe.StringData(string(number)) == unsafe.StringData(original) {
						test.Fatal("changed-identity fixture reused the original backing")
					}
				}
				for step := 0; step < 4; step++ {
					chunk, complete, err := encoding.Next(context.Background())
					if len(chunk) != 0 || complete {
						test.Fatal("unproven current pointee exposed canonical output", len(chunk), complete)
					}
					if err != nil {
						if !errors.Is(err, ErrIntegrity) {
							test.Fatal("changed canonical identity did not fail integrity", err)
						}
						assertLegacyCanonicalNumberPendingReleased(test, encoding, err)
						return
					}
				}
				test.Fatal("changed canonical pointee did not terminate")
			})
		}
	}
}
