//go:build postgres

package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docDisplayJobProbe struct {
	outbound.DocDisplayStore
	initialized     int
	discovered      int
	verified        int
	discoveryFrom   []int
	saved           int
	completed       int
	beforeSave      func() error
	beforeDiscovery func() error
}

func (probe *docDisplayJobProbe) InitializeDocDisplay(ctx context.Context, job domain.DocDisplayJob, source domain.DocDisplayStringSource) (domain.DocDisplayPreparation, error) {
	probe.initialized++
	return probe.DocDisplayStore.InitializeDocDisplay(ctx, job, source)
}

func (probe *docDisplayJobProbe) SaveDocDisplayDiscovery(ctx context.Context, job domain.DocDisplayJob, step domain.DocFieldDiscoveryStep) (domain.DocDisplayPreparation, error) {
	if probe.beforeDiscovery != nil {
		if err := probe.beforeDiscovery(); err != nil {
			return domain.DocDisplayPreparation{}, err
		}
	}
	progress, err := probe.DocDisplayStore.SaveDocDisplayDiscovery(ctx, job, step)
	if err == nil {
		probe.discovered++
		probe.discoveryFrom = append(probe.discoveryFrom, step.From())
		if progress.Source != nil {
			probe.verified++
		}
	}
	return progress, err
}

func (probe *docDisplayJobProbe) SaveDocDisplayStep(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, step domain.DocDisplayStringStep) (domain.DocDisplayPreparation, error) {
	if probe.beforeSave != nil {
		if err := probe.beforeSave(); err != nil {
			return domain.DocDisplayPreparation{}, err
		}
	}
	progress, err := probe.DocDisplayStore.SaveDocDisplayStep(ctx, job, checkpoint, step)
	if err == nil {
		probe.saved++
	}
	return progress, err
}

func (probe *docDisplayJobProbe) CompleteDocDisplay(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, now time.Time) error {
	err := probe.DocDisplayStore.CompleteDocDisplay(ctx, job, checkpoint, now)
	if err == nil {
		probe.completed++
	}
	return err
}
