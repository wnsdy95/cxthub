package backendclient

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

type docJobStatus struct {
	ID      string             `json:"id"`
	DocHash domain.ContentHash `json:"doc_hash"`
	State   string             `json:"state"`
	Reason  string             `json:"reason,omitempty"`
}

func (c *BackendClient) finalizeDocument(ctx context.Context, repo string, doc chunkedDocWire) error {
	var job docJobStatus
	path := c.reposPath(repo) + "/push/doc-jobs"
	if err := c.do(ctx, http.MethodPost, path, doc, &job); err != nil {
		return fmt.Errorf("accept document %s for finalization (no ref updates sent): %w", doc.Hash, err)
	}
	id := job.ID
	delay := time.Second
	for {
		if err := domain.ValidateContentHash(domain.ContentHash(job.ID)); err != nil || job.ID != id || job.DocHash != doc.Hash {
			return fmt.Errorf("%w: document finalization receipt identity", domain.ErrHashMismatch)
		}
		switch job.State {
		case "completed":
			return nil
		case "rejected":
			return fmt.Errorf("document %s finalization rejected (%s, job %s; no ref updates sent)", doc.Hash, job.Reason, id)
		case "waiting", "running", "retrying":
		default:
			return fmt.Errorf("invalid document finalization state %q", job.State)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("document %s finalization remains durably queued as %s (no ref updates sent): %w", doc.Hash, id, ctx.Err())
		case <-timer.C:
		}
		// Each HTTP request keeps the ordinary deadline. Caller cancellation stops
		// waiting, never the server's accepted work; replay uses the same identity.
		if err := c.do(ctx, http.MethodGet, path+"/"+id, nil, &job); err != nil {
			return fmt.Errorf("document finalization remains durably queued as %s (no ref updates sent): %w", id, err)
		}
		delay = min(delay*2, 5*time.Second)
	}
}
