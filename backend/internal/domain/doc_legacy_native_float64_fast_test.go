package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"testing"
)

func legacyNativeFloat64FastToken(mantissa uint64, exponent int, negative bool, tail string) string {
	token := strconv.FormatUint(mantissa, 10) + tail + "e" + strconv.Itoa(exponent-len(tail))
	if negative {
		token = "-" + token
	}
	return token
}

func legacyNativeFloat64FastOracle(test *testing.T, token string, actualBits uint64) {
	test.Helper()
	actual := math.Float64frombits(actualBits)
	if math.IsInf(actual, 0) || math.IsNaN(actual) {
		test.Fatalf("fast conversion accepted nonfinite bits %016x for %s", actualBits, token)
	}
	var native any
	if err := json.Unmarshal([]byte(token), &native); err != nil {
		test.Fatalf("native oracle rejected fast-accepted token %s: %v", token, err)
	}
	nativeNumber, ok := native.(float64)
	if !ok {
		test.Fatalf("native oracle returned %T for %s, want float64", native, token)
	}
	if expectedBits := math.Float64bits(nativeNumber); actualBits != expectedBits {
		test.Fatalf("fast conversion bits for %s = %016x, want native %016x", token, actualBits, expectedBits)
	}
}

func TestLegacyNativeFloat64FastExactBoundaries(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		mantissa uint64
		exponent int
		accepted bool
	}{
		{name: "zero-integer", mantissa: 0, exponent: 0, accepted: true},
		{name: "zero-positive-limit", mantissa: 0, exponent: 37, accepted: true},
		{name: "zero-negative-limit", mantissa: 0, exponent: -22, accepted: true},
		{name: "zero-positive-outside", mantissa: 0, exponent: 38, accepted: false},
		{name: "zero-negative-outside", mantissa: 0, exponent: -23, accepted: false},
		{name: "mantissa-below-limit", mantissa: 1<<52 - 1, exponent: 0, accepted: true},
		{name: "mantissa-at-limit", mantissa: 1 << 52, exponent: 0, accepted: false},
		{name: "mantissa-above-limit", mantissa: 1<<52 + 1, exponent: 0, accepted: false},
		{name: "negative-exponent-limit", mantissa: 1, exponent: -22, accepted: true},
		{name: "negative-exponent-outside", mantissa: 1, exponent: -23, accepted: false},
		{name: "single-multiply-limit", mantissa: 1, exponent: 22, accepted: true},
		{name: "two-multiply-start", mantissa: 1, exponent: 23, accepted: true},
		{name: "positive-exponent-limit", mantissa: 1, exponent: 37, accepted: true},
		{name: "positive-exponent-outside", mantissa: 1, exponent: 38, accepted: false},
		{name: "scaled-mantissa-limit", mantissa: 1000000000000000, exponent: 22, accepted: true},
		{name: "scaled-mantissa-outside", mantissa: 1000000000000001, exponent: 22, accepted: false},
		{name: "first-multiply-at-limit", mantissa: 100000000000000, exponent: 23, accepted: true},
		{name: "first-multiply-above-limit", mantissa: 100000000000001, exponent: 23, accepted: false},
		{name: "largest-first-multiply-above-limit", mantissa: 10, exponent: 37, accepted: false},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, negative := range []bool{false, true} {
				actualBits, ok := legacyNativeFloat64Exact(fixture.mantissa, fixture.exponent, negative)
				if ok != fixture.accepted {
					test.Fatalf("exact acceptance for mantissa=%d exponent=%d negative=%t = %t, want %t", fixture.mantissa, fixture.exponent, negative, ok, fixture.accepted)
				}
				if ok {
					legacyNativeFloat64FastOracle(test, legacyNativeFloat64FastToken(fixture.mantissa, fixture.exponent, negative, ""), actualBits)
				}
			}
		})
	}
}

func TestLegacyNativeFloat64FastSignedZero(test *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	minimumInt := -maximumInt - 1
	for _, exponent := range []int{minimumInt, -100000, -349, -348, -23, -22, -1, 0, 1, 22, 37, 38, 347, 348, 100000, maximumInt} {
		for _, negative := range []bool{false, true} {
			actualBits, ok := legacyNativeFloat64Fast(0, exponent, negative, false)
			expectedBits := uint64(0)
			if negative {
				expectedBits = 0x8000000000000000
			}
			if !ok || actualBits != expectedBits {
				test.Fatalf("zero at exponent=%d negative=%t = (%016x, %t), want (%016x, true)", exponent, negative, actualBits, ok, expectedBits)
			}
			legacyNativeFloat64FastOracle(test, legacyNativeFloat64FastToken(0, exponent, negative, ""), actualBits)
		}
	}
}

