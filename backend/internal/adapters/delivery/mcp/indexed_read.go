package mcp

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"strings"
)

type indexedContextReader interface {
	ReadDocEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int, int) (domain.DocEventPage, error)
	SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error)
}

func (s *Server) readEventPage(ctx context.Context, repo, hash domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	if reader, ok := s.context.(indexedContextReader); ok {
		return reader.ReadDocEvents(ctx, repo, hash, "", offset, limit)
	}
	doc, err := s.context.GetDoc(ctx, repo, hash)
	if err != nil {
		return domain.DocEventPage{}, err
	}
	page := domain.DocEventPage{Total: len(doc.CIR.Events), Offset: offset, Next: -1}
	if offset <= page.Total {
		end := min(offset+limit, page.Total)
		page.Events = doc.CIR.Events[offset:end]
		if end < page.Total {
			page.Next = end
		}
	}
	return page, nil
}

func (s *Server) indexedSearchPage(ctx context.Context, repo domain.Repo, a toolArgs, reader indexedContextReader) (string, error) {
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
	if cur.Scan != "" {
		found := false
		for _, snap := range snaps {
			if snap.ID == cur.Scan {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("cursor snapshot is no longer available")
		}
	}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	var candidates map[domain.ContentHash]bool
	if search, ok := s.context.(interface {
		MatchingDocHashes(context.Context, domain.ContentHash, string) (map[domain.ContentHash]bool, error)
	}); ok {
		candidates, err = search.MatchingDocHashes(ctx, repo.ID, q)
		if err != nil {
			return "", err
		}
	}
	limit := pageLimit(a.Limit, 20, 100)
	hits := []map[string]any{}
	documents := 0
	for ordinal, snap := range snaps {
		key := searchSnapshotKey(snap, ordinal, len(snaps), cur.Version)
		if key > cur.Top || (cur.After != "" && key >= cur.After) || (cur.Scan != "" && cur.Scan != snap.ID) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		cur.Scan = snap.ID
		if cur.Index == 0 {
			if strings.Contains(strings.ToLower(snap.Message), q) {
				hits = append(hits, map[string]any{"snapshot_id": snap.ID, "branch": snap.Branch, "kind": "commit", "seq": -1, "role": "", "snippet": truncateRunes(snap.Message, 240), "created_at": snap.CreatedAt})
			}
			cur.Index = 1
		}
		if len(hits) == limit {
			return pageJSON(map[string]any{"notice": archiveNotice, "hits": hits, "next_cursor": encodeCursor(cur)})
		}
		var found []domain.DocEventIndex
		if candidates == nil || candidates[snap.DocHash] {
			found, err = reader.SearchDocEvents(ctx, repo.ID, snap.DocHash, q, cur.Index-2, limit-len(hits)+1)
		}
		if err != nil {
			return "", err
		}
		for _, event := range found {
			if len(hits) == limit {
				return pageJSON(map[string]any{"notice": archiveNotice, "hits": hits, "next_cursor": encodeCursor(cur)})
			}
			hits = append(hits, map[string]any{"snapshot_id": snap.ID, "branch": snap.Branch, "kind": "event", "seq": event.Seq, "role": event.Role, "snippet": truncateRunes(event.Text, 240), "created_at": snap.CreatedAt})
			cur.Index = event.Index + 2
		}
		cur.After = key
		cur.Scan = ""
		cur.Index = 0
		documents++
		if documents == 100 {
			return pageJSON(map[string]any{"notice": archiveNotice, "hits": hits, "next_cursor": encodeCursor(cur)})
		}
	}
	return pageJSON(map[string]any{"notice": archiveNotice, "hits": hits, "next_cursor": ""})
}
