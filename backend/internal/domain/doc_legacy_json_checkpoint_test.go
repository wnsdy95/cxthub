package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

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
		})
	}
	for _, token := range []string{`0`, `-1e999`, `null`, `true`, `false`, `"x"`, `"\ud800"`} {
		test.Run("scalar_at_max_parents/"+token, func(test *testing.T) {
			raw := []byte(token)
			if !legacyJSONCheckpointOracleNativeValid(raw, 10000) {
				test.Fatal("bad fixture: native JSON rejected scalar at maximum parents")
			}
		})
	}
}
