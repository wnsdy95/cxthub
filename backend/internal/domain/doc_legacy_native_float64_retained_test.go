package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeFloat64RetainedLayout struct {
	bytes       int64
	allocations int64
}

func legacyNativeFloat64RetainedType(test *testing.T, kind reflect.Type, path string, active map[reflect.Type]bool) legacyNativeFloat64RetainedLayout {
	test.Helper()
	if active[kind] {
		test.Fatalf("%s: numeric state has a recursive ownership type %s", path, kind)
	}
	active[kind] = true
	defer delete(active, kind)
	var owned legacyNativeFloat64RetainedLayout
	switch kind.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
	case reflect.Struct:
		for fieldIndex := 0; fieldIndex < kind.NumField(); fieldIndex++ {
			field := kind.Field(fieldIndex)
			child := legacyNativeFloat64RetainedType(test, field.Type, path+"."+field.Name, active)
			owned.bytes += child.bytes
			owned.allocations += child.allocations
		}
	case reflect.Array:
		child := legacyNativeFloat64RetainedType(test, kind.Elem(), path+"[]", active)
		owned.bytes = int64(kind.Len()) * child.bytes
		owned.allocations = int64(kind.Len()) * child.allocations
	case reflect.Pointer:
		if kind.Elem().Kind() != reflect.Struct {
			test.Fatalf("%s: numeric state points outside a fixed converter struct: %s", path, kind)
		}
		owned = legacyNativeFloat64RetainedType(test, kind.Elem(), path+"*", active)
		owned.bytes += int64(kind.Elem().Size())
		owned.allocations++
	default:
		test.Fatalf("%s: numeric reducer can retain source text, containers, pages, or context through %s", path, kind)
	}
	if owned.bytes > canonicalCheckBytes || owned.allocations > 16 {
		test.Fatalf("%s: numeric state has excessive fixed ownership: %+v", path, owned)
	}
	return owned
}

