package backendclient

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// ReportWorktrees sends, with PUT /repos/{repoID}/worktrees, the names of the
// repository's worktrees on this computer and the branch checked out in each.
// The list replaces what the account stored for this machine; a worktree that
// is no longer listed keeps its name on the server, marked removed.
func (c *BackendClient) ReportWorktrees(ctx context.Context, repoID, machine string, worktrees []domain.WorktreeRegistration) error {
	if err := domain.ValidateContentHash(domain.ContentHash(repoID)); err != nil {
		return err
	}
	if worktrees == nil {
		worktrees = []domain.WorktreeRegistration{}
	}
	body := struct {
		Machine   string                        `json:"machine,omitempty"`
		Worktrees []domain.WorktreeRegistration `json:"worktrees"`
	}{Machine: machine, Worktrees: worktrees}
	return c.doLimited(ctx, http.MethodPut, c.reposPath(repoID)+"/worktrees", body, nil, 4<<20)
}
