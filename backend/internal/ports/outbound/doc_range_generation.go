package outbound

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocRangeGenerationRead interface {
	Descriptor() domain.DocRangeGeneration
	Binding() domain.DocReadGenerationBinding
	Envelope(context.Context) (json.RawMessage, error)
	PageHash(context.Context, int) (domain.ContentHash, error)
	ReadPage(context.Context, int) ([]domain.DocRangeEntry, error)
	Chunk(context.Context, int) (domain.ConversationManifestChunk, error)
}

type DocRangeGenerationStore interface {
	RepositoryTransactionState
	OpenDocRangeGeneration(context.Context, domain.ContentHash, domain.ContentHash) (DocRangeGenerationRead, error)
}

type LegacyByteRangePublicationStore interface {
	ConfigureLegacyByteRangePublication(bool)
}