func legacyNativeFloat64RetainedReachable(value reflect.Value, seen map[uintptr]bool, snapshot *[]uint64) int64 {
	var live int64
	switch value.Kind() {
	case reflect.Struct:
		for fieldIndex := 0; fieldIndex < value.NumField(); fieldIndex++ {
			live += legacyNativeFloat64RetainedReachable(value.Field(fieldIndex), seen, snapshot)
		}
	case reflect.Array:
		for elementIndex := 0; elementIndex < value.Len(); elementIndex++ {
			live += legacyNativeFloat64RetainedReachable(value.Index(elementIndex), seen, snapshot)
		}
	case reflect.Pointer:
		address := value.Pointer()
		*snapshot = append(*snapshot, uint64(address))
		if address != 0 && !seen[address] {
			seen[address] = true
			live += int64(value.Type().Elem().Size())
			live += legacyNativeFloat64RetainedReachable(value.Elem(), seen, snapshot)
		}
	case reflect.Bool:
		if value.Bool() {
			*snapshot = append(*snapshot, 1)
		} else {
			*snapshot = append(*snapshot, 0)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*snapshot = append(*snapshot, uint64(value.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		*snapshot = append(*snapshot, value.Uint())
	case reflect.Float32, reflect.Float64:
		*snapshot = append(*snapshot, math.Float64bits(value.Float()))
	}
	return live
}

func legacyNativeFloat64RetainedObserve(test *testing.T, parser *legacyNativeFloat64) (int64, []uint64) {
	test.Helper()
	if parser == nil {
		test.Fatal("numeric constructor returned no helper")
	}
	value := reflect.ValueOf(parser)
	layout := legacyNativeFloat64RetainedType(test, value.Type().Elem(), "numeric", make(map[reflect.Type]bool))
	layout.bytes += int64(value.Type().Elem().Size())
	layout.allocations++
	if layout.bytes > canonicalCheckBytes {
		test.Fatal("fixed numeric reducer exceeds one scan quantum of storage", layout)
	}
	var snapshot []uint64
	live := legacyNativeFloat64RetainedReachable(value, make(map[uintptr]bool), &snapshot)
	var estimate docRangeHeaderHeapEstimate
	parser.retained(&estimate)
	if estimate.err != nil || estimate.bytes < live {
		test.Fatal("numeric retained estimate omits reachable fixed allocations", live, estimate.bytes, estimate.err)
	}
	if live > layout.bytes || estimate.bytes > 4*(layout.bytes+64*layout.allocations) {
		test.Fatal("numeric state or accounting amplifies its actual fixed ownership", live, layout, estimate.bytes)
	}
	return live, snapshot
}

func legacyNativeFloat64RetainedPages(test *testing.T, input *legacyNormalizationInput, borrowed []byte) {
	test.Helper()
	var capacity int
	windowOwned := input.window == nil
	for pageIndex, page := range input.pages {
		capacity += cap(page)
		if page == nil {
			continue
		}
		ordinal := input.ordinals[pageIndex]
		if ordinal < 0 || ordinal >= len(input.source.Parts) || len(page) != int(input.source.Parts[ordinal].Bytes) || cap(page) > ConversationManifestChunkBytes {
			test.Fatal("numeric input retained a non-part or oversized page", ordinal, len(page), cap(page))
		}
		address := reflect.ValueOf(page).Pointer()
		if len(borrowed) != 0 && address == reflect.ValueOf(borrowed).Pointer() {
			test.Fatal("numeric input retained a borrowed source page")
		}
		if len(input.window) != 0 && reflect.ValueOf(input.window).Pointer() == address {
			windowOwned = len(input.window) == len(page) && input.windowStart == ordinal*ConversationManifestChunkBytes
		}
	}
	if capacity > 2*ConversationManifestChunkBytes || !windowOwned || input.unbounded {
		test.Fatal("numeric helper added hidden input storage or escaped bounded input", capacity, windowOwned, input.unbounded)
	}
}

func legacyNativeFloat64RetainedStep(test *testing.T, ctx context.Context, parser *legacyNativeFloat64, input *legacyNormalizationInput, source *legacySourceProofBuffer) (uint64, bool, bool, error) {
	test.Helper()
	position, work, loads, reads := parser.position, input.workBytes, input.loads, len(source.reads)
	bits, complete, rejected, err := parser.step(ctx, input)
	advance := parser.position - position
	if advance < 0 || advance > canonicalCheckBytes || input.workBytes < work+advance || input.workBytes > ConversationManifestChunkBytes {
		test.Fatal("numeric call escaped its raw-scan or shared action work bound", advance, work, input.workBytes)
	}
	if input.loads < loads || input.loads > 2 || len(source.reads)-reads != input.loads-loads {
		test.Fatal("numeric helper reset or exceeded shared authenticated loads", loads, input.loads, source.reads[reads:])
	}
	for _, read := range source.reads[reads:] {
		if read[0] < 0 || read[0] >= len(source.body) || read[0]%ConversationManifestChunkBytes != 0 || read[1] != min(ConversationManifestChunkBytes, len(source.body)-read[0]) {
			test.Fatal("numeric helper bypassed authenticated part reads", read)
		}
	}
	legacyNativeFloat64RetainedObserve(test, parser)
	legacyNativeFloat64RetainedPages(test, input, nil)
	if err != nil && (bits != 0 || complete || rejected) || err == nil && !complete && !rejected && bits != 0 {
		test.Fatal("numeric pending or canceled call exposed a usable outcome", bits, complete, rejected, err)
	}
	return bits, complete, rejected, err
}

func legacyNativeFloat64RetainedRun(test *testing.T, token string, expected uint64, wantRejected bool) int64 {
	test.Helper()
	buffer := &legacySourceProofBuffer{body: []byte(token)}
	source := buffer.input()
	borrowed := make([]byte, ConversationManifestChunkBytes)
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		part, err := buffer.Read(ctx, offset, length)
		if err != nil {
			return nil, err
		}
		copy(borrowed, part)
		return borrowed[:length], nil
	}
	parser, err := newLegacyNativeFloat64(0, len(token))
	if err != nil || parser == nil || len(buffer.reads) != 0 {
		test.Fatal("numeric constructor failed or eagerly read source", err, len(buffer.reads))
	}
	peak, _ := legacyNativeFloat64RetainedObserve(test, parser)
	allowance := 128 + 4*((len(token)+canonicalCheckBytes-1)/canonicalCheckBytes)
	for actionIndex := 0; actionIndex < allowance; actionIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		for callIndex := 0; callIndex < 16; callIndex++ {
			position, work := parser.position, input.workBytes
			bits, complete, rejected, err := legacyNativeFloat64RetainedStep(test, ctx, parser, input, buffer)
			clear(borrowed)
			legacyNativeFloat64RetainedPages(test, input, borrowed)
			for pageIndex, page := range input.pages {
				if page != nil && !bytes.Equal(page, buffer.body[input.ordinals[pageIndex]*ConversationManifestChunkBytes:input.ordinals[pageIndex]*ConversationManifestChunkBytes+len(page)]) {
					test.Fatal("numeric input lost its authenticated page after borrowed storage changed")
				}
			}
			live, _ := legacyNativeFloat64RetainedObserve(test, parser)
			peak = max(peak, live)
			if err != nil || input.failure != nil {
				cancel()
				test.Fatal("finite numeric resource fixture failed", err, input.failure)
			}
			if complete || rejected {
				cancel()
				if parser.position != len(token) || rejected != wantRejected || !wantRejected && (bits != expected || !complete) {
					test.Fatal("numeric resource fixture changed complete-span acceptance or bits", parser.position, len(token), bits, expected, complete, rejected)
				}
				legacyNativeFloat64RetainedObserve(test, parser)
				return peak
			}
			if parser.position == position && input.workBytes == work {
				break
			}
		}
		cancel()
	}
	test.Fatal("numeric resource fixture exhausted its finite progress allowance")
	return 0
}

func TestLegacyNativeFloat64RetainedFixedStateAcrossTokenLengths(test *testing.T) {
	const midpoint = "1.00000000000000011102230246251565404236316680908203125"
	for _, fixture := range []struct {
		name   string
		prefix string
		suffix string
		bits   uint64
	}{
		{name: "unit-exponent-zero-prefix", prefix: "1e", suffix: "0", bits: 0x3ff0000000000000},
		{name: "negative-zero-exponent-zero-prefix", prefix: "-0E-", suffix: "999999", bits: 0x8000000000000000},
		{name: "fallback-midpoint-exponent-zero-prefix", prefix: midpoint + "e+", suffix: "0", bits: 0x3ff0000000000000},
		{name: "fallback-distant-sticky", prefix: midpoint, suffix: "1e0", bits: 0x3ff0000000000001},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var firstPeak int64
			for _, length := range []int{canonicalCheckBytes - 1, canonicalCheckBytes, canonicalCheckBytes + 1, ConversationManifestChunkBytes - 1, ConversationManifestChunkBytes, ConversationManifestChunkBytes + 1, 2*ConversationManifestChunkBytes + 73} {
				test.Run(fmt.Sprint(length), func(test *testing.T) {
					token := fixture.prefix + strings.Repeat("0", length-len(fixture.prefix)-len(fixture.suffix)) + fixture.suffix
					peak := legacyNativeFloat64RetainedRun(test, token, fixture.bits, false)
					if firstPeak == 0 {
						firstPeak = peak
					} else if peak != firstPeak {
						test.Fatal("reachable numeric storage grew with token length", length, peak, firstPeak)
					}
				})
			}
		})
	}
}

