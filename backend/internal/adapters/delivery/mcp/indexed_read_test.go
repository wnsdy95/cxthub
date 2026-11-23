package mcp

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type indexedPageBackend struct {
	pageBackend
	reader *app.Service
	whole  int
}

func (b *indexedPageBackend) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	b.whole++
	return domain.SessionDoc{}, fmt.Errorf("whole archive reads forbidden")
}
func (b *indexedPageBackend) ReadDocFragments(ctx context.Context, r, h domain.ContentHash, i, o, n, budget int) (domain.DocFragmentPage, error) {
	return b.reader.ReadDocFragments(ctx, r, h, i, o, n, budget)
}
func (b *indexedPageBackend) SearchDocEvents(ctx context.Context, r, h domain.ContentHash, q string, a, n int) ([]domain.DocEventIndex, error) {
	return b.reader.SearchDocEvents(ctx, r, h, q, a, n)
}

type rootCandidateBackend struct {
	indexedPageBackend
	searches int
	err      error
}

func (b *rootCandidateBackend) MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error) {
	return map[domain.ContentHash]bool{}, nil
}

func (b *rootCandidateBackend) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	b.searches++
	if b.err != nil {
		return nil, b.err
	}
	return []domain.DocEventIndex{{Index: 0, Seq: 7, Role: string(domain.RoleUser), Text: "needle"}}, nil
}
