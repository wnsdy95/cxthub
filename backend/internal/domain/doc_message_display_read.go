package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

type docMessageDisplayCursor struct {
	Version   int                   `json:"version"`
	Plan      ContentHash           `json:"plan"`
	Part      int                   `json:"part"`
	From      int                   `json:"from"`
	Separator int                   `json:"separator"`
	Prefix    int                   `json:"prefix"`
	TextBytes int                   `json:"text_bytes"`
	Kind      DocMessageDisplayKind `json:"kind,omitempty"`
	Child     ContentHash           `json:"child,omitempty"`
	BodyBytes int                   `json:"body_bytes,omitempty"`
	Body      json.RawMessage       `json:"body,omitempty"`
}

type docMessageDisplayReadWork struct {
	inputs docMessageInputReader
	parts  docMessageDisplayPartReader
}

func docMessageDisplayBodyPosition(cursor docMessageDisplayCursor) (int, error) {
	var position int
	switch cursor.Kind {
	case DocMessageDisplayMarkdown:
		var body docMappedMarkdownCursor
		if json.Unmarshal(cursor.Body, &body) != nil || body.Version != 1 || body.Plan != cursor.Child || body.Render.Version != 1 || ValidateContentHash(body.Render.Plan) != nil || body.Render.Page < 0 || body.Render.Page > MaxDocRangePages || body.Render.Operation < 0 || body.Render.Operation >= DocMarkdownPlanOperations || body.Render.Byte < 0 || body.Render.Byte > MaxConversationDocumentBytes {
			return 0, ErrIntegrity
		}
		encoded, err := canonicalJSON(body)
		if err != nil || !bytes.Equal(encoded, cursor.Body) {
			return 0, ErrIntegrity
		}
		position = body.Render.TextBytes
	case DocMessageDisplayPlaceholder:
		var body DocMessagePlaceholderCursor
		if json.Unmarshal(cursor.Body, &body) != nil || body.Version != 1 || body.Plan != cursor.Child {
			return 0, ErrIntegrity
		}
		encoded, err := canonicalJSON(body)
		if err != nil || !bytes.Equal(encoded, cursor.Body) {
			return 0, ErrIntegrity
		}
		position = body.From
	default:
		return 0, ErrIntegrity
	}
	if position < 0 || position > cursor.BodyBytes {
		return 0, ErrIntegrity
	}
	return position, nil
}

func decodeDocMessageDisplayCursor(plan DocMessageDisplayPlan, source DocDisplayProjectionSource, raw json.RawMessage) (docMessageDisplayCursor, error) {
	var cursor docMessageDisplayCursor
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Plan != source.Plan || cursor.Part < 0 || cursor.Part > plan.Count || cursor.From < 0 || cursor.From > source.TextBytes || int64(cursor.From) < int64(cursor.Part)*2-1 || cursor.TextBytes < cursor.From || cursor.TextBytes > source.TextBytes || cursor.Part < plan.Count && cursor.TextBytes == source.TextBytes || cursor.Separator < 0 || cursor.Separator > 2 || cursor.Prefix < 0 || cursor.Prefix > cursor.Separator {
		return docMessageDisplayCursor{}, ErrIntegrity
	}
	encoded, err := canonicalJSON(cursor)
	if err != nil || !bytes.Equal(encoded, raw) {
		return docMessageDisplayCursor{}, ErrIntegrity
	}
	if len(cursor.Body) == 0 {
		if cursor.Kind != "" || cursor.Child != "" || cursor.BodyBytes != 0 || cursor.Separator != 0 || cursor.Prefix != 0 || cursor.From != cursor.TextBytes || cursor.Part == 0 && cursor.From != 0 || cursor.Part == plan.Count && cursor.TextBytes != plan.TextBytes {
			return docMessageDisplayCursor{}, ErrIntegrity
		}
		return cursor, nil
	}
	if cursor.Part == plan.Count || ValidateContentHash(cursor.Child) != nil || cursor.BodyBytes <= 0 || cursor.BodyBytes > source.TextBytes-cursor.From-cursor.Separator || cursor.Part == 0 && (cursor.From != 0 || cursor.Separator != 0) || cursor.Part > 0 && cursor.Separator == 0 {
		return docMessageDisplayCursor{}, ErrIntegrity
	}
	position, err := docMessageDisplayBodyPosition(cursor)
	if err != nil || position == cursor.BodyBytes || cursor.Prefix < cursor.Separator && position != 0 || cursor.TextBytes != cursor.From+cursor.Prefix+position {
		return docMessageDisplayCursor{}, ErrIntegrity
	}
	return cursor, nil
}

