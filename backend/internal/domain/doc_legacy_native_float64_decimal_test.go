package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeFloat64DecimalTestSeed(test *testing.T, coefficient string, point int, negative, truncated bool) *legacyNativeFloat64Decimal {
	test.Helper()
	var digits [800]byte
	if len(coefficient) > len(digits) {
		test.Fatal("decimal fixture exceeds the native seed capacity")
	}
	copy(digits[:], coefficient)
	decimal, err := newLegacyNativeFloat64Decimal(digits, len(coefficient), point, negative, truncated)
	if err != nil {
		test.Fatal(err)
	}
	if decimal.digits != digits || decimal.count != len(coefficient) || decimal.point != point || decimal.negative != negative || decimal.truncated != truncated {
		test.Fatal("constructor normalized or changed the original decimal seed")
	}
	return decimal
}

func legacyNativeFloat64DecimalTestDrain(test *testing.T, decimal *legacyNativeFloat64Decimal) (uint64, bool) {
	test.Helper()
	for transition := 0; transition < 128; transition++ {
		input := &legacyNormalizationInput{loads: 2, semanticBytes: 11, fields: 3, values: 5}
		bits, complete, rejected, err := decimal.step(context.Background(), input)
		if err != nil || input.workBytes <= 0 || input.workBytes > 64*1024 || input.loads != 2 || input.semanticBytes != 11 || input.fields != 3 || input.values != 5 || input.unbounded {
			test.Fatal("decimal transition failed or exceeded its shared accounting", transition, input.workBytes, err)
		}
		if complete {
			repeat := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes, loads: 2}
			repeatedBits, repeatedComplete, repeatedRejected, repeatedErr := decimal.step(context.Background(), repeat)
			if repeatedErr != nil || !repeatedComplete || repeatedBits != bits || repeatedRejected != rejected || repeat.workBytes != ConversationManifestChunkBytes {
				test.Fatal("terminal decimal result is not stable without further work", repeatedErr)
			}
			if rejected && bits != 0 {
				test.Fatal("semantic overflow published floating-point bits")
			}
			return bits, rejected
		}
		if bits != 0 || rejected {
			test.Fatal("pending decimal transition published a result")
		}
	}
	test.Fatal("finite decimal fixture did not complete within 128 transitions")
	return 0, false
}

func legacyNativeFloat64DecimalTestOracle(test *testing.T, coefficient string, point int, negative bool) (uint64, bool) {
	test.Helper()
	if coefficient == "" {
		coefficient = "0"
	}
	raw := coefficient + "e" + strconv.Itoa(point-len(coefficient))
	if negative {
		raw = "-" + raw
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		var typeError *json.UnmarshalTypeError
		if !errors.As(err, &typeError) {
			test.Fatal("finite decimal oracle fixture is not valid JSON", raw, err)
		}
		return 0, true
	}
	value, valid := decoded.(float64)
	if !valid {
		test.Fatal("native oracle did not produce float64", decoded)
	}
	return math.Float64bits(value), false
}

func TestLegacyNativeFloat64DecimalSeedOracle(test *testing.T) {
	fixtures := []struct {
		coefficient string
		point       int
	}{
		{"", 0},
		{"1", -331},
		{"1", -330},
		{"1", -324},
		{"2", -323},
		{"3", -323},
		{"5", -323},
		{"49406564584124654", -323},
		{"22250738585072014", -307},
		{"2225073858507201", -307},
		{"1", 0},
		{"10", 0},
		{"1", 1},
		{"1000", 4},
		{"1234567890123456789", 1},
		{"9007199254740993", 16},
		{"9007199254740995", 16},
		{"18446744073709551617", 20},
		{"100000000000000011102230246251565404236316680908203124", 1},
		{"100000000000000011102230246251565404236316680908203125", 1},
		{"100000000000000011102230246251565404236316680908203126", 1},
		{"100000000000000033306690738754696212708950042724609375", 1},
		{"17976931348623157", 309},
		{"17976931348623159", 309},
		{"1", 310},
		{"1", 311},
		{"1" + strings.Repeat("0", 799), 1},
		{strings.Repeat("9", 800), 1},
	}
	for index, fixture := range fixtures {
		for _, negative := range []bool{false, true} {
			test.Run(strconv.Itoa(index)+"/negative="+strconv.FormatBool(negative), func(test *testing.T) {
				decimal := legacyNativeFloat64DecimalTestSeed(test, fixture.coefficient, fixture.point, negative, false)
				bits, rejected := legacyNativeFloat64DecimalTestDrain(test, decimal)
				wantBits, wantRejected := legacyNativeFloat64DecimalTestOracle(test, fixture.coefficient, fixture.point, negative)
				if bits != wantBits || rejected != wantRejected {
					test.Fatalf("decimal seed differs from native oracle: bits=%016x rejected=%t want=%016x rejected=%t", bits, rejected, wantBits, wantRejected)
				}
			})
		}
	}
}

