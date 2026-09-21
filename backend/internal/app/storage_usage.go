package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"log"
	"time"
)

func (s *IdentityService) storageNamespace(ctx context.Context, actor, ns string, write bool) (domain.Namespace, error) {
	if s.organization == nil {
		return domain.Namespace{}, domain.ErrUsageUnavailable
	}
	if ns == "self" {
		u, err := s.repositories.GetUser(ctx, actor)
		if err != nil {
			return domain.Namespace{}, err
		}
		n, err := s.organization.GetNamespaceBySlug(ctx, u.Username)
		if err != nil {
			return n, err
		}
		ns = n.ID
	}
	n, err := s.organization.GetNamespace(ctx, ns)
	if err != nil {
		return n, err
	}
	if n.Kind == domain.NamespaceUser {
		if n.UserID != actor {
			return n, domain.ErrForbidden
		}
		return n, nil
	}
	role, ok := s.OrganizationRoleOf(ctx, n.OrganizationID, actor)
	min := domain.OrganizationAdmin
	if write {
		min = domain.OrganizationOwner
	}
	if !ok || !role.AtLeast(min) {
		return n, domain.ErrForbidden
	}
	return n, nil
}
func (s *IdentityService) StorageUsage(ctx context.Context, actor, ns string, start, end time.Time) (domain.StorageUsage, error) {
	n, err := s.storageNamespace(ctx, actor, ns, false)
	if err != nil {
		return domain.StorageUsage{}, err
	}
	st, ok := s.repositories.(outbound.StorageAccounting)
	if !ok {
		return domain.StorageUsage{}, domain.ErrUsageUnavailable
	}
	return st.ReadStorageUsage(ctx, n.ID, start, end)
}
func (s *IdentityService) ReconcileStorageUsage(ctx context.Context, actor, ns string) error {
	n, err := s.storageNamespace(ctx, actor, ns, true)
	if err != nil {
		return err
	}
	st, ok := s.repositories.(outbound.StorageAccounting)
	if !ok {
		return domain.ErrUsageUnavailable
	}
	return st.ReconcileStorageUsage(ctx, n.ID)
}

func (s *IdentityService) RunStorageMaintenance(ctx context.Context) {
	st, ok := s.repositories.(interface {
		ReconcileNextStorageUsage(context.Context) (bool, error)
	})
	if !ok {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i := 0; i < 8; i++ {
				call, cancel := context.WithTimeout(ctx, 30*time.Second)
				did, err := st.ReconcileNextStorageUsage(call)
				cancel()
				if err != nil {
					log.Printf("storage reconciliation failed: %v", err)
					break
				}
				if !did {
					break
				}
			}
		}
	}
}
