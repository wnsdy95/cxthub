package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
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
		if err := local.AttemptPRDelivery(ctx, repo, pr); err != nil {
			return fmt.Errorf("record PR delivery attempt: %w", err)
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

// QueuePullRequest hands discovered host evidence to a durable delivery queue.
// Success means local responsibility is recorded, not that context is merged.
// Server acceptance is attempted independently of source publication/pull.
func (s *SyncRepoService) QueuePullRequest(ctx context.Context, in inbound.SyncInput, pr outbound.MergedPullRequest) error {
	repo, err := s.repoID(ctx, in)
	if err != nil {
		return err
	}
	local, ok := s.store.(outbound.PRDeliveryStore)
	if !ok {
		return fmt.Errorf("durable PR delivery storage unavailable")
	}
	request := domain.PullRequestMerge{Number: pr.Number, BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeCommitSHA}
	if err := local.QueuePRDelivery(ctx, repo, request); err != nil {
		return err
	}
	remote, ok := s.remote.(interface {
		SubmitPRPromotion(context.Context, string, domain.PullRequestMerge) error
	})
	if !ok {
		return nil
	}
	if err := local.AttemptPRDelivery(ctx, repo, request); err != nil {
		return err
	}
	if err := remote.SubmitPRPromotion(ctx, repo, request); err != nil {
		return nil
	} // durable local retry owns delivery
	return local.AcceptPRDelivery(ctx, repo, request)
}
