package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

func (page DocDecodedTextLookupPage) validateForSource(source ContentHash) error {
	if ValidateContentHash(source) != nil || page.Version != 1 || page.Source != source || page.Index < 0 || page.Index >= MaxDocRangePages || len(page.Pages) == 0 || len(page.Pages) > DocDecodedTextLookupEntries {
		return ErrIntegrity
	}
	for index, ordinal := range page.Pages {
		if ordinal < 0 || ordinal >= MaxDocRangePages || index > 0 && ordinal < page.Pages[index-1] {
			return ErrIntegrity
		}
	}
	return nil
}

func (page DocDecodedTextLookupPage) encodeForSource(source ContentHash) ([]byte, error) {
	if page.validateForSource(source) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(page)
	if err != nil || len(raw) > MaxDocDecodedTextLookupBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func decodeDocTextLookupPage(source, expected ContentHash, raw []byte) (DocDecodedTextLookupPage, error) {
	var page DocDecodedTextLookupPage
	if len(raw) == 0 || len(raw) > MaxDocDecodedTextLookupBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocDecodedTextLookupPage{}, ErrIntegrity
	}
	canonical, err := page.encodeForSource(source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocDecodedTextLookupPage{}, ErrIntegrity
	}
	return page, nil
}

type docPreparedTextReader struct {
	textBytes  int
	pageCount  int
	lookupRoot DocRangeDirectoryRoot
	source     ContentHash
	load       func(context.Context, ContentHash) ([]byte, error)
	readPage   func(context.Context, int) (DocDisplayPage, error)
	adjacent   func(DocDisplayPage, DocDisplayPage) bool
	pages      [2]*DocDisplayPage
	lookups    [2]*DocDecodedTextLookupPage
}

func newDocPreparedTextReader(textBytes, pageCount int, lookup DocRangeDirectoryRoot, source ContentHash, load func(context.Context, ContentHash) ([]byte, error), readPage func(context.Context, int) (DocDisplayPage, error), adjacent func(DocDisplayPage, DocDisplayPage) bool) (*docPreparedTextReader, error) {
	if !validDocDisplayProjectionCount(pageCount, textBytes, true) || ValidateContentHash(source) != nil || load == nil || readPage == nil || adjacent == nil {
		return nil, ErrIntegrity
	}
	entries := (textBytes + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride
	if lookup.validate((entries+DocDecodedTextLookupEntries-1)/DocDecodedTextLookupEntries) != nil {
		return nil, ErrIntegrity
	}
	return &docPreparedTextReader{textBytes: textBytes, pageCount: pageCount, lookupRoot: lookup, source: source, load: load, readPage: readPage, adjacent: adjacent}, nil
}

func (reader *docPreparedTextReader) page(ctx context.Context, index int) (DocDisplayPage, error) {
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	if index < 0 || index >= reader.pageCount {
		return DocDisplayPage{}, ErrIntegrity
	}
	for _, page := range reader.pages {
		if page != nil && page.Index == index {
			return *page, nil
		}
	}
	page, err := reader.readPage(ctx, index)
	if err != nil {
		return DocDisplayPage{}, err
	}
	if page.Source != reader.source || page.Index != index || page.From < 0 || len(page.Text) == 0 || len(page.Text) > DocDisplayPageTextBytes || page.From > reader.textBytes-len(page.Text) {
		return DocDisplayPage{}, ErrIntegrity
	}
	reader.pages[0], reader.pages[1] = reader.pages[1], &page
	return page, nil
}

func (reader *docPreparedTextReader) lookup(ctx context.Context, index int) (DocDecodedTextLookupPage, error) {
	if err := ctx.Err(); err != nil {
		return DocDecodedTextLookupPage{}, err
	}
	for _, page := range reader.lookups {
		if page != nil && page.Index == index {
			return *page, nil
		}
	}
	hash, err := ResolveDocRangeLeaf(ctx, reader.lookupRoot, index, reader.load)
	if err != nil {
		return DocDecodedTextLookupPage{}, err
	}
	raw, err := reader.load(ctx, hash)
	if err != nil {
		return DocDecodedTextLookupPage{}, err
	}
	lookup, err := decodeDocTextLookupPage(reader.source, hash, raw)
	entries := (reader.textBytes + DocDecodedTextLookupStride - 1) / DocDecodedTextLookupStride
	if err != nil || lookup.Index != index || len(lookup.Pages) != min(DocDecodedTextLookupEntries, entries-index*DocDecodedTextLookupEntries) {
		return DocDecodedTextLookupPage{}, ErrIntegrity
	}
	for _, ordinal := range lookup.Pages {
		if ordinal >= reader.pageCount {
			return DocDecodedTextLookupPage{}, ErrIntegrity
		}
	}
	reader.lookups[0], reader.lookups[1] = reader.lookups[1], &lookup
	return lookup, nil
}

func (reader *docPreparedTextReader) selectPage(ctx context.Context, from int) (DocDisplayPage, error) {
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	if from < 0 || from >= reader.textBytes {
		return DocDisplayPage{}, ErrValidation
	}
	bucket := from / DocDecodedTextLookupStride
	lookup, err := reader.lookup(ctx, bucket/DocDecodedTextLookupEntries)
	if err != nil {
		return DocDisplayPage{}, err
	}
	page, err := reader.page(ctx, lookup.Pages[bucket%DocDecodedTextLookupEntries])
	if err != nil {
		return DocDisplayPage{}, err
	}
	start := bucket * DocDecodedTextLookupStride
	if page.From > start || start >= page.From+len(page.Text) {
		return DocDisplayPage{}, ErrIntegrity
	}
	if from < page.From+len(page.Text) {
		return page, nil
	}
	next, err := reader.page(ctx, page.Index+1)
	if err != nil {
		return DocDisplayPage{}, err
	}
	if !reader.adjacent(page, next) || from < next.From || from >= next.From+len(next.Text) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return next, nil
}

func (reader *docPreparedTextReader) readBytes(ctx context.Context, from, length int) (result []byte, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = nil, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if from < 0 || length < 0 || length > 4096 || length > reader.textBytes || from > reader.textBytes-length {
		return nil, ErrValidation
	}
	if length == 0 {
		return []byte{}, nil
	}
	page, err := reader.selectPage(ctx, from)
	if err != nil {
		return nil, err
	}
	output := make([]byte, 0, length)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		offset := from + len(output) - page.From
		if offset < 0 || offset >= len(page.Text) {
			return nil, ErrIntegrity
		}
		count := min(length-len(output), len(page.Text)-offset)
		output = append(output, page.Text[offset:offset+count]...)
		if len(output) == length {
			return output, nil
		}
		next, err := reader.page(ctx, page.Index+1)
		if err != nil {
			return nil, err
		}
		if !reader.adjacent(page, next) {
			return nil, ErrIntegrity
		}
		page = next
	}
}