func TestLegacyNativeFloat64FastFiniteParity(test *testing.T) {
	mantissas := []uint64{
		0, 1, 2, 3, 5, 7, 9, 10, 11, 123, 1234567,
		1000000000000000, 1000000000000001,
		1<<52 - 1, 1 << 52, 1<<52 + 1,
		1<<53 - 1, 1 << 53, 1<<53 + 1, 9007199254740995,
		17976931348623157, 17976931348623159,
		2225073858507201, 22250738585072014,
		1000000000000000111, 1234567890123456789,
		9999999999999999998, 9999999999999999999,
	}
	accepted := 0
	fallbacks := 0
	for exponent := -348; exponent <= 347; exponent++ {
		for _, mantissa := range mantissas {
			for _, negative := range []bool{false, true} {
				actualBits, ok := legacyNativeFloat64Fast(mantissa, exponent, negative, false)
				if !ok {
					fallbacks++
					continue
				}
				accepted++
				legacyNativeFloat64FastOracle(test, legacyNativeFloat64FastToken(mantissa, exponent, negative, ""), actualBits)
			}
		}
	}
	if accepted == 0 || fallbacks == 0 {
		test.Fatalf("finite matrix did not cover both paths: accepted=%d fallbacks=%d", accepted, fallbacks)
	}
}

func TestLegacyNativeFloat64FastTruncatedParity(test *testing.T) {
	mantissas := []uint64{
		1000000000000000000, 1000000000000000001,
		1000000000000000111, 1000000000000000333,
		1234567890123456789, 1797693134862315708,
		2225073858507201383, 9999999999999999998, 9999999999999999999,
	}
	exponents := []int{-348, -343, -342, -326, -325, -324, -323, -309, -308, -307, -100, -42, -41, -40, -23, -22, -21, -20, -19, -18, -17, -16, -1, 0, 1, 22, 37, 100, 289, 290, 291, 307, 308, 347}
	tails := []string{"1", "5", "9", "00000000000000001", "99999999999999999"}
	accepted := 0
	fallbacks := 0
	for _, exponent := range exponents {
		for _, mantissa := range mantissas {
			for _, negative := range []bool{false, true} {
				actualBits, ok := legacyNativeFloat64Fast(mantissa, exponent, negative, true)
				if !ok {
					fallbacks++
					continue
				}
				accepted++
				for _, tail := range tails {
					legacyNativeFloat64FastOracle(test, legacyNativeFloat64FastToken(mantissa, exponent, negative, tail), actualBits)
				}
			}
		}
	}
	if accepted == 0 || fallbacks == 0 {
		test.Fatalf("truncated matrix did not cover both paths: accepted=%d fallbacks=%d", accepted, fallbacks)
	}
}

func TestLegacyNativeFloat64FastTruncatedUpperBound(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		mantissa uint64
		exponent int
		accepted bool
	}{
		{name: "stable-interval", mantissa: 1234567890123456789, exponent: -18, accepted: true},
		{name: "even-midpoint-crossing", mantissa: 1000000000000000111, exponent: -18, accepted: false},
		{name: "odd-midpoint-crossing", mantissa: 1000000000000000333, exponent: -18, accepted: false},
		{name: "truncated-skips-exact", mantissa: 1, exponent: 0, accepted: false},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, negative := range []bool{false, true} {
				lowerBits, lowerOK := legacyNativeFloat64EiselLemire(fixture.mantissa, fixture.exponent, negative)
				upperBits, upperOK := legacyNativeFloat64EiselLemire(fixture.mantissa+1, fixture.exponent, negative)
				if !lowerOK || !upperOK {
					test.Fatal("upper-bound fixture did not reach the native interval comparison")
				}
				if equal := math.Float64frombits(lowerBits) == math.Float64frombits(upperBits); equal != fixture.accepted {
					test.Fatalf("interval endpoint equality = %t, want %t", equal, fixture.accepted)
				}
				actualBits, ok := legacyNativeFloat64Fast(fixture.mantissa, fixture.exponent, negative, true)
				if ok != fixture.accepted {
					test.Fatalf("truncated acceptance = %t, want %t", ok, fixture.accepted)
				}
				if ok {
					for _, tail := range []string{"1", "9", "00000000000000001", "99999999999999999"} {
						legacyNativeFloat64FastOracle(test, legacyNativeFloat64FastToken(fixture.mantissa, fixture.exponent, negative, tail), actualBits)
					}
				}
			}
		})
	}
}

