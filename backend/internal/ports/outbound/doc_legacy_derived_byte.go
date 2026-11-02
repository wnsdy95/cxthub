package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type LegacyDerivedByteRangeReader interface {
	ReadLegacyDerivedByteChunk(context.Context, domain.ContentHash, domain.ContentHash, domain.DocReadGenerationBinding, int, domain.ConversationManifestChunk) ([]byte, error)
}

type LegacyDerivedByteRangePublicationStore interface {
	ConfigureLegacyDerivedByteRangePublication(bool)
}
