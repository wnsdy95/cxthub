//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docEventDisplayAcknowledgementProbe struct {
	outbound.DocDisplayStore
	outbound.DocEventDisplayStore
	afterSave  func() error
	beforeSave func() error
}

func (probe *docEventDisplayAcknowledgementProbe) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	if probe.beforeSave != nil {
		if err := probe.beforeSave(); err != nil {
			return domain.DocEventDisplayPreparation{}, err
		}
	}
	state, err := probe.DocEventDisplayStore.SaveDocEventDisplay(ctx, job, step)
	if err == nil && probe.afterSave != nil {
		return domain.DocEventDisplayPreparation{}, probe.afterSave()
	}
	return state, err
}
