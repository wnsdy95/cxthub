package backendclient

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"net/http"
)

func (c *BackendClient) PushHistoryEvent(ctx context.Context, e domain.HistoryEvent) error {
	if err := domain.ValidateHistoryEvent(e); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, c.reposPath(e.RepoID)+"/history", e, nil)
}
func (c *BackendClient) PullHistoryEvents(ctx context.Context, repoID string) ([]domain.HistoryEvent, error) {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return nil, err
	}
	var events []domain.HistoryEvent
	if err := c.do(ctx, http.MethodGet, c.reposPath(repoID)+"/history", nil, &events); err != nil {
		return nil, err
	}
	for _, e := range events {
		if e.RepoID != repoID {
			return nil, domain.ErrHashMismatch
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
	}
	return events, nil
}
