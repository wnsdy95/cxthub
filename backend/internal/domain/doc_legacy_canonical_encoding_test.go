package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzLegacyCanonicalEncoding(fuzzer *testing.F) {
	prefix := strings.Repeat("p", 16000)
	for _, value := range []any{
		legacyKeyOrderNestedFixture(17),
		map[string]any{prefix + "z": true, prefix + "a": false, prefix + "m": nil},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			fuzzer.Fatal(err)
		}
		fuzzer.Add(raw)
	}
	for _, raw := range []string{`null`, `{"z":[null,false,{"z":1,"a":2}],"a":"<&>\u2028🙂"}`, `9007199254740993`, `"\ud800"`} {
		fuzzer.Add([]byte(raw))
	}
	for _, candidate := range canonicalInputEventCases(fuzzer) {
		if len(candidate.raw) <= 64<<10 {
			fuzzer.Add(candidate.raw)
		}
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 64<<10 || !json.Valid(raw) {
			return
		}
		wrapped := append([]byte("[["), raw...)
		wrapped = append(wrapped, ']', ']')
		var parents []any
		if json.Unmarshal(wrapped, &parents) != nil {
			return
		}
		_ = parents[0].([]any)[0]
		var event CIREvent
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		_, err := canonicalEventJSONObject(event)
		if err != nil {
			return
		}
		if err != nil {
			test.Fatal(err)
		}
	})
}