func TestLegacyNativeFloat64DecimalExactBoundaries(test *testing.T) {
	halfSubnormal := new(big.Int).Exp(big.NewInt(5), big.NewInt(1075), nil)
	normalBoundary := new(big.Int).Mul(new(big.Int).Set(halfSubnormal), new(big.Int).SetUint64(1<<53-1))
	overflowBoundary := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 1024), new(big.Int).Lsh(big.NewInt(1), 970))
	for _, boundary := range []struct {
		name        string
		coefficient *big.Int
		exponent    int
		bits        [3]uint64
		rejected    [3]bool
	}{
		{"half-minimum-subnormal", halfSubnormal, -1075, [3]uint64{0, 0, 1}, [3]bool{}},
		{"normal-subnormal", normalBoundary, -1075, [3]uint64{1<<52 - 1, 1 << 52, 1 << 52}, [3]bool{}},
		{"overflow", overflowBoundary, 0, [3]uint64{0x7fefffffffffffff, 0, 0}, [3]bool{false, true, true}},
	} {
		for offset := -1; offset <= 1; offset++ {
			coefficient := new(big.Int).Add(boundary.coefficient, big.NewInt(int64(offset))).String()
			point := len(coefficient) + boundary.exponent
			for _, negative := range []bool{false, true} {
				test.Run(boundary.name+"/"+strconv.Itoa(offset)+"/negative="+strconv.FormatBool(negative), func(test *testing.T) {
					decimal := legacyNativeFloat64DecimalTestSeed(test, coefficient, point, negative, false)
					bits, rejected := legacyNativeFloat64DecimalTestDrain(test, decimal)
					wantBits, wantRejected := boundary.bits[offset+1], boundary.rejected[offset+1]
					if negative && !wantRejected {
						wantBits |= uint64(1) << 63
					}
					oracleBits, oracleRejected := legacyNativeFloat64DecimalTestOracle(test, coefficient, point, negative)
					if bits != wantBits || rejected != wantRejected || bits != oracleBits || rejected != oracleRejected {
						test.Fatalf("exact boundary differs: bits=%016x rejected=%t want=%016x rejected=%t oracle=%016x rejected=%t", bits, rejected, wantBits, wantRejected, oracleBits, oracleRejected)
					}
				})
			}
		}
	}
}

func TestLegacyNativeFloat64DecimalTruncatedTie(test *testing.T) {
	tie := "100000000000000011102230246251565404236316680908203125"
	coefficient := tie + strings.Repeat("0", 800-len(tie))
	for _, negative := range []bool{false, true} {
		for _, truncated := range []bool{false, true} {
			decimal := legacyNativeFloat64DecimalTestSeed(test, coefficient, 1, negative, truncated)
			bits, rejected := legacyNativeFloat64DecimalTestDrain(test, decimal)
			wantBits := uint64(0x3ff0000000000000)
			if truncated {
				wantBits++
			}
			if negative {
				wantBits |= uint64(1) << 63
			}
			if bits != wantBits || rejected {
				test.Fatalf("retained zero tail or distant sticky changed tie rounding: bits=%016x want=%016x truncated=%t rejected=%t", bits, wantBits, truncated, rejected)
			}
		}
	}
}

func TestLegacyNativeFloat64DecimalHugePointsAndSeedValidation(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	for _, point := range []int{-maximum - 1, -331, 311, maximum} {
		for _, negative := range []bool{false, true} {
			for _, coefficient := range []string{"", "10"} {
				decimal := legacyNativeFloat64DecimalTestSeed(test, coefficient, point, negative, false)
				bits, rejected := legacyNativeFloat64DecimalTestDrain(test, decimal)
				wantRejected := coefficient != "" && point > 310
				wantBits := uint64(0)
				if negative && !wantRejected {
					wantBits = uint64(1) << 63
				}
				if bits != wantBits || rejected != wantRejected {
					test.Fatal("huge point overflowed control arithmetic or lost zero sign", point, bits, rejected)
				}
			}
		}
	}
	for _, fixture := range []struct {
		coefficient string
		count       int
	}{
		{"1", -1},
		{"1", 801},
		{"0", 1},
		{"x", 1},
		{"1/", 2},
	} {
		var digits [800]byte
		copy(digits[:], fixture.coefficient)
		decimal, err := newLegacyNativeFloat64Decimal(digits, fixture.count, 1, false, false)
		if decimal != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid decimal seed was admitted", fixture.count, fixture.coefficient, err)
		}
	}
}

