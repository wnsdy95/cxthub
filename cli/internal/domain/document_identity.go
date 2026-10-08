package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
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

// DocumentRepresentation is the tagged chunk-descriptor wire union. Its
// helpers validate metadata only: callers must still verify current bodies
// with VerifyConversationManifest (or the existing legacy body verifier).
// Legacy uses Format/Envelope/Chunks; root uses only RootManifest.
type DocumentRepresentation struct {
	Hash         ContentHash
	Identity     DocumentIdentity
	Format       string
	Envelope     json.RawMessage
	Chunks       []ContentHash
	RootManifest json.RawMessage
}

func (doc DocumentRepresentation) DocumentRef() DocumentRef {
	return DocumentRef{Hash: doc.Hash, Identity: doc.Identity}
}

// ConversationManifest returns independent, canonical root metadata bound to
// Hash. It deliberately neither loads bodies nor returns a verified proof.
func (doc DocumentRepresentation) ConversationManifest() (ConversationManifest, error) {
	if err := doc.DocumentRef().Validate(); err != nil {
		return ConversationManifest{}, err
	}
	if doc.Identity != DocumentIdentityRootV1 || doc.Format != "" || doc.Envelope != nil || doc.Chunks != nil {
		return ConversationManifest{}, fmt.Errorf("%w: ambiguous root representation", ErrHashMismatch)
	}
	manifest, err := DecodeConversationManifest(doc.RootManifest)
	if err != nil {
		return ConversationManifest{}, err
	}
	hash, err := ConversationManifestHash(manifest)
	if err != nil {
		return ConversationManifest{}, err
	}
	if hash != doc.Hash {
		return ConversationManifest{}, ErrHashMismatch
	}
	return manifest, nil
}

func (doc DocumentRepresentation) MarshalJSON() ([]byte, error) {
	if err := doc.DocumentRef().Validate(); err != nil {
		return nil, err
	}
	if doc.Identity == DocumentIdentityRootV1 {
		if _, err := doc.ConversationManifest(); err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Hash         ContentHash      `json:"hash"`
			Identity     DocumentIdentity `json:"identity"`
			RootManifest json.RawMessage  `json:"root_manifest"`
		}{doc.Hash, doc.Identity, doc.RootManifest})
	}
	if doc.RootManifest != nil {
		return nil, fmt.Errorf("%w: root manifest requires explicit identity", ErrHashMismatch)
	}
	// Preserve the old field order, omission and nil encoding exactly.
	return json.Marshal(struct {
		Hash     ContentHash     `json:"hash"`
		Format   string          `json:"format,omitempty"`
		Envelope json.RawMessage `json:"envelope"`
		Chunks   []ContentHash   `json:"chunks"`
	}{doc.Hash, doc.Format, doc.Envelope, doc.Chunks})
}

// UnmarshalJSON rejects ambiguous outer tags before choosing a representation.
// RootManifest's canonical inner encoding is checked by the shared codec.
// A failed decode leaves the receiver unchanged; no malformed root falls back.
func (doc *DocumentRepresentation) UnmarshalJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return ErrHashMismatch
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrHashMismatch
	}
	var next DocumentRepresentation
	fields := map[string]any{"hash": &next.Hash, "identity": &next.Identity, "format": &next.Format, "envelope": &next.Envelope, "chunks": &next.Chunks, "root_manifest": &next.RootManifest}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		target, known := fields[key]
		if err != nil || !ok || !known || seen[key] {
			return fmt.Errorf("%w: ambiguous document field", ErrHashMismatch)
		}
		seen[key] = true
		if err := decoder.Decode(target); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrHashMismatch
	}
	if _, err := decoder.Token(); err != io.EOF || !seen["hash"] {
		return ErrHashMismatch
	}
	if next.Identity == DocumentIdentityRootV1 {
		if seen["format"] || seen["envelope"] || seen["chunks"] || !seen["root_manifest"] {
			return ErrHashMismatch
		}
	} else if seen["root_manifest"] || !seen["envelope"] || !seen["chunks"] {
		return ErrHashMismatch
	}
	if _, err := next.MarshalJSON(); err != nil {
		return err
	}
	*doc = next
	return nil
}
