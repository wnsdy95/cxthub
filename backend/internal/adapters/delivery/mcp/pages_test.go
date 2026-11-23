package mcp

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pageBackend struct {
	fakeContextBackend
	docs   map[domain.ContentHash]domain.SessionDoc
	refs   []domain.Ref
	events []domain.HistoryEvent
}

func (f pageBackend) GetDoc(_ context.Context, repo, id domain.ContentHash) (domain.SessionDoc, error) {
	d, ok := f.docs[id]
	if !ok {
		return d, domain.ErrNotFound
	}
	return d, nil
}
func (f pageBackend) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	return f.refs, nil
}
func (f pageBackend) ListHistory(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error) {
	return f.events, nil
}
func pageHash(i int) domain.ContentHash { return domain.ContentHash(fmt.Sprintf("sha256:%064x", i)) }
