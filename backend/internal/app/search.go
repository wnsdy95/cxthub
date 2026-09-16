package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Search reads metadata and indexed readable events. Identical inherited events
// are attributed to their oldest snapshot; archival tool bodies are not searched.

const (
	searchDefaultLimit = 50
	searchMaxLimit     = 200
	searchSnippetRunes = 120
)

func (s *Service) Search(ctx context.Context, in inbound.SearchInput) (inbound.SearchOutput, error) {
	q := strings.ToLower(strings.TrimSpace(in.Query))
	if len([]rune(q)) < 2 || len([]rune(q)) > 256 {
		return inbound.SearchOutput{}, fmt.Errorf("%w: Search term must contain 2 to 256 characters", domain.ErrValidation)
	}
	limit := in.Limit
	if limit <= 0 || limit > searchMaxLimit {
		limit = searchDefaultLimit
	}
	snaps, err := s.meta.ListSnapshots(ctx, in.RepoID, "")
	if err != nil {
		return inbound.SearchOutput{}, err
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.Before(snaps[j].CreatedAt) })

	out := inbound.SearchOutput{Hits: []inbound.SearchHit{}}
	add := func(h inbound.SearchHit) bool {
		if len(out.Hits) >= limit {
			out.Truncated = true
			return false
		}
		out.Hits = append(out.Hits, h)
		return true
	}

	// 1) Commit message/author match.
	for _, sn := range snaps {
		if !strings.Contains(strings.ToLower(sn.Message), q) && !strings.Contains(strings.ToLower(sn.Author.Name), q) {
			continue
		}
		if !add(inbound.SearchHit{
			SnapshotID: sn.ID, Branch: sn.Branch, Kind: "commit",
			Snippet: searchSnippet(sn.Message, q), CreatedAt: sn.CreatedAt.UTC().Format(time.RFC3339),
		}) {
			return out, nil
		}
	}

	// 2) Conversation body (message/turn text blocks + reasoning summary) match.
	candidates, err := s.MatchingDocHashes(ctx, in.RepoID, q)
	if err != nil {
		return out, err
	}
	docSeen := map[domain.ContentHash]bool{}
	evSeen := map[string]bool{}
	for _, sn := range snaps {
		if sn.DocHash == "" || docSeen[sn.DocHash] || (candidates != nil && !candidates[sn.DocHash]) {
			continue
		}
		docSeen[sn.DocHash] = true
		after := -1
		for {
			hits, err := s.SearchDocEvents(ctx, in.RepoID, sn.DocHash, q, after, 200)
			if err != nil {
				return out, err
			}
			for _, hit := range hits {
				after = hit.Index
				key := string(hit.Hash)
				if evSeen[key] {
					continue
				}
				evSeen[key] = true
				if !add(inbound.SearchHit{SnapshotID: sn.ID, Branch: sn.Branch, Kind: "event", Role: hit.Role, Seq: hit.Seq, Snippet: searchSnippet(hit.Text, q), CreatedAt: sn.CreatedAt.UTC().Format(time.RFC3339)}) {
					return out, nil
				}
			}
			if len(hits) < 200 {
				break
			}
		}
	}
	return out, nil
}

// searchableText extracts the search target text from the event — only the readable conversation surface is considered
// (tool input/output is excluded as it is large original text; it covers the practical requirements of commit message search and doc).
func searchableText(ev domain.CIREvent) string {
	switch ev.Kind {
	case domain.EventMessage, domain.EventTurn:
		var b strings.Builder
		for _, blk := range ev.Blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	case domain.EventReasoning:
		return ev.RedactedSummary
	}
	return ""
}

// searchSnippet safely truncates the first match to a single line.
func searchSnippet(text, q string) string {
	text = strings.Join(strings.Fields(text), " ") // Replace newlines and consecutive spaces with a single space.
	byteIdx := strings.Index(strings.ToLower(text), q)
	if byteIdx < 0 {
		byteIdx = 0 // Show the front part if the match position is lost due to normalization
	}
	runes := []rune(text)
	start := len([]rune(text[:byteIdx])) - 40
	if start < 0 {
		start = 0
	}
	end := start + searchSnippetRunes
	if end > len(runes) {
		end = len(runes)
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}
