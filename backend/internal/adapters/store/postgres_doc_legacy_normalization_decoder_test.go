//go:build postgres

package store

import (
	"context"
	"io"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyNormalizationDecoderTestSource struct {
	body  []byte
	parts []domain.ConversationManifestChunk
}

func (source *legacyNormalizationDecoderTestSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset > len(source.body) {
		return nil, domain.ErrIntegrity
	}
	if length > len(source.body)-offset {
		return nil, io.EOF
	}
	return source.body[offset : offset+length], nil
}
