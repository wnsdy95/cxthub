package app

import (
	"context"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Retain the adapter's optional interfaces while making expensive archive
// reads/publication/indexing fail if the memory path accidentally invokes them.
type memoryIndependenceStore struct {
	*store.FSStore
	t *testing.T
}

func (s *memoryIndependenceStore) unexpected(operation string) error {
	s.t.Helper()
	s.t.Errorf("memory-only operation called %s", operation)
	return domain.ErrIntegrity
}
func (s *memoryIndependenceStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, s.unexpected("GetDoc")
}
func (s *memoryIndependenceStore) PutDoc(context.Context, domain.ContentHash, domain.SessionDoc) (bool, error) {
	return false, s.unexpected("PutDoc")
}
func (s *memoryIndependenceStore) PutVerifiedDoc(context.Context, domain.ContentHash, domain.VerifiedSessionDoc) (bool, error) {
	return false, s.unexpected("PutVerifiedDoc")
}
func (s *memoryIndependenceStore) VerifyStoredDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedDocReference, error) {
	return domain.VerifiedDocReference{}, s.unexpected("VerifyStoredDoc")
}
func (s *memoryIndependenceStore) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	return domain.DocReadIndex{}, s.unexpected("DocReadIndex")
}
func (s *memoryIndependenceStore) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	return nil, s.unexpected("SearchDocEvents")
}
func (s *memoryIndependenceStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	return domain.DocChunkManifest{}, s.unexpected("GetDocManifest")
}
func (s *memoryIndependenceStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	return nil, s.unexpected("GetChunk")
}
func (s *memoryIndependenceStore) PutChunks(context.Context, domain.ContentHash, map[domain.ContentHash][]byte) (int, int, error) {
	return 0, 0, s.unexpected("PutChunks")
}
