package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type historyReadSpy struct {
	*store.FSStore
	whole, chunks            int
	forbidden                domain.ContentHash
	tamperChunk, tamperEvent bool
}

func (s *historyReadSpy) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	s.whole++
	return domain.SessionDoc{}, errors.New("whole document read forbidden")
}
func (s *historyReadSpy) GetChunk(ctx context.Context, repo, hash domain.ContentHash) ([]byte, error) {
	s.chunks++
	if hash == s.forbidden {
		return nil, errors.New("unused early source chunk read")
	}
	b, err := s.FSStore.GetChunk(ctx, repo, hash)
	if s.tamperChunk && len(b) > 0 {
		b[0] ^= 1
	}
	return b, err
}
func (s *historyReadSpy) DocReadIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocReadIndex, error) {
	idx, err := s.FSStore.DocReadIndex(ctx, repo, hash)
	if s.tamperEvent && len(idx.Events) > 0 {
		idx.Events[len(idx.Events)-1].Hash = domain.HashContent([]byte("tampered event"))
	}
	return idx, err
}
