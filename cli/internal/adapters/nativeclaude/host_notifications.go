package nativeclaude

import (
	"bytes"
	"encoding/json"
)

// Claude 2.1.287 emits these on the host channel independently of commands.
// Validate and drain them without retaining private notice text, advancing a
// pending operation, or treating a render notification as permission approval.
func (s *Session) validateHostNotification(m map[string]json.RawMessage, subtype string) error {
	if s.version != supportedVersion {
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
		if !exchangeKeys(m, "type", "subtype", "session_id", "uuid", "content", "isMeta", "level", "timestamp") {
			return ErrProtocol
		}
		var text string
		raw := m["content"]
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &text) != nil || len(text) > 16<<10 {
			return ErrProtocol
		}
		// Headless SDK producers omit isMeta and can use the notice level.
		// Actionable tool/prevent-continuation fields remain unsupported.
		if _, ok := m["isMeta"]; ok {
			if _, err := boolean(m, "isMeta"); err != nil {
				return ErrProtocol
			}
		}
		if raw, ok := m["timestamp"]; ok {
			if _, err := exchangeString(raw, 64); err != nil {
				return ErrProtocol
			}
		}
		if level, err := stringField(m, "level"); err != nil || (level != "info" && level != "notice" && level != "warning" && level != "error") {
			return ErrProtocol
		}
	default:
		return ErrProtocol
	}
	return nil
}

// Subscription responses carry session-level usage notifications, including
// after the last assistant block. They cannot approve a request or complete it:
// even "allowed" still requires the normal correlated result/lifecycle proof.
// The account-specific optional details are bounded and discarded, not copied
// into receipts, prompts, budgets or native permission decisions.
func (s *Session) validateRateLimitNotification(m map[string]json.RawMessage) error {
	q := s.firstQuestion
	if s.version != supportedVersion || q == nil || q.phase < 2 ||
		!exchangeKeys(m, "type", "rate_limit_info", "uuid", "session_id") {
		return ErrProtocol
	}
	if id, err := stringField(m, "session_id"); err != nil || id != s.id {
		return ErrProtocol
	}
	if _, err := stringField(m, "uuid"); err != nil {
		return ErrProtocol
	}
	raw := m["rate_limit_info"]
	if len(raw) > 16<<10 {
		return ErrLimit
	}
	info, err := object(raw)
	if err != nil {
		return err
	}
	status, err := stringField(info, "status")
	if err != nil || status != "allowed" && status != "allowed_warning" && status != "rejected" {
		return ErrProtocol
	}
	return nil
}
