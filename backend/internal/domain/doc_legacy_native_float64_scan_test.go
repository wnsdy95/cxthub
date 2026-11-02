package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const legacyNativeFloat64ScanQuantum = 64 << 10

type legacyNativeFloat64ScanFixture struct {
	name  string
	token string
}

func legacyNativeFloat64ScanStep(test *testing.T, parser *legacyNativeFloat64, buffer *legacySourceProofBuffer, input *legacyNormalizationInput) (uint64, bool, bool, error) {
	test.Helper()
	before, work, loads, reads := parser.position, input.workBytes, input.loads, len(buffer.reads)
	var minimum docRangeHeaderHeapEstimate
	minimum.allocation(1, int64(reflect.TypeOf(*parser).Size()))
	maximum := minimum
	maximum.allocation(1, int64(reflect.TypeFor[legacyNativeFloat64Decimal]().Size()))
	var previous docRangeHeaderHeapEstimate
	parser.retained(&previous)
	if previous.err != nil || previous.bytes < minimum.bytes || previous.bytes > maximum.bytes {
		test.Fatal("numeric state exceeds the fixed parser/converter shape", previous, minimum, maximum)
	}
	bits, complete, rejected, err := parser.step(input.ctx, input)
	progress := parser.position - before
	if progress < 0 || progress > legacyNativeFloat64ScanQuantum || progress > input.workBytes-work || input.workBytes-work > legacyNativeFloat64ScanQuantum ||
		input.workBytes < work || input.workBytes > ConversationManifestChunkBytes || input.loads < loads || input.loads > 2 || input.unbounded {
		test.Fatal("numeric scan reset or exceeded its raw/shared quantum", before, parser.position, work, input.workBytes, loads, input.loads, input.unbounded)
	}
	if input.failure != nil || len(buffer.reads)-reads != input.loads-loads {
		test.Fatal("numeric reads escaped the shared authenticated input", input.failure, buffer.reads[reads:], input.loads-loads)
	}
	for _, read := range buffer.reads[reads:] {
		if read[0] < 0 || read[0] >= len(buffer.body) || read[0]%ConversationManifestChunkBytes != 0 ||
			read[1] != min(ConversationManifestChunkBytes, len(buffer.body)-read[0]) {
			test.Fatal("numeric scan bypassed authenticated source-part reads", read)
		}
	}
	var retained docRangeHeaderHeapEstimate
	parser.retained(&retained)
	if retained.err != nil || retained.bytes < minimum.bytes || retained.bytes > maximum.bytes {
		test.Fatal("numeric retained state exceeds one fixed parser and fallback converter", retained, minimum, maximum)
	}
	for _, page := range input.pages {
		clear(page)
	}
	return bits, complete, rejected, err
}

func legacyNativeFloat64ScanParity(test *testing.T, token string, from int, cuts []int) {
	test.Helper()
	var expected any
	oracleErr := json.Unmarshal([]byte(token), &expected)
	var syntax *json.SyntaxError
	var semantic *json.UnmarshalTypeError
	malformed := errors.As(oracleErr, &syntax)
	if oracleErr != nil && !malformed && !errors.As(oracleErr, &semantic) {
		test.Fatal("unexpected encoding/json numeric oracle error", oracleErr)
	}
	var expectedBits uint64
	if oracleErr == nil {
		value, ok := expected.(float64)
		if !ok {
			test.Fatalf("numeric oracle has dynamic type %T, want float64", expected)
		}
		expectedBits = math.Float64bits(value)
	}
	buffer := &legacySourceProofBuffer{body: []byte(strings.Repeat(" ", from) + token + "]")}
	source := buffer.input()
	end := from + len(token)
	parser, err := newLegacyNativeFloat64(from, end)
	if len(buffer.reads) != 0 {
		test.Fatal("numeric constructor read raw source", buffer.reads)
	}
	if err != nil {
		if len(token) == 0 && malformed && errors.Is(err, ErrIntegrity) {
			return
		}
		test.Fatal("numeric constructor rejected a nonempty selected span", err)
	}
	if parser == nil || parser.position != from {
		test.Fatal("numeric constructor lost its raw source position")
	}
	cutIndex := 0
	maximumSteps := 4096 + len(token)/legacyNativeFloat64ScanQuantum + 2*len(cuts)
	for stepIndex := 0; stepIndex < maximumSteps; stepIndex++ {
		for cutIndex < len(cuts) && cuts[cutIndex] <= parser.position-from {
			cutIndex++
		}
		remaining := ConversationManifestChunkBytes
		if cutIndex < len(cuts) {
			shadowWork := 2 * int(reflect.TypeOf(*parser).Size())
			remaining = min(remaining, shadowWork+cuts[cutIndex]-(parser.position-from))
		}
		input := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes - remaining}
		bits, complete, rejected, stepErr := legacyNativeFloat64ScanStep(test, parser, buffer, input)
		if parser.position < from || parser.position > end {
			test.Fatal("numeric scan escaped its selected source span", parser.position, from, end)
		}
		if stepErr != nil {
			if !malformed || !errors.Is(stepErr, ErrIntegrity) || bits != 0 || complete || rejected {
				test.Fatal("numeric syntax/error outcome differs from encoding/json", stepErr, oracleErr, bits, complete, rejected)
			}
			return
		}
		if rejected {
			if oracleErr == nil || malformed || parser.position != end {
				test.Fatal("numeric semantic rejection ignored grammar, raw extent, or native acceptance", oracleErr, bits, parser.position, end)
			}
			return
		}
		if complete {
			if oracleErr != nil || parser.position != end || bits != expectedBits {
				test.Fatalf("numeric completion at %d/%d has bits %016x, want %016x; oracle error %v", parser.position, end, bits, expectedBits, oracleErr)
			}
			return
		}
		if bits != 0 {
			test.Fatal("pending numeric scan exposed provisional bits", bits)
		}
	}
	test.Fatal("numeric fixture did not terminate within its finite scan/conversion schedule", len(token), len(cuts), parser.position, end)
}

