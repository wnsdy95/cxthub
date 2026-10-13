package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) rootEventProjection(ctx context.Context, repo, hash domain.ContentHash, selection domain.RootEventSelection) (*domain.VerifiedRootEvents, error) {
	reader, ok := service.blobs.(outbound.VerifiedRootEventReader)
	if !ok {
		return nil, nil
	}
	snapshot, err := service.meta.GetSnapshot(ctx, repo, hash)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
		return nil, err
	}
	if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
		return nil, nil
	}
	projection, err := reader.ReadVerifiedRootEvents(ctx, repo, hash, selection)
	if err != nil {
		return nil, err
	}
	if !projection.Reference().Matches(snapshot) {
		return nil, domain.ErrIntegrity
	}
	return &projection, ctx.Err()
}

func (service *Service) docEventBaseHashes(ctx context.Context, repo, hash domain.ContentHash) ([]domain.ContentHash, error) {
	projection, err := service.rootEventProjection(ctx, repo, hash, domain.RootEventSelection{HashesOnly: true})
	if err != nil {
		return nil, err
	}
	if projection != nil {
		return projection.EventHashes()
	}
	index, err := service.docReadIndex(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	hashes := make([]domain.ContentHash, len(index.Events))
	for position, event := range index.Events {
		hashes[position] = event.Hash
	}
	return hashes, ctx.Err()
}
