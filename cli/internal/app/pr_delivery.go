package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Only acceptance releases local delivery responsibility; it does not claim the
// server has already appended the context. Pull observes completed server state.
func (s *SyncRepoService) flushPRDeliveries(ctx context.Context, repo string) error {
	local, ok := s.store.(outbound.PRDeliveryStore)
	if !ok {
		return nil
	}
	pending, err := local.PendingPRDeliveries(ctx, repo)
	if err != nil {
		return fmt.Errorf("read durable PR deliveries: %w", err)
	}
	remote, ok := s.remote.(interface {
		SubmitPRPromotion(context.Context, string, domain.PullRequestMerge) error
	})
	if !ok {
		return nil
	}
	for i, pr := range pending {
		if i >= 20 {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := remote.SubmitPRPromotion(ctx, repo, pr); err != nil {
			continue
		}
		if err := local.AcceptPRDelivery(ctx, repo, pr); err != nil {
			return fmt.Errorf("record server PR acceptance: %w", err)
		}
	}
	return nil
}
