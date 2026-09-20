package mcp

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
)

func (s *Server) searchPage(ctx context.Context, repo domain.Repo, a toolArgs) (string, error) {
	query := strings.ToLower(strings.TrimSpace(a.Query))
	if len([]rune(query)) < 2 || len([]rune(query)) > 256 {
		return "", fmt.Errorf("query must contain 2 to 256 characters")
	}
	if reader, ok := s.context.(indexedContextReader); ok {
		return s.indexedSearchPage(ctx, repo, a, reader)
	}
	cur, err := cursorFor(string(repo.ID), "context_search", a)
	if err != nil {
		return "", err
	}
	snaps, err := s.scopeSnapshots(ctx, repo, a, &cur)
	if err != nil {
		return "", err
	}
	if cur.Top == "" && len(snaps) > 0 {
		cur.Top = searchSnapshotKey(snaps[0], 0, len(snaps), cur.Version)
	}
	hits := []map[string]any{}
	limit := pageLimit(a.Limit, 20, 100)
	scanned := 0
	documents := 0
	next := ""
	for ordinal, snap := range snaps {
		key := searchSnapshotKey(snap, ordinal, len(snaps), cur.Version)
		if key > cur.Top || (cur.After != "" && key >= cur.After) {
			continue
		}
		if cur.Scan != "" && snap.ID != cur.Scan {
			continue
		}
		doc, err := s.context.GetDoc(ctx, repo.ID, snap.DocHash)
		if err != nil {
			return "", err
		}
		if cur.Index > len(doc.CIR.Events)+1 {
			return "", fmt.Errorf("search cursor is outside this document")
		}
		documents++
		for cur.Index <= len(doc.CIR.Events) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			i := cur.Index
			text := snap.Message
			kind := "commit"
			seq := -1
			role := ""
			if i > 0 {
				event := doc.CIR.Events[i-1]
				text = eventText(event)
				kind = "event"
				seq = event.Seq
				role = string(event.Role)
			}
			cur.Index++
			scanned++
			if strings.Contains(strings.ToLower(text), query) {
				hits = append(hits, map[string]any{"snapshot_id": snap.ID, "branch": snap.Branch, "kind": kind, "seq": seq, "role": role, "snippet": truncateRunes(text, 240), "created_at": snap.CreatedAt})
			}
			if len(hits) >= limit || scanned >= 5000 {
				cur.Scan = snap.ID
				if cur.Index > len(doc.CIR.Events) {
					cur.Scan = ""
					cur.Index = 0
					cur.After = key
				}
				next = encodeCursor(cur)
				return pageJSON(map[string]any{"notice": archiveNotice, "hits": hits, "scanned_events": scanned, "next_cursor": next})
			}
		}
		cur.After = key
		cur.Scan = ""
		cur.Index = 0
		if documents >= 100 {
			next = encodeCursor(cur)
			break
		}
	}
	if cur.Scan != "" {
		return "", fmt.Errorf("searched snapshot is no longer in this history selection; restart without cursor")
	}
	return pageJSON(map[string]any{"notice": archiveNotice, "hits": hits, "scanned_events": scanned, "next_cursor": next})
}