func legacyNativeFloat64ScanCuts(token string, from int) []int {
	candidates := []int{1, 2, 3, 18, 19, 20, 798, 799, 800, 801, 802, len(token) - 2, len(token) - 1, len(token)}
	significant := 0
	for position := 0; position < len(token); position++ {
		current := token[position]
		if current == 'e' || current == 'E' {
			break
		}
		if current < '0' || current > '9' || current == '0' && significant == 0 {
			continue
		}
		significant++
		if significant == 19 || significant == 800 {
			candidates = append(candidates, position-1, position, position+1, position+2)
		}
		if significant > 800 {
			break
		}
	}
	for _, position := range []int{strings.IndexByte(token, '.'), strings.IndexAny(token, "eE"), strings.LastIndexAny(token, "123456789")} {
		if position >= 0 {
			for offset := -1; offset <= 8; offset++ {
				candidates = append(candidates, position+offset)
			}
		}
	}
	for seam := legacyNativeFloat64ScanQuantum; seam < len(token); seam += legacyNativeFloat64ScanQuantum {
		candidates = append(candidates, seam-1, seam, seam+1)
	}
	for seam := ConversationManifestChunkBytes - from%ConversationManifestChunkBytes; seam < len(token); seam += ConversationManifestChunkBytes {
		candidates = append(candidates, seam-1, seam, seam+1)
	}
	sort.Ints(candidates)
	cuts := candidates[:0]
	for _, candidate := range candidates {
		if candidate > 0 && candidate <= len(token) && (len(cuts) == 0 || cuts[len(cuts)-1] != candidate) {
			cuts = append(cuts, candidate)
		}
	}
	return cuts
}

