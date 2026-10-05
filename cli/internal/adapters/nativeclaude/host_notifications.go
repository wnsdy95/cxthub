package nativeclaude

import (
	"bytes"
	"encoding/json"
)

// Claude 2.1.287 emits these on the host channel independently of commands.
// Validate and drain them without retaining private notice text, advancing a
// pending operation, or treating a render notification as permission approval.
func (s *Session) validateHostNotification(m map[string]json.RawMessage, subtype string) error {
	if s.version != "2.1.287" {
		return ErrProtocol
	}
	if id, err := stringField(m, "session_id"); err != nil || id != s.id {
		return ErrProtocol
	}
	if _, err := stringField(m, "uuid"); err != nil {
		return ErrProtocol
	}
	switch subtype {
	case "ui_invalidate":
		if event, err := stringField(m, "event"); err != nil || event != "ui.render" {
			return ErrProtocol
		}
		// Instance-specific UI interactions need their own supported contract.
		if raw, ok := m["instances"]; ok {
			var instances []json.RawMessage
			if json.Unmarshal(raw, &instances) != nil || instances == nil || len(instances) != 0 {
				return ErrProtocol
			}
		}
	case "informational":
		var text string
		raw := m["content"]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &text) != nil || len(text) > 16<<10 {
			return ErrProtocol
		}
		if _, err := boolean(m, "isMeta"); err != nil {
			return ErrProtocol
		}
		if level, err := stringField(m, "level"); err != nil || (level != "info" && level != "warning" && level != "error") {
			return ErrProtocol
		}
	default:
		return ErrProtocol
	}
	return nil
}
