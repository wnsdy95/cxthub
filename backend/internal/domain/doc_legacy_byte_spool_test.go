package domain

import (
	"bytes"
	"context"
)

type legacyByteSpoolTestSource struct {
	body           []byte
	appends        []int
	reads          []canonicalSpan
	appendMode     string
	readMode       string
	appendError    error
	readError      error
	failReadAt     int
	extentError    error
	endError       error
	cancel         context.CancelFunc
	cancelAppendAt int
	cancelReadAt   int
}

func (source *legacyByteSpoolTestSource) Append(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source.appends = append(source.appends, len(body))
	if source.appendError != nil {
		return source.appendError
	}
	start := len(source.body)
	switch source.appendMode {
	case "drop":
	case "truncate":
		source.body = append(source.body, body[:len(body)-1]...)
	default:
		source.body = append(source.body, body...)
	}
	switch source.appendMode {
	case "corrupt":
		source.body[start] ^= 1
	case "extra":
		source.body = append(source.body, 0)
	case "rewrite-first":
		if len(source.appends) == 2 {
			source.body[0] ^= 1
		}
	case "mutate-input":
		clear(body)
	}
	if source.cancel != nil && len(source.appends) == source.cancelAppendAt {
		source.cancel()
	}
	return nil
}

func (source *legacyByteSpoolTestSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source.reads = append(source.reads, canonicalSpan{offset: offset, length: length})
	if source.cancel != nil && len(source.reads) == source.cancelReadAt {
		source.cancel()
	}
	if source.readError != nil && (source.failReadAt == 0 || source.failReadAt == len(source.reads)) {
		return nil, source.readError
	}
	if length == 0 && source.endError != nil {
		return nil, source.endError
	}
	if offset < 0 || length < 0 || offset > len(source.body) || length > len(source.body)-offset {
		if source.extentError != nil {
			return nil, source.extentError
		}
		return nil, ErrIntegrity
	}
	body := source.body[offset : offset+length]
	if length == 0 {
		return body, nil
	}
	switch source.readMode {
	case "corrupt":
		body = bytes.Clone(body)
		body[0] ^= 1
	case "short":
		body = body[:len(body)-1]
	case "long":
		body = append(bytes.Clone(body), 0)
	case "empty":
		body = nil
	}
	return body, nil
}
