package graphwire

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestReadGenerationSurvivesBothGraphEncodings(t *testing.T) {
	for _, current := range []bool{true, false} {
		generation := &domain.GraphReadGeneration{
			Generation:  domain.HashContent([]byte("applied generation")),
			SourceEpoch: "12345678-abcd-4321-9876-123456789abc:2", AppliedEpoch: "12345678-abcd-4321-9876-123456789abc:1",
			Rules: 2, Complete: true, Current: current,
		}
		if current {
			generation.AppliedEpoch = generation.SourceEpoch
		}
		graph := domain.GraphState{ReadGeneration: generation, Version: 1}
		for _, wire := range []any{Encode(graph), EncodeV2(graph)} {
			body, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				ReadGeneration *domain.GraphReadGeneration `json:"read_generation"`
			}
			if err := json.Unmarshal(body, &decoded); err != nil || !reflect.DeepEqual(decoded.ReadGeneration, generation) {
				t.Fatalf("lost generation: %s: %v", body, err)
			}
		}
	}
	for _, wire := range []any{Encode(domain.GraphState{}), EncodeV2(domain.GraphState{})} {
		body, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if _, exists := fields["read_generation"]; exists {
			t.Fatal("legacy response acquired generation metadata")
		}
	}
}
