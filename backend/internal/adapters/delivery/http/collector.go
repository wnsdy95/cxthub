package http

import (
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// A report names at most CollectorPushLimit pushes of about a hundred bytes.
const collectorReportBodyLimit = 256 << 10

func (s *Server) collectorReports(w http.ResponseWriter) (inbound.CollectorReports, bool) {
	service, ok := s.b.(inbound.CollectorReports)
	if !ok {
		s.writeError(w, http.StatusServiceUnavailable, "collectors_unavailable", "collector service unavailable")
	}
	return service, ok
}

func (s *Server) putCollector(w http.ResponseWriter, r *http.Request) {
	var report struct {
		Machine  string                  `json:"machine"`
		Version  string                  `json:"cxt_version"`
		RunError string                  `json:"run_error"`
		Pushes   *[]domain.CollectorPush `json:"pushes"`
	}
	if !s.decodeSingleJSON(w, r, collectorReportBodyLimit, &report) {
		return
	}
	if report.Pushes == nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "pushes is required")
		return
	}
	service, ok := s.collectorReports(w)
	if !ok {
		return
	}
	err := service.ReportCollector(r.Context(), s.repoID(r), domain.CollectorReport{Machine: report.Machine, Version: report.Version, RunError: report.RunError, Pushes: *report.Pushes})
	s.respond(w, struct {
		Heard bool `json:"heard"`
	}{true}, err)
}

func (s *Server) getPushOrigins(w http.ResponseWriter, r *http.Request) {
	service, ok := s.collectorReports(w)
	if !ok {
		return
	}
	view, err := service.PushOrigins(r.Context(), s.repoID(r))
	s.respond(w, view, err)
}
