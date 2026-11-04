package documentquery

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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

func (queries *Queries) openDocVisibleBase(ctx context.Context, repo, hash domain.ContentHash) (docViewSource, error) {
	snapshot, err := queries.metadata.GetSnapshot(ctx, repo, hash)
	if err != nil {
		return docViewSource{}, err
	}
	if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
		return docViewSource{}, err
	}
	if snapshot.DocIdentity != domain.DocumentIdentityLegacy {
		if err := queries.checkVisibleRootSnapshot(hash, snapshot); err != nil {
			return docViewSource{}, err
		}
		return queries.docViewSource(ctx, repo, hash, false)
	}
	storage := queries.view.RangeGenerations
	if storage == nil {
		return docViewSource{}, domain.ErrDocVisibleProjectionUnsupported
	}
	return openLegacyDocRangeMetadataSource(ctx, storage, repo, hash)
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

type docVisibleReadSelection struct {
	selection domain.DocVisibleSelector
	source    docViewSource
	units     outbound.DocVisibleGenerationRead
}

func (queries *Queries) requireVisibleRoot(ctx context.Context, repo, hash domain.ContentHash) error {
	snapshot, err := queries.metadata.GetSnapshot(ctx, repo, hash)
	if err != nil {
		return err
	}
	return queries.checkVisibleRootSnapshot(hash, snapshot)
}

func (queries *Queries) checkVisibleRootSnapshot(hash domain.ContentHash, snapshot domain.Snapshot) error {
	if err := matchReadSnapshot(domain.DocumentRef{Hash: hash, Identity: snapshot.DocIdentity}, snapshot); err != nil {
		return err
	}
	if snapshot.DocIdentity != domain.DocumentIdentityRootV1 || queries.view.RangeGenerations == nil ||
		queries.view.VisibleGenerations == nil || queries.view.Range == nil || queries.view.Chunks == nil {
		return domain.ErrDocVisibleProjectionUnsupported
	}
	return nil
}

func (queries *Queries) openDocVisibleSelection(ctx context.Context, selection domain.DocVisibleSelector, continuation bool) (docVisibleReadSelection, error) {
	var empty docVisibleReadSelection
	if err := queries.requireVisibleRoot(ctx, selection.Repo, selection.Hash); err != nil {
		return empty, err
	}
	storage := queries.view.VisibleGenerations
	if !storage.InReadOnlyTransaction(ctx) {
		return empty, domain.ErrConflict
	}
	units, err := storage.OpenDocVisibleGeneration(ctx, selection.Repo, selection.Hash)
	if errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		return empty, errDocViewGenerationNeedsPreparation
	}
	if err != nil {
		return empty, err
	}
	source, err := queries.docViewSource(ctx, selection.Repo, selection.Hash, false)
	if err != nil {
		return empty, err
	}
	generation := units.Descriptor()
	if !source.root || source.binding != units.Binding() || source.total != generation.Events {
		return empty, domain.ErrIntegrity
	}
	var previous docViewSource
	if selection.Base != "" {
		if queries.docBoundaryRead() == nil {
			return empty, domain.ErrDocVisibleProjectionUnsupported
		}
		previous, err = queries.openDocVisibleBase(ctx, selection.Repo, selection.Base)
		if err != nil {
			return empty, err
		}
	}
	binding, err := visibleSelectionGeneration(generation, source.binding, selection.Base, previous.binding)
	if err != nil {
		return empty, err
	}
	if continuation && binding != selection.Generation {
		return empty, domain.ErrDocVisibleGenerationStale
	}
	inherited := 0
	if selection.Base != "" {
		inherited, err = queries.docViewBoundary(ctx, selection.Repo, selection.Hash, selection.Base, source, previous)
		if err != nil {
			return empty, err
		}
	}
	if continuation && (selection.Total != source.total || selection.Inherited != inherited) {
		return empty, domain.ErrValidation
	}
	selection.Generation, selection.Total, selection.Inherited = binding, source.total, inherited
	if !continuation && selection.Start == -1 {
		selection.Start = inherited
	}
	if selection.Start > source.total {
		return empty, domain.ErrValidation
	}
	return docVisibleReadSelection{selection: selection, source: source, units: units}, nil
}

func visibleSelectionGeneration(generation domain.DocVisibleGeneration, source domain.DocReadGenerationBinding, base domain.ContentHash, previous domain.DocReadGenerationBinding) (domain.ContentHash, error) {
	raw, err := generation.Encode()
	if err != nil {
		return "", err
	}
	if source.Validate() != nil || source.RangeGeneration != generation.RangeGeneration || base != "" && previous.Validate() != nil {
		return "", domain.ErrIntegrity
	}
	encoded, err := json.Marshal(struct {
		Projection string
		Descriptor domain.ContentHash
		Source     domain.DocReadGenerationBinding
		Base       domain.ContentHash
		Previous   domain.DocReadGenerationBinding
	}{domain.DocVisibleProjection, domain.HashContent(raw), source, base, previous})
	if err != nil {
		return "", err
	}
	return domain.HashContent(encoded), nil
}
