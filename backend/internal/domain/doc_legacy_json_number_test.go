package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func legacyJSONNumberScanValid(raw []byte, split int) bool {
	mode := legacyJSONValueIdle
	for _, chunk := range [][]byte{raw[:split], raw[split:]} {
		for _, current := range chunk {
			next, valid := legacyJSONNumberNext(mode, current)
			if !valid {
				return false
			}
			mode = next
		}
	}
	return legacyJSONNumberFinal(mode)
}

func TestLegacyJSONNumberTransitions(test *testing.T) {
	type transition struct {
		bytes string
		next  legacyJSONValueMode
	}
	cases := []struct {
		mode        legacyJSONValueMode
		final       bool
		transitions []transition
	}{
		{legacyJSONValueIdle, false, []transition{
			{"-", legacyJSONValueMinus}, {"0", legacyJSONValueZero}, {"123456789", legacyJSONValueInteger},
		}},
		{legacyJSONValueMinus, false, []transition{
			{"0", legacyJSONValueZero}, {"123456789", legacyJSONValueInteger},
		}},
		{legacyJSONValueZero, true, []transition{
			{".", legacyJSONValuePoint}, {"eE", legacyJSONValueExponent},
		}},
		{legacyJSONValueInteger, true, []transition{
			{"0123456789", legacyJSONValueInteger}, {".", legacyJSONValuePoint}, {"eE", legacyJSONValueExponent},
		}},
		{legacyJSONValuePoint, false, []transition{{"0123456789", legacyJSONValueFraction}}},
		{legacyJSONValueFraction, true, []transition{
			{"0123456789", legacyJSONValueFraction}, {"eE", legacyJSONValueExponent},
		}},
		{legacyJSONValueExponent, false, []transition{
			{"+-", legacyJSONValueExponentSign}, {"0123456789", legacyJSONValueExponentDigits},
		}},
		{legacyJSONValueExponentSign, false, []transition{{"0123456789", legacyJSONValueExponentDigits}}},
		{legacyJSONValueExponentDigits, true, []transition{{"0123456789", legacyJSONValueExponentDigits}}},
		{legacyJSONValueString, false, nil},
		{legacyJSONValueEscape, false, nil},
		{legacyJSONValueUnicode, false, nil},
		{legacyJSONValueLiteral, false, nil},
		{legacyJSONValueMode(255), false, nil},
	}
	for _, fixture := range cases {
		if final := legacyJSONNumberFinal(fixture.mode); final != fixture.final {
			test.Fatalf("mode %d: final = %v, want %v", fixture.mode, final, fixture.final)
		}
		for current := 0; current <= 255; current++ {
			wantNext, wantValid := fixture.mode, false
			for _, candidate := range fixture.transitions {
				if strings.IndexByte(candidate.bytes, byte(current)) >= 0 {
					wantNext, wantValid = candidate.next, true
					break
				}
			}
			if next, valid := legacyJSONNumberNext(fixture.mode, byte(current)); next != wantNext || valid != wantValid {
				test.Fatalf("mode %d, byte %#x: next = %d, valid = %v, want %d, %v", fixture.mode, current, next, valid, wantNext, wantValid)
			}
		}
	}
}

func TestLegacyJSONNumberOracleEverySplit(test *testing.T) {
	for _, token := range []string{
		`0`, `-0`, `1`, `9`, `10`, `-12`, `123456789012345678901234567890`,
		`0.0`, `-0.001`, `1.2345678901234567890123456789`, `1e0`, `0E0`, `-0e-0`,
		`1e+9`, `1E-002`, `-1.25E+3`, `1e999`, `1e-999`,
		`-1E+9999999999999999999999999999999`, `1e-9999999999999999999999999999999`,
		``, `-`, `+1`, `--1`, `-+1`, `00`, `01`, `-01`, `.1`, `-.1`, `1.`, `-0.`,
		`1.e2`, `1..2`, `1.2.3`, `1e`, `1E+`, `1e-`, `1e++2`, `1e--2`, `1e+-2`,
		`1e2e3`, `1e2.3`, `0x1`, `NaN`, `Infinity`, `-Infinity`, `1true`,
	} {
		raw := []byte(token)
		expected := json.Valid(raw)
		for split := 0; split <= len(raw); split++ {
			if actual := legacyJSONNumberScanValid(raw, split); actual != expected {
				test.Fatalf("token %q, split %d: number valid = %v, want json.Valid = %v", token, split, actual, expected)
			}
			if actual := legacyJSONValueScanValid(raw, 2, split); actual != expected {
				test.Fatalf("token %q, split %d: scanner valid = %v, want json.Valid = %v", token, split, actual, expected)
			}
		}
	}
	for _, token := range []string{`-12.34e+567`, `-0.001E-002`} {
		for length := 0; length <= len(token); length++ {
			raw := []byte(token[:length])
			if actual, expected := legacyJSONNumberScanValid(raw, length/2), json.Valid(raw); actual != expected {
				test.Fatalf("prefix %q: number valid = %v, want json.Valid = %v", raw, actual, expected)
			}
		}
	}
}

