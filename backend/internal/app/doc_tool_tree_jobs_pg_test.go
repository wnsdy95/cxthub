//go:build postgres

package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docToolValueWorkerStore struct {
	*store.PostgresStore
	steps int
}

func (storage *docToolValueWorkerStore) SaveDocEventDisplay(ctx context.Context, job domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	state, err := storage.PostgresStore.SaveDocEventDisplay(ctx, job, step)
	if err == nil && (state.Phase == "tool_values" || state.Phase == "tool_values_done" || state.Phase == "tool_policy") {
		storage.steps++
	}
	return state, err
}
