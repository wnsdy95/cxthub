//go:build postgres

package http

import (
	"context"
	"strings"
	"sync"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
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

type snapshotGraftPGPoisonBackend struct{ *repositoryRefPGPoisonBackend }

func (*snapshotGraftPGPoisonBackend) GraftSnapshotParents(context.Context, domain.ContentHash, domain.ContentHash, []domain.ContentHash, uint64) error {
	panic("graft reached the broad PostgreSQL HTTP backend")
}