func readDocMessageDisplayChild(ctx context.Context, inputs *docMessageInputReader, part DocMessageDisplayPart, verifyFirst bool, load func(context.Context, ContentHash) ([]byte, error)) (docMessageDisplayChild, error) {
	blocks, err := inputs.read(ctx, part.Block/DocMessageInputPageEntries, load)
	if err != nil {
		return docMessageDisplayChild{}, err
	}
	input := blocks[part.Block%DocMessageInputPageEntries]
	if part.First.From < input.Anchor().From || part.First.To > input.Anchor().To {
		return docMessageDisplayChild{}, ErrIntegrity
	}
	raw, err := load(ctx, part.Plan)
	if err != nil {
		return docMessageDisplayChild{}, err
	}
	var selected DocMessageDisplayBlockPlan
	switch part.Kind {
	case DocMessageDisplayMarkdown:
		decoded, err := DecodeDocDecodedMarkdownPlan(part.Plan, raw)
		if err != nil {
			return docMessageDisplayChild{}, err
		}
		selected.Markdown = &decoded
	case DocMessageDisplayPlaceholder:
		decoded, err := DecodeDocMessagePlaceholderPlan(part.Plan, raw)
		if err != nil {
			return docMessageDisplayChild{}, err
		}
		selected.Placeholder = &decoded
	default:
		return docMessageDisplayChild{}, ErrIntegrity
	}
	child, err := bindDocMessageDisplayChild(input, selected)
	if err != nil || child.Kind != part.Kind || child.Input.source.Plan != part.Plan || child.Input.source.TextBytes != part.TextBytes {
		return docMessageDisplayChild{}, ErrIntegrity
	}
	if verifyFirst {
		first, err := firstDocMessageDisplaySource(ctx, input, child, load)
		if err != nil {
			return docMessageDisplayChild{}, err
		}
		if first != part.First {
			return docMessageDisplayChild{}, ErrIntegrity
		}
	}
	return child, nil
}

