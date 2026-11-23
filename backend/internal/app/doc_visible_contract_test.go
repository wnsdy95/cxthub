package app

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docVisibleContractMeta struct {
	outbound.MetadataStore
}

func (*docVisibleContractMeta) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, domain.ErrNotFound
}

func (*docVisibleContractMeta) GetSnapshot(_ context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	return domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo, DocIdentity: domain.DocumentIdentityRootV1}, nil
}

func (*docVisibleContractMeta) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	return read(ctx)
}

func (*docVisibleContractMeta) WithinRepository(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

type docVisibleContractStore struct {
	outbound.BlobStore
	outbound.DocRangeStore
	outbound.ConversationChunkReader
	ranges              domain.DocRangeGeneration
	visible             domain.DocVisibleGeneration
	binding             domain.DocReadGenerationBinding
	pageHashReads       int
	onVisibleDescriptor func()
}

func (*docVisibleContractStore) InReadOnlyTransaction(context.Context) bool {
	return true
}

func (*docVisibleContractStore) HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return true, nil
}

func (*docVisibleContractStore) EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	panic("unexpected preparation write")
}

func (storage *docVisibleContractStore) OpenDocRangeGeneration(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	return &docVisibleContractRangeRead{storage: storage}, nil
}

type docVisibleContractRangeRead struct {
	outbound.DocRangeGenerationRead
	storage *docVisibleContractStore
}

func (read *docVisibleContractRangeRead) Descriptor() domain.DocRangeGeneration {
	return read.storage.ranges
}

func (read *docVisibleContractRangeRead) Binding() domain.DocReadGenerationBinding {
	return read.storage.binding
}

func (read *docVisibleContractRangeRead) Envelope(ctx context.Context) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := read.Binding().Validate(); err != nil {
		return nil, err
	}
	return bytes.Clone(read.storage.ranges.Envelope), nil
}

func (read *docVisibleContractRangeRead) PageHash(context.Context, int) (domain.ContentHash, error) {
	read.storage.pageHashReads++
	return h('4'), nil
}
