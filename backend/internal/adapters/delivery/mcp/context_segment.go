package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Transport fragments one shared application page; it never decides whether a
// commit, parent prefix or PR belongs in the selection. The byte budget applies
// even when one capture has many publication receipts.
func (s *Server) contextSegmentPage(ctx context.Context, repo domain.Repo, a toolArgs) (string, error) {
	cur, err := cursorFor(string(repo.ID), "context_list", a)
	if err != nil {
		return "", err
	}
	position, branch := a.Position, a.Branch
	if cur.Snapshot != "" {
		position = string(cur.Snapshot)
	}
	if cur.ContextBranch != "" {
		branch = cur.ContextBranch
	}
	view, err := s.context.QueryContext(ctx, repo.ID, domain.ContextSelection{Branch: branch, Position: position, Scope: a.Scope, CodeCommit: a.CodeCommit, SegmentLimit: 1, SegmentOffset: cur.Index, SegmentStateHash: cur.Projection})
	if err != nil {
		return "", err
	}
	if view.Segments == nil || view.Segments.Version != domain.ContextSegmentVersion || view.Segments.StateHash == "" {
		return "", fmt.Errorf("server does not provide context segment version 1")
	}
	page := view.Segments
	if (cur.Projection != "" && cur.Projection != page.StateHash) || (cur.Scan != "" && cur.Scan != page.PageHash) {
		return "", fmt.Errorf("context segments changed; restart without cursor")
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return "", err
	}
	start := cur.Offset
	if start >= len(raw) || (start > 0 && !utf8.RuneStart(raw[start])) {
		return "", fmt.Errorf("invalid segment fragment offset")
	}
	end := fragmentEnd(raw, start, pageBytes)
	cur.Version, cur.Projection, cur.Snapshot, cur.ContextBranch = 4, page.StateHash, view.Position, view.Branch
	cur.Offset, cur.Scan = end, page.PageHash
	next := ""
	if end < len(raw) {
		next = encodeCursor(cur)
	} else if page.Next >= 0 {
		cur.Index, cur.Offset, cur.Scan = page.Next, 0, ""
		next = encodeCursor(cur)
	}
	return pageJSON(map[string]any{"notice": archiveNotice, "state_hash": page.StateHash, "page_hash": page.PageHash, "snapshot_offset": page.Offset, "byte_offset": start, "json_fragment": string(raw[start:end]), "page_complete": end == len(raw), "next_cursor": next})
}