func TestLegacyNativeFloat64ScanGo12610Bits(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
		bits  uint64
	}{
		{name: "positive-zero", token: "0", bits: 0},
		{name: "negative-zero", token: "-0", bits: 0x8000000000000000},
		{name: "negative-fractional-zero", token: "-0.000", bits: 0x8000000000000000},
		{name: "negative-zero-positive-exponent", token: "-0e+999999", bits: 0x8000000000000000},
		{name: "negative-zero-negative-exponent", token: "-0.000e-999999", bits: 0x8000000000000000},
		{name: "fraction", token: "0.1", bits: 0x3fb999999999999a},
		{name: "integer-round-down", token: "9007199254740993", bits: 0x4340000000000000},
		{name: "integer-round-up", token: "9007199254740995", bits: 0x4340000000000002},
		{name: "half-even-down", token: "1.00000000000000011102230246251565404236316680908203125", bits: 0x3ff0000000000000},
		{name: "half-even-below", token: "1.00000000000000011102230246251565404236316680908203124", bits: 0x3ff0000000000000},
		{name: "half-even-above", token: "1.00000000000000011102230246251565404236316680908203126", bits: 0x3ff0000000000001},
		{name: "half-even-up", token: "1.00000000000000033306690738754696212708950042724609375", bits: 0x3ff0000000000002},
		{name: "smallest-normal", token: "2.2250738585072014e-308", bits: 0x0010000000000000},
		{name: "smallest-subnormal", token: "4.9406564584124654e-324", bits: 1},
		{name: "above-half-subnormal", token: "3e-324", bits: 1},
		{name: "below-half-subnormal", token: "2e-324", bits: 0},
		{name: "signed-underflow", token: "-2e-324", bits: 0x8000000000000000},
		{name: "maximum-finite", token: "1.7976931348623157e308", bits: 0x7fefffffffffffff},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var expected any
			if err := json.Unmarshal([]byte(fixture.token), &expected); err != nil {
				test.Fatal("encoding/json rejected a finite bit fixture", err)
			}
			value, ok := expected.(float64)
			if !ok || math.Float64bits(value) != fixture.bits {
				test.Fatal("installed encoding/json differs from the Go1.26.10 bit fixture", expected, fixture.bits)
			}
			legacyNativeFloat64ScanParity(test, fixture.token, 0, nil)
		})
	}
	for _, token := range []string{"1e309", "-1e309", "1.7976931348623159e308", "-1e999999"} {
		test.Run("overflow-"+token, func(test *testing.T) {
			legacyNativeFloat64ScanParity(test, token, 0, nil)
		})
	}
}

func TestLegacyNativeFloat64ScanSourcePathParity(test *testing.T) {
	halfway := "1.00000000000000011102230246251565404236316680908203125"
	fixtures := []legacyNativeFloat64ScanFixture{
		{name: "leading-fractional-zeros", token: "0." + strings.Repeat("0", 800) + "125e803"},
		{name: "leading-exponent-zeros", token: "1e+" + strings.Repeat("0", 801) + "1"},
		{name: "fallback-explicit-dot", token: "2." + strings.Repeat("2", 4000) + "e+1"},
	}
	for _, dot := range []string{"", ".0"} {
		test.Run("900-digit-native-1e-100-dot-"+strconv.Quote(dot), func(test *testing.T) {
			token := "1" + strings.Repeat("0", 899) + dot + "e-999"
			var expected any
			if err := json.Unmarshal([]byte(token), &expected); err != nil {
				test.Fatal("encoding/json rejected the compensated 900-digit fixture", err)
			}
			value, ok := expected.(float64)
			if !ok || math.Float64bits(value) != math.Float64bits(1e-100) {
				test.Fatal("native compensated integer no longer produces 1e-100", expected)
			}
			legacyNativeFloat64ScanParity(test, token, 0, legacyNativeFloat64ScanCuts(token, 0))
		})
	}
	for _, digits := range []int{18, 19, 20, 799, 800, 801} {
		text := strconv.Itoa(digits)
		coefficient := "1234567890123456789" + strings.Repeat("0", max(0, digits-19))
		coefficient = coefficient[:digits]
		fixtures = append(fixtures,
			legacyNativeFloat64ScanFixture{name: "coefficient-" + text, token: coefficient + "e-" + strconv.Itoa(digits-1)},
			legacyNativeFloat64ScanFixture{name: "fraction-" + text, token: "0." + coefficient},
			legacyNativeFloat64ScanFixture{name: "sticky-" + text, token: halfway + strings.Repeat("0", digits) + "1"},
		)
	}
	for _, digits := range []int{799, 800, 801, 900} {
		for _, ending := range []string{"124", "125", "126"} {
			midpoint := "1.00000000000000011102230246251565404236316680908203" + ending
			integer := strings.ReplaceAll(midpoint, ".", "")
			integer += strings.Repeat("0", digits-len(integer))
			exponent := "e-" + strconv.Itoa(digits-1)
			name := "compensated-" + strconv.Itoa(digits) + "-" + ending
			fixtures = append(fixtures,
				legacyNativeFloat64ScanFixture{name: name + "-no-dot", token: integer + exponent},
				legacyNativeFloat64ScanFixture{name: name + "-dot", token: integer + ".0" + exponent},
			)
		}
	}
	for _, exponent := range []string{"9999", "10000", "99999", "100000"} {
		magnitude, err := strconv.Atoi(exponent)
		if err != nil {
			test.Fatal(err)
		}
		for _, suffix := range []string{"", "0", "7"} {
			name := "freeze-" + exponent + "-suffix-" + suffix
			fixtures = append(fixtures,
				legacyNativeFloat64ScanFixture{name: name + "-positive", token: "0." + strings.Repeat("0", magnitude-1) + "1e" + exponent + suffix},
				legacyNativeFloat64ScanFixture{name: name + "-negative", token: "-0." + strings.Repeat("0", magnitude-1) + "1E+000" + exponent + suffix},
				legacyNativeFloat64ScanFixture{name: name + "-negative-exponent", token: "1e-000" + exponent + suffix},
			)
		}
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeFloat64ScanParity(test, fixture.token, 0, legacyNativeFloat64ScanCuts(fixture.token, 0))
		})
	}
}

