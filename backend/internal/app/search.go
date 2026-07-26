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

// Search — Team tool's "Where did they say that?" requirement. The server scans snapshot metadata (commit messages, authors) and
// doc (CIR) conversation bodies. It performs a linear scan without an index, but it is sufficient for dogfood scale,
// and each doc is read only once (to prevent rescan of shared docs by inheritance). Events are globally deduplicated —
// to prevent inheritance prefixes from hitting child snapshots repeatedly, and each event is attributed to the
// "first occurrence" (oldest) snapshot. Early termination (Truncated) on reaching the limit.

const (
	searchDefaultLimit = 50
	searchMaxLimit     = 200
	searchSnippetRunes = 120
)

func (s *Service) Search(ctx context.Context, in inbound.SearchInput) (inbound.SearchOutput, error) {
	q := strings.ToLower(strings.TrimSpace(in.Query))
	if len([]rune(q)) < 2 {
		return inbound.SearchOutput{}, fmt.Errorf("%w: Search term must be at least 2 characters", domain.ErrValidation)
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
	docSeen := map[domain.ContentHash]bool{}
	evSeen := map[string]bool{}
	for _, sn := range snaps {
		if sn.DocHash == "" || docSeen[sn.DocHash] {
			continue
		}
		docSeen[sn.DocHash] = true
		doc, derr := s.blobs.GetDoc(ctx, in.RepoID, sn.DocHash)
		if derr != nil {
			continue // Skip partial data — Search is best-effort.
		}
		for _, ev := range doc.CIR.Events {
			text := searchableText(ev)
			if text == "" || !strings.Contains(strings.ToLower(text), q) {
				continue
			}
			if key := eventKey(ev); evSeen[key] {
				continue
			} else {
				evSeen[key] = true
			}
			if !add(inbound.SearchHit{
				SnapshotID: sn.ID, Branch: sn.Branch, Kind: "event", Role: string(ev.Role), Seq: ev.Seq,
				Snippet: searchSnippet(text, q), CreatedAt: sn.CreatedAt.UTC().Format(time.RFC3339),
			}) {
				return out, nil
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
