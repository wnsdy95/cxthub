package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type workerWebhookBackend struct {
	Backend
	got   context.Context
	calls int
}

func (b *workerWebhookBackend) DocumentIdentitiesSupported() []domain.DocumentIdentity {
	return []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
}
func (b *workerWebhookBackend) RootPublicationEnabled() bool { return false }
func (b *workerWebhookBackend) PromoteMergedPR(ctx context.Context, _ string, _ domain.PullRequestMerge) (int, error) {
	b.got = ctx
	b.calls++
	return 0, nil
}
