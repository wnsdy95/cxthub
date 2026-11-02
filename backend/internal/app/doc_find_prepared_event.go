package app

import (
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docPreparedFindEvent struct {
	hash   domain.ContentHash
	pages  int
	page   func(int) (domain.DocDisplayPage, error)
	window func(*domain.DocDisplayPage, domain.DocDisplayPage) (domain.DocDisplaySearchWindow, error)
}

func openDocPreparedFindEvent(source docViewSource, metadata domain.DocRangeEntry, pinned domain.ContentHash) (docPreparedFindEvent, error) {
	entry := metadata.DocEventIndex
	switch metadata.Kind {
	case domain.EventToolResult:
		if source.displayGeneration == nil || source.displayPage == nil {
			return docPreparedFindEvent{}, domain.ErrDocFindDisplayUnavailable
		}
		generation, err := source.displayGeneration(entry)
		if err != nil {
			var missing *docDisplayPreparationRequired
			if errors.As(err, &missing) && source.eventDisplayGeneration != nil && source.eventDisplayPage != nil {
				prepared, projectionError := openDocPreparedProjectionEvent(source, entry)
				if projectionError == nil {
					if pinned != "" && pinned != prepared.hash {
						return docPreparedFindEvent{}, domain.ErrIntegrity
					}
					return prepared, nil
				}
				if !errors.As(projectionError, &missing) {
					return docPreparedFindEvent{}, projectionError
				}
			}
			if pinned != "" && errors.As(err, &missing) {
				return docPreparedFindEvent{}, domain.ErrIntegrity
			}
			return docPreparedFindEvent{}, err
		}
		encoded, err := generation.Encode()
		if err != nil || generation.Source.EventHash != entry.Hash || generation.Source.EventBytes != entry.Length {
			return docPreparedFindEvent{}, domain.ErrIntegrity
		}
		if pinned != "" && pinned != domain.HashContent(encoded) {
			if source.eventDisplayGeneration == nil || source.eventDisplayPage == nil {
				return docPreparedFindEvent{}, domain.ErrIntegrity
			}
			prepared, err := openDocPreparedProjectionEvent(source, entry)
			if err != nil {
				var missing *docDisplayPreparationRequired
				if errors.As(err, &missing) {
					return docPreparedFindEvent{}, domain.ErrIntegrity
				}
				return docPreparedFindEvent{}, err
			}
			if prepared.hash != pinned {
				return docPreparedFindEvent{}, domain.ErrIntegrity
			}
			return prepared, nil
		}
		return docPreparedFindEvent{hash: domain.HashContent(encoded), pages: generation.Pages.Count,
			page: func(position int) (domain.DocDisplayPage, error) { return source.displayPage(entry, position) },
			window: func(previous *domain.DocDisplayPage, current domain.DocDisplayPage) (domain.DocDisplaySearchWindow, error) {
				return domain.BuildDocDisplaySearchWindow(generation.Source, previous, current)
			}}, nil
	case domain.EventReasoning, domain.EventMessage, domain.EventToolCall:
		return openDocPreparedProjectionEvent(source, entry)
	default:
		return docPreparedFindEvent{}, domain.ErrDocFindDisplayUnavailable
	}
}

func openDocPreparedProjectionEvent(source docViewSource, entry domain.DocEventIndex) (docPreparedFindEvent, error) {
	if source.eventDisplayGeneration == nil || source.eventDisplayPage == nil {
		return docPreparedFindEvent{}, domain.ErrDocFindDisplayUnavailable
	}
	generation, err := source.eventDisplayGeneration(entry)
	if err != nil {
		return docPreparedFindEvent{}, err
	}
	encoded, err := generation.Encode()
	if err != nil || generation.Source.EventHash != entry.Hash || generation.Source.EventBytes != entry.Length {
		return docPreparedFindEvent{}, domain.ErrIntegrity
	}
	return docPreparedFindEvent{hash: domain.HashContent(encoded), pages: generation.Pages.Count,
		page: func(position int) (domain.DocDisplayPage, error) { return source.eventDisplayPage(entry, position) },
		window: func(previous *domain.DocDisplayPage, current domain.DocDisplayPage) (domain.DocDisplaySearchWindow, error) {
			return domain.BuildDocDisplayProjectionSearchWindow(generation.Source, previous, current)
		}}, nil
}