func TestLegacyNativeFloat64FastFallbacks(test *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	minimumInt := -maximumInt - 1
	for _, fixture := range []struct {
		name     string
		mantissa uint64
		exponent int
		accepted bool
		finite   bool
	}{
		{name: "halfway-ambiguity", mantissa: 9007199254740993, exponent: 0, accepted: false, finite: true},
		{name: "odd-halfway-rounds-up", mantissa: 9007199254740995, exponent: 0, accepted: true, finite: true},
		{name: "underflow", mantissa: 1, exponent: -324, accepted: false, finite: true},
		{name: "subnormal", mantissa: 5, exponent: -324, accepted: false, finite: true},
		{name: "largest-subnormal-neighbor", mantissa: 2225073858507201, exponent: -323, accepted: false, finite: true},
		{name: "smallest-normal", mantissa: 22250738585072014, exponent: -324, accepted: true, finite: true},
		{name: "large-finite", mantissa: 1, exponent: 308, accepted: true, finite: true},
		{name: "maximum-finite", mantissa: 17976931348623157, exponent: 292, accepted: true, finite: true},
		{name: "overflow-boundary", mantissa: 17976931348623159, exponent: 292, accepted: false, finite: false},
		{name: "overflow", mantissa: 1, exponent: 309, accepted: false, finite: false},
		{name: "power-below-table", mantissa: 1, exponent: -349, accepted: false, finite: true},
		{name: "power-above-table", mantissa: 1, exponent: 348, accepted: false, finite: false},
		{name: "minimum-exponent", mantissa: 1, exponent: minimumInt, accepted: false, finite: true},
		{name: "maximum-exponent", mantissa: 1, exponent: maximumInt, accepted: false, finite: false},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, negative := range []bool{false, true} {
				actualBits, ok := legacyNativeFloat64Fast(fixture.mantissa, fixture.exponent, negative, false)
				if ok != fixture.accepted {
					test.Fatalf("fast acceptance = %t, want %t", ok, fixture.accepted)
				}
				token := legacyNativeFloat64FastToken(fixture.mantissa, fixture.exponent, negative, "")
				var native any
				if err := json.Unmarshal([]byte(token), &native); (err == nil) != fixture.finite {
					test.Fatalf("native finite acceptance = %t, want %t: %v", err == nil, fixture.finite, err)
				}
				if ok {
					legacyNativeFloat64FastOracle(test, token, actualBits)
				}
			}
		})
	}
}

func TestLegacyNativeFloat64FastPowers(test *testing.T) {
	if legacyNativeFloat64PowerMin != -348 || legacyNativeFloat64PowerMax != 347 || len(legacyNativeFloat64Powers) != 696 {
		test.Fatal("native power table extent changed")
	}
	digest := sha256.New()
	var wordBytes [16]byte
	for exponent := -348; exponent <= 347; exponent++ {
		power, binaryExponent, ok := legacyNativeFloat64Power(exponent)
		if !ok || power.high>>63 != 1 {
			test.Fatalf("power %d is missing or unnormalized", exponent)
		}
		expectedExponent := 1 + int(math.Floor(float64(exponent)*math.Log2(10)))
		if binaryExponent != expectedExponent {
			test.Fatalf("power %d binary exponent = %d, want %d", exponent, binaryExponent, expectedExponent)
		}
		binary.BigEndian.PutUint64(wordBytes[:8], power.high)
		binary.BigEndian.PutUint64(wordBytes[8:], power.low)
		digest.Write(wordBytes[:])
	}
	if actual := hex.EncodeToString(digest.Sum(nil)); actual != "6fe5dd4274439ffa8a348bfe6ca7c44dda88bef40ac13cf040d19dcd584cc54a" {
		test.Fatalf("installed Go 1.26.10 high/low power words changed: sha256=%s", actual)
	}
	for _, fixture := range []struct {
		exponent       int
		power          legacyNativeFloat64Power128
		binaryExponent int
	}{
		{exponent: -348, power: legacyNativeFloat64Power128{0xfa8fd5a0081c0288, 0x1732c869cd60e453}, binaryExponent: -1156},
		{exponent: 0, power: legacyNativeFloat64Power128{0x8000000000000000, 0}, binaryExponent: 1},
		{exponent: 347, power: legacyNativeFloat64Power128{0xd13eb46469447567, 0x4b7195f2d2d1a9fb}, binaryExponent: 1153},
	} {
		power, binaryExponent, ok := legacyNativeFloat64Power(fixture.exponent)
		if !ok || power != fixture.power || binaryExponent != fixture.binaryExponent {
			test.Fatalf("native power anchor %d changed", fixture.exponent)
		}
	}
	maximumInt := int(^uint(0) >> 1)
	for _, exponent := range []int{-maximumInt - 1, -349, 348, maximumInt} {
		power, binaryExponent, ok := legacyNativeFloat64Power(exponent)
		if ok || power != (legacyNativeFloat64Power128{}) || binaryExponent != 0 {
			test.Fatalf("out-of-range power %d was not rejected", exponent)
		}
	}
	if len(legacyNativeFloat64ExactPowers) != 23 {
		test.Fatal("native exact power table extent changed")
	}
	for exponent, power := range legacyNativeFloat64ExactPowers {
		legacyNativeFloat64FastOracle(test, legacyNativeFloat64FastToken(1, exponent, false, ""), math.Float64bits(power))
	}
}