func legacyNativeFloat64DecimalTestString(decimal *legacyNativeFloat64Decimal) string {
	if decimal.count == 0 {
		return "0"
	}
	digits := string(decimal.digits[:decimal.count])
	if decimal.point <= 0 {
		return "0." + strings.Repeat("0", -decimal.point) + digits
	}
	if decimal.point < decimal.count {
		return digits[:decimal.point] + "." + digits[decimal.point:]
	}
	return digits + strings.Repeat("0", decimal.point-decimal.count)
}

func TestLegacyNativeFloat64DecimalShiftOrder(test *testing.T) {
	for _, fixture := range []struct {
		coefficient string
		shift       int
		want        string
	}{
		{"", -100, "0"},
		{"", 100, "0"},
		{"1000", 0, "1000"},
		{"1", 100, "1267650600228229401496703205376"},
		{"1", -100, "0.0000000000000000000000000000007888609052210118054117285652827862296732064351090230047702789306640625"},
		{"12345678", 8, "3160493568"},
		{"12345678", -8, "48225.3046875"},
		{"195312", 9, "99999744"},
		{"1953125", 9, "1000000000"},
	} {
		decimal := legacyNativeFloat64DecimalTestSeed(test, fixture.coefficient, len(fixture.coefficient), true, false)
		decimal.phase, decimal.afterShift, decimal.shift = legacyNativeFloat64DecimalShift, legacyNativeFloat64DecimalRange, fixture.shift
		for transition := 0; decimal.phase == legacyNativeFloat64DecimalShift; transition++ {
			if transition >= 2 {
				test.Fatal("bounded source Shift did not yield after each 60-bit helper")
			}
			before := *decimal
			input := &legacyNormalizationInput{}
			bits, complete, rejected, err := decimal.step(context.Background(), input)
			if err != nil || complete || rejected || bits != 0 || !decimal.negative || input.workBytes > before.transitionWork() {
				test.Fatal("shift transition changed its result contract or accounting", err, input.workBytes)
			}
			if before.count > 0 && (before.shift > 60 || before.shift < -60) && decimal.shift != before.shift-60 && decimal.shift != before.shift+60 {
				test.Fatal("multi-helper Shift did not preserve the original 60-bit first operation", before.shift, decimal.shift)
			}
		}
		if got := legacyNativeFloat64DecimalTestString(decimal); got != fixture.want {
			test.Fatal("source Shift order changed decimal digits", fixture.coefficient, fixture.shift, got, fixture.want)
		}
	}
	for _, fixture := range []struct {
		coefficient string
		left        bool
		wantDigits  string
		wantPoint   int
		truncated   bool
	}{
		{strings.Repeat("9", 800), true, "1" + strings.Repeat("9", 799), 801, true},
		{"5" + strings.Repeat("0", 799), true, "1", 801, false},
		{strings.Repeat("9", 800), false, "4" + strings.Repeat("9", 799), 800, true},
		{"1" + strings.Repeat("0", 799), false, "5", 799, false},
	} {
		decimal := legacyNativeFloat64DecimalTestSeed(test, fixture.coefficient, 800, false, false)
		if fixture.left {
			decimal.shiftLeft(1)
		} else {
			decimal.shiftRight(1)
		}
		if string(decimal.digits[:decimal.count]) != fixture.wantDigits || decimal.point != fixture.wantPoint || decimal.truncated != fixture.truncated {
			test.Fatal("800-digit shift lost source truncation or trailing-zero rules", fixture.left, decimal.count, decimal.point, decimal.truncated)
		}
	}
	decimal := legacyNativeFloat64DecimalTestSeed(test, "1", -330, false, false)
	sawSplit := false
	for transition := 0; transition < 128; transition++ {
		before := *decimal
		bits, complete, rejected, err := decimal.step(context.Background(), &legacyNormalizationInput{})
		if err != nil || rejected || bits != 0 {
			test.Fatal("subnormal control-order fixture failed", bits, rejected, err)
		}
		if before.phase == legacyNativeFloat64DecimalSubnormal {
			if decimal.exponent != before.exponent || decimal.shiftDelta != -decimal.shift {
				test.Fatal("subnormal exponent advanced before its original Shift")
			}
		}
		if before.phase == legacyNativeFloat64DecimalShift && before.afterShift == legacyNativeFloat64DecimalRange {
			if decimal.shift != 0 {
				sawSplit = true
				if decimal.exponent != before.exponent || decimal.shiftDelta != before.shiftDelta {
					test.Fatal("split subnormal Shift advanced the exponent between original helpers")
				}
			} else if decimal.exponent != -1022 || decimal.shiftDelta != 0 {
				test.Fatal("completed subnormal Shift did not advance its exponent exactly once")
			}
		}
		if complete {
			if !sawSplit {
				test.Fatal("subnormal fixture did not exercise a multi-helper original Shift")
			}
			return
		}
	}
	test.Fatal("subnormal source control-order fixture did not terminate")
}