func TestLegacyNativeFloat64RetainedOverflowState(test *testing.T) {
	for _, sign := range []string{"", "-"} {
		test.Run("sign="+sign, func(test *testing.T) {
			var firstPeak int64
			for _, length := range []int{canonicalCheckBytes + 1, ConversationManifestChunkBytes + 1, 2*ConversationManifestChunkBytes + 73} {
				token := sign + "1e" + strings.Repeat("0", length-len(sign)-8) + "999999"
				peak := legacyNativeFloat64RetainedRun(test, token, 0, true)
				if firstPeak == 0 {
					firstPeak = peak
				} else if peak != firstPeak {
					test.Fatal("overflow reducer retained token-sized diagnostic ownership", length, peak, firstPeak)
				}
			}
		})
	}
}

func TestLegacyNativeFloat64RetainedSharedWorkBudget(test *testing.T) {
	for _, used := range []int{0, ConversationManifestChunkBytes - 17, ConversationManifestChunkBytes - 1, ConversationManifestChunkBytes} {
		test.Run(fmt.Sprint(used), func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte("1e" + strings.Repeat("0", 2*ConversationManifestChunkBytes) + "0")}
			input := &legacyNormalizationInput{ctx: context.Background(), source: buffer.input(), workBytes: used}
			parser, err := newLegacyNativeFloat64(0, len(buffer.body))
			if err != nil {
				test.Fatal(err)
			}
			stalled := false
			for callIndex := 0; callIndex < 16; callIndex++ {
				position, work := parser.position, input.workBytes
				_, complete, rejected, err := legacyNativeFloat64RetainedStep(test, input.ctx, parser, input, buffer)
				if err != nil || complete || rejected || input.failure != nil {
					test.Fatal("shared budget yielded a failure or premature numeric outcome", complete, rejected, err, input.failure)
				}
				if parser.position == position && input.workBytes == work {
					stalled = true
					break
				}
			}
			if !stalled || parser.position > ConversationManifestChunkBytes-used || used == 0 && parser.position == 0 {
				test.Fatal("repeated helper calls did not share one finite action budget", used, parser.position, input.workBytes, stalled)
			}
			if used == ConversationManifestChunkBytes && (len(buffer.reads) != 0 || input.loads != 0) {
				test.Fatal("exhausted shared work budget still loaded source", buffer.reads, input.loads)
			}
		})
	}
}

