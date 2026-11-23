package http

import (
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Publication containers must not replace an earlier identity declaration.
// Decode directly into the typed value so CIR bodies are parsed only once.
type publicationField[T any] struct {
	value T
	seen  bool
}

func (field *publicationField[T]) UnmarshalJSON(raw []byte) error {
	if field.seen {
		return domain.ErrUnsupportedDocumentIdentity
	}
	field.seen = true
	return json.Unmarshal(raw, &field.value)
}

func (body *objectsBody) UnmarshalJSON(raw []byte) error {
	input := struct {
		Snapshots    publicationField[[]domain.Snapshot]     `json:"snapshots"`
		Docs         publicationField[[]domain.SessionDoc]   `json:"docs"`
		ChunkedDocs  publicationField[[]inbound.ChunkedDoc]  `json:"chunked_docs"`
		ChunkObjects publicationField[[]inbound.ChunkObject] `json:"chunk_objects"`
	}{
		Snapshots:    publicationField[[]domain.Snapshot]{value: body.Snapshots},
		Docs:         publicationField[[]domain.SessionDoc]{value: body.Docs},
		ChunkedDocs:  publicationField[[]inbound.ChunkedDoc]{value: body.ChunkedDocs},
		ChunkObjects: publicationField[[]inbound.ChunkObject]{value: body.ChunkObjects},
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	body.Snapshots, body.Docs = input.Snapshots.value, input.Docs.value
	body.ChunkedDocs, body.ChunkObjects = input.ChunkedDocs.value, input.ChunkObjects.value
	return nil
}
