package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docFindPreparedWindows = 8

type docDisplayPreparationRequired struct {
	target domain.DocDisplayTarget
}

func (failure *docDisplayPreparationRequired) Error() string {
	return "display preparation is required"
}

func resetDocPreparedEvent(selector *docFindProgress) {
	selector.Index++
	selector.Field, selector.Byte, selector.Prepared = 0, 0, domain.DocFindPreparedProgress{}
}

func scanDocPreparedFindEvent(ctx context.Context, source docViewSource, metadata domain.DocRangeEntry, selector *docFindProgress, query string, page *domain.DocFindPage, budget int) (int, int, error) {
	prepared, err := openDocPreparedFindEvent(source, metadata, selector.Prepared.Generation)
	if err != nil {
		return 0, 0, err
	}
	if selector.Prepared.Generation != "" && selector.Prepared.Generation != prepared.hash {
		return 0, 0, domain.ErrIntegrity
	}
	selector.Prepared.Generation = prepared.hash
	if prepared.pages == 0 {
		if selector.Prepared.Page != 0 || selector.Byte != 0 {
			return 0, 0, domain.ErrValidation
		}
		resetDocPreparedEvent(selector)
		return 0, 0, nil
	}
	if selector.Prepared.Page >= prepared.pages {
		return 0, 0, domain.ErrValidation
	}
	current, err := prepared.page(selector.Prepared.Page)
	if err != nil {
		return 0, 0, err
	}
	if current.Index != selector.Prepared.Page {
		return 0, 0, domain.ErrIntegrity
	}
	var previous *domain.DocDisplayPage
	if selector.Prepared.Page > 0 {
		prior, err := prepared.page(selector.Prepared.Page - 1)
		if err != nil {
			return 0, 0, err
		}
		previous = &prior
	}
	cost := 128 + 2*len(current.Text) + len(current.Runs)*64
	if previous != nil {
		cost += len(previous.Text) + len(previous.Runs)*64 + 4*(domain.MaxDocFindQueryRunes-1)
	}
	if cost > budget {
		return 0, 0, nil
	}
	window, err := prepared.window(previous, current)
	if err != nil {
		return 0, 0, err
	}
	if selector.Byte != 0 && (selector.Byte < window.From() || selector.Byte >= window.To()) {
		return 0, 0, domain.ErrValidation
	}
	matches, next, err := domain.FindDocDisplayWindow(ctx, window, query, selector.Byte, domain.DocFindPageMatches-len(page.Matches))
	if err != nil {
		return 0, 0, err
	}
	for _, match := range matches {
		position := selector.Prepared.Page
		match.Index, match.PreparedPage = selector.Index, &position
		page.Matches = append(page.Matches, match)
	}
	selector.Byte = next
	if next == window.To() {
		selector.Prepared.Page++
		selector.Byte = 0
		if selector.Prepared.Page == prepared.pages {
			resetDocPreparedEvent(selector)
		}
	}
	return cost, 1, nil
}

func scanDocPreparedFindRange(ctx context.Context, source docViewSource, selector *docFindProgress, query string) (domain.DocFindPage, error) {
	if selector.Field != 0 || domain.ValidateDocFindQuery(query) != nil {
		return domain.DocFindPage{}, domain.ErrValidation
	}
	page := domain.DocFindPage{From: selector.From, To: selector.To, Matches: []domain.DocFindMatch{}}
	readBytes, displayBytes, events, windows := 0, 0, 0, 0
	for selector.Index < selector.To && readBytes < docFindScanBytes && displayBytes < docFindDisplayBuildBytes && events < docFindScanEvents && windows < docFindPreparedWindows && len(page.Matches) < domain.DocFindPageMatches {
		if err := ctx.Err(); err != nil {
			return domain.DocFindPage{}, err
		}
		metadata, err := source.entry(selector.Index)
		if err != nil {
			return domain.DocFindPage{}, err
		}
		if metadata.EmptyReasoning || metadata.Kind == domain.EventCompaction {
			if selector.Byte != 0 || selector.Prepared.Page != 0 || selector.Prepared.Generation != "" {
				return domain.DocFindPage{}, domain.ErrValidation
			}
			resetDocPreparedEvent(selector)
			events++
			continue
		}
		entry := metadata.DocEventIndex
		preparedKind := metadata.Kind == domain.EventToolResult || metadata.Kind == domain.EventReasoning || metadata.Kind == domain.EventMessage || metadata.Kind == domain.EventToolCall
		prepared := selector.Prepared.Page != 0 || selector.Prepared.Generation != "" || entry.Length > docViewBytes && preparedKind
		if prepared && !preparedKind {
			return domain.DocFindPage{}, domain.ErrValidation
		}
		var display domain.DocFindDisplayText
		if !prepared {
			if entry.Length <= docViewBytes && readBytes+entry.Length > docFindScanBytes || entry.Length > docViewBytes && docFindScanBytes-readBytes < docViewBytes+domain.MaxDocFindFieldsBytes {
				break
			}
			var scanned int
			display, scanned, err = readDocFindDisplay(ctx, source, metadata, docFindDisplayBuildBytes-displayBytes)
			readBytes += scanned
			if err := ctx.Err(); err != nil {
				return domain.DocFindPage{}, err
			}
			if err != nil && scanned == 0 && entry.Length <= docViewBytes {
				return domain.DocFindPage{}, err
			}
			if errors.Is(err, domain.ErrDocFindDisplayUnavailable) && (events > 0 || windows > 0) {
				break
			}
			if errors.Is(err, domain.ErrDocFindDisplayUnavailable) && scanned == entry.Length && entry.Length <= docViewBytes && preparedKind {
				prepared = true
			} else if err != nil {
				return domain.DocFindPage{}, err
			}
		}
		if prepared {
			prior := selector.Index
			cost, scanned, err := scanDocPreparedFindEvent(ctx, source, metadata, selector, query, &page, docFindDisplayBuildBytes-displayBytes)
			var missing *docDisplayPreparationRequired
			if errors.As(err, &missing) && (events > 0 || windows > 0) {
				break
			}
			if err != nil {
				return domain.DocFindPage{}, err
			}
			displayBytes += cost
			windows += scanned
			if selector.Index != prior {
				events++
			} else if scanned == 0 {
				break
			}
			continue
		}
		displayBytes += docFindDisplayCost(display)
		first, prior := len(page.Matches), selector.Index
		if err := scanDocFindTexts(ctx, []string{display.Text()}, selector, query, &page); err != nil {
			return domain.DocFindPage{}, err
		}
		if err := mapDocFindDisplayMatches(display, entry, page.Matches[first:]); err != nil {
			return domain.DocFindPage{}, err
		}
		for index := first; index < len(page.Matches); index++ {
			match := &page.Matches[index]
			position := 0
			match.PreparedPage = &position
		}
		if selector.Index != prior {
			selector.Prepared = domain.DocFindPreparedProgress{}
			events++
		}
	}
	page.Index, page.Field, page.Byte = selector.Index, selector.Field, selector.Byte
	progress := selector.Prepared
	page.Prepared = &progress
	return page, ctx.Err()
}
