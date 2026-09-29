package http

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type federationBackend interface {
	GetOIDCView(context.Context, string, string, string) (app.OIDCView, error)
	ConfigureOIDC(context.Context, string, string, app.OIDCConnectionInput) (app.OIDCView, error)
	DisableOIDC(context.Context, string, string, string) error
	BeginOIDC(context.Context, string, string, string) (app.OIDCAuthorization, error)
	CompleteOIDC(context.Context, string, string, string) (string, error)
}

func (s *Server) registerFederationRoutes(mux *http.ServeMux) {
	s.registerSAMLRoutes(mux)
	s.registerCredentialAssuranceRoutes(mux)
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/oidc", s.requireUser(s.oidcView))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/oidc", s.requireUser(s.rateLimit(10, time.Minute, s.oidcConfigure)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/oidc/disable", s.requireUser(s.rateLimit(10, time.Minute, s.oidcDisable)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/oidc/authorize", s.requireUser(s.rateLimit(10, time.Minute, s.oidcAuthorize)))
	mux.HandleFunc("GET /api/v1/auth/enterprise/oidc/callback", s.rateLimit(30, time.Minute, s.oidcCallback))
}
func (s *Server) federationBackend(w http.ResponseWriter) federationBackend {
	b, ok := s.id.(federationBackend)
	if !ok {
		s.respond(w, nil, domain.ErrFederationUnavailable)
		return nil
	}
	return b
}
func (s *Server) oidcView(w http.ResponseWriter, r *http.Request) {
	b := s.federationBackend(w)
	if b == nil {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.GetOIDCView(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r))
	s.respond(w, out, err)
}
func (s *Server) oidcConfigure(w http.ResponseWriter, r *http.Request) {
	b := s.federationBackend(w)
	if b == nil {
		return
	}
	var in app.OIDCConnectionInput
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.ConfigureOIDC(r.Context(), u.ID, r.PathValue("enterpriseID"), in)
	s.respond(w, out, err)
}
func (s *Server) oidcDisable(w http.ResponseWriter, r *http.Request) {
	b := s.federationBackend(w)
	if b == nil {
		return
	}
	var in struct {
		Revision string `json:"revision"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	err := b.DisableOIDC(r.Context(), u.ID, r.PathValue("enterpriseID"), in.Revision)
	s.respond(w, map[string]string{"status": "disabled"}, err)
}
func (s *Server) oidcAuthorize(w http.ResponseWriter, r *http.Request) {
	b := s.federationBackend(w)
	if b == nil {
		return
	}
	var in struct{}
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.BeginOIDC(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r))
	s.respond(w, out, err)
}
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	b := s.federationBackend(w)
	if b == nil {
		return
	}
	// Never render or log provider errors, state or authorization codes.
	q := r.URL.Query()
	if q.Get("error") != "" || len(q["state"]) != 1 || len(q["code"]) != 1 {
		s.writeError(w, 401, "identity_verification_failed", "Restart identity verification from your Enterprise page.")
		return
	}
	slug, err := b.CompleteOIDC(r.Context(), s.requestToken(r), q.Get("state"), q.Get("code"))
	if err != nil {
		s.writeError(w, 401, "identity_verification_failed", "Identity verification was not applied. Sign in again and restart from your Enterprise page.")
		return
	}
	http.Redirect(w, r, "/enterprises/"+url.PathEscape(slug)+"?tab=identity", http.StatusSeeOther)
}
