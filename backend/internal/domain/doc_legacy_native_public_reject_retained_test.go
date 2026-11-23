package domain

import (
	"context"
	"io"
)

type legacyNativePublicRejectSource struct {
	prefix  string
	suffix  string
	digits  int
	page    []byte
	loads   int
	eofZero int
	eofOne  int
}

func (fixture *legacyNativePublicRejectSource) fill(offset, length int) []byte {
	body := fixture.page[:length]
	for index := range body {
		body[index] = '9'
	}
	if offset < len(fixture.prefix) {
		copy(body, fixture.prefix[offset:])
	}
	suffixStart := len(fixture.prefix) + fixture.digits
	if offset+length > suffixStart {
		copy(body[max(0, suffixStart-offset):], fixture.suffix[max(0, offset-suffixStart):])
	}
	return body
}

func (fixture *legacyNativePublicRejectSource) read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	extent := len(fixture.prefix) + fixture.digits + len(fixture.suffix)
	if offset == extent && length == 0 {
		fixture.eofZero++
		return nil, nil
	}
	if offset == extent && length == 1 {
		fixture.eofOne++
		return nil, io.EOF
	}
	if offset < 0 || offset >= extent || offset%ConversationManifestChunkBytes != 0 ||
		length != min(ConversationManifestChunkBytes, extent-offset) {
		return nil, ErrIntegrity
	}
	fixture.loads++
	return fixture.fill(offset, length), nil
}
