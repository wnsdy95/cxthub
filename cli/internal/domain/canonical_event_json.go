package domain

import (
	"encoding/json"
	"errors"
)

var errCanonicalEventShape = errors.New("canonical event requires generic normalization")

// canonicalDecodedEventJSON is only for typed-decoded, sequence-normalized
// verifier events. Input/Output already contain plain encoding/json values.
// Arbitrary in-memory Go values must continue through canonicalJSON.
func canonicalDecodedEventJSON(e Event) ([]byte, error) {
	m, err := canonicalEventJSONObject(e)
	if err == errCanonicalEventShape {
		return canonicalJSON(e)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func canonicalEventJSONObject(e Event) (map[string]any, error) {
	m, err := eventJSONObject(e)
	if err != nil {
		return nil, err
	}
	// Reuse the union's emitted fields. Only these known nested Go types need
	// conversion for lexical key ordering; payload maps/slices stay untouched.
	for key, value := range m {
		switch v := value.(type) {
		case []ContentBlock:
			blocks := make([]map[string]any, len(v))
			for i, block := range v {
				blocks[i] = map[string]any{"type": block.Type, "text": block.Text}
			}
			m[key] = blocks
		case *ProviderMetadata:
			metadata := map[string]any{}
			if v.TurnID != "" {
				metadata["turn_id"] = v.TurnID
			}
			if v.CreateTime != nil {
				metadata["create_time"] = v.CreateTime
			}
			m[key] = metadata
		case *LockedBlob:
			m[key] = map[string]any{"provider": v.Provider, "scheme": v.Scheme, "blob": v.Blob}
		case []Event:
			events := make([]map[string]any, len(v))
			for i, event := range v {
				events[i], err = canonicalEventJSONObject(event)
				if err != nil {
					return nil, err
				}
			}
			m[key] = events
		case nil, bool, string, int, float64, map[string]any, []any:
			// Remaining values come from the union or the typed JSON decoder.
		default:
			// A new typed field needs an explicit canonical ordering rule.
			return nil, errCanonicalEventShape
		}
	}
	return m, nil
}
