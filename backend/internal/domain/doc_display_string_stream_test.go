package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

type docDisplayStringTestReader struct {
	reader     io.Reader
	chunk      int
	maxRequest int
	readBytes  int
	onRead     func()
}

func (reader *docDisplayStringTestReader) Read(buffer []byte) (int, error) {
	reader.maxRequest = max(reader.maxRequest, len(buffer))
	if reader.chunk > 0 {
		buffer = buffer[:min(reader.chunk, len(buffer))]
	}
	count, err := reader.reader.Read(buffer)
	reader.readBytes += count
	if reader.onRead != nil {
		reader.onRead()
	}
	return count, err
}

type docDisplayStringTestErrorReader struct {
	data string
	err  error
}

func (reader *docDisplayStringTestErrorReader) Read(buffer []byte) (int, error) {
	count := copy(buffer, reader.data)
	reader.data = reader.data[count:]
	return count, reader.err
}

type docDisplayStringTestContext struct {
	context.Context
	remaining int
}

func (ctx *docDisplayStringTestContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

type docDisplayStringTestGeneratedEvent struct {
	prefix string
	count  int
	suffix string
}

func (event docDisplayStringTestGeneratedEvent) size() int {
	return len(event.prefix) + event.count + len(event.suffix)
}

func (event docDisplayStringTestGeneratedEvent) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, ErrValidation
	}
	count := min(len(buffer), max(0, event.size()-int(offset)))
	for position := range count {
		absolute := int(offset) + position
		switch {
		case absolute < len(event.prefix):
			buffer[position] = event.prefix[absolute]
		case absolute < len(event.prefix)+event.count:
			buffer[position] = 'x'
		default:
			buffer[position] = event.suffix[absolute-len(event.prefix)-event.count]
		}
	}
	if count != len(buffer) {
		return count, io.EOF
	}
	return count, nil
}

func (event docDisplayStringTestGeneratedEvent) reader() io.Reader {
	return io.NewSectionReader(event, 0, int64(event.size()))
}

func (event docDisplayStringTestGeneratedEvent) hash(test *testing.T) ContentHash {
	test.Helper()
	digest := sha256.New()
	count, err := io.Copy(digest, event.reader())
	if err != nil || count != int64(event.size()) {
		test.Fatal("generated source hash", count, err)
	}
	return ContentHash("sha256:" + hex.EncodeToString(digest.Sum(nil)))
}
