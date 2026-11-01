package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func legacyJSONValueScanValid(raw []byte, parents, split int) bool {
	var scanner legacyJSONValueScanner
	if scanner.reset(parents) != nil {
		return false
	}
	complete := false
	for _, chunk := range [][]byte{raw[:split], raw[split:]} {
		for _, current := range chunk {
			if complete {
				switch current {
				case ' ', '\t', '\r', '\n':
					continue
				default:
					return false
				}
			}
			consumed, done, err := scanner.add(current)
			if err != nil {
				return false
			}
			if !consumed {
				if !done {
					return false
				}
				switch current {
				case ' ', '\t', '\r', '\n':
				default:
					return false
				}
			}
			complete = done
		}
	}
	done, err := scanner.finish()
	return err == nil && done
}

func TestLegacyJSONValueScannerOracleEverySplit(test *testing.T) {
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
		{"unicode", `"한글 😀"`, true},
		{"invalid_utf8", "\"\xff\xc0\x80\xed\xa0\x80\"", true},
		{"invalid_utf8_key", "{\"\xff\":\"\xfe\"}", true},
		{"empty_object", `{}`, true},
		{"empty_array", `[]`, true},
		{"nested_empty", `[[[],{}],{"a":[],"b":{}}]`, true},
		{"duplicate_keys", `{"a":1,"a":true,"a":null}`, true},
		{"nested_numbers", `{"a":[1,2e999],"b":0}`, true},
		{"whitespace", " \t\r\n{ \"a\" : [ -1 , true , null , \"x\" ] } \n", true},
		{"number_whitespace", " \t-0.1e+2\r\n", true},
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
			if valid := json.Valid(raw); valid != fixture.valid {
				test.Fatalf("bad fixture: json.Valid = %v, want %v", valid, fixture.valid)
			}
			for split := 0; split <= len(raw); split++ {
				if valid := legacyJSONValueScanValid(raw, 0, split); valid != fixture.valid {
					test.Fatalf("split %d: valid = %v, want %v for %q", split, valid, fixture.valid, raw)
				}
			}
		})
	}
}

func TestLegacyJSONValueScannerControlBytes(test *testing.T) {
	for current := byte(0); current < 0x20; current++ {
		raw := []byte{'"', current, '"'}
		if json.Valid(raw) || legacyJSONValueScanValid(raw, 0, 1) {
			test.Fatalf("accepted raw control byte %#x", current)
		}
	}
	for current := 0x20; current <= 0xff; current++ {
		if current == '"' || current == '\\' {
			continue
		}
		raw := []byte{'"', byte(current), '"'}
		if !json.Valid(raw) || !legacyJSONValueScanValid(raw, 0, 2) {
			test.Fatalf("rejected raw string byte %#x", current)
		}
	}
}

func TestLegacyJSONValueScannerPrefixes(test *testing.T) {
	for _, raw := range []string{
		`-12.34e+567`, `null`, `true`, `false`, `"\uD800\u00ab\"\\"`,
		`{"a":[null,{},[],1e999,"\u0000"],"a":false}`,
	} {
		for length := 0; length <= len(raw); length++ {
			prefix := []byte(raw[:length])
			if actual, expected := legacyJSONValueScanValid(prefix, 0, length/2), json.Valid(prefix); actual != expected {
				test.Fatalf("prefix %q: valid = %v, want %v", prefix, actual, expected)
			}
		}
	}
}

func TestLegacyJSONValueScannerNumberDelimiters(test *testing.T) {
	for _, raw := range []string{"0", "-0", "12", "-123.5", "0.001", "1e999", "-1.25E+3"} {
		for _, delimiter := range []byte(" \t\r\n,]}") {
			var scanner legacyJSONValueScanner
			if err := scanner.reset(2); err != nil {
				test.Fatal(err)
			}
			for offset := 0; offset < len(raw); offset++ {
				consumed, complete, err := scanner.add(raw[offset])
				if err != nil || !consumed || complete {
					test.Fatalf("%q at %d: consumed = %v, complete = %v, error = %v", raw, offset, consumed, complete, err)
				}
			}
			consumed, complete, err := scanner.add(delimiter)
			if err != nil || consumed || !complete {
				test.Fatalf("%q delimiter %q: consumed = %v, complete = %v, error = %v", raw, delimiter, consumed, complete, err)
			}
			if complete, err := scanner.finish(); err != nil || !complete {
				test.Fatalf("%q finish: complete = %v, error = %v", raw, complete, err)
			}
			if consumed, complete, err := scanner.add('x'); err != nil || consumed || !complete {
				test.Fatalf("%q after completion: consumed = %v, complete = %v, error = %v", raw, consumed, complete, err)
			}
		}
	}
}

