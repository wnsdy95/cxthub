package backendclient

import (
	"context"
	"net/url"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func (c *BackendClient) ResolveRepositoryConnection(ctx context.Context, remote string) (domain.RepositoryConnection, error) {
	var out domain.RepositoryConnection
	err := c.do(ctx, "GET", "/repository-connections?remote_url="+url.QueryEscape(remote), nil, &out)
	return out, err
}
