package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

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
		return ConversationManifest{}, fmt.Errorf("%w: ambiguous root representation", ErrIntegrity)
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
		return ConversationManifest{}, ErrIntegrity
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
		return nil, fmt.Errorf("%w: root manifest requires explicit identity", ErrIntegrity)
	}
	// Preserve the old field order, omission and nil encoding exactly.
	return json.Marshal(struct {
		Hash     ContentHash     `json:"hash"`
		Format   string          `json:"format,omitempty"`
		Envelope json.RawMessage `json:"envelope"`
		Chunks   []ContentHash   `json:"chunks"`
	}{doc.Hash, doc.Format, doc.Envelope, doc.Chunks})
}

// UnmarshalJSON preserves deployed legacy decoding (including skipped unknown
// fields) while requiring an explicit, strict root union. Decode is metadata
// only; legacy required fields are checked at the command/body boundary.
func (doc *DocumentRepresentation) UnmarshalJSON(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return ErrIntegrity
	}
	var declaration struct {
		Identity     singleDocumentIdentity `json:"identity"`
		RootManifest documentRootPresence   `json:"root_manifest"`
	}
	if err := json.Unmarshal(raw, &declaration); err != nil {
		return err
	}
	if declaration.Identity.value == DocumentIdentityLegacy {
		if declaration.RootManifest {
			return ErrIntegrity
		}
		// Match encoding/json's existing case folding and last-wins behavior
		// for nonidentity legacy fields. Never materialize skipped extensions.
		var legacy struct {
			Hash     ContentHash     `json:"hash"`
			Format   string          `json:"format"`
			Envelope json.RawMessage `json:"envelope"`
			Chunks   []ContentHash   `json:"chunks"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return err
		}
		*doc = DocumentRepresentation{Hash: legacy.Hash, Format: legacy.Format, Envelope: legacy.Envelope, Chunks: legacy.Chunks}
		return nil
	}
	if !utf8.Valid(raw) || len(raw) > MaxConversationManifestBytes+1024 {
		return ErrIntegrity
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrIntegrity
	}
	var next DocumentRepresentation
	fields := map[string]any{"hash": &next.Hash, "identity": &next.Identity, "root_manifest": &next.RootManifest}
	seen := make(map[string]bool)
	for decoder.More() {
		start := decoder.InputOffset()
		token, err := decoder.Token()
		key, ok := token.(string)
		target, known := fields[key]
		if err != nil || !ok || !known || seen[key] {
			return ErrIntegrity
		}
		spelling := bytes.TrimSpace(raw[start:decoder.InputOffset()])
		spelling = bytes.TrimSpace(bytes.TrimPrefix(spelling, []byte(",")))
		if !bytes.Equal(spelling, []byte(`"`+key+`"`)) {
			return ErrIntegrity
		}
		seen[key] = true
		if err := decoder.Decode(target); err != nil {
			return err
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrIntegrity
	}
	if _, err := decoder.Token(); err != io.EOF || len(seen) != 3 || next.Identity != DocumentIdentityRootV1 {
		return ErrIntegrity
	}
	if _, err := next.ConversationManifest(); err != nil {
		return err
	}
	*doc = next
	return nil
}

// Presence includes null and case-folded spellings. A malformed root declaration
// cannot be silently discarded by the compatible legacy decoder.
type documentRootPresence bool

func (present *documentRootPresence) UnmarshalJSON(_ []byte) error { *present = true; return nil }
