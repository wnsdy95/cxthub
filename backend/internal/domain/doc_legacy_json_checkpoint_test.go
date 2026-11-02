package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type legacyJSONCheckpointOracleStep struct {
	consumed bool
	complete bool
	err      error
}

func legacyJSONCheckpointOracleFinish(scanner *legacyJSONValueScanner, trace []legacyJSONCheckpointOracleStep) []legacyJSONCheckpointOracleStep {
	complete, err := scanner.finish()
	trace = append(trace, legacyJSONCheckpointOracleStep{complete: complete, err: err})
	for _, current := range []byte{'0', ']', 'x'} {
		consumed, complete, err := scanner.add(current)
		trace = append(trace, legacyJSONCheckpointOracleStep{consumed: consumed, complete: complete, err: err})
	}
	complete, err = scanner.finish()
	return append(trace, legacyJSONCheckpointOracleStep{complete: complete, err: err})
}

func legacyJSONCheckpointOracleOriginal(test *testing.T, raw []byte, parents int, snapshot bool) ([]byte, []legacyJSONCheckpointOracleStep) {
	test.Helper()
	var scanner legacyJSONValueScanner
	if err := scanner.reset(parents); err != nil {
		test.Fatalf("parents %d: reset: %v", parents, err)
	}
	trace := make([]legacyJSONCheckpointOracleStep, 0, len(raw)+5)
	for _, current := range raw {
		consumed, complete, err := scanner.add(current)
		trace = append(trace, legacyJSONCheckpointOracleStep{consumed: consumed, complete: complete, err: err})
	}
	var checkpoint []byte
	if snapshot {
		var err error
		checkpoint, err = scanner.checkpoint(context.Background())
		if err != nil {
			test.Fatalf("parents %d, prefix length %d: checkpoint: %v", parents, len(raw), err)
		}
		if len(checkpoint) < 10 || len(checkpoint) > 10010 {
			test.Fatalf("parents %d, prefix length %d: checkpoint length = %d", parents, len(raw), len(checkpoint))
		}
	}
	return checkpoint, legacyJSONCheckpointOracleFinish(&scanner, trace)
}

func legacyJSONCheckpointOracleResume(test *testing.T, checkpoint, suffix []byte) []legacyJSONCheckpointOracleStep {
	test.Helper()
	detached := append([]byte(nil), checkpoint...)
	scanner, err := restoreLegacyJSONValueScanner(context.Background(), detached)
	if err != nil {
		test.Fatalf("restore: %v", err)
	}
	for index := range detached {
		detached[index] ^= 0xff
	}
	trace := make([]legacyJSONCheckpointOracleStep, 0, len(suffix)+5)
	for _, current := range suffix {
		consumed, complete, err := scanner.add(current)
		trace = append(trace, legacyJSONCheckpointOracleStep{consumed: consumed, complete: complete, err: err})
	}
	return legacyJSONCheckpointOracleFinish(&scanner, trace)
}

func legacyJSONCheckpointOracleCompare(test *testing.T, parents, split int, stage string, actual, expected []legacyJSONCheckpointOracleStep) {
	test.Helper()
	if len(actual) != len(expected) {
		test.Fatalf("parents %d, split %d, %s: trace length = %d, want %d", parents, split, stage, len(actual), len(expected))
	}
	for offset, wanted := range expected {
		observed := actual[offset]
		if observed != wanted {
			test.Fatalf("parents %d, split %d, %s step %d: (consumed, complete, error) = (%v, %v, %v), want (%v, %v, %v)", parents, split, stage, offset, observed.consumed, observed.complete, observed.err, wanted.consumed, wanted.complete, wanted.err)
		}
	}
}

func legacyJSONCheckpointOracleNativeValid(raw []byte, parents int) bool {
	if !json.Valid(raw) {
		return false
	}
	if parents == 0 {
		return true
	}
	wrapped := strings.Repeat("[", parents) + string(raw) + strings.Repeat("]", parents)
	return json.Valid([]byte(wrapped))
}

