package inbound

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type SessionNoticeInput struct {
	Cwd       string
	Provider  domain.ProviderKind
	SessionID string
}
type SessionNotices interface {
	DeliverSessionNotice(context.Context, SessionNoticeInput, func(domain.SessionNotice) error) error
}
