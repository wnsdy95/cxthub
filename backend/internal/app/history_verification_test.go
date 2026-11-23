package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type countingHistoryBlobs struct {
	outbound.BlobStore
	reads   map[domain.ContentHash]int
	corrupt bool
}

func (b *countingHistoryBlobs) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	b.reads[hash]++
	doc, err := b.BlobStore.GetDoc(ctx, repo, hash)
	if b.corrupt {
		doc.CIR.Envelope.SourceProvider = "tampered"
	}
	return doc, err
}