func TestLegacyNativeFloat64ScanFiniteSplitSchedules(test *testing.T) {
	for _, token := range []string{"0", "-0", "-0.00e+009", "1.234567890123456789", "18446744073709551617", "1e999", "1.00000000000000011102230246251565404236316680908203125"} {
		test.Run("small-"+token, func(test *testing.T) {
			for split := 0; split <= len(token); split++ {
				test.Run(strconv.Itoa(split), func(test *testing.T) {
					legacyNativeFloat64ScanParity(test, token, 0, []int{split})
				})
			}
		})
	}
	halfway := "1.00000000000000011102230246251565404236316680908203125"
	for _, length := range []int{legacyNativeFloat64ScanQuantum - 1, legacyNativeFloat64ScanQuantum, legacyNativeFloat64ScanQuantum + 1, 2*ConversationManifestChunkBytes + 17} {
		for _, fixture := range []legacyNativeFloat64ScanFixture{
			{name: "halfway-zero-tail", token: halfway + strings.Repeat("0", length)},
			{name: "halfway-sticky-tail", token: halfway + strings.Repeat("0", length) + "1"},
			{name: "negative-zero-exponent", token: "-0e+" + strings.Repeat("9", length)},
			{name: "underflow-exponent", token: "1e-" + strings.Repeat("9", length)},
			{name: "leading-exponent-zeros", token: "1e+" + strings.Repeat("0", length) + "1"},
			{name: "explicit-dot", token: "2." + strings.Repeat("2", length) + "e+1"},
		} {
			test.Run(fixture.name+"-"+strconv.Itoa(length), func(test *testing.T) {
				test.Run("whole-quanta", func(test *testing.T) {
					legacyNativeFloat64ScanParity(test, fixture.token, 0, nil)
				})
				test.Run("finite-part-and-work-seams", func(test *testing.T) {
					from := ConversationManifestChunkBytes - 3
					legacyNativeFloat64ScanParity(test, fixture.token, from, legacyNativeFloat64ScanCuts(fixture.token, from))
				})
			})
		}
	}
}

