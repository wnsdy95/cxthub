package inbound

import (
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Root manifests are not supported by the current publication adapter. Any
// presence, even null, must fail instead of being discarded as an unknown key.
type unsupportedRootManifest struct{}

func (*unsupportedRootManifest) UnmarshalJSON([]byte) error {
	return domain.ErrUnsupportedDocumentIdentity
}

func (doc *ChunkedDoc) UnmarshalJSON(raw []byte) error {
	// Reuse the enclosing-object discriminator guard; unknown legacy manifest
	// fields are skipped, without allocating their values or a second CIR body.
	var declaration domain.SessionDoc
	if err := json.Unmarshal(raw, &declaration); err != nil {
		return err
	}
	type wire ChunkedDoc
	next := wire(*doc)
	input := struct {
		*wire
		RootManifest unsupportedRootManifest `json:"root_manifest"`
	}{wire: &next}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	*doc = ChunkedDoc(next)
	return nil
}
