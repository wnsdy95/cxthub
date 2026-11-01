package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docVisibleReadSelection struct {
	selection domain.DocVisibleSelector
	source    docViewSource
	units     outbound.DocVisibleGenerationRead
}

func (service *Service) openDocVisibleSelection(ctx context.Context, selection domain.DocVisibleSelector, continuation bool) (docVisibleReadSelection, error) {
	var empty docVisibleReadSelection
	if err := service.requireVisibleRoot(ctx, selection.Repo, selection.Hash); err != nil {
		return empty, err
	}
	storage := service.blobs.(outbound.DocVisibleGenerationStore)
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
	source, err := service.docViewSource(ctx, selection.Repo, selection.Hash, false)
	if err != nil {
		return empty, err
	}
	generation := units.Descriptor()
	if !source.root || source.binding != units.Binding() || source.total != generation.Events {
		return empty, domain.ErrIntegrity
	}
	var previous docViewSource
	if selection.Base != "" {
		if _, ok := service.blobs.(outbound.DocRangeBoundaryStore); !ok {
			return empty, domain.ErrDocVisibleProjectionUnsupported
		}
		previous, err = service.openDocVisibleBase(ctx, selection.Repo, selection.Base)
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
		inherited, err = service.docViewBoundary(ctx, selection.Repo, selection.Hash, selection.Base, source, previous)
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