func TestLegacyNativeFloat64ScanInvalidJSONNumbers(test *testing.T) {
	for index, token := range []string{"", "-", "+1", "00", "01", "-01", ".1", "-.1", "1.", "1.e1", "1e", "1e+", "1e-", "1e--1", "1e+-1", "0x1p0", "1_0", "1e1_0", "NaN", "Inf", "-Inf", "1 2", "1,", "1]", "1\x00", "1.2.3", "1ee1"} {
		test.Run("syntax-"+strconv.Itoa(index), func(test *testing.T) {
			legacyNativeFloat64ScanParity(test, token, 0, legacyNativeFloat64ScanCuts(token, 0))
		})
	}
	for _, fixture := range []legacyNativeFloat64ScanFixture{
		{name: "zero-exponent", token: "-0e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17)},
		{name: "overflow-exponent", token: "1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17)},
		{name: "underflow-exponent", token: "1e-" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17)},
		{name: "fraction", token: "0." + strings.Repeat("0", legacyNativeFloat64ScanQuantum+1) + "1"},
	} {
		for _, suffix := range []string{"x", "+", "\x00"} {
			test.Run(fixture.name+"-suffix-"+strconv.Quote(suffix), func(test *testing.T) {
				token := fixture.token + suffix
				legacyNativeFloat64ScanParity(test, token, 0, legacyNativeFloat64ScanCuts(token, 0))
			})
		}
	}
}

func TestLegacyNativeFloat64ScanSharedBudgets(test *testing.T) {
	test.Run("exhausted-work", func(test *testing.T) {
		buffer := &legacySourceProofBuffer{body: []byte("-0e99999")}
		source := buffer.input()
		parser, err := newLegacyNativeFloat64(0, len(buffer.body))
		if err != nil || parser == nil {
			test.Fatal("numeric constructor failed", err)
		}
		previous := *parser
		input := &legacyNormalizationInput{ctx: context.Background(), source: source, workBytes: ConversationManifestChunkBytes}
		bits, complete, rejected, err := legacyNativeFloat64ScanStep(test, parser, buffer, input)
		if err != nil || bits != 0 || complete || rejected || len(buffer.reads) != 0 || !reflect.DeepEqual(previous, *parser) {
			test.Fatal("exhausted owner work admitted a numeric transition", bits, complete, rejected, err)
		}
	})
	test.Run("loads-shared-with-framing", func(test *testing.T) {
		from := 2*ConversationManifestChunkBytes - 3
		token := "-0.0e+000"
		buffer := &legacySourceProofBuffer{body: []byte(strings.Repeat(" ", from) + token + "]")}
		source := buffer.input()
		parser, err := newLegacyNativeFloat64(from, from+len(token))
		if err != nil || parser == nil {
			test.Fatal("numeric constructor failed", err)
		}
		ctx := context.Background()
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		if _, err := input.current(0); err != nil {
			test.Fatal("framing could not consume its first source load", err)
		}
		bits, complete, rejected, err := legacyNativeFloat64ScanStep(test, parser, buffer, input)
		if err != nil || bits != 0 || complete || rejected || parser.position != from+3 || input.loads != 2 {
			test.Fatal("numeric scan bypassed the owner's already-spent load", parser.position, input.loads, bits, complete, rejected, err)
		}
		for stepIndex := 0; stepIndex < 4096; stepIndex++ {
			input = &legacyNormalizationInput{ctx: ctx, source: source}
			bits, complete, rejected, err = legacyNativeFloat64ScanStep(test, parser, buffer, input)
			if err != nil || rejected {
				test.Fatal("load-budget resume rejected negative zero", err, rejected)
			}
			if complete {
				if bits != 0x8000000000000000 || parser.position != from+len(token) {
					test.Fatal("load-budget resume lost native negative-zero bits or extent", bits, parser.position)
				}
				return
			}
			if bits != 0 {
				test.Fatal("load-budget resume exposed provisional bits", bits)
			}
		}
		test.Fatal("load-budget resume did not terminate")
	})
}

func legacyNativeFloat64ScanFixedType(test *testing.T, path string, state reflect.Type, allowConverter bool) int {
	test.Helper()
	converters := 0
	switch state.Kind() {
	case reflect.Struct:
		for index := 0; index < state.NumField(); index++ {
			field := state.Field(index)
			converters += legacyNativeFloat64ScanFixedType(test, path+"."+field.Name, field.Type, allowConverter)
		}
	case reflect.Array:
		converters = state.Len() * legacyNativeFloat64ScanFixedType(test, path+"[]", state.Elem(), allowConverter)
	case reflect.Pointer:
		if !allowConverter || state.Elem() != reflect.TypeFor[legacyNativeFloat64Decimal]() {
			test.Fatalf("numeric state %s retains an unbounded or nonnumeric pointer %s", path, state)
		}
		legacyNativeFloat64ScanFixedType(test, path+"*", state.Elem(), false)
		converters = 1
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
	default:
		test.Fatalf("numeric state %s retains %s instead of fixed arrays/scalars", path, state)
	}
	return converters
}

func TestLegacyNativeFloat64ScanFixedState(test *testing.T) {
	parser, err := newLegacyNativeFloat64(0, 1)
	if err != nil || parser == nil {
		test.Fatal("numeric constructor failed", err)
	}
	if converters := legacyNativeFloat64ScanFixedType(test, "legacyNativeFloat64", reflect.TypeOf(*parser), true); converters > 1 {
		test.Fatal("numeric scanner can retain more than one fixed fallback converter", converters)
	}
	var expected docRangeHeaderHeapEstimate
	parser.retained(&expected)
	for _, length := range []int{19, 800, 801, legacyNativeFloat64ScanQuantum + 1, 2*ConversationManifestChunkBytes + 17} {
		other, err := newLegacyNativeFloat64(7, 7+length)
		if err != nil || other == nil {
			test.Fatal("numeric constructor rejected a finite raw span", length, err)
		}
		var retained docRangeHeaderHeapEstimate
		other.retained(&retained)
		if retained.err != nil || retained.bytes != expected.bytes {
			test.Fatal("numeric constructor retained token-sized storage", length, expected, retained)
		}
	}
}
