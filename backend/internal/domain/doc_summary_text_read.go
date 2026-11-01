package domain

import (
	"context"
	"unicode/utf8"
)

func ReadDocSummaryTextPage(ctx context.Context, generation DocSummaryTextGeneration, index int, load func(context.Context, ContentHash) ([]byte, error)) (result DocDisplayPage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocDisplayPage{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	if generation.Validate() != nil || index < 0 || index >= generation.Pages.Count || load == nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	hash, err := ResolveDocRangeLeaf(ctx, generation.Pages, index, load)
	if err != nil {
		return DocDisplayPage{}, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return DocDisplayPage{}, err
	}
	page, err := DecodeDocSummaryTextPage(generation.Source, hash, raw)
	if err != nil || page.Index != index || len(page.Text) > generation.TextBytes || page.From > generation.TextBytes-len(page.Text) {
		return DocDisplayPage{}, ErrIntegrity
	}
	remaining := generation.Pages.Count - index - 1
	bytesRemaining := generation.TextBytes - page.From - len(page.Text)
	if remaining == 0 && bytesRemaining != 0 || remaining > 0 && (int64(bytesRemaining) < int64(remaining-1)*DocDisplayPageRuns+1 || int64(bytesRemaining) > int64(remaining)*DocDisplayPageTextBytes || utf8.RuneCountInString(page.Text) < DocDisplayPageRuns) {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

func adjacentDocSummaryTextPages(first, last DocDisplayPage) bool {
	if last.Index != first.Index+1 || last.From != first.From+len(first.Text) || len(first.Runs) == 0 || len(last.Runs) == 0 {
		return false
	}
	before, after := first.Runs[len(first.Runs)-1], last.Runs[0]
	return after.SourceFrom >= before.SourceFrom && after.SourceTo >= before.SourceTo
}

func newDocSummaryTextReader(generation DocSummaryTextGeneration, load func(context.Context, ContentHash) ([]byte, error)) (*docPreparedTextReader, error) {
	if generation.Validate() != nil || load == nil {
		return nil, ErrIntegrity
	}
	hash, err := generation.Source.Hash()
	if err != nil {
		return nil, err
	}
	return newDocPreparedTextReader(generation.TextBytes, generation.Pages.Count, generation.Lookup, hash, load, func(ctx context.Context, index int) (DocDisplayPage, error) {
		return ReadDocSummaryTextPage(ctx, generation, index, load)
	}, adjacentDocSummaryTextPages)
}

func ReadDocSummaryTextBytes(ctx context.Context, generation DocSummaryTextGeneration, from, length int, load func(context.Context, ContentHash) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, err := newDocSummaryTextReader(generation, load)
	if err != nil {
		return nil, err
	}
	return reader.readBytes(ctx, from, length)
}

func LocateDocSummaryTextSource(ctx context.Context, generation DocSummaryTextGeneration, from, length int, load func(context.Context, ContentHash) ([]byte, error)) (result DocFindSpan, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocFindSpan{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocFindSpan{}, err
	}
	reader, err := newDocSummaryTextReader(generation, load)
	if err != nil {
		return DocFindSpan{}, err
	}
	return reader.locateSource(ctx, from, length, false)
}
