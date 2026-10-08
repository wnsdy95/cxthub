package http

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) getDocTurns(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.b.(inbound.AgentHistoryReader)
	if !ok {
		s.writeError(w, 503, "unavailable", "history turn paging unavailable")
		return
	}
	req := domain.AgentHistoryPageRequest{Before: -1, Limit: 16, MaxBytes: domain.MaxAgentHistoryPageBytes}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	for key, dst := range map[string]*int{"before": &req.Before, "limit": &req.Limit, "max_bytes": &req.MaxBytes} {
		if values, ok := query[key]; ok {
			var err error
			if len(values) != 1 {
				err = domain.ErrValidation
			} else {
				*dst, err = strconv.Atoi(values[0])
			}
			if err != nil {
				s.respond(w, nil, fmt.Errorf("%w: invalid %s", domain.ErrValidation, key))
				return
			}
		}
	}
	if len(query["covered_by"]) > 1 || len(query["incomplete_tail"]) > 1 || len(query["doc_identity"]) > 1 || len(query["covered_by_identity"]) > 1 {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	for key := range query {
		if key != "doc_identity" && strings.EqualFold(key, "doc_identity") || key != "covered_by_identity" && strings.EqualFold(key, "covered_by_identity") {
			s.respond(w, nil, domain.ErrValidation)
			return
		}
	}
	req.DocIdentity = domain.DocumentIdentity(query.Get("doc_identity"))
	req.CoveredByIdentity = domain.DocumentIdentity(query.Get("covered_by_identity"))
	req.CoveredBy = domain.ContentHash(query.Get("covered_by"))
	req.IncompleteTail = query.Get("incomplete_tail")
	if err := domain.ValidateAgentHistoryPageRequest(req); err != nil {
		s.respond(w, nil, err)
		return
	}
	out, err := reader.ReadAgentHistoryPage(r.Context(), s.repoID(r), domain.ContentHash(r.PathValue("hash")), req)
	switch {
	case errors.Is(err, domain.ErrContextBudgetExceeded):
		s.writeError(w, 422, "context_budget_exceeded", err.Error())
	case errors.Is(err, domain.ErrAgentHistoryUnavailable):
		s.writeError(w, 503, "unavailable", err.Error())
	default:
		s.respond(w, out, err)
	}
}
