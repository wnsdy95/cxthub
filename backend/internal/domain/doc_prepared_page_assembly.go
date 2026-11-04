package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"unicode/utf8"
)

type docPreparedPageCursor struct {
	Version int         `json:"version"`
	Plan    ContentHash `json:"plan"`
	Page    int         `json:"page"`
	Byte    int         `json:"byte"`
	From    int         `json:"from"`
}

func (cursor docPreparedPageCursor) validate(source DocDisplayProjectionSource, count, minRunes int) error {
	if cursor.Version != 1 || cursor.Plan != source.Plan || cursor.Page < 0 || cursor.Page > count || cursor.Byte < 0 || cursor.Byte >= DocDisplayPageTextBytes || cursor.From < 0 || cursor.From > source.TextBytes || cursor.Page < count && cursor.Page > cursor.From/minRunes || (cursor.Page == count) != (cursor.From == source.TextBytes) || cursor.Page == count && cursor.Byte != 0 || cursor.From == 0 && (cursor.Page != 0 || cursor.Byte != 0) {
		return ErrIntegrity
	}
	return nil
}

func newDocPreparedPageAssemblyInput(source DocDisplayProjectionSource, count, minRunes int, readPage func(context.Context, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error)) (DocDisplayAssemblyInput, error) {
	if source.Validate() != nil || source.TextBytes <= 0 || count <= 0 || count > MaxDocRangePages || minRunes <= 0 || minRunes > DocDisplayPageTextBytes || int64(source.TextBytes) > int64(count)*DocDisplayPageTextBytes || count-1 > (source.TextBytes-1)/minRunes || readPage == nil {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	start := docPreparedPageCursor{Version: 1, Plan: source.Plan}
	initial, err := canonicalJSON(start)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	decode := func(raw json.RawMessage) (docPreparedPageCursor, error) {
		var cursor docPreparedPageCursor
		if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.validate(source, count, minRunes) != nil {
			return docPreparedPageCursor{}, ErrIntegrity
		}
		encoded, err := canonicalJSON(cursor)
		if err != nil || !bytes.Equal(raw, encoded) {
			return docPreparedPageCursor{}, ErrIntegrity
		}
		return cursor, nil
	}
	input := DocDisplayAssemblyInput{source: source, initial: initial}
	input.cursor = func(raw json.RawMessage) (int, error) {
		cursor, err := decode(raw)
		return cursor.From, err
	}
	input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (page DocDisplayPage, next json.RawMessage, failure error) {
		defer func() {
			if err := ctx.Err(); err != nil {
				failure = err
			}
			if failure != nil {
				page, next = DocDisplayPage{}, nil
			}
		}()
		if err := ctx.Err(); err != nil {
			return page, nil, err
		}
		cursor, err := decode(raw)
		if err != nil || cursor.From == source.TextBytes || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns || load == nil {
			return page, nil, ErrIntegrity
		}
		display, err := readPage(ctx, cursor.Page, newDocMappedObjectLoader(load))
		if err != nil {
			return page, nil, err
		}
		if display.validateCoordinates(source.EventBytes, false) != nil || cursor.Byte >= len(display.Text) || display.From+cursor.Byte != cursor.From || !utf8.RuneStart(display.Text[cursor.Byte]) {
			return page, nil, ErrIntegrity
		}
		limit := min(len(display.Text), cursor.Byte+maxBytes)
		for limit < len(display.Text) && !utf8.RuneStart(display.Text[limit]) {
			limit--
		}
		page = DocDisplayPage{Version: 2, From: cursor.From}
		page.Source, err = source.Hash()
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		first := sort.Search(len(display.Runs), func(index int) bool { return display.Runs[index].To > cursor.Byte })
		previous := 0
		if first > 0 {
			previous = display.Runs[first-1].To
		}
		for _, run := range display.Runs[first:] {
			from := max(previous, cursor.Byte)
			to := min(run.To, limit)
			if from < to {
				if len(page.Runs) == maxRuns {
					limit = from
					break
				}
				projected := run
				projected.To = to - cursor.Byte
				if projected.Linear {
					projected.SourceFrom += from - previous
					projected.SourceTo -= run.To - to
				}
				page.Runs = append(page.Runs, projected)
			}
			previous = run.To
			if previous >= limit {
				break
			}
		}
		page.Text = display.Text[cursor.Byte:limit]
		if len(page.Text) == 0 || page.validateCoordinates(source.EventBytes, false) != nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		cursor.From += len(page.Text)
		cursor.Byte = limit
		if limit == len(display.Text) {
			cursor.Page++
			cursor.Byte = 0
		}
		if cursor.validate(source, count, minRunes) != nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		next, err = canonicalJSON(cursor)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		return page, next, nil
	}
	return input, input.validate()
}
