package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type GitHubConnections interface {
	Overview(context.Context, string) (app.GitHubOverview, error)
	Start(context.Context, string, string, int64) (app.GitHubStart, error)
	Setup(context.Context, string, string, int64) (app.GitHubStart, error)
	Complete(context.Context, string, string, string) (string, error)
	Edit(context.Context, string, string, int64, string, domain.GitHubBinding, domain.GitHubTeamMapping) error
	Enterprise(context.Context, string, string) ([]app.GitHubEnterpriseConnection, error)
	Receive(context.Context, string, string, []byte) error
}

func (s *Server) SetGitHubConnections(g GitHubConnections, secret, returnURL string) {
	s.githubConnections = g
	s.githubAppSecret = secret
	s.githubReturnURL = returnURL
}
func (s *Server) registerGitHubRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/github/connections", s.requireUser(s.githubConnectionsView))
	mux.HandleFunc("POST /api/v1/github/requests", s.requireUser(s.githubConnectStart))
	mux.HandleFunc("GET /api/v1/github/setup", s.requireUser(s.githubConnectSetup))
	mux.HandleFunc("GET /api/v1/github/callback", s.requireUser(s.githubConnectCallback))
	mux.HandleFunc("POST /api/v1/github/connections/{namespaceID}", s.requireUser(s.githubConnectionEdit))
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/github-connections", s.requireUser(s.githubEnterpriseConnections))
	mux.HandleFunc("POST /api/v1/github/webhook", s.githubAppWebhook)
}
func (s *Server) githubAvailable(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if s.githubConnections == nil {
		s.writeError(w, 503, "unavailable", "GitHub App is not configured")
		return false
	}
	return true
}
func (s *Server) githubConnectionsView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.githubConnections == nil {
		s.respond(w, app.GitHubOverview{Owners: []app.GitHubOwnerView{}}, nil)
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.githubConnections.Overview(r.Context(), u.ID)
	s.respond(w, out, err)
}
func (s *Server) githubConnectStart(w http.ResponseWriter, r *http.Request) {
	if !s.githubAvailable(w) {
		return
	}
	var body struct {
		NamespaceID    string `json:"namespace_id"`
		InstallationID int64  `json:"installation_id"`
	}
	if !s.decodeLimited(w, r, &body, 4096) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.githubConnections.Start(r.Context(), u.ID, body.NamespaceID, body.InstallationID)
	s.respond(w, out, err)
}
func (s *Server) githubConnectSetup(w http.ResponseWriter, r *http.Request) {
	if !s.githubAvailable(w) {
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.URL.Query().Get("setup_action") == "request" {
		// A request awaiting GitHub organization approval grants no access. The
		// administrator must restart the verified flow after approval.
		http.Redirect(w, r, s.githubReturnURL+"?result=approval-required", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.githubConnections.Setup(r.Context(), u.ID, r.URL.Query().Get("state"), id)
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	http.Redirect(w, r, out.URL, http.StatusSeeOther)
}
func (s *Server) githubConnectCallback(w http.ResponseWriter, r *http.Request) {
	if !s.githubAvailable(w) {
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	u, _ := userFrom(r.Context())
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, s.githubReturnURL+"?result=cancelled", http.StatusSeeOther)
		return
	}
	_, err := s.githubConnections.Complete(r.Context(), u.ID, r.URL.Query().Get("state"), code)
	result := "connected"
	if err != nil {
		result = "failed"
	}
	http.Redirect(w, r, s.githubReturnURL+"?result="+result, http.StatusSeeOther)
}
func (s *Server) githubConnectionEdit(w http.ResponseWriter, r *http.Request) {
	if !s.githubAvailable(w) {
		return
	}
	var body struct {
		Generation int64                    `json:"generation"`
		Action     string                   `json:"action"`
		Binding    domain.GitHubBinding     `json:"binding"`
		Mapping    domain.GitHubTeamMapping `json:"mapping"`
	}
	if !s.decodeLimited(w, r, &body, 8192) {
		return
	}
	u, _ := userFrom(r.Context())
	err := s.githubConnections.Edit(r.Context(), u.ID, r.PathValue("namespaceID"), body.Generation, body.Action, body.Binding, body.Mapping)
	s.respond(w, map[string]bool{"saved": err == nil}, err)
}
func (s *Server) githubEnterpriseConnections(w http.ResponseWriter, r *http.Request) {
	if !s.githubAvailable(w) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := s.githubConnections.Enterprise(r.Context(), u.ID, r.PathValue("enterpriseID"))
	s.respond(w, out, err)
}
func (s *Server) githubAppWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.githubAvailable(w) {
		return
	}
	if s.githubAppSecret == "" {
		s.respond(w, nil, domain.ErrForbidden)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.writeError(w, 413, "payload_too_large", "Webhook exceeds size limit")
		return
	}
	mac := hmac.New(sha256.New, []byte(s.githubAppSecret))
	mac.Write(raw)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(want)) {
		s.writeError(w, 401, "unauthenticated", "Webhook signature mismatch")
		return
	}
	err = s.githubConnections.Receive(r.Context(), r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event"), raw)
	s.respond(w, map[string]bool{"accepted": err == nil}, err)
}
