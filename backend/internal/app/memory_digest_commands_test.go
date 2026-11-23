package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryDigestCommandPorts struct {
	test           *testing.T
	repo           domain.ContentHash
	target         domain.ContentHash
	previousHash   domain.ContentHash
	previous       domain.MemoryDigest
	digest         domain.MemoryDigest
	expected       domain.ContentHash
	next           domain.ContentHash
	calls          []string
	snapshotErr    error
	parentErr      error
	objectErr      error
	attachmentErr  error
	objectScopeErr error
	attachScopeErr error
	observe        func(string, context.Context)
}

func (ports *memoryDigestCommandPorts) record(stage string, ctx context.Context, repo domain.ContentHash) {
	ports.test.Helper()
	if repo != ports.repo {
		ports.test.Fatal("command changed repository", repo)
	}
	ports.calls = append(ports.calls, stage)
	if ports.observe != nil {
		ports.observe(stage, ctx)
	}
}

func (ports *memoryDigestCommandPorts) GetSnapshot(ctx context.Context, repo, target domain.ContentHash) (domain.Snapshot, error) {
	ports.record("snapshot", ctx, repo)
	if target != ports.target {
		ports.test.Fatal("command changed target", target)
	}
	return domain.Snapshot{ID: target, RepoID: repo}, ports.snapshotErr
}

func (ports *memoryDigestCommandPorts) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	ports.record("parent", ctx, repo)
	if hash != ports.previousHash {
		ports.test.Fatal("command changed causal parent", hash)
	}
	return ports.previous, ports.parentErr
}

func (ports *memoryDigestCommandPorts) PutMemory(ctx context.Context, repo domain.ContentHash, digest domain.MemoryDigest) (domain.ContentHash, error) {
	ports.record("object", ctx, repo)
	ports.digest = digest
	return domain.HashContent([]byte("stored memory object")), ports.objectErr
}

func (ports *memoryDigestCommandPorts) CompareAndSwapSnapshotMemory(ctx context.Context, repo, target, expected, next domain.ContentHash) error {
	ports.record("attachment", ctx, repo)
	if target != ports.target {
		ports.test.Fatal("CAS changed target", target)
	}
	ports.expected, ports.next = expected, next
	return ports.attachmentErr
}

func (ports *memoryDigestCommandPorts) WithinMemoryObjectWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) (domain.ContentHash, error)) (domain.ContentHash, error) {
	ports.record("object scope", ctx, repo)
	if ports.objectScopeErr != nil {
		return "", ports.objectScopeErr
	}
	return write(context.WithValue(ctx, memoryDigestCommandScopeKey{}, "object"))
}

func (ports *memoryDigestCommandPorts) WithinMemoryAttachmentWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	ports.record("attachment scope", ctx, repo)
	if ports.attachScopeErr != nil {
		return ports.attachScopeErr
	}
	return write(context.WithValue(ctx, memoryDigestCommandScopeKey{}, "attachment"))
}

type memoryDigestCommandScopeKey struct{}
