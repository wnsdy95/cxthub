package inbound

import (
	"bytes"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
)

// RepoProfilePatch preserves absent fields and applies configuration and about
// metadata together in one application transaction.
type RepoProfilePatch struct {
	RequiredDocIdentity *domain.DocumentIdentity `json:"required_doc_identity"`
	Description         *string                  `json:"description"`
	Website             *string                  `json:"website"`
	Topics              []string                 `json:"topics"`
	DefaultBranch       *string                  `json:"default_branch"`
	ProtectDefault      *bool                    `json:"protect_default"`
}

// Keep the established decoding of legacy profile fields. The new requirement
// must be explicit and unambiguous: null, case aliases and duplicates fail.
func (p *RepoProfilePatch) UnmarshalJSON(raw []byte) error {
	type wire RepoProfilePatch
	var decoded wire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*p = RepoProfilePatch(decoded)
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if _, err := d.Token(); err != nil {
		return err
	}
	seen := false
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return domain.ErrValidation
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		if strings.EqualFold(key, "required_doc_identity") {
			if key != "required_doc_identity" || seen {
				return domain.ErrValidation
			}
			seen = true
			var identity domain.DocumentIdentity
			if err = json.Unmarshal(value, &identity); err != nil {
				return err
			}
		}
	}
	*p = RepoProfilePatch(decoded)
	return nil
}