func TestLegacyJSONValueScannerClosingByteConsumed(test *testing.T) {
	for _, raw := range []string{
		`null`, `true`, `false`, `""`, `"\uD800"`, `{}`, `[]`,
		`{"a":[1,2e999],"b":0}`, "[1 \t, 2\r\n]", `[[{}],{"a":{}}]`,
	} {
		var scanner legacyJSONValueScanner
		if err := scanner.reset(1); err != nil {
			test.Fatal(err)
		}
		for offset := 0; offset < len(raw); offset++ {
			consumed, complete, err := scanner.add(raw[offset])
			if err != nil || !consumed || complete != (offset == len(raw)-1) {
				test.Fatalf("%q at %d: consumed = %v, complete = %v, error = %v", raw, offset, consumed, complete, err)
			}
		}
		if consumed, complete, err := scanner.add(','); err != nil || consumed || !complete {
			test.Fatalf("%q after completion: consumed = %v, complete = %v, error = %v", raw, consumed, complete, err)
		}
		if complete, err := scanner.finish(); err != nil || !complete {
			test.Fatalf("%q finish: complete = %v, error = %v", raw, complete, err)
		}
	}
}

func TestLegacyJSONValueScannerDepth(test *testing.T) {
	cases := []struct {
		parents int
		depth   int
	}{
		{0, 10000}, {0, 10001}, {1, 9999}, {1, 10000},
		{2, 9998}, {2, 9999}, {9999, 1}, {9999, 2}, {10000, 1},
	}
	for _, fixture := range cases {
		raw := strings.Repeat("[", fixture.depth) + strings.Repeat("]", fixture.depth)
		wrapped := strings.Repeat("[", fixture.parents) + raw + strings.Repeat("]", fixture.parents)
		expected := json.Valid([]byte(wrapped))
		if expected != (fixture.parents+fixture.depth <= 10000) {
			test.Fatal("unexpected JSON oracle depth limit")
		}
		if actual := legacyJSONValueScanValid([]byte(raw), fixture.parents, fixture.depth); actual != expected {
			test.Fatalf("parents %d, depth %d: valid = %v, want %v", fixture.parents, fixture.depth, actual, expected)
		}
	}
	for _, parents := range []int{0, 1, 2} {
		layers := (10000 - parents) / 2
		for _, extra := range []int{0, 1} {
			raw := strings.Repeat(`{"key":[`, layers+extra) + strings.Repeat(`]}`, layers+extra)
			wrapped := strings.Repeat("[", parents) + raw + strings.Repeat("]", parents)
			if actual, expected := legacyJSONValueScanValid([]byte(raw), parents, len(raw)/2), json.Valid([]byte(wrapped)); actual != expected {
				test.Fatalf("mixed containers, parents %d, layers %d: valid = %v, want %v", parents, layers+extra, actual, expected)
			}
		}
	}
	for _, raw := range []string{`0`, `-1e999`, `null`, `true`, `false`, `"x"`, `"\ud800"`} {
		wrapped := strings.Repeat("[", 10000) + raw + strings.Repeat("]", 10000)
		if !json.Valid([]byte(wrapped)) || !legacyJSONValueScanValid([]byte(raw), 10000, len(raw)/2) {
			test.Fatalf("scalar counted as a container: %q", raw)
		}
	}
}

func TestLegacyJSONValueScannerResetKeepsInactiveStack(test *testing.T) {
	var scanner legacyJSONValueScanner
	for index := range scanner.stack {
		scanner.stack[index] = legacyJSONValueObjectColon
	}
	if err := scanner.reset(1); err != nil {
		test.Fatal(err)
	}
	for index, frame := range scanner.stack {
		if frame != legacyJSONValueObjectColon {
			test.Fatalf("reset cleared inactive frame %d", index)
		}
	}
	for iteration := 0; iteration < 32; iteration++ {
		if err := scanner.reset(2); err != nil {
			test.Fatal(err)
		}
		for _, current := range []byte(`{"key":[{},[],0]}`) {
			if _, _, err := scanner.add(current); err != nil {
				test.Fatal(err)
			}
		}
		if complete, err := scanner.finish(); err != nil || !complete {
			test.Fatalf("reuse %d: complete = %v, error = %v", iteration, complete, err)
		}
		if scanner.stack[3] != legacyJSONValueObjectColon || scanner.stack[len(scanner.stack)-1] != legacyJSONValueObjectColon {
			test.Fatal("tiny value changed inactive stack frames")
		}
	}
}

