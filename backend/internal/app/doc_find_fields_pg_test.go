//go:build postgres

package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docFindFieldsCompletionHook struct {
	outbound.DocFindFieldsStore
	before func()
}

func (storage *docFindFieldsCompletionHook) FinishDocFindFields(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	if job.State == "completed" && storage.before != nil {
		before := storage.before
		storage.before = nil
		before()
	}
	return storage.DocFindFieldsStore.FinishDocFindFields(ctx, job, now)
}