func TestLegacyNativeFloat64DecimalShiftCutoffs(test *testing.T) {
	for shift := 1; shift <= 60; shift++ {
		cutoff := new(big.Int).Exp(big.NewInt(5), big.NewInt(int64(shift)), nil)
		for offset := -1; offset <= 1; offset++ {
			coefficient := new(big.Int).Add(cutoff, big.NewInt(int64(offset)))
			decimal := legacyNativeFloat64DecimalTestSeed(test, coefficient.String(), len(coefficient.String()), false, true)
			decimal.shiftLeft(uint(shift))
			want := new(big.Int).Lsh(new(big.Int).Set(coefficient), uint(shift)).String()
			if legacyNativeFloat64DecimalTestString(decimal) != want || !decimal.truncated {
				test.Fatal("left-shift cutoff changed exact digits or cleared sticky state", shift, offset)
			}
		}
		decimal := legacyNativeFloat64DecimalTestSeed(test, "12345678", 8, false, true)
		decimal.shiftRight(uint(shift))
		coefficient := new(big.Int).Mul(big.NewInt(12345678), cutoff).String()
		point := len(coefficient) - shift
		coefficient = strings.TrimRight(coefficient, "0")
		want := legacyNativeFloat64DecimalTestSeed(test, coefficient, point, false, true)
		if legacyNativeFloat64DecimalTestString(decimal) != legacyNativeFloat64DecimalTestString(want) || !decimal.truncated {
			test.Fatal("right-shift digit propagation changed or cleared sticky state", shift)
		}
	}
}

func TestLegacyNativeFloat64DecimalRoundedInteger(test *testing.T) {
	for _, fixture := range []struct {
		coefficient string
		point       int
		truncated   bool
		want        uint64
	}{
		{"", 0, false, 0},
		{"25", 1, false, 2},
		{"25", 1, true, 3},
		{"35", 1, false, 4},
		{"2499", 1, false, 2},
		{"2501", 1, false, 3},
		{"5", 0, false, 0},
		{"5", 0, true, 1},
		{"50", 0, false, 1},
		{"99999999999999995", 16, false, 10000000000000000},
		{"1", 21, false, 0xffffffffffffffff},
		{"1", -1, true, 0},
	} {
		decimal := legacyNativeFloat64DecimalTestSeed(test, fixture.coefficient, fixture.point, false, fixture.truncated)
		integer, work := decimal.roundedInteger()
		if integer != fixture.want || work < 0 || work > 24 {
			test.Fatal("native RoundedInteger tie or overflow sentinel changed", fixture.coefficient, fixture.point, integer, fixture.want, work)
		}
	}
}

