package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"unicode/utf8"
)

// MCP receives only the read port; it cannot submit or retry verification.
func (s *Server) SetGitChanges(query inbound.GitChangeQuery) { s.gitChanges = query }
func (s *Server) gitChangesPage(ctx context.Context, repo domain.Repo, a toolArgs) (string, error) {
	if s.gitChanges == nil {
		return "", fmt.Errorf("Git change query unavailable")
	}
	cur, err := cursorFor(string(repo.ID), "git_changes", a)
	if err != nil {
		return "", err
	}
	if a.ChangeID == "" {
		page, err := s.gitChanges.List(ctx, repo.ID, cur.After, pageLimit(a.Limit, 10, 20))
		if err != nil {
			return "", err
		}
		rows := []map[string]any{}
		for _, j := range page.Items {
			row := map[string]any{"id": j.ID, "commit": j.Request.Commit, "target": j.Request.Target, "state": j.State, "reason": j.Reason, "updated_at": j.UpdatedAt}
			row["coverage"] = j.Coverage
			row["verified_paths"] = j.VerifiedPaths
			row["unverified_paths"] = j.UnverifiedPaths
			rows = append(rows, row)
		}
		next := ""
		if page.NextCursor != "" {
			cur.After = page.NextCursor
			next = encodeCursor(cur)
		}
		return pageJSON(map[string]any{"notice": archiveNotice, "scope": "historical_evidence", "items": rows, "next_cursor": next})
	}
	j, err := s.gitChanges.Get(ctx, repo.ID, a.ChangeID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return "", err
	}
	hash := domain.HashContent(raw)
	if cur.Projection != "" && cur.Projection != hash {
		return "", fmt.Errorf("verification changed; restart without cursor")
	}
	cur.Projection = hash
	if cur.Offset >= len(raw) || (cur.Offset > 0 && !utf8.RuneStart(raw[cur.Offset])) {
		return "", fmt.Errorf("invalid evidence fragment offset")
	}
	start := cur.Offset
	end := fragmentEnd(raw, start, pageBytes)
	cur.Offset = end
	next := ""
	if end < len(raw) {
		next = encodeCursor(cur)
	}
	return pageJSON(map[string]any{"notice": archiveNotice, "scope": "historical_evidence", "change_id": j.ID, "state_hash": hash, "byte_offset": start, "json_fragment": string(raw[start:end]), "next_cursor": next})
}
