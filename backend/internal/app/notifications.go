package app

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/notificationworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ProcessNotification(ctx context.Context) (bool, error) {
	if _, supported := service.meta.(outbound.NotificationStore); !supported || service.repositories == nil {
		return false, nil
	}
	worker, err := service.notificationWorker(notificationworker.Compatibility)
	if err != nil {
		return false, err
	}
	return worker.Process(ctx)
}

func (service *Service) RunNotificationWorker(ctx context.Context) {
	worker, err := service.notificationWorker(notificationworker.Compatibility)
	if err != nil {
		log.Printf("notification worker: %v", err)
		return
	}
	worker.Run(ctx)
}

func (s *IdentityService) notificationManager(ctx context.Context, user, repository string) (domain.Repository, error) {
	role, member := s.RoleOf(ctx, repository, user)
	if !member || !role.AtLeast(domain.RoleMaintainer) {
		return domain.Repository{}, domain.ErrForbidden
	}
	return s.repositories.GetRepository(ctx, repository)
}
func (s *IdentityService) ListNotifications(ctx context.Context, user, repository string) ([]domain.NotificationJob, error) {
	return identityResult(ctx, s, func(ctx context.Context) ([]domain.NotificationJob, error) {
		return s.listNotifications(ctx, user, repository)
	})
}
func (s *IdentityService) listNotifications(ctx context.Context, user, repository string) ([]domain.NotificationJob, error) {
	if _, err := s.notificationManager(ctx, user, repository); err != nil {
		return nil, err
	}
	if err := s.checkRepositoryDocumentIdentities(ctx, repository); err != nil {
		return nil, err
	}
	st, ok := s.repositories.(outbound.NotificationStore)
	if !ok {
		return nil, fmt.Errorf("notification storage unavailable")
	}
	return st.ListNotifications(ctx, repository)
}
func (s *IdentityService) RetryNotification(ctx context.Context, user, repository, id string) error {
	return s.withIdentity(ctx, func(ctx context.Context) error { return s.retryNotification(ctx, user, repository, id) })
}

func (s *IdentityService) retryNotification(ctx context.Context, user, repository, id string) error {
	repositoryRecord, err := s.notificationManager(ctx, user, repository)
	if err != nil {
		return err
	}
	if repositoryRecord.Archived || repositoryRecord.WebhookURL == "" {
		return fmt.Errorf("%w: configure an active webhook first", domain.ErrConflict)
	}
	if err := s.checkRepositoryDocumentIdentities(ctx, repository); err != nil {
		return err
	}
	st, ok := s.repositories.(outbound.NotificationStore)
	if !ok {
		return fmt.Errorf("notification storage unavailable")
	}
	return st.RetryNotification(ctx, repository, id, repositoryRecord.WebhookURL, time.Now().UTC())
}
