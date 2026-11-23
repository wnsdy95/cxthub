package documentquery

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (queries *Queries) openDocViewBase(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, error) {
	if storage := queries.view.RangeGenerations; storage != nil && storage.InReadOnlyTransaction(ctx) {
		snapshot, err := queries.metadata.GetSnapshot(ctx, repo, hash)
		if errors.Is(err, domain.ErrNotFound) {
			return queries.docViewSnapshotSource(ctx, repo, hash, nil, true)
		}
		if err != nil {
			return docViewSource{}, err
		}
		if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
			return docViewSource{}, err
		}
		if snapshot.DocIdentity == domain.DocumentIdentityLegacy {
			return openLegacyDocRangeMetadataSource(ctx, storage, repo, hash)
		}
		return queries.docViewSnapshotSource(ctx, repo, hash, &snapshot, false)
	}
	return queries.docViewSource(ctx, repo, hash, true)
}

func docViewInherited(ctx context.Context, source, base docViewSource) (int, error) {
	inherited, total := 0, min(source.total, base.total)
	for inherited < total {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		page := inherited / domain.DocRangePageEvents
		if inherited%domain.DocRangePageEvents == 0 && source.block != nil && base.block != nil {
			left, err := source.block(page)
			if err != nil {
				return 0, err
			}
			right, err := base.block(page)
			if err != nil {
				return 0, err
			}
			if left == right {
				inherited = min(total, inherited+domain.DocRangePageEvents)
				continue
			}
		}
		left, err := source.location(inherited)
		if err != nil {
			return 0, err
		}
		right, err := base.location(inherited)
		if err != nil {
			return 0, err
		}
		if left.Hash != right.Hash {
			break
		}
		inherited++
	}
	return inherited, nil
}

func (queries *Queries) docViewBoundary(ctx context.Context, repo, hash, base domain.ContentHash, source, previous docViewSource) (int, error) {
	storage := queries.docBoundaryRead()
	durable := source.root && previous.root || source.binding.Validate() == nil && previous.binding.Validate() == nil
	if storage == nil || !durable {
		return docViewInherited(ctx, source, previous)
	}
	if !storage.InReadOnlyTransaction(ctx) {
		return 0, domain.ErrConflict
	}
	inherited, err := storage.DocRangeBoundary(ctx, repo, hash, base)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return 0, errDocViewBoundaryNeedsPreparation
	}
	if err != nil {
		return 0, err
	}
	if inherited < 0 || inherited > min(source.total, previous.total) {
		return 0, domain.ErrIntegrity
	}
	return inherited, nil
}
