//go:build postgres

package app_test

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentWorkersPGPorts struct {
	*store.PostgresStore
	observing bool
	claims    [4]int
	reads     int
}

func (storage *documentWorkersPGPorts) observe(ctx context.Context) error {
	if !storage.observing {
		return nil
	}
	if _, system := inbound.RepositoryActor(ctx); !system {
		return domain.ErrUnauthorized
	}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1); err != nil {
		return err
	}
	bound, release, err := app.AcquireResources(ctx, app.ResourceBulk)
	if err != nil {
		return err
	}
	defer release()
	if bound != ctx {
		return domain.ErrIntegrity
	}
	return nil
}

func (storage *documentWorkersPGPorts) ClaimDocJob(ctx context.Context, repo domain.ContentHash, now time.Time, lease time.Duration) (domain.DocFinalizationJob, error) {
	storage.claims[0]++
	if err := storage.observe(ctx); err != nil {
		return domain.DocFinalizationJob{}, err
	}
	return storage.PostgresStore.ClaimDocJob(ctx, repo, now, lease)
}

func (storage *documentWorkersPGPorts) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if err := storage.observe(ctx); err != nil {
		return err
	}
	return storage.PostgresStore.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if storage.observing {
			storage.reads++
			if !storage.InReadOnlyTransaction(bound) {
				return domain.ErrConflict
			}
		}
		return read(bound)
	})
}
