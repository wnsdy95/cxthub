package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func legacyKeyOrderNestedFixture(count int) map[string]any {
	fields := make(map[string]any, count)
	for position := 0; position < count; position++ {
		name := fmt.Sprintf("key-%06d", count-position)
		fields[name] = []any{
			map[string]any{"identity": name, "nested": map[string]any{"value": position}},
			[]map[string]any{{"first": name}, nil, {"last": position}},
			[]any{nil, map[string]any{"tail": []any{map[string]any{"label": name}}}},
		}
	}
	return fields
}

func TestLegacyCanonicalKeyIntegrationSubtreeHandles(test *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		value := []any{
			legacyKeyOrderNestedFixture(17 + iteration%7),
			map[string]any{"after": []map[string]any{{"sentinel": "after every reordered subtree"}}},
		}
		before, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		after, err := json.Marshal(value)
		if err != nil || !bytes.Equal(before, after) {
			test.Fatal("key ordering mutated caller-owned maps or arrays", iteration, err)
		}
	}
}