func NewDocMessageDisplayAssemblyInput(plan DocMessageDisplayPlan) (DocDisplayAssemblyInput, error) {
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	initial, err := canonicalJSON(docMessageDisplayCursor{Version: 1, Plan: source.Plan})
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	input := DocDisplayAssemblyInput{source: source, initial: initial}
	input.cursor = func(raw json.RawMessage) (int, error) {
		cursor, err := decodeDocMessageDisplayCursor(plan, source, raw)
		return cursor.TextBytes, err
	}
	single := func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error), work *docMessageDisplayReadWork) (page DocDisplayPage, next json.RawMessage, failure error) {
		defer func() {
			if err := ctx.Err(); err != nil {
				page, next, failure = DocDisplayPage{}, nil, err
			}
		}()
		if err := ctx.Err(); err != nil {
			return DocDisplayPage{}, nil, err
		}
		cursor, err := decodeDocMessageDisplayCursor(plan, source, raw)
		if err != nil || cursor.Part == plan.Count || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns || load == nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		bounded := newDocMappedObjectLoader(load)
		parts, err := work.parts.read(ctx, cursor.Part/DocMessageDisplayPageParts, bounded)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		part := parts[cursor.Part%DocMessageDisplayPageParts]
		if cursor.From != part.From {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		child, err := readDocMessageDisplayChild(ctx, &work.inputs, part, len(cursor.Body) == 0 || cursor.Prefix < part.Separator, bounded)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		if len(cursor.Body) == 0 {
			cursor.Kind, cursor.Child, cursor.BodyBytes = part.Kind, part.Plan, part.TextBytes
			cursor.Separator, cursor.Body = part.Separator, bytes.Clone(child.Input.initial)
		} else if cursor.Kind != part.Kind || cursor.Child != part.Plan || cursor.BodyBytes != part.TextBytes || cursor.Separator != part.Separator {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		bodyPosition, err := child.Input.cursor(cursor.Body)
		if err != nil || cursor.TextBytes != part.From+cursor.Prefix+bodyPosition {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		page = DocDisplayPage{Version: 2, From: cursor.TextBytes}
		page.Source, err = source.Hash()
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		var output strings.Builder
		if cursor.Prefix < part.Separator {
			length := min(maxBytes, part.Separator-cursor.Prefix)
			output.WriteString(strings.Repeat("\n", length))
			page.Runs = append(page.Runs, DocDisplayRun{To: length, SourceFrom: part.First.From, SourceTo: part.First.To})
			cursor.Prefix += length
			cursor.TextBytes += length
		}
		remaining := maxBytes - output.Len()
		if cursor.Prefix == part.Separator && len(page.Runs) < maxRuns && remaining > 0 && (remaining >= utf8.UTFMax || part.TextBytes-bodyPosition <= remaining) {
			body, bodyCursor, err := child.Input.read(ctx, cursor.Body, remaining, maxRuns-len(page.Runs), bounded)
			if err != nil {
				if !errors.Is(err, errDocMappedReplayBudget) || output.Len() == 0 {
					return DocDisplayPage{}, nil, err
				}
			} else {
				position, err := child.Input.cursor(bodyCursor)
				if err != nil || body.From != bodyPosition || position != bodyPosition+len(body.Text) || body.Text == "" || body.validateCoordinates(source.EventBytes, false) != nil || len(body.Text) > remaining || len(body.Runs) > maxRuns-len(page.Runs) {
					return DocDisplayPage{}, nil, ErrIntegrity
				}
				base := output.Len()
				output.WriteString(body.Text)
				for _, run := range body.Runs {
					run.To += base
					page.Runs = append(page.Runs, run)
				}
				cursor.Body, cursor.TextBytes = bodyCursor, cursor.TextBytes+len(body.Text)
				if position == part.TextBytes {
					cursor = docMessageDisplayCursor{Version: 1, Plan: source.Plan, Part: cursor.Part + 1, From: cursor.TextBytes, TextBytes: cursor.TextBytes}
				}
			}
		}
		page.Text = output.String()
		if page.validateCoordinates(source.EventBytes, false) != nil || len(page.Text) > maxBytes || len(page.Runs) > maxRuns {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		next, err = canonicalJSON(cursor)
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		if _, err := decodeDocMessageDisplayCursor(plan, source, next); err != nil {
			return DocDisplayPage{}, nil, err
		}
		return page, next, nil
	}
	input.read = batchDocMessageDisplayRead(input, plan, single)
	return input, input.validate()
}

func batchDocMessageDisplayRead(input DocDisplayAssemblyInput, plan DocMessageDisplayPlan, single func(context.Context, json.RawMessage, int, int, func(context.Context, ContentHash) ([]byte, error), *docMessageDisplayReadWork) (DocDisplayPage, json.RawMessage, error)) func(context.Context, json.RawMessage, int, int, func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, json.RawMessage, error) {
	return func(ctx context.Context, raw json.RawMessage, maxBytes, maxRuns int, load func(context.Context, ContentHash) ([]byte, error)) (page DocDisplayPage, next json.RawMessage, failure error) {
		defer func() {
			if err := ctx.Err(); err != nil {
				page, next, failure = DocDisplayPage{}, nil, err
			}
		}()
		position, err := input.cursor(raw)
		if err != nil || maxBytes <= 0 || maxBytes > DocDisplayPageTextBytes || maxRuns <= 0 || maxRuns > DocDisplayPageRuns || load == nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		page = DocDisplayPage{Version: 2, From: position}
		page.Source, err = input.source.Hash()
		if err != nil {
			return DocDisplayPage{}, nil, err
		}
		bounded := newDocMappedObjectLoader(load)
		work := docMessageDisplayReadWork{inputs: docMessageInputReader{generation: plan.Source.Input}, parts: docMessageDisplayPartReader{plan: plan}}
		next = bytes.Clone(raw)
		var output strings.Builder
		encodedText := 0
		for parts := 0; parts < DocMessageDisplayStepBlocks && position < input.source.TextBytes && output.Len() < maxBytes && len(page.Runs) < maxRuns; parts++ {
			remaining := maxBytes - output.Len()
			if output.Len() > 0 && remaining < utf8.UTFMax && input.source.TextBytes-position > remaining {
				break
			}
			fragment, candidate, err := single(ctx, next, remaining, maxRuns-len(page.Runs), bounded, &work)
			if err != nil {
				if errors.Is(err, errDocMappedReplayBudget) && output.Len() > 0 {
					break
				}
				return DocDisplayPage{}, nil, err
			}
			newPosition, err := input.cursor(candidate)
			if err != nil || fragment.From != position || newPosition != position+len(fragment.Text) || len(fragment.Text) == 0 || len(fragment.Text) > remaining || len(fragment.Runs) > maxRuns-len(page.Runs) || fragment.validateCoordinates(input.source.EventBytes, false) != nil {
				return DocDisplayPage{}, nil, ErrIntegrity
			}
			cost := 0
			for _, character := range fragment.Text {
				cost += docDisplayEncodedRuneBytes(character)
			}
			if encodedText+cost+docDisplayEncodedHeaderBytes+(len(page.Runs)+len(fragment.Runs))*docDisplayEncodedRunBytes > MaxDocDisplayPageBytes {
				if output.Len() == 0 {
					return DocDisplayPage{}, nil, ErrIntegrity
				}
				break
			}
			base := output.Len()
			output.WriteString(fragment.Text)
			for _, run := range fragment.Runs {
				run.To += base
				page.Runs = append(page.Runs, run)
			}
			encodedText += cost
			next, position = candidate, newPosition
		}
		page.Text = output.String()
		if page.validateCoordinates(input.source.EventBytes, false) != nil {
			return DocDisplayPage{}, nil, ErrIntegrity
		}
		return page, next, nil
	}
}
