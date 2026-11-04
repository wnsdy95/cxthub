package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocDisplayMappedReplayObjects = 64

type docMappedMarkdownCursor struct {
	Version int                   `json:"version"`
	Plan    ContentHash           `json:"plan"`
	Render  DocMarkdownPlanCursor `json:"render"`
}

func newDocMappedObjectLoader(load func(context.Context, ContentHash) ([]byte, error)) func(context.Context, ContentHash) ([]byte, error) {
	calls := 0
	return func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if calls == DocDisplayMappedReplayObjects {
			return nil, errDocMappedReplayBudget
		}
		calls++
		return load(ctx, hash)
	}
}

func NewDocDecodedMarkdownAssemblyInput(plan DocDecodedMarkdownPlan) (DocDisplayAssemblyInput, error) {
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	return newDocMarkdownAssemblyInput(source, plan.Markdown, true, func(load func(context.Context, ContentHash) ([]byte, error)) (*docPreparedTextReader, error) {
		return newDocDecodedTextReader(plan.Input, load)
	})
}

func NewDocSummaryMarkdownAssemblyInput(plan DocSummaryMarkdownPlan) (DocDisplayAssemblyInput, error) {
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	return newDocMarkdownAssemblyInput(source, plan.Markdown, false, func(load func(context.Context, ContentHash) ([]byte, error)) (*docPreparedTextReader, error) {
		return newDocSummaryTextReader(plan.Input, load)
	})
}

func newDocMarkdownAssemblyInput(source DocDisplayProjectionSource, plan DocMarkdownPlan, literal bool, readerFactory func(func(context.Context, ContentHash) ([]byte, error)) (*docPreparedTextReader, error)) (DocDisplayAssemblyInput, error) {
	if source.Validate() != nil || plan.Validate() != nil || source.TextBytes != plan.TextBytes || readerFactory == nil {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	render, err := NewDocMarkdownPlanCursor(plan)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	initial, err := canonicalJSON(docMappedMarkdownCursor{Version: 1, Plan: source.Plan, Render: render})
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	decode := func(raw json.RawMessage) (docMappedMarkdownCursor, error) {
		var cursor docMappedMarkdownCursor
		if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Plan != source.Plan || cursor.Render.Validate(plan) != nil || cursor.Render.TextBytes == 0 && cursor.Render != render {
			return docMappedMarkdownCursor{}, ErrIntegrity
		}
		encoded, err := canonicalJSON(cursor)
		if err != nil || !bytes.Equal(raw, encoded) {
			return docMappedMarkdownCursor{}, ErrIntegrity
		}
		return cursor, nil
	}
	input := DocDisplayAssemblyInput{source: source, initial: initial}
	input.cursor = func(raw json.RawMessage) (int, error) {
		cursor, err := decode(raw)
		return cursor.Render.TextBytes, err
	}
	input.read = func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (page DocDisplayPage, next json.RawMessage, failure error) {
		defer func() {
			if err := ctx.Err(); err != nil {
				page, next, failure = DocDisplayPage{}, nil, err
			}
		}()
		if err := ctx.Err(); err != nil {
			return DocDisplayPage{}, nil, err
		}
		cursor, err := decode(raw)
		if err != nil || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns || load == nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		bounded := newDocMappedObjectLoader(load)
		reader, err := readerFactory(bounded)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		fragment, err := readDocMarkdownPlanFragment(ctx, plan, cursor.Render, bounded, reader.readBytes, docMarkdownReplayPolicy{MaxBytes: maxBytes, MaxRuns: maxRuns, SourceBytes: source.EventBytes, MapSource: func(ctx context.Context, from, length int) (DocFindSpan, error) {
			return reader.locateSource(ctx, from, length, literal)
		}})
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		cursor.Render = fragment.Next()
		next, err = canonicalJSON(cursor)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		page = fragment.page
		page.Source, err = source.Hash()
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		return page, next, nil
	}
	return input, input.validate()
}

func NewDocMessagePlaceholderAssemblyInput(plan DocMessagePlaceholderPlan) (DocDisplayAssemblyInput, error) {
	encoded, err := plan.Encode()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	plan, err = DecodeDocMessagePlaceholderPlan(HashContent(encoded), encoded)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	start, err := NewDocMessagePlaceholderCursor(plan)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	initial, err := canonicalJSON(start)
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	decode := func(raw json.RawMessage) (DocMessagePlaceholderCursor, error) {
		var cursor DocMessagePlaceholderCursor
		if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Plan != source.Plan || cursor.From < 0 || cursor.From > source.TextBytes {
			return DocMessagePlaceholderCursor{}, ErrIntegrity
		}
		encoded, err := canonicalJSON(cursor)
		if err != nil || !bytes.Equal(raw, encoded) {
			return DocMessagePlaceholderCursor{}, ErrIntegrity
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
				page, next, failure = DocDisplayPage{}, nil, err
			}
		}()
		cursor, err := decode(raw)
		if err != nil || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns || load == nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		fragment, err := readDocMessagePlaceholderFragment(ctx, plan, cursor, newDocMappedObjectLoader(load), min(maxBytes, 4096))
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		next, err = canonicalJSON(fragment.Next())
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		hash, err := source.Hash()
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		page = DocDisplayPage{Version: 2, Source: hash, From: fragment.From(), Text: fragment.Text(), Runs: []DocDisplayRun{{To: len(fragment.Text()), SourceFrom: plan.Block.Anchor.From, SourceTo: plan.Block.Anchor.To}}}
		return page, next, nil
	}
	return input, input.validate()
}
