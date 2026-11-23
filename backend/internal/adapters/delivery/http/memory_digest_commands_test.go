package http

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type memoryDigestHTTPBackend struct{ *repositoryRefHTTPBackend }

func (*memoryDigestHTTPBackend) PutMemoryDigest(context.Context, domain.ContentHash, domain.MemoryDigest) (domain.ContentHash, error) {
	panic("memory command reached broad HTTP backend")
}

func (*memoryDigestHTTPBackend) PutMemoryDigestCAS(context.Context, domain.ContentHash, domain.MemoryDigest) (domain.ContentHash, error) {
	panic("memory CAS reached broad HTTP backend")
}

type memoryDigestHTTPScope struct {
	test  *testing.T
	calls []string
	err   error
}

func (scope *memoryDigestHTTPScope) check(ctx context.Context, stage string) {
	scope.test.Helper()
	if actor, system := inbound.RepositoryActor(ctx); actor != "actor" || system {
		scope.test.Fatal("memory command lost authenticated actor", actor, system)
	}
	scope.calls = append(scope.calls, stage)
}

func (scope *memoryDigestHTTPScope) WithinMemoryObjectWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.ContentHash, error)) (domain.ContentHash, error) {
	scope.check(ctx, "object")
	return write(ctx)
}

func (scope *memoryDigestHTTPScope) WithinMemoryAttachmentWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	scope.check(ctx, "attachment")
	if scope.err != nil {
		return scope.err
	}
	return write(ctx)
}
