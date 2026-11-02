package app

import (
	"context"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/app/archivequery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (service *Service) Search(ctx context.Context, input inbound.SearchInput) (inbound.SearchOutput, error) {
	queries, err := service.ArchiveQueries()
	if err != nil {
		return inbound.SearchOutput{}, err
	}
	return queries.Search(ctx, input)
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

func searchSnippet(text, query string) string {
	return archivequery.SearchSnippet(text, query)
}
