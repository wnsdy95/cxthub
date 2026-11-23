package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchPullReadKey struct{}
type branchPullStore struct {
	*store.FSStore
	t           *testing.T
	events      []domain.HistoryEvent
	reads       int
	memoryCalls int
	readErr     error
}

func (s *branchPullStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	s.t.Fatal("plan attempted write transaction")
	return nil
}
func (s *branchPullStore) WithinReadSnapshot(ctx context.Context, f func(context.Context) error) error {
	if ctx.Value(branchPullReadKey{}) == true {
		return f(ctx)
	}
	s.reads++
	if err := f(context.WithValue(ctx, branchPullReadKey{}, true)); err != nil {
		return err
	}
	return s.readErr
}
func (s *branchPullStore) check(ctx context.Context) {
	s.t.Helper()
	if ctx.Value(branchPullReadKey{}) != true {
		s.t.Fatal("read escaped repositoryRead")
	}
}
func (s *branchPullStore) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	s.check(ctx)
	return s.FSStore.GetRepo(ctx, id)
}
func (s *branchPullStore) ListSnapshots(ctx context.Context, id domain.ContentHash, b string) ([]domain.Snapshot, error) {
	s.check(ctx)
	return s.FSStore.ListSnapshots(ctx, id, b)
}
func (s *branchPullStore) ListHistoryEvents(ctx context.Context, id domain.ContentHash) ([]domain.HistoryEvent, error) {
	s.check(ctx)
	return s.events, nil
}
func (s *branchPullStore) GetMemory(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error) {
	s.memoryCalls++
	panic("branch plan opened memory body")
}
func (s *branchPullStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	s.t.Fatal("plan opened transcript")
	return domain.SessionDoc{}, nil
}
func (s *branchPullStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	s.t.Fatal("plan opened doc manifest")
	return domain.DocChunkManifest{}, nil
}
func (s *branchPullStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	s.t.Fatal("plan opened doc chunk")
	return nil, nil
}
