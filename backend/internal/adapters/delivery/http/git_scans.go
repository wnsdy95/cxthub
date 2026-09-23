package http

import (
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) SetGitScans(scans inbound.GitScans) { s.gitScans = scans }
func (s *Server) scansAvailable(w http.ResponseWriter) bool {
	if s.gitScans == nil {
		s.writeError(w, 503, "unavailable", "Git observation processing unavailable")
		return false
	}
	return true
}
func (s *Server) listGitScans(w http.ResponseWriter, r *http.Request) {
	if !s.scansAvailable(w) {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.respond(w, nil, domain.ErrValidation)
			return
		}
		limit = n
	}
	out, err := s.gitScans.ListScans(r.Context(), s.repoID(r), r.URL.Query().Get("cursor"), limit)
	s.respond(w, out, err)
}
func (s *Server) retryGitScan(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, 415, "bad_request", "Content-Type must be application/json")
		return
	}
	if !s.scansAvailable(w) {
		return
	}
	err := s.gitScans.RetryScan(r.Context(), s.repoID(r), r.PathValue("scanID"))
	s.respond(w, map[string]bool{"queued": err == nil}, err)
}

// Called only after the shared webhook signature has been verified. Ignore the
// bounded commits array; the worker traverses immutable parents from BOTH tips.
func (s *Server) githubPush(w http.ResponseWriter, r *http.Request, body []byte) {
	if !s.scansAvailable(w) {
		return
	}
	var p struct {
		Ref        string `json:"ref"`
		Before     string `json:"before"`
		After      string `json:"after"`
		Forced     bool   `json:"forced"`
		Repository struct {
			CloneURL string `json:"clone_url"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	if !strings.HasPrefix(p.Ref, "refs/heads/") {
		s.respond(w, map[string]string{"status": "ignored"}, nil)
		return
	}
	zero := func(s string) string {
		if (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0") == "" {
			return ""
		}
		return s
	}
	origin := p.Repository.CloneURL
	if origin == "" {
		origin = p.Repository.HTMLURL
	}
	n, err := s.gitScans.ObservePush(inbound.WithSystemActor(r.Context()), origin, p.Ref, zero(p.Before), zero(p.After), p.Forced, r.Header.Get("X-GitHub-Delivery"))
	s.respond(w, map[string]interface{}{"status": "accepted", "queued": n}, err)
}
