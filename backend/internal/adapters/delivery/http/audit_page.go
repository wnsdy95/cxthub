package http

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Server) withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := domain.NewID("req_")
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(inbound.WithCorrelation(r.Context(), id)))
	})
}
func (s *Server) auditPage(r *http.Request) (domain.OrganizationAuditPage, error) {
	user, _ := userFrom(r.Context())
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			return domain.OrganizationAuditPage{}, domain.ErrValidation
		}
	}
	return s.id.OrganizationAuditPage(r.Context(), user.ID, r.PathValue("organizationID"), r.URL.Query().Get("cursor"), limit)
}
func (s *Server) organizationAuditPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.auditPage(r)
	s.respond(w, p, err)
}

// Export is paginated with the same cursor and authorization contract as the UI.
func (s *Server) exportOrganizationAudit(w http.ResponseWriter, r *http.Request) {
	p, err := s.auditPage(r)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="organization-audit.jsonl"`)
	w.Header().Set("X-Next-Cursor", p.NextCursor)
	enc := json.NewEncoder(w)
	for _, e := range p.Events {
		if err = enc.Encode(e); err != nil {
			return
		}
	}
}
