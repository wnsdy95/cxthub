package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// DocumentIdentity is independent of CIR version and chunk representation.
// Its empty value preserves the exact legacy wire encoding and hash meaning.
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

// DocumentRef is a comparison key, never evidence of bytes or ownership.
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

func (doc SessionDoc) DocumentRef() DocumentRef {
	return DocumentRef{Hash: doc.Hash, Identity: doc.Identity}
}

func (snapshot Snapshot) DocumentRef() DocumentRef {
	return DocumentRef{Hash: snapshot.DocHash, Identity: snapshot.DocIdentity}
}

func (ref DocumentRef) MatchesSnapshot(snapshot Snapshot) bool {
	return ref.Validate() == nil && snapshot.ID == snapshot.DocHash && ref == snapshot.DocumentRef()
}

// VerifySessionDocIdentity explicitly checks either identity against a full
// CIR. It returns no store publication proof. The legacy publication boundary
// remains closed to roots until all storage and negotiation paths are ready.
func VerifySessionDocIdentity(ctx context.Context, doc SessionDoc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := doc.DocumentRef().Validate(); err != nil {
		return err
	}
	if doc.Identity == DocumentIdentityLegacy {
		if err := ValidateSessionDocHash(doc); err != nil {
			return err
		}
		return ctx.Err()
	}
	manifest, _, err := ConversationManifestForCIR(doc.CIR)
	if err != nil {
		return err
	}
	hash, err := ConversationManifestHash(manifest)
	if err != nil {
		return err
	}
	if hash != doc.Hash {
		return ErrIntegrity
	}
	return ctx.Err()
}
