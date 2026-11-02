package http

import (
	"context"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type legacyPRPromoter interface {
	PromoteRepositoryPR(context.Context, domain.ContentHash, domain.PullRequestMerge) (inbound.UpdateRefOutput, error)
}

type legacyPRPromotionCommands struct {
	promoter legacyPRPromoter
	durable  inbound.PRPromotionDeliverer
	jobs     prJobBackend
}

func legacyPRPromotionBindings(backend Backend) (inbound.PRPromotionDeliverer, inbound.PRPromotionCommands) {
	if backend == nil {
		return nil, nil
	}
	durable, _ := backend.(inbound.PRPromotionDeliverer)
	jobs, _ := backend.(prJobBackend)
	commands := &legacyPRPromotionCommands{promoter: backend, durable: durable, jobs: jobs}
	if jobs == nil {
		return commands, nil
	}
	return commands, commands
}

func (commands *legacyPRPromotionCommands) DeliverPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	if commands.durable != nil {
		return commands.durable.DeliverPRPromotion(ctx, repo, request)
	}
	return commands.promoter.PromoteRepositoryPR(ctx, repo, request)
}

func (commands *legacyPRPromotionCommands) PromoteRepositoryPR(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (inbound.UpdateRefOutput, error) {
	return commands.promoter.PromoteRepositoryPR(ctx, repo, request)
}

func (commands *legacyPRPromotionCommands) SubmitPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	return commands.jobs.SubmitPRPromotion(ctx, repo, request)
}

func (commands *legacyPRPromotionCommands) ListPRPromotions(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	return commands.jobs.ListPRPromotions(ctx, repo)
}

func (commands *legacyPRPromotionCommands) RetryPRPromotion(ctx context.Context, repo domain.ContentHash, id string) error {
	return commands.jobs.RetryPRPromotion(ctx, repo, id)
}

func (server *Server) SetPRPromotionCommands(commands inbound.PRPromotionCommands) error {
	if commands == nil {
		return fmt.Errorf("%w: PR promotion commands are required", domain.ErrValidation)
	}
	switch value := reflect.ValueOf(commands); value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: PR promotion commands are required", domain.ErrValidation)
		}
	}
	server.prPromoter, server.prPromotions = commands, commands
	return nil
}
