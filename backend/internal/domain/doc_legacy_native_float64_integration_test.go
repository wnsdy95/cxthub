package domain

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

const Float64IntegrationMidpoint = "1.00000000000000011102230246251565404236316680908203125"

func Float64IntegrationBits(test *testing.T, token string) uint64 {
	test.Helper()
	var value any
	if err := json.Unmarshal([]byte(token), &value); err != nil {
		test.Fatal("original encoding/json rejected the scalar numeric oracle", err)
	}
	number, ok := value.(float64)
	if !ok || math.IsInf(number, 0) || math.IsNaN(number) {
		test.Fatalf("original native numeric oracle has unusable type or value: %T", value)
	}
	return math.Float64bits(number)
}

func TestLegacyNativeFloat64IntegrationNativeBits(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
		bits  uint64
	}{
		{name: "positive-zero", token: "0", bits: 0},
		{name: "negative-zero", token: "-0.000E+300", bits: 0x8000000000000000},
		{name: "negative-underflow", token: "-2e-324", bits: 0x8000000000000000},
		{name: "minimum-subnormal", token: "5e-324", bits: 1},
		{name: "negative-minimum-subnormal", token: "-5e-324", bits: 0x8000000000000001},
		{name: "minimum-normal", token: "2.2250738585072014e-308", bits: 0x0010000000000000},
		{name: "inexact-fraction", token: "0.1", bits: 0x3fb999999999999a},
		{name: "integer-rounds-down", token: "9007199254740993", bits: 0x4340000000000000},
		{name: "integer-rounds-up", token: "9007199254740995", bits: 0x4340000000000002},
		{name: "even-midpoint", token: Float64IntegrationMidpoint, bits: 0x3ff0000000000000},
		{name: "above-midpoint", token: Float64IntegrationMidpoint + "1", bits: 0x3ff0000000000001},
		{name: "odd-midpoint", token: "1.00000000000000033306690738754696212708950042724609375", bits: 0x3ff0000000000002},
		{name: "maximum-finite", token: "1.7976931348623157e308", bits: 0x7fefffffffffffff},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if bits := Float64IntegrationBits(test, fixture.token); bits != fixture.bits {
				test.Fatalf("original numeric oracle changed bits: got=%016x want=%016x", bits, fixture.bits)
			}
		})
	}
}

func TestLegacyNativeFloat64IntegrationGiantSelectedValues(test *testing.T) {
	const digits = 2*ConversationManifestChunkBytes + 17
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "negative-zero-giant-exponent", token: "-0e+" + strings.Repeat("9", digits)},
		{name: "negative-underflow-giant-exponent", token: "-1e-" + strings.Repeat("9", digits)},
		{name: "leading-exponent-zeroes", token: "1e+" + strings.Repeat("0", digits) + "1"},
		{name: "midpoint-zero-tail", token: Float64IntegrationMidpoint + strings.Repeat("0", digits)},
		{name: "midpoint-distant-sticky", token: Float64IntegrationMidpoint + strings.Repeat("0", digits) + "1"},
		{name: "negative-midpoint-distant-sticky", token: "-" + Float64IntegrationMidpoint + strings.Repeat("0", digits) + "1"},
		{name: "huge-fractional-coefficient", token: "2." + strings.Repeat("2", digits) + "e+1"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if len(fixture.token) <= 2*ConversationManifestChunkBytes {
				test.Fatal("giant numeric fixture no longer crosses two authenticated source parts")
			}
		})
	}
}
