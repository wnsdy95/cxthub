package domain

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestLegacyNativeAnyNumberGo12610GiantSemanticCharacterization(test *testing.T) {
	test.Log("Go 1.26.10 encoding/json oracle; giant tokens characterize semantics only, not conversion latency, heap, or work boundedness")
	const midpoint = "1.00000000000000011102230246251565404236316680908203125"
	fixtures := []struct {
		name   string
		token  string
		oracle string
	}{
		{name: "d06-long-exponent-saturates-before-compensation", token: "0." + strings.Repeat("0", 100000) + "1e100001", oracle: "0"},
		{name: "d06-negative-long-exponent-preserves-underflow-sign", token: "-0." + strings.Repeat("0", 100000) + "1e100001", oracle: "-0"},
		{name: "d06-short-mathematical-equivalent-is-not-native-equivalent", token: "1e0", oracle: "1"},
		{name: "unsaturated-exponent-compensates-leading-zeroes", token: "0." + strings.Repeat("0", 9998) + "1e9999", oracle: "1"},
		{name: "no-dot-900-digits-native-compensation", token: "1" + strings.Repeat("0", 899) + "e-899", oracle: "1e-100"},
		{name: "dot-after-900-digits-native-compensation", token: "1" + strings.Repeat("0", 899) + ".0e-899", oracle: "1e-100"},
	}
	midpointDigits := strings.ReplaceAll(midpoint, ".", "")
	for _, digits := range []int{801, 900, 1200} {
		coefficient := midpointDigits + strings.Repeat("0", digits-len(midpointDigits))
		exponent := "e-" + strconv.Itoa(digits-1)
		oracle := midpoint + "e" + strconv.Itoa(800-digits)
		fixtures = append(fixtures,
			struct{ name, token, oracle string }{name: "no-dot-compensated-" + strconv.Itoa(digits) + "-digits", token: coefficient + exponent, oracle: oracle},
			struct{ name, token, oracle string }{name: "dot-after-compensated-" + strconv.Itoa(digits) + "-digits", token: coefficient + ".0" + exponent, oracle: oracle},
		)
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			var oracle any
			if err := json.Unmarshal([]byte(fixture.oracle), &oracle); err != nil {
				test.Fatal("short Go numeric oracle was rejected", err)
			}
			_, ok := oracle.(float64)
			if !ok {
				test.Fatalf("Go numeric oracle returned %T, want float64", oracle)
			}
		})
	}
}
