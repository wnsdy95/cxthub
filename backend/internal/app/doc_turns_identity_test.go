package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type agentPageReadKey struct{}
type agentPageReadStore struct {
	*store.FSStore
	transactions int
	proofs       map[domain.ContentHash]int
	after        func(domain.ContentHash)
	snapshot     func(domain.Snapshot) domain.Snapshot
}

func (s *agentPageReadStore) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	if ctx.Value(agentPageReadKey{}) != nil {
		return errors.New("nested repository read")
	}
	s.transactions++
	return fn(context.WithValue(ctx, agentPageReadKey{}, true))
}
func (s *agentPageReadStore) WithinRepository(context.Context, domain.ContentHash, func(context.Context) error) error {
	return errors.New("unexpected repository write")
}
func (s *agentPageReadStore) bound(ctx context.Context) error {
	if ctx.Value(agentPageReadKey{}) != true {
		return errors.New("read outside snapshot")
	}
	return ctx.Err()
}
func (s *agentPageReadStore) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if err := s.bound(ctx); err != nil {
		return domain.Snapshot{}, err
	}
	snap, err := s.FSStore.GetSnapshot(ctx, repo, hash)
	if err == nil && s.snapshot != nil {
		snap = s.snapshot(snap)
	}
	return snap, err
}
func (s *agentPageReadStore) ReadVerifiedDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	if err := s.bound(ctx); err != nil {
		return domain.VerifiedSessionDoc{}, err
	}
	s.proofs[hash]++
	doc, err := s.FSStore.ReadVerifiedDoc(ctx, repo, hash)
	if err == nil && s.after != nil {
		s.after(hash)
	}
	return doc, err
}
func (s *agentPageReadStore) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	if err := s.bound(ctx); err != nil {
		return domain.DocReadIndex{}, err
	}
	return s.FSStore.DocReadIndex(ctx, repo, hash)
}
func (s *agentPageReadStore) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if err := s.bound(ctx); err != nil {
		return domain.SessionDoc{}, err
	}
	return s.FSStore.GetDoc(ctx, repo, hash)
}
func (s *agentPageReadStore) GetDocManifest(ctx context.Context, repo, hash domain.ContentHash) (domain.DocChunkManifest, error) {
	if err := s.bound(ctx); err != nil {
		return domain.DocChunkManifest{}, err
	}
	return s.FSStore.GetDocManifest(ctx, repo, hash)
}
func (s *agentPageReadStore) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	if err := s.bound(ctx); err != nil {
		return nil, err
	}
	return s.FSStore.GetChunk(ctx, repo, hash)
}
