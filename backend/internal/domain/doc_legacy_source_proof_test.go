package domain

import (
	"context"
)

type legacySourceProofBuffer struct {
	body     []byte
	reads    [][2]int
	onAppend func()
}

func (buffer *legacySourceProofBuffer) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	buffer.body = append(buffer.body, body...)
	if buffer.onAppend != nil {
		buffer.onAppend()
	}
	return ctx.Err()
}

func (buffer *legacySourceProofBuffer) Read(ctx context.Context, offset, length int) ([]byte, error) {
	buffer.reads = append(buffer.reads, [2]int{offset, length})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > len(buffer.body) || length > len(buffer.body)-offset {
		return nil, ErrIntegrity
	}
	return buffer.body[offset : offset+length], nil
}
