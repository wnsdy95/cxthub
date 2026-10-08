package domain

import (
	"bytes"
	"context"
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

func (doc SessionDoc) DocumentRef() DocumentRef {
	return DocumentRef{Hash: doc.Hash, Identity: doc.Identity}
}

func (snapshot Snapshot) DocumentRef() DocumentRef {
	return DocumentRef{Hash: snapshot.DocHash, Identity: snapshot.DocIdentity}
}

// MatchesSnapshot checks the immutable identity join, not body validity.
func (ref DocumentRef) MatchesSnapshot(snapshot Snapshot) bool {
	return ref.Validate() == nil && snapshot.ID == snapshot.DocHash && ref == snapshot.DocumentRef()
}

// VerifySessionDocIdentity is an explicit opt-in verifier for future dual
// readers. Existing production consumers remain behind ValidateSessionDocHash's
// legacy-only fence. Success verifies the supplied full CIR, never ownership.
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
	return validateRootSessionDoc(ctx, doc)
}

func validateRootSessionDoc(ctx context.Context, doc SessionDoc) error {
	// The shared builder fully verifies the materialized CIR and all produced
	// chunks. Check its result against the claimed root without a second scan.
	// Its synchronous work uses a background context; cancellation is observed
	// at the boundaries here, not cooperatively during the builder itself.
	manifest, _, err := ConversationManifestForCIR(doc.CIR)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	got, err := ConversationManifestHash(manifest)
	if err != nil {
		return err
	}
	if got != doc.Hash {
		return fmt.Errorf("%w: root hash mismatch: got %s want %s", ErrHashMismatch, got, doc.Hash)
	}
	return ctx.Err()
}
