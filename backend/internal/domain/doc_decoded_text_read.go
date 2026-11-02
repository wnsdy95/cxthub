package domain

import (
	"context"
	"unicode/utf8"
)

const DocDecodedTextLookupEntries = 128
const DocDecodedTextLookupStride = 512
const MaxDocDecodedTextLookupBytes = 32 << 10

type DocDecodedTextLookupPage struct {
	Version int         `json:"version"`
	Source  ContentHash `json:"source"`
	Index   int         `json:"index"`
	Pages   []int       `json:"pages"`
}

func (page DocDecodedTextLookupPage) Validate(source DocDecodedTextSource) error {
	hash, err := source.Hash()
	if err != nil {
		return ErrIntegrity
	}
	return page.validateForSource(hash)
}

func (page DocDecodedTextLookupPage) Encode(source DocDecodedTextSource) ([]byte, error) {
	hash, err := source.Hash()
	if err != nil {
		return nil, ErrIntegrity
	}
	return page.encodeForSource(hash)
}

func DecodeDocDecodedTextLookupPage(source DocDecodedTextSource, expected ContentHash, raw []byte) (DocDecodedTextLookupPage, error) {
	hash, err := source.Hash()
	if err != nil {
		return DocDecodedTextLookupPage{}, ErrIntegrity
	}
	return decodeDocTextLookupPage(hash, expected, raw)
}

func ReadDocDecodedTextPage(ctx context.Context, generation DocDecodedTextGeneration, index int, loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocDisplayPage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocDisplayPage{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocDisplayPage{}, err
	}
	if generation.Validate() != nil || index < 0 || index >= generation.Pages.Count || loadObject == nil {
		return DocDisplayPage{}, ErrIntegrity
	}
	hash, err := ResolveDocRangeLeaf(ctx, generation.Pages, index, loadObject)
	if err != nil {
		return DocDisplayPage{}, err
	}
	raw, err := loadObject(ctx, hash)
	if err != nil {
		return DocDisplayPage{}, err
	}
	page, err := DecodeDocDecodedTextPage(generation.Source, hash, raw)
	if err != nil || page.Index != index || int64(page.From) > int64(index)*DocDisplayPageTextBytes || page.From > generation.TextBytes-len(page.Text) {
		return DocDisplayPage{}, ErrIntegrity
	}
	if index == 0 && (page.From != 0 || page.Runs[0].SourceFrom != generation.Source.Token.From+1) {
		return DocDisplayPage{}, ErrIntegrity
	}
	remaining := generation.Pages.Count - index - 1
	textRemaining := generation.TextBytes - page.From - len(page.Text)
	rawEnd := page.Runs[len(page.Runs)-1].SourceTo
	if !validDocJSONStringByteRatio(generation.Source.Token.To-1-rawEnd, textRemaining) {
		return DocDisplayPage{}, ErrIntegrity
	}
	if remaining == 0 {
		if textRemaining != 0 || rawEnd != generation.Source.Token.To-1 {
			return DocDisplayPage{}, ErrIntegrity
		}
	} else if textRemaining <= (remaining-1)*DocDisplayPageRuns || int64(textRemaining) > int64(remaining)*DocDisplayPageTextBytes || rawEnd >= generation.Source.Token.To-1 || utf8.RuneCountInString(page.Text) < DocDisplayPageRuns {
		return DocDisplayPage{}, ErrIntegrity
	}
	return page, nil
}

func newDocDecodedTextReader(generation DocDecodedTextGeneration, load func(context.Context, ContentHash) ([]byte, error)) (*docPreparedTextReader, error) {
	if generation.Validate() != nil || load == nil {
		return nil, ErrIntegrity
	}
	source, err := generation.Source.Hash()
	if err != nil {
		return nil, err
	}
	return newDocPreparedTextReader(generation.TextBytes, generation.Pages.Count, generation.Lookup, source, load,
		func(ctx context.Context, index int) (DocDisplayPage, error) {
			return ReadDocDecodedTextPage(ctx, generation, index, load)
		}, adjacentDocDecodedTextPages)
}

func adjacentDocDecodedTextPages(first, last DocDisplayPage) bool {
	return last.Index == first.Index+1 && last.From == first.From+len(first.Text) && first.Runs[len(first.Runs)-1].SourceTo == last.Runs[0].SourceFrom
}

func ReadDocDecodedTextBytes(ctx context.Context, generation DocDecodedTextGeneration, from, length int, loadObject func(context.Context, ContentHash) ([]byte, error)) (result []byte, failure error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, err := newDocDecodedTextReader(generation, loadObject)
	if err != nil {
		return nil, err
	}
	return reader.readBytes(ctx, from, length)
}

func LocateDocDecodedTextSource(ctx context.Context, generation DocDecodedTextGeneration, from, length int, loadObject func(context.Context, ContentHash) ([]byte, error)) (result DocFindSpan, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocFindSpan{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocFindSpan{}, err
	}
	if generation.Validate() != nil || loadObject == nil {
		return DocFindSpan{}, ErrIntegrity
	}
	if from < 0 || length <= 0 || length > generation.TextBytes || from > generation.TextBytes-length {
		return DocFindSpan{}, ErrValidation
	}
	reader, err := newDocDecodedTextReader(generation, loadObject)
	if err != nil {
		return DocFindSpan{}, err
	}
	return reader.locateSource(ctx, from, length, true)
}

func docDecodedTextSourceBoundary(page DocDisplayPage, offset int, end bool) (int, error) {
	if offset < 0 || offset > len(page.Text) || offset < len(page.Text) && !utf8.RuneStart(page.Text[offset]) {
		return 0, ErrValidation
	}
	previous := 0
	for _, run := range page.Runs {
		if offset < run.To || end && offset == run.To {
			if run.Linear {
				return run.SourceFrom + offset - previous, nil
			}
			if end {
				return run.SourceTo, nil
			}
			return run.SourceFrom, nil
		}
		previous = run.To
	}
	return 0, ErrIntegrity
}
