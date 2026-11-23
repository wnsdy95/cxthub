package domain

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

type legacyNativeFloat64ScanFixture struct {
	name  string
	token string
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
}
