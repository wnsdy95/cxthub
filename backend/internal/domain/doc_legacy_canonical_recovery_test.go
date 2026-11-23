package domain

import (
	"context"
	"testing"
)

type legacyCanonicalRecoveryRead struct {
	segment string
	offset  int
	length  int
}

type legacyCanonicalRecoveryBuffer struct {
	test     testing.TB
	envelope []byte
	stream   []byte
	reads    []legacyCanonicalRecoveryRead
}

func (buffer *legacyCanonicalRecoveryBuffer) read(ctx context.Context, segment string, offset, length int) ([]byte, error) {
	buffer.test.Helper()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := buffer.envelope
	if segment == "stream" {
		body = buffer.stream
	}
	if offset < 0 || length <= 0 || offset > len(body) || length > len(body)-offset || length > 512<<10 {
		buffer.test.Fatalf("invalid %s callback range: %d + %d / %d", segment, offset, length, len(body))
	}
	buffer.reads = append(buffer.reads, legacyCanonicalRecoveryRead{segment: segment, offset: offset, length: length})
	return body[offset : offset+length], nil
}
