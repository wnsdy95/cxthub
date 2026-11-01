package domain

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"unicode/utf8"
)

func countDocToolTreeJSON(decoder *json.Decoder, depth int) (int, error) {
	if depth > 32 {
		return 0, ErrDocFindDisplayUnavailable
	}
	token, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return 1, nil
	}
	count := 1
	for decoder.More() {
		if delimiter == '{' {
			if _, err := decoder.Token(); err != nil {
				return 0, err
			}
		}
		child, err := countDocToolTreeJSON(decoder, depth+1)
		if err != nil {
			return 0, err
		}
		count += child
	}
	if _, err := decoder.Token(); err != nil {
		return 0, err
	}
	return count, nil
}

func FuzzDocToolTreeRestart(fuzzer *testing.F) {
	for _, raw := range []string{
		`0`, `-0.010e+999999`, `null`, `"\ud800\udfff\ud800\u0000"`,
		`[0,{"a":true,"a":null},[]]`,
		`{"kind":"tool_call","output":{"kind":"tool_result"},"escaped\u006bey":[false,1]}`,
	} {
		fuzzer.Add([]byte(raw))
	}
	fuzzer.Fuzz(func(test *testing.T, value []byte) {
		if len(value) > 8192 || !utf8.Valid(value) || !json.Valid(value) {
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		expected, err := countDocToolTreeJSON(decoder, 0)
		if err != nil {
			return
		}
		if _, err := decoder.Token(); err != io.EOF {
			test.Fatal("reference did not consume the whole value", err)
		}
		fixture := newDocToolTreeFixture(test, `{"kind":"tool_result","output":`+string(value)+`}`)
		if err := fixture.scan(test); err != nil {
			test.Fatal("valid bounded JSON did not survive scanner restarts", err)
		}
		if len(fixture.nodes) != expected {
			test.Fatal("scanner lost or duplicated nested values", len(fixture.nodes), expected)
		}
	})
}
