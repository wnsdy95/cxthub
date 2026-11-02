package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocumentFindQuery interface {
	FindDocPage(context.Context, domain.ContentHash, domain.ContentHash, domain.DocFindPageRequest) (domain.DocFindPage, error)
}

type ConversationFindQuery interface {
	QueryConversationFind(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) (domain.ConversationFindPage, error)
}

type ConversationFindDemand interface {
	RequestConversationFind(context.Context, domain.ContentHash, domain.ContentHash, domain.ConversationFindRequest) error
}
