package app

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docFindScanBytes = 256 << 10
const docFindFragmentBytes = 64 << 10
const docFindScanEvents = 128

func docFindSelection(repo, hash domain.ContentHash, request domain.DocFindPageRequest) (domain.DocFindSelector, error) {
	var selector domain.DocFindSelector
	if request.Legacy || request.Base != "" || request.Total != 0 || request.Inherited != 0 || validateHashes(repo, hash) != nil || domain.ValidateDocFindQuery(request.Query) != nil || request.Representation != "" && request.Representation != domain.DocFindDisplayRepresentation && request.Representation != domain.DocFindPreparedRepresentation {
		return selector, domain.ErrValidation
	}
	queryHash := domain.HashContent([]byte(request.Query))
	if request.Cursor != "" {
		if request.Selection != "" || request.From != 0 || request.To != 0 {
			return selector, domain.ErrValidation
		}
		var err error
		selector, err = domain.DecodeDocFindSelector(request.Cursor)
		if err != nil || selector.QueryHash != queryHash || selector.Selection.Repo != repo || selector.Selection.Hash != hash || selector.Representation != request.Representation {
			return domain.DocFindSelector{}, domain.ErrValidation
		}
		return selector, nil
	}
	selection, err := domain.DecodeDocVisibleSelector(request.Selection)
	if err != nil || selection.Repo != repo || selection.Hash != hash || selection.Purpose != "units" {
		return selector, domain.ErrValidation
	}
	version := 1
	if request.Representation == domain.DocFindDisplayRepresentation {
		version = 2
	}
	selector = domain.DocFindSelector{Version: version, Representation: request.Representation, Selection: selection, QueryHash: queryHash,
		From: request.From, To: request.To, Index: request.From}
	if request.Representation == domain.DocFindPreparedRepresentation {
		selector.Version, selector.Prepared = 3, &domain.DocFindPreparedProgress{}
	}
	return selector, selector.Validate()
}

func (service *Service) FindDocPage(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindPageRequest) (domain.DocFindPage, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocFindPage{}, err
	}
	return queries.FindDocPage(ctx, repo, hash, request)
}

func scanDocFindPage(ctx context.Context, source docViewSource, selector domain.DocFindSelector, query string) (domain.DocFindPage, error) {
	position := docFindProgress{From: selector.From, To: selector.To, Index: selector.Index, Field: selector.Field, Byte: selector.Byte, Representation: selector.Representation}
	if selector.Prepared != nil {
		position.Prepared = *selector.Prepared
	}
	page, err := scanDocFindRange(ctx, source, &position, query)
	if err != nil {
		return domain.DocFindPage{}, err
	}
	page.Projection, page.Hash, page.Generation = domain.DocFindProjection, selector.Selection.Hash, selector.Selection.Generation
	if selector.Representation == domain.DocFindDisplayRepresentation {
		page.Projection = domain.DocFindDisplayProjection
	}
	if selector.Representation == domain.DocFindPreparedRepresentation {
		page.Projection = domain.DocFindPreparedProjection
		selector.Prepared = &position.Prepared
	}
	if position.Index < position.To {
		selector.Index, selector.Field, selector.Byte = position.Index, position.Field, position.Byte
		next, err := selector.Encode()
		if err != nil {
			return domain.DocFindPage{}, err
		}
		page.NextCursor = &next
	}
	return page, nil
}

type docFindProgress struct {
	From, To, Index, Field, Byte int
	Representation               string
	Prepared                     domain.DocFindPreparedProgress
}

