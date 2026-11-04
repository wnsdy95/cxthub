package domain

import (
	"encoding/json"
	"errors"
)

var errCanonicalEventShape = errors.New("canonical event requires generic normalization")

// canonicalDecodedEventJSON is only for typed-decoded, sequence-normalized
// verifier events. Input/Output already contain plain encoding/json values.
// Arbitrary in-memory Go values must continue through canonicalJSON.
func canonicalDecodedEventJSON(e CIREvent) ([]byte, error) {
	m, err := canonicalEventJSONObject(e)
	if err == errCanonicalEventShape {
		return canonicalJSON(e)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func canonicalEventJSONObject(e CIREvent) (map[string]any, error) {
	return canonicalEventJSONObjectWithBlocks(e, false)
}

func legacyCanonicalEventJSONObject(event CIREvent) (map[string]any, error) {
	return canonicalEventJSONObjectWithBlocks(event, true)
}

func canonicalEventJSONObjectWithBlocks(e CIREvent, typedBlocks bool) (map[string]any, error) {
	return canonicalEventJSONObjectWithReplacement(e, typedBlocks, false)
}

func canonicalEventJSONObjectWithReplacement(e CIREvent, typedBlocks, deferredReplacement bool) (map[string]any, error) {
	m, err := eventJSONObject(e)
	if err != nil {
		return nil, err
	}
	// Reuse the union's emitted fields. Only these known nested Go types need
	// conversion for lexical key ordering; payload maps/slices stay untouched.
	for key, value := range m {
		switch v := value.(type) {
		case []ContentBlock:
			if typedBlocks {
				continue
			}
			if v == nil {
				m[key] = nil
				continue
			}
			blocks := make([]map[string]any, len(v))
			for i, block := range v {
				blocks[i] = map[string]any{"type": block.Type, "text": block.Text}
			}
			m[key] = blocks
		case *ProviderMetadata:
			if v == nil {
				m[key] = nil
				continue
			}
			metadata := map[string]any{}
			if v.TurnID != "" {
				metadata["turn_id"] = v.TurnID
			}
			if v.CreateTime != nil {
				metadata["create_time"] = v.CreateTime
			}
			m[key] = metadata
		case *LockedBlob:
			if v == nil {
				m[key] = nil
				continue
			}
			m[key] = map[string]any{"provider": v.Provider, "scheme": v.Scheme, "blob": v.Blob}
		case []CIREvent:
			if v == nil {
				m[key] = nil
				continue
			}
			if deferredReplacement {
				m[key] = &legacyCanonicalReplacement{events: v}
				continue
			}
			events := make([]map[string]any, len(v))
			for i, event := range v {
				events[i], err = canonicalEventJSONObjectWithBlocks(event, typedBlocks)
				if err != nil {
					return nil, err
				}
			}
			m[key] = events
		case nil, bool, string, int, float64, EventKind, Role, map[string]any, []any:
			// Remaining values come from the union or the typed JSON decoder.
		default:
			// A new typed field needs an explicit canonical ordering rule.
			return nil, errCanonicalEventShape
		}
	}
	return m, nil
}
