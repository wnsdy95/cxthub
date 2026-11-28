package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// DocumentIdentity is independent of CIR version and chunk representation.
// The empty value is deliberately the only spelling for legacy identity.
type DocumentIdentity string

const (
	DocumentIdentityLegacy DocumentIdentity = ""
	DocumentIdentityRootV1 DocumentIdentity = ConversationManifestIdentity
)

var ErrUnsupportedDocumentIdentity = errors.New("unsupported document identity")

func (identity DocumentIdentity) Validate() error {
	switch identity {
	case DocumentIdentityLegacy, DocumentIdentityRootV1:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedDocumentIdentity, identity)
	}
}

func (identity DocumentIdentity) MarshalJSON() ([]byte, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(string(identity))
}

func (identity *DocumentIdentity) UnmarshalJSON(raw []byte) error {
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return ErrUnsupportedDocumentIdentity
	}
	next := DocumentIdentity(value)
	if err := next.Validate(); err != nil {
		return err
	}
	*identity = next
	return nil
}

// DocumentRef is an in-process comparison key, not proof of bytes or ownership.
type DocumentRef struct {
	Hash     ContentHash
	Identity DocumentIdentity
}

func (ref DocumentRef) Validate() error {
	if err := ref.Identity.Validate(); err != nil {
		return err
	}
	return ValidateContentHash(ref.Hash)
}