func scanDocFindRange(ctx context.Context, source docViewSource, selector *docFindProgress, query string) (domain.DocFindPage, error) {
	if selector.From < 0 || selector.To < selector.From || selector.To > source.total || selector.Index < selector.From || selector.Index > selector.To || selector.Field < 0 || selector.Byte < 0 {
		return domain.DocFindPage{}, domain.ErrValidation
	}
	if selector.Representation == domain.DocFindPreparedRepresentation {
		return scanDocPreparedFindRange(ctx, source, selector, query)
	}
	if selector.Representation == domain.DocFindDisplayRepresentation {
		return scanDocDisplayFindRange(ctx, source, selector, query)
	}
	if selector.Representation != "" {
		return domain.DocFindPage{}, domain.ErrValidation
	}
	page := domain.DocFindPage{From: selector.From, To: selector.To, Matches: []domain.DocFindMatch{}}
	scanned, events := 0, 0
	for selector.Index < selector.To && scanned < docFindScanBytes && events < docFindScanEvents && len(page.Matches) < domain.DocFindPageMatches {
		if err := ctx.Err(); err != nil {
			return domain.DocFindPage{}, err
		}
		metadata, err := source.entry(selector.Index)
		if err != nil {
			return domain.DocFindPage{}, err
		}
		if metadata.EmptyReasoning || metadata.Kind == domain.EventCompaction {
			selector.Index++
			selector.Field, selector.Byte = 0, 0
			events++
			continue
		}
		entry := metadata.DocEventIndex
		if entry.Length > docViewBytes {
			if docFindScanBytes-scanned < docViewBytes+domain.MaxDocFindFieldsBytes {
				break
			}
			var count int
			if metadata.Kind == domain.EventReasoning || metadata.Kind == domain.EventMessage {
				count, err = scanDocFindPublicFields(ctx, source, metadata, selector, query, &page, docFindScanBytes-scanned)
			} else {
				count, err = scanDocFindFragment(ctx, source, entry, selector, query, &page, docFindScanBytes-scanned)
			}
			if err != nil {
				return domain.DocFindPage{}, err
			}
			scanned += count
			if selector.Byte == 0 {
				events++
			}
			continue
		}
		if scanned+entry.Length > docFindScanBytes {
			break
		}
		raw, err := source.read(entry.Offset, entry.Length)
		if err != nil {
			return domain.DocFindPage{}, err
		}
		event, err := domain.DecodeIndexedEvent(raw, entry)
		if err != nil {
			return domain.DocFindPage{}, err
		}
		scanned += len(raw)
		previous := selector.Index
		firstMatch := len(page.Matches)
		if err := scanDocFindTexts(ctx, domain.DocFindTexts(event), selector, query, &page); err != nil {
			return domain.DocFindPage{}, err
		}
		if firstMatch < len(page.Matches) {
			matches := page.Matches[firstMatch:]
			spans, err := domain.LocateDocFindTextSources(raw, matches)
			if err != nil {
				return domain.DocFindPage{}, err
			}
			for index := range matches {
				matches[index].SourceRange = &spans[index]
			}
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

func scanDocFindTexts(ctx context.Context, fields []string, selector *docFindProgress, query string, page *domain.DocFindPage) error {
	if selector.Field > len(fields) || selector.Field == len(fields) && selector.Byte != 0 {
		return domain.ErrValidation
	}
	for selector.Field < len(fields) && len(page.Matches) < domain.DocFindPageMatches {
		text := fields[selector.Field]
		matches, err := domain.FindDocText(ctx, text, query, selector.Byte, domain.DocFindPageMatches-len(page.Matches))
		if err != nil {
			return err
		}
		for _, match := range matches {
			match.Index, match.Field = selector.Index, selector.Field
			match, err = domain.WithDocFindPreview(text, match)
			if err != nil {
				return err
			}
			page.Matches = append(page.Matches, match)
		}
		if len(page.Matches) == domain.DocFindPageMatches {
			last := page.Matches[len(page.Matches)-1]
			_, size := utf8.DecodeRuneInString(text[last.Offset:])
			selector.Byte = last.Offset + size
			if selector.Byte == len(text) {
				selector.Field++
				selector.Byte = 0
			}
			break
		}
		selector.Field++
		selector.Byte = 0
	}
	if selector.Field == len(fields) {
		selector.Index++
		selector.Field, selector.Byte = 0, 0
	}
	return nil
}

func scanDocFindPublicFields(ctx context.Context, source docViewSource, metadata domain.DocRangeEntry, selector *docFindProgress, query string, page *domain.DocFindPage, budget int) (int, error) {
	entry := metadata.DocEventIndex
	if source.findFields == nil {
		return 0, domain.ErrDocFindFieldsUnavailable
	}
	fields, err := source.findFields(entry)
	if err != nil {
		return 0, err
	}
	if err := fields.Validate(entry.Hash, entry.Length); err != nil {
		return 0, err
	}
	selected := fields.Summary
	if metadata.Kind == domain.EventMessage {
		selected = fields.Blocks
	}
	if selected == nil {
		if metadata.Kind == domain.EventReasoning {
			return 0, domain.ErrIntegrity
		}
		return domain.MaxDocFindFieldsBytes, scanDocFindTexts(ctx, nil, selector, query, page)
	}
	span := *selected
	if span.To-span.From > docViewBytes {
		from, to := span.From, span.To
		if metadata.Kind == domain.EventReasoning {
			from++
			to--
		}
		count, err := scanDocFindRawRange(ctx, source, entry, selector, query, page, budget-domain.MaxDocFindFieldsBytes, from, to)
		return count + domain.MaxDocFindFieldsBytes, err
	}
	raw, err := source.read(entry.Offset+span.From, span.To-span.From)
	if err != nil {
		return 0, err
	}
	var texts []string
	if metadata.Kind == domain.EventMessage {
		var blocks []domain.ContentBlock
		if json.Unmarshal(raw, &blocks) != nil {
			return 0, domain.ErrIntegrity
		}
		texts = domain.DocFindTexts(domain.CIREvent{Kind: domain.EventMessage, Blocks: blocks})
	} else {
		var summary string
		if json.Unmarshal(raw, &summary) != nil {
			return 0, domain.ErrIntegrity
		}
		texts = []string{summary}
	}
	firstMatch := len(page.Matches)
	if err := scanDocFindTexts(ctx, texts, selector, query, page); err != nil {
		return 0, err
	}
	if firstMatch < len(page.Matches) {
		matches := page.Matches[firstMatch:]
		spans, err := domain.LocateDocFindPublicTextSources(raw, metadata.Kind, matches)
		if err != nil {
			return 0, err
		}
		for index := range matches {
			spans[index].From += span.From
			spans[index].To += span.From
			matches[index].SourceRange = &spans[index]
		}
	}
	return len(raw) + domain.MaxDocFindFieldsBytes, err
}

func scanDocFindFragment(ctx context.Context, source docViewSource, entry domain.DocEventIndex, selector *docFindProgress,
	query string, page *domain.DocFindPage, budget int) (int, error) {
	return scanDocFindRawRange(ctx, source, entry, selector, query, page, budget, 0, entry.Length)
}

func scanDocFindRawRange(ctx context.Context, source docViewSource, entry domain.DocEventIndex, selector *docFindProgress,
	query string, page *domain.DocFindPage, budget, from, to int) (int, error) {
	if selector.Byte == 0 {
		selector.Byte = from
	}
	if selector.Field != 0 || selector.Byte < from || selector.Byte >= to {
		return 0, domain.ErrValidation
	}
	raw, err := source.read(entry.Offset+selector.Byte, min(docFindFragmentBytes, budget, to-selector.Byte))
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 || !utf8.RuneStart(raw[0]) {
		return 0, domain.ErrValidation
	}
	if !utf8.Valid(raw) {
		boundary := len(raw) - 1
		for boundary > 0 && len(raw)-boundary < utf8.UTFMax && !utf8.RuneStart(raw[boundary]) {
			boundary--
		}
		if utf8.FullRune(raw[boundary:]) || !utf8.Valid(raw[:boundary]) {
			return 0, domain.ErrIntegrity
		}
		raw = raw[:boundary]
	}
	if len(raw) == 0 {
		return 0, domain.ErrIntegrity
	}
	end := selector.Byte + len(raw)
	frontier := end
	if end < to {
		frontier -= domain.MaxDocFindQueryRunes * utf8.UTFMax
		for frontier > selector.Byte && !utf8.RuneStart(raw[frontier-selector.Byte]) {
			frontier--
		}
	}
	text := string(raw)
	matches, err := domain.FindDocText(ctx, text, query, 0, domain.DocFindPageMatches-len(page.Matches))
	if err != nil {
		return 0, err
	}
	for _, match := range matches {
		if match.Offset+selector.Byte >= frontier {
			break
		}
		match, err = domain.WithDocFindPreview(text, match)
		if err != nil {
			return 0, err
		}
		match.Offset += selector.Byte
		match.PreviewOffset += selector.Byte
		match.Index, match.Raw = selector.Index, true
		match.SourceRange = &domain.DocFindSpan{From: match.Offset, To: match.Offset + match.Length}
		page.Matches = append(page.Matches, match)
	}
	if len(page.Matches) == domain.DocFindPageMatches {
		last := page.Matches[len(page.Matches)-1]
		_, size := utf8.DecodeRune(raw[last.Offset-selector.Byte:])
		selector.Byte = last.Offset + size
		if selector.Byte == to {
			selector.Index++
			selector.Byte = 0
		}
	} else if end == to {
		selector.Index++
		selector.Byte = 0
	} else {
		selector.Byte = frontier
	}
	return len(raw), nil
}
