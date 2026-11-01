package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"unicode/utf8"
)

type docToolLiteralAssemblyDescriptor struct {
	Version    int         `json:"version"`
	Profile    string      `json:"profile"`
	EventHash  ContentHash `json:"event_hash"`
	EventBytes int         `json:"event_bytes"`
	Text       string      `json:"text"`
	Anchor     DocFindSpan `json:"anchor"`
}

func newDocToolLiteralAssemblyInput(event ContentHash, eventBytes int, text string, anchor DocFindSpan) (DocDisplayAssemblyInput, error) {
	if len(text) == 0 || len(text) > 8 || !utf8.ValidString(text) || anchor.From < 0 || anchor.From >= anchor.To || anchor.To > eventBytes {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	raw, err := canonicalJSON(docToolLiteralAssemblyDescriptor{Version: 1, Profile: "tool-literal-assembly-v1", EventHash: event, EventBytes: eventBytes, Text: text, Anchor: anchor})
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: event, EventBytes: eventBytes, Plan: HashContent(raw), TextBytes: len(text)}
	hash, err := source.Hash()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	return newDocPreparedPageAssemblyInput(source, 1, 1, func(context.Context, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
		return DocDisplayPage{Version: 2, Source: hash, Text: text, Runs: []DocDisplayRun{{To: len(text), SourceFrom: anchor.From, SourceTo: anchor.To}}}, nil
	})
}

type docToolDecodedAssemblyDescriptor struct {
	Version    int                      `json:"version"`
	Profile    string                   `json:"profile"`
	Generation DocDecodedTextGeneration `json:"generation"`
	Skip       int                      `json:"skip"`
}

type docToolDecodedAssemblyCursor struct {
	Version int         `json:"version"`
	Source  ContentHash `json:"source"`
	Plan    ContentHash `json:"plan"`
	From    int         `json:"from"`
	Offset  int         `json:"offset"`
	Done    bool        `json:"done"`
}

func newDocToolDecodedAssemblyInput(generation DocDecodedTextGeneration, skip int) (DocDisplayAssemblyInput, error) {
	if generation.Validate() != nil || skip < 0 || skip > generation.TextBytes {
		return DocDisplayAssemblyInput{}, ErrIntegrity
	}
	raw, err := canonicalJSON(docToolDecodedAssemblyDescriptor{Version: 1, Profile: "tool-decoded-assembly-v1", Generation: generation, Skip: skip})
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: generation.Source.EventHash, EventBytes: generation.Source.EventBytes, Plan: HashContent(raw), TextBytes: generation.TextBytes - skip}
	hash, err := source.Hash()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	initial, err := canonicalJSON(docToolDecodedAssemblyCursor{Version: 1, Source: hash, Plan: source.Plan, Offset: skip, Done: source.TextBytes == 0})
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	decode := func(raw json.RawMessage) (docToolDecodedAssemblyCursor, error) {
		var cursor docToolDecodedAssemblyCursor
		if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Source != hash || cursor.Plan != source.Plan || cursor.From < 0 || cursor.From > source.TextBytes || cursor.Offset != skip+cursor.From || cursor.Done != (cursor.From == source.TextBytes) {
			return docToolDecodedAssemblyCursor{}, ErrIntegrity
		}
		canonical, err := canonicalJSON(cursor)
		if err != nil || !bytes.Equal(raw, canonical) {
			return docToolDecodedAssemblyCursor{}, ErrIntegrity
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
		if err != nil || cursor.Done || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns || load == nil {
			return page, nil, ErrIntegrity
		}
		reader, err := newDocDecodedTextReader(generation, newDocMappedObjectLoader(load))
		if err != nil {
			return page, nil, err
		}
		display, err := reader.selectPage(ctx, cursor.Offset)
		if err != nil {
			return page, nil, err
		}
		offset := cursor.Offset - display.From
		if display.validateCoordinates(source.EventBytes, true) != nil || offset < 0 || offset >= len(display.Text) || !utf8.RuneStart(display.Text[offset]) {
			return page, nil, ErrIntegrity
		}
		limit := min(len(display.Text), offset+maxBytes)
		for limit < len(display.Text) && !utf8.RuneStart(display.Text[limit]) {
			limit--
		}
		page = DocDisplayPage{Version: 2, Source: hash, From: cursor.From}
		previous := 0
		for _, run := range display.Runs {
			from, to := max(previous, offset), min(run.To, limit)
			if from < to {
				if len(page.Runs) == maxRuns {
					limit = from
					break
				}
				projected := run
				projected.To = to - offset
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
		page.Text = display.Text[offset:limit]
		if page.validateCoordinates(source.EventBytes, true) != nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		cursor.From += len(page.Text)
		cursor.Offset += len(page.Text)
		cursor.Done = cursor.From == source.TextBytes
		next, err = canonicalJSON(cursor)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		if _, err := decode(next); err != nil {
			return DocDisplayPage{}, nil, err
		}
		return page, next, nil
	}
	return input, input.validate()
}