func TestLegacyNativeFloat64RetainedSharedLoadBudget(test *testing.T) {
	from := 2 * ConversationManifestChunkBytes
	buffer := &legacySourceProofBuffer{body: []byte(strings.Repeat(" ", from) + "1e" + strings.Repeat("0", canonicalCheckBytes+1) + "0")}
	source := buffer.input()
	input := &legacyNormalizationInput{ctx: context.Background(), source: source}
	for ordinal := 0; ordinal < 2; ordinal++ {
		if _, err := input.part(ordinal); err != nil {
			test.Fatal("could not spend the enclosing action's source loads", err)
		}
	}
	parser, err := newLegacyNativeFloat64(from, len(buffer.body))
	if err != nil {
		test.Fatal(err)
	}
	_, before := legacyNativeFloat64RetainedObserve(test, parser)
	_, complete, rejected, err := legacyNativeFloat64RetainedStep(test, input.ctx, parser, input, buffer)
	_, after := legacyNativeFloat64RetainedObserve(test, parser)
	if err != nil || complete || rejected || parser.position != from || input.loads != 2 || len(buffer.reads) != 2 || input.failure != nil || !reflect.DeepEqual(before, after) {
		test.Fatal("numeric helper obtained two extra loads or changed state on an unavailable part", parser.position, input.workBytes, input.loads, err)
	}
	fresh := &legacyNormalizationInput{ctx: context.Background(), source: source}
	_, complete, rejected, err = legacyNativeFloat64RetainedStep(test, fresh.ctx, parser, fresh, buffer)
	if err != nil || complete || rejected || parser.position <= from || fresh.loads != 1 {
		test.Fatal("numeric helper could not resume under a fresh shared source budget", parser.position, fresh.loads, complete, rejected, err)
	}
}

