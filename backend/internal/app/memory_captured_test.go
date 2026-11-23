package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type poisonedMemoryBlob struct{ outbound.BlobStore }

func (*poisonedMemoryBlob) GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	panic("detached memory projection read live blob")
}

type capturedMemoryCancelAfterCheck struct {
	context.Context
	checks int
	after  int
	cancel context.CancelFunc
}

func (bound *capturedMemoryCancelAfterCheck) Err() error {
	bound.checks++
	err := bound.Context.Err()
	if bound.checks == bound.after && bound.cancel != nil {
		bound.cancel()
	}
	return err
}