func legacyJSONCheckpointOracleCuts(test *testing.T, raw []byte, parents int, cuts []int) []legacyJSONCheckpointOracleStep {
	test.Helper()
	_, original := legacyJSONCheckpointOracleOriginal(test, raw, parents, false)
	expectedValid := legacyJSONCheckpointOracleNativeValid(raw, parents)
	seen := make(map[int]bool, len(cuts))
	for _, split := range cuts {
		if split < 0 || split > len(raw) {
			test.Fatalf("invalid fixture split %d for %d bytes", split, len(raw))
		}
		if seen[split] {
			continue
		}
		seen[split] = true
		if actual := legacyJSONValueScanValid(raw, parents, split); actual != expectedValid {
			test.Fatalf("parents %d, split %d: original validity = %v, want native JSON validity %v", parents, split, actual, expectedValid)
		}
		prefix := raw[:split]
		if actual, expected := legacyJSONValueScanValid(prefix, parents, split), legacyJSONCheckpointOracleNativeValid(prefix, parents); actual != expected {
			test.Fatalf("parents %d, split %d: original prefix validity = %v, want native JSON validity %v", parents, split, actual, expected)
		}
		checkpoint, prefixTrace := legacyJSONCheckpointOracleOriginal(test, prefix, parents, true)
		legacyJSONCheckpointOracleCompare(test, parents, split, "prefix", prefixTrace[:split], original[:split])
		atEOF := legacyJSONCheckpointOracleResume(test, checkpoint, nil)
		legacyJSONCheckpointOracleCompare(test, parents, split, "prefix EOF and terminal calls", atEOF, prefixTrace[split:])
		continued := legacyJSONCheckpointOracleResume(test, checkpoint, raw[split:])
		legacyJSONCheckpointOracleCompare(test, parents, split, "suffix, EOF and terminal calls", continued, original[split:])
	}
	return original
}

func legacyJSONCheckpointOracleEverySplit(test *testing.T, raw []byte, parents int) []legacyJSONCheckpointOracleStep {
	test.Helper()
	cuts := make([]int, len(raw)+1)
	for split := range cuts {
		cuts[split] = split
	}
	return legacyJSONCheckpointOracleCuts(test, raw, parents, cuts)
}