func TestLegacyNativeFloat64RetainedPreCanceledAndCompleted(test *testing.T) {
	for _, token := range []string{"1", "-0"} {
		test.Run(token, func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(token)}
			source := buffer.input()
			parser, err := newLegacyNativeFloat64(0, len(token))
			if err != nil {
				test.Fatal(err)
			}
			for _, completed := range []bool{false, true} {
				if completed {
					ready := false
					for callIndex := 0; callIndex < 128; callIndex++ {
						input := &legacyNormalizationInput{ctx: context.Background(), source: source}
						bits, complete, rejected, err := legacyNativeFloat64RetainedStep(test, input.ctx, parser, input, buffer)
						if err != nil || rejected {
							test.Fatal("short accepted numeric fixture failed", rejected, err)
						}
						if complete {
							expected := uint64(0x3ff0000000000000)
							if token == "-0" {
								expected = 0x8000000000000000
							}
							if bits != expected {
								test.Fatal("completed numeric fixture lost its nonzero result bits", bits, expected)
							}
							ready = true
							break
						}
					}
					if !ready {
						test.Fatal("short numeric fixture never completed")
					}
				}
				liveBefore, before := legacyNativeFloat64RetainedObserve(test, parser)
				reads := len(buffer.reads)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				input := &legacyNormalizationInput{ctx: ctx, source: source}
				bits, complete, rejected, err := legacyNativeFloat64RetainedStep(test, ctx, parser, input, buffer)
				liveAfter, after := legacyNativeFloat64RetainedObserve(test, parser)
				if !errors.Is(err, context.Canceled) || bits != 0 || complete || rejected || len(buffer.reads) != reads || input.workBytes != 0 || input.loads != 0 || input.failure != nil || liveAfter != liveBefore || !reflect.DeepEqual(before, after) {
					test.Fatal("pre-canceled numeric call read, retained, mutated, or returned a completed value", completed, bits, complete, rejected, err)
				}
			}
		})
	}
}

type legacyNativeFloat64RetainedCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (ctx *legacyNativeFloat64RetainedCancelContext) Err() error {
	ctx.checks++
	if ctx.cancel != nil && ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestLegacyNativeFloat64RetainedCompletionCancellation(test *testing.T) {
	for _, token := range []string{"1", "-0", "1.00000000000000011102230246251565404236316680908203125"} {
		test.Run(token, func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(token)}
			source := buffer.input()
			parser, err := newLegacyNativeFloat64(0, len(token))
			if err != nil {
				test.Fatal(err)
			}
			completionCall, completionChecks := -1, 0
			for callIndex := 0; callIndex < 128; callIndex++ {
				ctx := &legacyNativeFloat64RetainedCancelContext{Context: context.Background()}
				input := &legacyNormalizationInput{ctx: ctx, source: source}
				_, complete, rejected, err := legacyNativeFloat64RetainedStep(test, ctx, parser, input, buffer)
				if err != nil || rejected {
					test.Fatal("completion cancellation baseline failed", rejected, err)
				}
				if complete {
					completionCall, completionChecks = callIndex, ctx.checks
					break
				}
			}
			if completionCall < 0 || completionChecks < 2 {
				test.Fatal("completion has no observable entry and final context checks", completionCall, completionChecks)
			}
			for checkpoint := 1; checkpoint <= completionChecks; checkpoint++ {
				test.Run(fmt.Sprint(checkpoint), func(test *testing.T) {
					replay := &legacySourceProofBuffer{body: []byte(token)}
					replaySource := replay.input()
					replayParser, err := newLegacyNativeFloat64(0, len(token))
					if err != nil {
						test.Fatal(err)
					}
					for callIndex := 0; callIndex < completionCall; callIndex++ {
						input := &legacyNormalizationInput{ctx: context.Background(), source: replaySource}
						_, complete, rejected, err := legacyNativeFloat64RetainedStep(test, input.ctx, replayParser, input, replay)
						if err != nil || complete || rejected {
							test.Fatal("completion checkpoint replay diverged", callIndex, complete, rejected, err)
						}
					}
					base, cancel := context.WithCancel(context.Background())
					defer cancel()
					ctx := &legacyNativeFloat64RetainedCancelContext{Context: base, cancel: cancel, cancelAt: checkpoint}
					input := &legacyNormalizationInput{ctx: ctx, source: replaySource}
					bits, complete, rejected, err := replayParser.step(ctx, input)
					legacyNativeFloat64RetainedObserve(test, replayParser)
					legacyNativeFloat64RetainedPages(test, input, nil)
					if !errors.Is(err, context.Canceled) || bits != 0 || complete || rejected || ctx.checks < checkpoint {
						test.Fatal("completion cancellation exposed provisional numeric bits", checkpoint, bits, complete, rejected, ctx.checks, err)
					}
				})
			}
		})
	}
}
