package inbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type PRPromotionDeliverer interface {
	DeliverPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (UpdateRefOutput, error)
}

type PRPromotionCommands interface {
	PRPromotionDeliverer
	SubmitPRPromotion(context.Context, domain.ContentHash, domain.PullRequestMerge) (domain.PRPromotionJob, error)
	PromoteRepositoryPR(context.Context, domain.ContentHash, domain.PullRequestMerge) (UpdateRefOutput, error)
	RetryPRPromotion(context.Context, domain.ContentHash, string) error
	ListPRPromotions(context.Context, domain.ContentHash) ([]domain.PRPromotionJob, error)
}
