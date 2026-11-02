package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) QueryConversationFind(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) (domain.ConversationFindPage, error) {
	queries, err := service.newDocumentQueries(documentquery.LegacyCompatibility, true)
	if err != nil {
		return domain.ConversationFindPage{}, err
	}
	return queries.QueryConversationFind(ctx, repo, hash, request)
}

func (service *Service) RequestConversationFind(ctx context.Context, repo, hash domain.ContentHash, request domain.ConversationFindRequest) error {
	if outbound.DocReadOnly(ctx) {
		return domain.ErrForbidden
	}
	queries, err := service.newDocumentQueries(documentquery.LegacyCompatibility, true)
	if err != nil {
		return err
	}
	return queries.PrepareConversationFind(ctx, repo, hash, request)
}
