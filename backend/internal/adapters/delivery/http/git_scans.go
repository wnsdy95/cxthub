package http

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"net/http"
	"strings"
)

// Called only after the shared webhook signature has been verified. Ignore the
// bounded commits array; the worker traverses immutable parents from BOTH tips.
func (s *Server) githubPush(w http.ResponseWriter, r *http.Request, body []byte) {
	observer, ok := s.b.(interface {
		ObserveRemotePush(ctx context.Context, origin, ref, before, after string, forced bool, delivery string) (int, error)
	})
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "unsupported", "this server does not read pushes")
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
	n, err := observer.ObserveRemotePush(inbound.WithSystemActor(r.Context()), origin, p.Ref, zero(p.Before), zero(p.After), p.Forced, r.Header.Get("X-GitHub-Delivery"))
	s.respond(w, map[string]interface{}{"status": "accepted", "queued": n}, err)
}

// Called only after the shared webhook signature has been verified. A change
// of a pull request moves no branch, so nothing but this delivery tells of
// it: the repositories of the origin are read again. Nothing of the pull
// request is taken from the delivery; the read of the provider is what counts.
func (s *Server) githubPullRequest(w http.ResponseWriter, r *http.Request, body []byte) {
	observer, ok := s.b.(interface {
		ObserveRemotePullRequest(ctx context.Context, origin string) (int, error)
	})
	if !ok {
		s.writeError(w, http.StatusNotImplemented, "unsupported", "this server does not read pull requests")
		return
	}
	var p struct {
		Repository struct {
			CloneURL string `json:"clone_url"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	origin := p.Repository.CloneURL
	if origin == "" {
		origin = p.Repository.HTMLURL
	}
	if origin == "" {
		s.respond(w, map[string]string{"status": "ignored"}, nil)
		return
	}
	n, err := observer.ObserveRemotePullRequest(inbound.WithSystemActor(r.Context()), origin)
	s.respond(w, map[string]interface{}{"status": "accepted", "queued": n}, err)
}
