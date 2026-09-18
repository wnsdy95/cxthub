package mcp

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) SetGitScans(q inbound.GitScanQuery) { s.gitScans = q }
func (s *Server) gitScansPage(ctx context.Context, repo domain.Repo, a toolArgs) (string, error) {
	if s.gitScans == nil {
		return "", fmt.Errorf("Git observation query unavailable")
	}
	cur, err := cursorFor(string(repo.ID), "git_observations", a)
	if err != nil {
		return "", err
	}
	page, err := s.gitScans.ListScans(ctx, repo.ID, cur.After, pageLimit(a.Limit, 10, 20))
	if err != nil {
		return "", err
	}
	rows := []map[string]any{}
	for _, j := range page.Items {
		rows = append(rows, map[string]any{"id": j.ID, "commit": j.Commit, "state": j.State, "indexed": j.Indexed, "reason": j.Reason, "updated_at": j.UpdatedAt})
	}
	next := ""
	if page.NextCursor != "" {
		cur.After = page.NextCursor
		next = encodeCursor(cur)
	}
	return pageJSON(map[string]any{"scope": "commit_discovery", "notice": "Completion covers discovery against currently indexed commits. Ancestors and late arrivals remain separate work. This does not assert current code or memory applicability.", "items": rows, "reconciliation": page.Reconciliation, "next_cursor": next})
}
