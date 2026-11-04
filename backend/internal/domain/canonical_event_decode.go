package domain

import (
	"bytes"
	"encoding/json"
)

func decodeCanonicalEvent(raw []byte) (CIREvent, error) {
	var decoded wire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return CIREvent{}, err
	}
	presence, err := canonicalEventPresenceFields(raw)
	if err != nil {
		return CIREvent{}, err
	}
	event := CIREvent(decoded)
	event.fieldPresence = presence
	return event, nil
}

func canonicalEventPresenceFields(raw []byte) (eventFieldPresence, error) {
	remaining := bytes.TrimSpace(raw)
	if bytes.Equal(remaining, []byte("null")) {
		return 0, nil
	}
	if len(remaining) < 2 || remaining[0] != '{' {
		return 0, ErrIntegrity
	}
	var presence eventFieldPresence
	for remaining = remaining[1:]; len(remaining) > 0; {
		remaining = bytes.TrimLeft(remaining, ", \t\r\n")
		if len(remaining) == 0 {
			return 0, ErrIntegrity
		}
		if remaining[0] == '}' {
			return presence, nil
		}
		key, rest := docRangeJSONValue(remaining)
		if len(key) < 2 || key[0] != '"' {
			return 0, ErrIntegrity
		}
		_, remaining = docRangeJSONValue(bytes.TrimLeft(rest, ": \t\r\n"))
		var name string
		if bytes.IndexByte(key, '\\') >= 0 {
			if err := json.Unmarshal(key, &name); err != nil {
				return 0, err
			}
		} else {
			name = string(key[1 : len(key)-1])
		}
		presence |= eventPresenceName(name)
	}
	return 0, ErrIntegrity
}
