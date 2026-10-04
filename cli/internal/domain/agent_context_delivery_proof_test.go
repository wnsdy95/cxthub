package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAgentContextDeliveryProofPair(t *testing.T) {
	hash := HashContent([]byte("semantic source"))
	for _, tc := range []struct {
		name            string
		context, memory ContentHash
		valid           bool
	}{
		{"legacy", "", "", true},
		{"complete", hash, hash, true},
		{"context only", hash, "", false},
		{"memory only", "", hash, false},
		{"malformed context", "invalid", hash, false},
		{"malformed memory", hash, "invalid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := AgentContextSelection{ContextDeliveryHash: tc.context, MemoryDeliveryHash: tc.memory}
			if err := s.ValidateSource(); (err == nil) != tc.valid || (!tc.valid && !errors.Is(err, ErrHashMismatch)) {
				t.Fatalf("proof pair validation: %v", err)
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "legacy" && strings.Contains(string(raw), "delivery_hash") {
				t.Fatal("legacy package identity acquired new wire fields")
			}
			var decoded AgentContextSelection
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded != s {
				t.Fatal("proof pair lost in wire roundtrip", err)
			}
		})
	}
}
