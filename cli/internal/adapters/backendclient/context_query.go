package backendclient

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c *BackendClient) QueryContext(ctx context.Context, repo string, in domain.ContextSelection) (domain.ContextQueryView, error) {
	if err := domain.ValidateContextSegmentSelection(in); err != nil {
		return domain.ContextQueryView{}, err
	}
	q := url.Values{}
	if in.SegmentLimit > 0 {
		q.Set("segment_limit", strconv.Itoa(in.SegmentLimit))
		q.Set("segment_offset", strconv.Itoa(in.SegmentOffset))
		if in.SegmentStateHash != "" {
			q.Set("segment_state_hash", string(in.SegmentStateHash))
		}
	}
	q.Set("scope", in.Scope)
	for k, v := range map[string]string{"position": in.Position, "branch": in.Branch, "code_commit": in.CodeCommit} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var out domain.ContextQueryView
	if domain.ValidateContentHash(domain.ContentHash(repo)) != nil || (strings.HasPrefix(in.Position, "sha256:") && domain.ValidateContentHash(domain.ContentHash(in.Position)) != nil) || (in.CodeCommit != "" && !domain.ValidGitOID(in.CodeCommit)) {
		return out, domain.ErrHashMismatch
	}
	if err := c.doLimited(ctx, http.MethodGet, c.reposPath(repo)+"/context-query?"+q.Encode(), nil, &out, 64<<20); err != nil {
		return out, err
	}
	if err := domain.ValidateContextQuery(repo, in, out); err != nil {
		return out, err
	}
	return out, nil
}
