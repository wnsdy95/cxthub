package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docChunkRangeReader(ctx context.Context, streamBytes int64, locate func(int) (domain.ConversationManifestChunk, error), load func(context.Context, int, domain.ConversationManifestChunk) ([]byte, error)) func(int, int) ([]byte, error) {
	cachedChunk := -1
	var body []byte
	return func(offset, length int) ([]byte, error) {
		if offset < 0 || length <= 0 || length > docViewBytes || int64(offset) > streamBytes-int64(length) {
			return nil, domain.ErrValidation
		}
		result := make([]byte, 0, length)
		for position := offset; position < offset+length; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			chunk := position / domain.ConversationManifestChunkBytes
			if cachedChunk != chunk {
				descriptor, err := locate(chunk)
				if err != nil {
					return nil, err
				}
				if descriptor.Bytes != min(int64(domain.ConversationManifestChunkBytes), streamBytes-int64(chunk)*domain.ConversationManifestChunkBytes) {
					return nil, domain.ErrIntegrity
				}
				body, err = load(ctx, chunk, descriptor)
				if err != nil {
					return nil, err
				}
				if int64(len(body)) != descriptor.Bytes || domain.HashContent(body) != descriptor.Hash {
					return nil, domain.ErrIntegrity
				}
				cachedChunk = chunk
			}
			start := position % domain.ConversationManifestChunkBytes
			count := min(offset+length-position, len(body)-start)
			if count <= 0 {
				return nil, domain.ErrIntegrity
			}
			result = append(result, body[start:start+count]...)
			position += count
		}
		return result, nil
	}
}
