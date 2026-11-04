package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docFindDisplayBuildBytes = 512 << 10

func scanDocDisplayFindRange(ctx context.Context, source docViewSource, selector *docFindProgress, query string) (domain.DocFindPage, error) {
	if selector.Field != 0 || domain.ValidateDocFindQuery(query) != nil {
		return domain.DocFindPage{}, domain.ErrValidation
	}
	page := domain.DocFindPage{From: selector.From, To: selector.To, Matches: []domain.DocFindMatch{}}
	readBytes, displayBytes, events := 0, 0, 0
	for selector.Index < selector.To && readBytes < docFindScanBytes && displayBytes < docFindDisplayBuildBytes && events < docFindScanEvents && len(page.Matches) < domain.DocFindPageMatches {
		if err := ctx.Err(); err != nil {
			return domain.DocFindPage{}, err
		}
		metadata, err := source.entry(selector.Index)
		if err != nil {
			return domain.DocFindPage{}, err
		}
		if metadata.EmptyReasoning || metadata.Kind == domain.EventCompaction {
			selector.Index++
			selector.Byte = 0
			events++
			continue
		}
		entry := metadata.DocEventIndex
		if entry.Length <= docViewBytes && readBytes+entry.Length > docFindScanBytes || entry.Length > docViewBytes && docFindScanBytes-readBytes < docViewBytes+domain.MaxDocFindFieldsBytes {
			break
		}
		display, scanned, err := readDocFindDisplay(ctx, source, metadata, docFindDisplayBuildBytes-displayBytes)
		readBytes += scanned
		if errors.Is(err, domain.ErrDocFindDisplayUnavailable) && displayBytes > 0 {
			break
		}
		if err != nil {
			return domain.DocFindPage{}, err
		}
		cost := docFindDisplayCost(display)
		if cost > docFindDisplayBuildBytes-displayBytes {
			return domain.DocFindPage{}, domain.ErrIntegrity
		}
		displayBytes += cost
		previous, first := selector.Index, len(page.Matches)
		if err := scanDocFindTexts(ctx, []string{display.Text()}, selector, query, &page); err != nil {
			return domain.DocFindPage{}, err
		}
		if err := mapDocFindDisplayMatches(display, entry, page.Matches[first:]); err != nil {
			return domain.DocFindPage{}, err
		}
		if selector.Index != previous {
			events++
		}
	}
	if err := ctx.Err(); err != nil {
		return domain.DocFindPage{}, err
	}
	page.Index, page.Field, page.Byte = selector.Index, selector.Field, selector.Byte
	return page, nil
}

func docFindDisplayCost(display domain.DocFindDisplayText) int {
	if shifted, ok := display.(docFindDisplayShift); ok {
		return 32 + domain.DocFindDisplayBytes(shifted.display)
	}
	return domain.DocFindDisplayBytes(display)
}

func mapDocFindDisplayMatches(display domain.DocFindDisplayText, entry domain.DocEventIndex, matches []domain.DocFindMatch) error {
	for index := range matches {
		match := &matches[index]
		span, err := display.SourceRange(match.Offset, match.Length)
		if err != nil {
			return err
		}
		if span.From < 0 || span.From >= span.To || span.To > entry.Length {
			return domain.ErrIntegrity
		}
		match.SourceRange = &span
	}
	return nil
}

func readDocFindDisplay(ctx context.Context, source docViewSource, metadata domain.DocRangeEntry, budget int) (domain.DocFindDisplayText, int, error) {
	entry := metadata.DocEventIndex
	if entry.Length <= docViewBytes {
		raw, err := source.read(entry.Offset, entry.Length)
		if err != nil {
			return nil, 0, err
		}
		if _, err := domain.DecodeIndexedEvent(raw, entry); err != nil {
			return nil, 0, err
		}
		display, err := domain.BuildDocEventDisplayWithin(ctx, raw, budget)
		return display, len(raw), err
	}
	if metadata.Kind != domain.EventMessage && metadata.Kind != domain.EventReasoning {
		return nil, 0, domain.ErrDocFindDisplayUnavailable
	}
	if source.findFields == nil {
		return nil, 0, domain.ErrDocFindFieldsUnavailable
	}
	fields, err := source.findFields(entry)
	if err != nil {
		return nil, 0, err
	}
	if err := fields.Validate(entry.Hash, entry.Length); err != nil {
		return nil, 0, err
	}
	selected := fields.Summary
	if metadata.Kind == domain.EventMessage {
		selected = fields.Blocks
	}
	if selected == nil {
		if metadata.Kind == domain.EventReasoning {
			return nil, 0, domain.ErrIntegrity
		}
		display, err := domain.BuildDocPublicDisplayWithin(ctx, []byte("[]"), domain.EventMessage, false, budget)
		return display, domain.MaxDocFindFieldsBytes, err
	}
	if selected.To-selected.From > docViewBytes {
		return nil, 0, domain.ErrDocFindDisplayUnavailable
	}
	raw, err := source.read(entry.Offset+selected.From, selected.To-selected.From)
	if err != nil {
		return nil, 0, err
	}
	display, err := domain.BuildDocPublicDisplayWithin(ctx, raw, metadata.Kind, metadata.CompactSummary || metadata.SeedSummary, budget-32)
	if err != nil {
		return nil, len(raw) + domain.MaxDocFindFieldsBytes, err
	}
	return docFindDisplayShift{display: display, shift: selected.From}, len(raw) + domain.MaxDocFindFieldsBytes, nil
}

type docFindDisplayShift struct {
	display domain.DocFindDisplayText
	shift   int
}

func (display docFindDisplayShift) Text() string { return display.display.Text() }
func (display docFindDisplayShift) SourceRange(offset, length int) (domain.DocFindSpan, error) {
	span, err := display.display.SourceRange(offset, length)
	if err != nil {
		return domain.DocFindSpan{}, err
	}
	span.From, span.To = span.From+display.shift, span.To+display.shift
	return span, nil
}
