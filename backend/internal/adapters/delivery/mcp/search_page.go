package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (s *Server) searchPage(ctx context.Context, repo domain.Repo, a toolArgs) (string, error) {
	query := strings.ToLower(strings.TrimSpace(a.Query))
	if len([]rune(query)) < 2 || len([]rune(query)) > 256 {
		return "", fmt.Errorf("query must contain 2 to 256 characters")
	}
	if s.documentEvents == nil {
		return "", fmt.Errorf("%w: document event queries are not configured", domain.ErrAgentHistoryUnavailable)
	}
	return s.indexedSearchPage(ctx, repo, a, s.documentEvents)
}
