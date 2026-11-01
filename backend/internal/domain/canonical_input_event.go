package domain

import (
	"encoding/json"
	"unicode/utf8"
)

func canonicalInputEventJSON(event CIREvent) ([]byte, error) {
	value, err := canonicalEventJSONObject(event)
	if err != nil || !canonicalEventJSONValue(value, 0) {
		return canonicalJSON(event)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return canonicalJSON(event)
	}
	return raw, nil
}

func canonicalEventJSONValue(input any, depth int) bool {
	if depth >= 64 {
		return false
	}
	switch value := input.(type) {
	case nil, bool, int, float64, json.Number, *json.Number:
		return true
	case string:
		return utf8.ValidString(value)
	case EventKind:
		return utf8.ValidString(string(value))
	case Role:
		return utf8.ValidString(string(value))
	case ProviderKind:
		return utf8.ValidString(string(value))
	case map[string]any:
		for key, child := range value {
			if !utf8.ValidString(key) || !canonicalEventJSONValue(child, depth+1) {
				return false
			}
		}
		return true
	case []any:
		for _, child := range value {
			if !canonicalEventJSONValue(child, depth+1) {
				return false
			}
		}
		return true
	case []map[string]any:
		for _, child := range value {
			if !canonicalEventJSONValue(child, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