func TestLegacyJSONNumberRejectsNonTokens(test *testing.T) {
	for _, token := range []string{
		" ", "\t", "\r", "\n", " 0", "0 ", "0\t", "0\r", "0\n", "- 1", "1 .0", "1e +1",
		`0,`, `0]`, `0}`, `1:`, `1[`, `1{`, `1"`, "0\x00", "0\xff",
		`null`, `true`, `false`, `"1"`, `[1]`, `{}`,
	} {
		raw := []byte(token)
		for split := 0; split <= len(raw); split++ {
			if legacyJSONNumberScanValid(raw, split) {
				test.Fatalf("accepted non-number token %q at split %d", token, split)
			}
			if actual, expected := legacyJSONValueScanValid(raw, 2, split), json.Valid(raw); actual != expected {
				test.Fatalf("token %q, split %d: scanner valid = %v, want json.Valid = %v", token, split, actual, expected)
			}
		}
	}
}

func TestLegacyJSONNumberScannerEOFAndDelimiters(test *testing.T) {
	for _, token := range []string{
		`0`, `-0`, `12`, `-123.5`, `0.001`, `1e999`, `-1.25E+3`,
		`-`, `1.`, `-0.`, `1e`, `1E+`, `1e-`,
	} {
		var scanner legacyJSONValueScanner
		if err := scanner.reset(2); err != nil {
			test.Fatal(err)
		}
		for offset := 0; offset < len(token); offset++ {
			if consumed, complete, err := scanner.add(token[offset]); err != nil || !consumed || complete {
				test.Fatalf("%q at %d: consumed = %v, complete = %v, error = %v", token, offset, consumed, complete, err)
			}
		}
		expected := json.Valid([]byte(token))
		atEOF := scanner
		complete, err := atEOF.finish()
		if expected {
			if err != nil || !complete {
				test.Fatalf("%q EOF: complete = %v, error = %v", token, complete, err)
			}
		} else if err != ErrIntegrity || complete {
			test.Fatalf("%q incomplete EOF: complete = %v, error = %v", token, complete, err)
		}
		for _, delimiter := range []byte(" \t\r\n,]}") {
			atDelimiter := scanner
			consumed, complete, err := atDelimiter.add(delimiter)
			if consumed || complete != expected || expected && err != nil || !expected && err != ErrIntegrity {
				test.Fatalf("%q delimiter %q: consumed = %v, complete = %v, error = %v", token, delimiter, consumed, complete, err)
			}
			if !expected {
				if consumed, complete, err := atDelimiter.add('0'); err != ErrIntegrity || consumed || complete {
					test.Fatalf("%q after invalid delimiter: consumed = %v, complete = %v, error = %v", token, consumed, complete, err)
				}
			}
		}
	}
}

func TestLegacyJSONNumberScannerContainerDelimiters(test *testing.T) {
	cases := []struct {
		prefix    string
		delimiter byte
		depth     int
		frame     legacyJSONValueFrame
	}{
		{`[0`, ']', 0, legacyJSONValueArrayAfter},
		{`[1`, ',', 1, legacyJSONValueArrayValue},
		{`[1.5`, ' ', 1, legacyJSONValueArrayAfter},
		{`{"n":1e+2`, '}', 0, legacyJSONValueObjectAfter},
		{`{"n":1`, ',', 1, legacyJSONValueObjectKey},
		{`{"n":-0`, '\t', 1, legacyJSONValueObjectAfter},
		{`[[1E-2`, ']', 1, legacyJSONValueArrayAfter},
	}
	for _, fixture := range cases {
		var scanner legacyJSONValueScanner
		if err := scanner.reset(2); err != nil {
			test.Fatal(err)
		}
		for offset := 0; offset < len(fixture.prefix); offset++ {
			if consumed, complete, err := scanner.add(fixture.prefix[offset]); err != nil || !consumed || complete {
				test.Fatalf("%q at %d: consumed = %v, complete = %v, error = %v", fixture.prefix, offset, consumed, complete, err)
			}
		}
		atEOF := scanner
		if complete, err := atEOF.finish(); err != ErrIntegrity || complete {
			test.Fatalf("%q unclosed container EOF: complete = %v, error = %v", fixture.prefix, complete, err)
		}
		consumed, complete, err := scanner.add(fixture.delimiter)
		if err != nil || !consumed || complete != (fixture.depth == 0) {
			test.Fatalf("%q delimiter %q: consumed = %v, complete = %v, error = %v", fixture.prefix, fixture.delimiter, consumed, complete, err)
		}
		if scanner.depth != fixture.depth || scanner.mode != legacyJSONValueIdle {
			test.Fatalf("%q delimiter %q: depth = %d, mode = %d", fixture.prefix, fixture.delimiter, scanner.depth, scanner.mode)
		}
		if scanner.depth > 0 && scanner.stack[scanner.depth-1] != fixture.frame {
			test.Fatalf("%q delimiter %q: frame = %d, want %d", fixture.prefix, fixture.delimiter, scanner.stack[scanner.depth-1], fixture.frame)
		}
	}
}