type legacyNativeFloat64DecimalTestContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *legacyNativeFloat64DecimalTestContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func TestLegacyNativeFloat64DecimalCancellationAndBudget(test *testing.T) {
	for _, fixture := range []struct {
		coefficient string
		point       int
	}{
		{"", 311},
		{"1", 311},
		{"1", -330},
		{strings.Repeat("9", 800), 309},
		{"17976931348623159", 309},
	} {
		decimal := legacyNativeFloat64DecimalTestSeed(test, fixture.coefficient, fixture.point, false, false)
		for transition := 0; transition < 128; transition++ {
			before := *decimal
			for _, cancelAt := range []int{1, 2} {
				ctx := &legacyNativeFloat64DecimalTestContext{Context: context.Background(), cancelAt: cancelAt}
				input := &legacyNormalizationInput{workBytes: 19, loads: 2}
				bits, complete, rejected, err := decimal.step(ctx, input)
				if !errors.Is(err, context.Canceled) || bits != 0 || complete || rejected || *decimal != before || ctx.checks != cancelAt {
					test.Fatal("cancellation exposed a result or changed the decimal checkpoint", transition, cancelAt, err)
				}
				if cancelAt == 1 && input.workBytes != 19 || input.loads != 2 || input.workBytes > ConversationManifestChunkBytes {
					test.Fatal("canceled decimal transition changed shared accounting", input.workBytes, input.loads)
				}
			}
			if decimal.phase == legacyNativeFloat64DecimalDone {
				break
			}
			required := decimal.transitionWork()
			if required <= 0 || required > 64*1024 {
				test.Fatal("decimal transition reservation is not bounded", required)
			}
			for _, remaining := range []int{0, 1, required - 1} {
				input := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - remaining, loads: 2}
				bits, complete, rejected, err := decimal.step(context.Background(), input)
				if err != nil || bits != 0 || complete || rejected || *decimal != before || input.workBytes != ConversationManifestChunkBytes-remaining || input.loads != 2 {
					test.Fatal("insufficient shared work mutated or ran a decimal transition", transition, remaining, err)
				}
			}
			input := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - required, loads: 2, semanticBytes: 11, fields: 3, values: 5}
			fresh := before
			freshInput := &legacyNormalizationInput{}
			freshBits, freshComplete, freshRejected, freshErr := fresh.step(context.Background(), freshInput)
			bits, complete, rejected, err := decimal.step(context.Background(), input)
			if err != nil || freshErr != nil || bits != freshBits || complete != freshComplete || rejected != freshRejected || *decimal != fresh || input.workBytes > ConversationManifestChunkBytes || input.workBytes != ConversationManifestChunkBytes-required+freshInput.workBytes || input.loads != 2 || input.semanticBytes != 11 || input.fields != 3 || input.values != 5 {
				test.Fatal("exactly reserved shared work changed a decimal transition", transition, err, freshErr)
			}
			if transition == 127 {
				test.Fatal("finite cancellation/budget schedule did not terminate")
			}
		}
	}
	decimal := legacyNativeFloat64DecimalTestSeed(test, "1", 1, false, false)
	before := *decimal
	failure := errors.New("authenticated source failed")
	input := &legacyNormalizationInput{failure: failure}
	bits, complete, rejected, err := decimal.step(context.Background(), input)
	if !errors.Is(err, failure) || bits != 0 || complete || rejected || *decimal != before || input.workBytes != 0 {
		test.Fatal("decimal conversion bypassed a prior private-source failure", err)
	}
}

func TestLegacyNativeFloat64DecimalRetained(test *testing.T) {
	decimal := legacyNativeFloat64DecimalTestSeed(test, "1000", 4, false, false)
	stateType := reflect.TypeOf(*decimal)
	for index := 0; index < stateType.NumField(); index++ {
		field := stateType.Field(index)
		switch field.Type.Kind() {
		case reflect.Int, reflect.Uint8, reflect.Uint64, reflect.Bool:
		case reflect.Array:
			if field.Type.Len() != 800 || field.Type.Elem().Kind() != reflect.Uint8 {
				test.Fatal("decimal workspace is not the native fixed digit array", field.Name)
			}
		default:
			test.Fatal("decimal checkpoint retains dynamic workspace", field.Name, field.Type)
		}
	}
	if unsafe.Sizeof(*decimal) > 1024 {
		test.Fatal("decimal checkpoint grew beyond its fixed-state allowance", unsafe.Sizeof(*decimal))
	}
	var estimate, expected docRangeHeaderHeapEstimate
	decimal.retained(&estimate)
	expected.allocation(1, int64(unsafe.Sizeof(*decimal)))
	if estimate != expected || estimate.err != nil {
		test.Fatal("decimal retained accounting omitted its fixed allocation", estimate, expected)
	}
	var absent *legacyNativeFloat64Decimal
	absent.retained(&estimate)
	if estimate != expected {
		test.Fatal("nil decimal retained nonexistent workspace")
	}
}
