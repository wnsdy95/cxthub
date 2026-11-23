package domain

import (
	"context"
	"testing"
)

type legacyMetadataWorkStream struct {
	*legacyTestStream
	test  testing.TB
	ctx   context.Context
	reads []canonicalSpan
}

func (source *legacyMetadataWorkStream) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if source.ctx != nil && ctx != source.ctx {
		source.test.Fatal("source read retained an earlier request context")
	}
	source.reads = append(source.reads, canonicalSpan{offset: offset, length: length})
	return source.legacyTestStream.Read(ctx, offset, length)
}