func TestLegacyJSONValueScannerResetLogicalState(test *testing.T) {
	var scanner legacyJSONValueScanner
	for _, previous := range []string{
		``, `[`, `{"key`, `{"key":`, `"\`, `"\u12`, `tr`, `-1e`, `{"x":[0,}`, `true`, `0`,
	} {
		if err := scanner.reset(0); err != nil {
			test.Fatal(err)
		}
		for _, current := range []byte(previous) {
			if _, _, err := scanner.add(current); err != nil {
				break
			}
		}
		if err := scanner.reset(2); err != nil {
			test.Fatal(err)
		}
		raw := `"fresh\u0000"`
		for offset := 0; offset < len(raw); offset++ {
			consumed, complete, err := scanner.add(raw[offset])
			if err != nil || !consumed || complete != (offset == len(raw)-1) {
				test.Fatalf("reset after %q at %d: consumed = %v, complete = %v, error = %v", previous, offset, consumed, complete, err)
			}
		}
		if complete, err := scanner.finish(); err != nil || !complete {
			test.Fatalf("reset after %q: complete = %v, error = %v", previous, complete, err)
		}
	}
	for _, parents := range []int{-1, 10001, int(^uint(0) >> 1)} {
		if err := scanner.reset(parents); err != ErrIntegrity {
			test.Fatalf("parents %d: error = %v, want ErrIntegrity", parents, err)
		}
		if _, complete, err := scanner.add('0'); err != ErrIntegrity || complete {
			test.Fatalf("invalid parents %d: complete = %v, error = %v", parents, complete, err)
		}
		if complete, err := scanner.finish(); err != ErrIntegrity || complete {
			test.Fatalf("invalid parents %d finish: complete = %v, error = %v", parents, complete, err)
		}
	}
	if err := scanner.reset(0); err != nil {
		test.Fatal(err)
	}
	if consumed, complete, err := scanner.add('0'); err != nil || !consumed || complete {
		test.Fatalf("reset after invalid parents: consumed = %v, complete = %v, error = %v", consumed, complete, err)
	}
	if complete, err := scanner.finish(); err != nil || !complete {
		test.Fatalf("reset after invalid parents finish: complete = %v, error = %v", complete, err)
	}
	if consumed, complete, err := scanner.add('x'); err != nil || consumed || !complete {
		test.Fatalf("after number EOF: consumed = %v, complete = %v, error = %v", consumed, complete, err)
	}
}

func TestLegacyJSONValueScannerFailureIsTerminal(test *testing.T) {
	for _, raw := range []string{`{]`, `[}`, `-x`, `"\x`, "\"\x00", `tX`, `1e+ `, `[1,]`} {
		var scanner legacyJSONValueScanner
		if err := scanner.reset(0); err != nil {
			test.Fatal(err)
		}
		rejected := false
		for _, current := range []byte(raw) {
			if _, _, err := scanner.add(current); err != nil {
				rejected = true
				break
			}
		}
		if !rejected {
			test.Fatalf("did not reject malformed value %q", raw)
		}
		if _, complete, err := scanner.add('0'); err != ErrIntegrity || complete {
			test.Fatalf("after rejecting %q: complete = %v, error = %v", raw, complete, err)
		}
		if complete, err := scanner.finish(); err != ErrIntegrity || complete {
			test.Fatalf("finish after rejecting %q: complete = %v, error = %v", raw, complete, err)
		}
	}
}

func TestLegacyJSONValueScannerLongLexemes(test *testing.T) {
	for _, raw := range []string{
		`"` + strings.Repeat("x", 128<<10) + `"`,
		strings.Repeat("9", 128<<10),
		`1e` + strings.Repeat("9", 128<<10),
		`{"key":"` + strings.Repeat(`\ud800`, 32<<10) + `"}`,
	} {
		if !json.Valid([]byte(raw)) || !legacyJSONValueScanValid([]byte(raw), 1, len(raw)/2) {
			test.Fatalf("rejected valid long lexeme of %d bytes", len(raw))
		}
	}
}

func FuzzLegacyJSONValueScanner(fuzzer *testing.F) {
	for _, raw := range []string{
		``, `null`, `false`, `0`, `-0.01E+999999999999999999`, `1e999`,
		`"\ud800\udfff\ud800\u0000"`, "\"\xff\xc0\x80\"", "{\"\xff\":0}",
		`[0,{"a":true,"a":null},[],{}]`, `{"a":1,}`, `[1,]`, `01`, `1e+`, `nullfalse`, `"\u12x4"`,
	} {
		fuzzer.Add([]byte(raw))
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		if actual, expected := legacyJSONValueScanValid(raw, 0, len(raw)/2), json.Valid(raw); actual != expected {
			test.Fatalf("valid = %v, want %v for %q", actual, expected, raw)
		}
	})
}
