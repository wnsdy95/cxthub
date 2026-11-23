package app

import (
	"context"

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
