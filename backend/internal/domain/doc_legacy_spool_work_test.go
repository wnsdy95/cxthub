package domain

import (
	"context"
	"testing"
)

type legacySpoolWorkCall struct {
	append bool
	span   canonicalSpan
}

type legacySpoolWorkSource struct {
	*legacyByteSpoolTestSource
	test  testing.TB
	ctx   context.Context
	calls []legacySpoolWorkCall
}

func (source *legacySpoolWorkSource) Append(ctx context.Context, body []byte) error {
	if source.ctx != nil && ctx != source.ctx {
		source.test.Fatal("spool write retained an earlier request context")
	}
	source.calls = append(source.calls, legacySpoolWorkCall{append: true, span: canonicalSpan{offset: len(source.body), length: len(body)}})
	return source.legacyByteSpoolTestSource.Append(ctx, body)
}

func (source *legacySpoolWorkSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if source.ctx != nil && ctx != source.ctx {
		source.test.Fatal("spool read retained an earlier request context")
	}
	source.calls = append(source.calls, legacySpoolWorkCall{span: canonicalSpan{offset: offset, length: length}})
	return source.legacyByteSpoolTestSource.Read(ctx, offset, length)
}
