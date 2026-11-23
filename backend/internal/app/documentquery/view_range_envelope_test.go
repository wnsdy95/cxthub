package documentquery

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type externalEnvelopeGenerationStore struct {
	*legacyFindGenerationStore
	generation       domain.DocRangeGeneration
	binding          domain.DocReadGenerationBinding
	externalEnvelope json.RawMessage
	envelopeError    error
	onEnvelope       func()
	envelopeReads    int
}

type externalEnvelopeGenerationRead struct {
	outbound.DocRangeGenerationRead
	storage *externalEnvelopeGenerationStore
}

func (storage *externalEnvelopeGenerationStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if repo != storage.repo || hash != storage.generation.Hash {
		return nil, domain.ErrNotFound
	}
	if err := storage.binding.Validate(); err != nil {
		return nil, err
	}
	return &externalEnvelopeGenerationRead{storage: storage}, nil
}

func (read *externalEnvelopeGenerationRead) Descriptor() domain.DocRangeGeneration {
	return read.storage.generation
}

func (read *externalEnvelopeGenerationRead) Binding() domain.DocReadGenerationBinding {
	return read.storage.binding
}

func (read *externalEnvelopeGenerationRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	read.storage.envelopeReads++
	if read.storage.onEnvelope != nil {
		read.storage.onEnvelope()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := read.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(read.storage.externalEnvelope), read.storage.envelopeError
}
