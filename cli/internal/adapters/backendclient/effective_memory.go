package backendclient

import (
	"context"
	"errors"
	"fmt"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"net/http"
	"net/url"
	"strconv"
)

var _ outbound.EffectiveMemoryReader = (*BackendClient)(nil)

func (c *BackendClient) QueryEffectiveMemory(ctx context.Context, repo string, in domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	var out domain.EffectiveMemoryPage
	if domain.ValidateContentHash(domain.ContentHash(repo)) != nil || in.Selection.Validate() != nil || (in.Content != "claims" && in.Content != "prompt") || in.Limit < 1 || in.Limit > 50 || len(in.Cursor) > 1024 {
		return out, domain.ErrHashMismatch
	}
	q := url.Values{"snapshot_id": {string(in.Selection.SnapshotID)}, "code_commit": {in.Selection.CodeCommit}, "content": {in.Content}, "limit": {strconv.Itoa(in.Limit)}}
	if in.Selection.Branch != "" {
		q.Set("branch", in.Selection.Branch)
	}
	if in.Selection.MemoryHash != "" {
		q.Set("memory_hash", string(in.Selection.MemoryHash))
	}
	if in.Cursor != "" {
		q.Set("cursor", in.Cursor)
	}
	// Server item pages are <=256 KiB; reserve bounded space for metadata and
	// JSON string escaping. A proxy or old server cannot force an archive download.
	err := c.doLimited(ctx, http.MethodGet, c.reposPath(repo)+"/effective-memory?"+q.Encode(), nil, &out, 320<<10)
	var responseErr *HTTPError
	if errors.As(err, &responseErr) && responseErr.Status == http.StatusConflict && responseErr.Code == "memory_cursor_stale" {
		return out, fmt.Errorf("%w: %w", domain.ErrEffectiveMemoryCursorStale, err)
	}
	if err == nil && domain.ValidateOptionalContentHash(out.DeliveryStateHash) != nil {
		return out, domain.ErrHashMismatch
	}
	return out, err
}