func TestLegacyJSONValueScannerCheckpointOracleEverySplit(test *testing.T) {
	cases := []struct {
		name  string
		raw   string
		valid bool
	}{
		{"null", `null`, true},
		{"true", `true`, true},
		{"false", `false`, true},
		{"zero", `0`, true},
		{"negative_zero", `-0`, true},
		{"integer", `123456789012345678901234567890`, true},
		{"fraction", `-123.0045`, true},
		{"exponent", `0.01E-002`, true},
		{"positive_exponent", `1e+9`, true},
		{"overflow_exponent", `1e999`, true},
		{"unbounded_exponent", `-1E+9999999999999999999999999999999`, true},
		{"underflow_exponent", `1e-9999999999999999999999999999999`, true},
		{"empty_string", `""`, true},
		{"escapes", `"a\"\\\/\b\f\n\r\t\u0000\u00aB"`, true},
		{"surrogates", `"\uD800\udfff\ud800x\udfff"`, true},
		{"unicode", "\"\ud55c\uae00 😀\"", true},
		{"invalid_utf8", "\"\xff\xc0\x80\xed\xa0\x80\"", true},
		{"invalid_utf8_key", "{\"\xff\":\"\xfe\"}", true},
		{"empty_object", `{}`, true},
		{"empty_array", `[]`, true},
		{"nested_empty", `[[[],{}],{"a":[],"b":{}}]`, true},
		{"duplicate_keys", `{"a":1,"a":true,"a":null}`, true},
		{"nested_numbers", `{"a":[1,2e999],"b":0}`, true},
		{"whitespace", " \t\r\n{ \"a\" : [ -1 , true , null , \"x\" ] } \n", true},
		{"number_whitespace", " \t-0.1e+2\r\n", true},
		{"escaped_unicode_keys", "{\"\\u0061\\\"\\\\\":{\"\\ud83d\\ude00\":[true,null,{\"\ud55c\uae00\\t\":[]},-0.5e+2]},\"\\u0000\":false}", true},
		{"array_value_ancestors", `[0,[1,{"\u0062":[null,{"\u0063":true}]}]]`, true},
		{"empty", ``, false},
		{"only_space", " \t\r\n", false},
		{"partial_null", `nul`, false},
		{"partial_true", `tru`, false},
		{"partial_false", `fals`, false},
		{"bad_null", `nulk`, false},
		{"bad_true", `trXe`, false},
		{"bad_false", `falSe`, false},
		{"capital_literal", `True`, false},
		{"nan", `NaN`, false},
		{"infinity", `-Infinity`, false},
		{"sign_only", `-`, false},
		{"plus", `+1`, false},
		{"double_sign", `--1`, false},
		{"leading_zero", `01`, false},
		{"negative_leading_zero", `-01`, false},
		{"double_zero", `00`, false},
		{"leading_point", `.1`, false},
		{"negative_leading_point", `-.1`, false},
		{"trailing_point", `1.`, false},
		{"point_exponent", `1.e2`, false},
		{"double_point", `1.2.3`, false},
		{"missing_exponent", `1e`, false},
		{"missing_positive_exponent", `1E+`, false},
		{"missing_negative_exponent", `1e-`, false},
		{"double_exponent_sign", `1e++2`, false},
		{"double_exponent", `1e2e3`, false},
		{"hex_number", `0x1`, false},
		{"number_literal", `1true`, false},
		{"number_colon", `1:`, false},
		{"number_open_array", `1[`, false},
		{"number_open_object", `1{`, false},
		{"number_quote", `1"`, false},
		{"open_string", `"text`, false},
		{"open_escape", `"text\`, false},
		{"open_unicode", `"\u123`, false},
		{"short_unicode", `"\u123"`, false},
		{"bad_unicode", `"\u12x4"`, false},
		{"bad_escape", `"\v"`, false},
		{"raw_newline", "\"a\nb\"", false},
		{"open_object", `{`, false},
		{"open_array", `[`, false},
		{"object_unquoted_key", `{a:1}`, false},
		{"object_numeric_key", `{1:1}`, false},
		{"object_missing_colon", `{"a" 1}`, false},
		{"object_double_colon", `{"a"::1}`, false},
		{"object_missing_value", `{"a":}`, false},
		{"object_missing_comma", `{"a":1 "b":2}`, false},
		{"object_leading_comma", `{,"a":1}`, false},
		{"object_double_comma", `{"a":1,,"b":2}`, false},
		{"object_trailing_comma", `{"a":1,}`, false},
		{"object_truncated_value", `{"a":nul}`, false},
		{"object_duplicate_missing_value", `{"a":1,"a":}`, false},
		{"object_bad_close", `{"a":1]`, false},
		{"array_leading_comma", `[,1]`, false},
		{"array_double_comma", `[1,,2]`, false},
		{"array_trailing_comma", `[1,]`, false},
		{"array_missing_comma", `[true false]`, false},
		{"array_missing_number_comma", `[1 2]`, false},
		{"array_colon", `[1:2]`, false},
		{"array_bad_close", `[1}`, false},
		{"nested_trailing_comma", `{"a":[[],],"b":{}}`, false},
		{"nested_object_trailing_comma", `[{"a":{},}]`, false},
		{"unexpected_array_close", `]`, false},
		{"unexpected_object_close", `}`, false},
		{"trailing_literal", `nullfalse`, false},
		{"trailing_string", `"a""b"`, false},
		{"trailing_container", `{}{}`, false},
		{"trailing_comma", `0,`, false},
		{"trailing_close", `0]`, false},
		{"form_feed", "\f0", false},
		{"vertical_tab", "0\v", false},
		{"nonbreaking_space", "\u00a00", false},
		{"invalid_utf8_outside", "\xff", false},
	}
	for _, fixture := range cases {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(fixture.raw)
			if actual := json.Valid(raw); actual != fixture.valid {
				test.Fatalf("bad fixture: native JSON validity = %v, want %v", actual, fixture.valid)
			}
			for _, parents := range []int{0, 2} {
				legacyJSONCheckpointOracleEverySplit(test, raw, parents)
			}
		})
	}
	for current := byte(0); current < 0x20; current++ {
		test.Run(fmt.Sprintf("raw_control_%02x", current), func(test *testing.T) {
			legacyJSONCheckpointOracleEverySplit(test, []byte{'"', current, '"'}, 0)
		})
	}
}

