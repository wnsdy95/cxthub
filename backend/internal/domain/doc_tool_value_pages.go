package domain

import (
	"context"
	"strings"
	"unicode/utf8"
)

type docToolValueBuilder struct {
	source   DocToolValueSource
	state    DocToolValueState
	buffer   docEventDisplayBuilder
	loadRaw  func(context.Context, int, int) ([]byte, error)
	read     int
	actions  int
	emitted  int
	frames   int
	page     DocToolValuePage
	text     strings.Builder
	encoded  int
	planHash ContentHash
	plan     DocToolOrderPlan
	orderKey ContentHash
	order    DocToolOrderPage
}

func newDocToolValueLoader(load func(context.Context, ContentHash) ([]byte, error)) func(context.Context, ContentHash) ([]byte, error) {
	var hashes [4]ContentHash
	var values [4][]byte
	next := 0
	return func(ctx context.Context, hash ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for index, saved := range hashes {
			if saved == hash {
				return values[index], nil
			}
		}
		raw, err := load(ctx, hash)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 || len(raw) > MaxDocDisplayPageBytes || HashContent(raw) != hash {
			return nil, ErrIntegrity
		}
		hashes[next], values[next] = hash, raw
		next = (next + 1) % len(hashes)
		return raw, nil
	}
}

func (builder *docToolValueBuilder) raw(ctx context.Context, from, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if from < builder.source.Tree.Root.From || length <= 0 || from > builder.source.Tree.Root.To-length {
		return nil, ErrIntegrity
	}
	if length > DocToolValueStepSourceBytes-builder.read {
		return nil, errDocToolValueYield
	}
	raw, err := builder.loadRaw(ctx, from, length)
	if err != nil {
		return nil, err
	}
	if len(raw) != length {
		return nil, ErrIntegrity
	}
	builder.read += length
	return raw, ctx.Err()
}

func (builder *docToolValueBuilder) resetPage() {
	builder.page = DocToolValuePage{Version: 1, Source: builder.state.Source, Display: DocDisplayPage{Version: 6, Source: builder.state.Source, Index: builder.state.Directory.Count, From: builder.state.TextBytes}}
	builder.text = strings.Builder{}
	builder.encoded = 0
}

func (builder *docToolValueBuilder) seal(final bool) error {
	if builder.text.Len() == 0 {
		return ErrIntegrity
	}
	builder.page.Sealed, builder.page.Final = true, final
	builder.page.Display.Text = builder.text.String()
	raw, err := builder.page.Encode(builder.source)
	if err != nil {
		return err
	}
	hash, err := builder.buffer.put(raw, nil)
	if err != nil {
		return err
	}
	directory, objects, err := builder.state.Directory.Append(builder.buffer.ctx, hash)
	if err != nil {
		return err
	}
	if err := builder.buffer.append(objects); err != nil {
		return err
	}
	builder.state.Directory, builder.state.Pending = directory, ""
	builder.resetPage()
	return nil
}

func (builder *docToolValueBuilder) emit(text string, anchor DocFindSpan, linear bool) error {
	if text == "" {
		return nil
	}
	if !utf8.ValidString(text) || anchor.From < builder.source.Tree.Root.From || anchor.To > builder.source.Tree.Root.To || anchor.To <= anchor.From || linear && anchor.To-anchor.From != len(text) {
		return ErrIntegrity
	}
	if len(text) > MaxConversationDocumentBytes-builder.state.TextBytes {
		return ErrDocFindDisplayUnavailable
	}
	for from := 0; from < len(text); {
		if err := builder.buffer.ctx.Err(); err != nil {
			return err
		}
		selected := anchor
		if linear {
			selected.From += from
		}
		last := len(builder.page.Display.Runs) - 1
		merge := last >= 0 && (linear && builder.page.Display.Runs[last].Linear && builder.page.Display.Runs[last].SourceTo == selected.From || !linear && !builder.page.Display.Runs[last].Linear && builder.page.Display.Runs[last].SourceFrom == selected.From && builder.page.Display.Runs[last].SourceTo == selected.To)
		if builder.text.Len() == DocDisplayPageTextBytes || len(builder.page.Display.Runs) == DocDisplayPageRuns && !merge {
			if err := builder.seal(false); err != nil {
				return err
			}
			continue
		}
		limit := min(len(text), from+DocDisplayPageTextBytes-builder.text.Len())
		encoded, end := 0, from
		for end < limit {
			character, width := utf8.DecodeRuneInString(text[end:])
			if end+width > limit || builder.encoded+encoded+docDisplayEncodedRuneBytes(character)+docDisplayEncodedHeaderBytes+(len(builder.page.Display.Runs)+1)*docDisplayEncodedRunBytes+128 > MaxDocDisplayPageBytes {
				break
			}
			encoded += docDisplayEncodedRuneBytes(character)
			end += width
		}
		if end == from {
			if err := builder.seal(false); err != nil {
				return err
			}
			continue
		}
		builder.text.WriteString(text[from:end])
		builder.encoded += encoded
		if linear {
			selected.To = selected.From + end - from
		}
		if merge {
			previous := &builder.page.Display.Runs[last]
			previous.To = builder.text.Len()
			if linear {
				previous.SourceTo = selected.To
			}
		} else {
			builder.page.Display.Runs = append(builder.page.Display.Runs, DocDisplayRun{To: builder.text.Len(), SourceFrom: selected.From, SourceTo: selected.To, Linear: linear})
		}
		builder.state.TextBytes += end - from
		builder.emitted += end - from
		from = end
	}
	return nil
}

func (builder *docToolValueBuilder) fragment(page DocDisplayPage) error {
	if page.validateCoordinates(builder.source.Tree.EventBytes, false) != nil {
		return ErrIntegrity
	}
	from := 0
	for _, run := range page.Runs {
		if err := builder.emit(page.Text[from:run.To], DocFindSpan{From: run.SourceFrom, To: run.SourceTo}, run.Linear); err != nil {
			return err
		}
		from = run.To
	}
	return nil
}

func (builder *docToolValueBuilder) savePending() error {
	if builder.text.Len() == 0 {
		return nil
	}
	builder.page.Display.Text = builder.text.String()
	raw, err := builder.page.Encode(builder.source)
	if err != nil {
		return err
	}
	hash := HashContent(raw)
	if hash != builder.state.Pending {
		if _, err := builder.buffer.put(raw, nil); err != nil {
			return err
		}
	}
	builder.state.Pending = hash
	return nil
}
