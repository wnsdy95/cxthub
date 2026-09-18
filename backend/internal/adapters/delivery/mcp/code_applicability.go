package mcp

import (
	"context"
	"fmt"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) SetCodeApplicability(q inbound.CodeApplicabilityQuery) { s.codeApplicability = q }
func (s *Server) codeApplicabilityPage(ctx context.Context, repo domain.Repo, a toolArgs) (string, error) {
	if s.codeApplicability == nil {
		return "", fmt.Errorf("code applicability query unavailable")
	}
	if len(a.Paths) > 20 {
		return "", fmt.Errorf("request at most 20 paths per call")
	}
	out, err := s.codeApplicability.QueryCodeApplicability(ctx, repo.ID, domain.CodeSelection{CodeCommit: a.CodeCommit, SourceCommit: a.SourceCommit, SourceParent: a.SourceParent, Paths: a.Paths})
	if err != nil {
		return "", err
	}
	raw, err := pageJSON(out)
	if len(raw) > pageBytes {
		return "", fmt.Errorf("result exceeds page budget; request fewer paths")
	}
	return raw, err
}
