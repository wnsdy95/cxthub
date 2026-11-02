package domain

import (
	"context"
	"unicode/utf8"
)

func firstDocMessageDisplaySource(ctx context.Context, input DocMessageInput, child docMessageDisplayChild, load func(context.Context, ContentHash) ([]byte, error)) (DocFindSpan, error) {
	page, cursor, err := child.Input.read(ctx, child.Input.initial, utf8.UTFMax, 1, load)
	if err != nil {
		return DocFindSpan{}, err
	}
	hash, err := child.Input.source.Hash()
	position, cursorErr := child.Input.cursor(cursor)
	if err != nil || cursorErr != nil || page.Version != 2 || page.Source != hash || page.From != 0 || len(page.Text) > utf8.UTFMax || len(page.Text) > child.Input.source.TextBytes || len(page.Runs) != 1 || position != len(page.Text) || page.validateCoordinates(input.source.EventBytes, false) != nil {
		return DocFindSpan{}, ErrIntegrity
	}
	_, width := utf8.DecodeRuneInString(page.Text)
	first, err := page.SourceRange(0, width)
	if err != nil || first.From < input.entry.Anchor.From || first.To > input.entry.Anchor.To || child.Kind == DocMessageDisplayMarkdown && (input.entry.Text == nil || first.From <= input.entry.Text.From || first.To >= input.entry.Text.To) || child.Kind == DocMessageDisplayPlaceholder && first != input.entry.Anchor {
		return DocFindSpan{}, ErrIntegrity
	}
	return first, nil
}
