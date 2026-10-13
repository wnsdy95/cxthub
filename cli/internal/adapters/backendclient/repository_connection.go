package backendclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

func (c *BackendClient) ConfirmAuthenticatedConnection(ctx context.Context) error {
	var account struct {
		ID string `json:"id"`
	}
	if err := c.doLimited(ctx, http.MethodGet, "/me", nil, &account, 1<<20); err != nil || account.ID == "" {
		return fmt.Errorf("destination login could not be verified")
	}
	return nil
}

func (c *BackendClient) ResolveRepositoryConnection(ctx context.Context, remote string) (domain.RepositoryConnection, error) {
	var out domain.RepositoryConnection
	err := c.do(ctx, "GET", "/repository-connections?remote_url="+url.QueryEscape(remote), nil, &out)
	return out, err
}
