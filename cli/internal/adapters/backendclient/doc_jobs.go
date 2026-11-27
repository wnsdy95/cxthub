package backendclient

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type docJobStatus = domain.DocFinalizationStatus

// The server takes a document in a job of its own and is asked whether the
// job has ended. A part of a conversation is taken within tens of
// milliseconds, so the first question comes soon; each wait after it is
// twice the one before, up to the longest.
const (
	docJobFirstWait   = 50 * time.Millisecond
	docJobLongestWait = 5 * time.Second
)

func (c *BackendClient) finalizeDocument(ctx context.Context, repo string, doc chunkedDocWire) error {
	if err := doc.DocumentRef().Validate(); err != nil {
		return err
	}
	if doc.Identity != domain.DocumentIdentityLegacy {
		if _, err := doc.ConversationManifest(); err != nil {
			return err
		}
	}
	var job docJobStatus
	path := c.reposPath(repo) + "/push/doc-jobs"
	if err := c.do(ctx, http.MethodPost, path, doc, &job); err != nil {
		return fmt.Errorf("accept document %s for finalization (no ref updates sent): %w", doc.Hash, err)
	}
	id := job.ID
	delay := docJobFirstWait
	for {
		if err := job.ValidateFor(domain.ContentHash(repo), doc); err != nil || job.ID != id {
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
		delay = min(delay*2, docJobLongestWait)
	}
}
