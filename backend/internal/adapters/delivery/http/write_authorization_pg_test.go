//go:build postgres

package http

import (
	"strings"
	"sync"
)

type pausedWriteBody struct {
	reader           *strings.Reader
	entered, release chan struct{}
	once             sync.Once
}

func (b *pausedWriteBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (b *pausedWriteBody) Close() error { return nil }