func TestLegacyJSONValueScannerCheckpointOracleNumberDelimiters(test *testing.T) {
	for _, number := range []string{
		`0`, `-0`, `12`, `-123.5`, `0.001`, `1e999`, `-1.25E+3`,
		`-`, `1.`, `-0.`, `1e`, `1E+`, `1e-`,
	} {
		for _, delimiter := range []byte(" \t\r\n,]}") {
			test.Run(fmt.Sprintf("%s/%02x", number, delimiter), func(test *testing.T) {
				raw := append([]byte(number), delimiter)
				validNumber := json.Valid([]byte(number))
				wanted := legacyJSONCheckpointOracleStep{complete: validNumber}
				if !validNumber {
					wanted.err = ErrIntegrity
				}
				for _, parents := range []int{0, 2, 10000} {
					original := legacyJSONCheckpointOracleEverySplit(test, raw, parents)
					if observed := original[len(number)]; observed != wanted {
						test.Fatalf("parents %d, delimiter: (consumed, complete, error) = (%v, %v, %v), want (%v, %v, %v)", parents, observed.consumed, observed.complete, observed.err, wanted.consumed, wanted.complete, wanted.err)
					}
					if observed := original[len(raw)]; observed != wanted {
						test.Fatalf("parents %d, finish after delimiter: (complete, error) = (%v, %v), want (%v, %v)", parents, observed.complete, observed.err, wanted.complete, wanted.err)
					}
				}
			})
		}
	}
}

func TestLegacyJSONValueScannerCheckpointOracleDepth(test *testing.T) {
	cases := []struct {
		name    string
		open    string
		token   string
		close   string
		parents int
		layers  int
		valid   bool
	}{
		{"array_limit", `[`, `1e+2`, `]`, 0, 10000, true},
		{"array_value_limit_base_two", `[0,`, `"\uD800\u00ab"`, `]`, 2, 9998, true},
		{"object_key_limit_base_one", `{"\u0061":`, `true`, `}`, 1, 9999, true},
		{"mixed_limit", `{"key":[`, `false`, `]}`, 0, 5000, true},
		{"array_over_limit", `[`, `0`, `]`, 0, 10001, false},
		{"array_over_limit_base_one", `[`, `null`, `]`, 1, 10000, false},
		{"one_container_remaining", `[`, `false`, `]`, 9999, 1, true},
		{"two_containers_over_budget", `[`, `true`, `]`, 9999, 2, false},
		{"no_array_budget", `[`, `0`, `]`, 10000, 1, false},
		{"no_object_budget", `{"key":`, `null`, `}`, 10000, 1, false},
	}
	for _, fixture := range cases {
		test.Run(fixture.name, func(test *testing.T) {
			prefix := strings.Repeat(fixture.open, fixture.layers)
			raw := []byte(prefix + fixture.token + strings.Repeat(fixture.close, fixture.layers))
			if actual := legacyJSONCheckpointOracleNativeValid(raw, fixture.parents); actual != fixture.valid {
				test.Fatalf("bad depth fixture: native JSON validity = %v, want %v", actual, fixture.valid)
			}
			cuts := []int{0, len(prefix) / 2}
			for split := len(prefix) - len(fixture.open); split <= len(prefix)+len(fixture.token); split++ {
				cuts = append(cuts, split)
			}
			for closed := 1; closed <= len(fixture.close); closed++ {
				cuts = append(cuts, len(prefix)+len(fixture.token)+closed)
			}
			cuts = append(cuts, (len(raw)+len(prefix))/2, len(raw)-1, len(raw))
			legacyJSONCheckpointOracleCuts(test, raw, fixture.parents, cuts)
		})
	}
	for _, token := range []string{`0`, `-1e999`, `null`, `true`, `false`, `"x"`, `"\ud800"`} {
		test.Run("scalar_at_max_parents/"+token, func(test *testing.T) {
			raw := []byte(token)
			if !legacyJSONCheckpointOracleNativeValid(raw, 10000) {
				test.Fatal("bad fixture: native JSON rejected scalar at maximum parents")
			}
			legacyJSONCheckpointOracleEverySplit(test, raw, 10000)
		})
	}
}
