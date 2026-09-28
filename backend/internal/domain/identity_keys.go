package domain

import (
	"strings"
	"time"
)

// IdentitySecret is operator-only encrypted material. It is never an API DTO.
type IdentitySecret struct {
	Kind, ID, EnterpriseID, Revision string
	Sealed                           string `json:"-"`
}

func (s IdentitySecret) Cursor() string { return s.Kind + ":" + s.ID }
func (s IdentitySecret) Purpose() (string, error) {
	if ValidateEnterpriseID(s.EnterpriseID) != nil || s.Revision == "" || s.Sealed == "" {
		return "", ErrIntegrity
	}
	switch s.Kind {
	case "oidc_connection":
		if s.ID == s.EnterpriseID {
			return s.ID + ":" + s.Revision, nil
		}
	case "oidc_verifier":
		if s.ID != "" {
			return s.ID, nil
		}
	case "saml_key":
		if s.ID == s.EnterpriseID {
			return "saml:" + s.ID + ":" + s.Revision, nil
		}
	case "saml_alternate":
		if s.ID == s.EnterpriseID {
			return "saml-rotation:" + s.ID + ":" + s.Revision, nil
		}
	}
	return "", ErrIntegrity
}

func IdentitySecretCursor(after string) (string, string, error) {
	if after == "" {
		return "", "", nil
	}
	k, id, ok := strings.Cut(after, ":")
	if !ok || id == "" || len(id) > 256 || strings.ContainsAny(id, ":\x00\r\n") {
		return "", "", ErrValidation
	}
	if k != "oidc_connection" && k != "oidc_verifier" && k != "saml_key" && k != "saml_alternate" {
		return "", "", ErrValidation
	}
	return k, id, nil
}

// IdentityKeyBatch is a durable operator receipt, not a key-retirement guarantee.
type IdentityKeyBatch struct {
	EnterpriseID string         `json:"enterprise_id,omitempty"`
	Operation    string         `json:"operation,omitempty"`
	Actor        string         `json:"actor,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	ActiveKey    string         `json:"active_key"`
	After        string         `json:"after"`
	Next         string         `json:"next"`
	Limit        int            `json:"limit"`
	Scanned      int            `json:"scanned"`
	Candidates   int            `json:"candidates"`
	Changed      int            `json:"changed"`
	Keys         map[string]int `json:"keys"`
	Complete     bool           `json:"complete"`
	Applied      bool           `json:"applied"`
	CreatedAt    time.Time      `json:"created_at"`
}
