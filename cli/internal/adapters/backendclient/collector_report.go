package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// ReportCollector sends, with PUT /repos/{repoID}/collector, what the
// collector of this computer says about itself: the pushes this clone made
// and what went wrong in its run. It replaces what the account stored for
// this machine; a push that was reported stays reported.
func (c *BackendClient) ReportCollector(ctx context.Context, repoID string, report domain.CollectorReport) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if report.Pushes == nil {
		report.Pushes = []domain.RecordedPush{}
	}
	return c.doLimited(ctx, http.MethodPut, c.reposPath(repoID)+"/collector", report, nil, 1<<20)
}
