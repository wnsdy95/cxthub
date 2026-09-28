package http

import (
	"context"
	"encoding/base64"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type samlBackend interface {
	GetSAMLView(context.Context, string, string, string) (app.SAMLView, error)
	ConfigureSAML(context.Context, string, string, app.SAMLConnectionInput) (app.SAMLView, error)
	DisableSAML(context.Context, string, string, string) error
	BeginSAML(context.Context, string, string, string) (app.OIDCAuthorization, error)
	SAMLMetadata(context.Context, string) (string, error)
	ReceiveSAML(context.Context, string, string, []byte) (string, error)
	CompleteSAML(context.Context, string, string) (string, error)
}

func (s *Server) registerSAMLRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/saml", s.requireUser(s.samlView))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/saml", s.requireUser(s.rateLimit(10, time.Minute, s.samlConfigure)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/saml/disable", s.requireUser(s.rateLimit(10, time.Minute, s.samlDisable)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/saml/authorize", s.requireUser(s.rateLimit(10, time.Minute, s.samlAuthorize)))
	mux.HandleFunc("GET /api/v1/auth/enterprise/saml/{enterpriseID}/metadata", s.rateLimit(30, time.Minute, s.samlMetadata))
	mux.HandleFunc("POST /api/v1/auth/enterprise/saml/{enterpriseID}/acs", s.rateLimit(30, time.Minute, s.samlACS))
	mux.HandleFunc("GET /api/v1/auth/enterprise/saml/finish", s.rateLimit(30, time.Minute, s.samlFinish))
}
func (s *Server) samlBackend(w http.ResponseWriter) samlBackend {
	b, ok := s.id.(samlBackend)
	if !ok {
		s.respond(w, nil, domain.ErrFederationUnavailable)
		return nil
	}
	return b
}
func (s *Server) samlView(w http.ResponseWriter, r *http.Request) {
	b := s.samlBackend(w)
	if b == nil {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.GetSAMLView(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r))
	s.respond(w, out, err)
}
func (s *Server) samlConfigure(w http.ResponseWriter, r *http.Request) {
	b := s.samlBackend(w)
	if b == nil {
		return
	}
	var in app.SAMLConnectionInput
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.ConfigureSAML(r.Context(), u.ID, r.PathValue("enterpriseID"), in)
	s.respond(w, out, err)
}
func (s *Server) samlDisable(w http.ResponseWriter, r *http.Request) {
	b := s.samlBackend(w)
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
	err := b.DisableSAML(r.Context(), u.ID, r.PathValue("enterpriseID"), in.Revision)
	s.respond(w, map[string]string{"status": "disabled"}, err)
}
func (s *Server) samlAuthorize(w http.ResponseWriter, r *http.Request) {
	b := s.samlBackend(w)
	if b == nil {
		return
	}
	var in struct{}
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.BeginSAML(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r))
	s.respond(w, out, err)
}
func identityCallbackHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}
func (s *Server) samlMetadata(w http.ResponseWriter, r *http.Request) {
	identityCallbackHeaders(w)
	b := s.samlBackend(w)
	if b == nil {
		return
	}
	out, err := b.SAMLMetadata(r.Context(), r.PathValue("enterpriseID"))
	if err != nil {
		s.respond(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml; charset=utf-8")
	_, _ = io.WriteString(w, out)
}

// This exact route accepts no cookie authority. CSRF exemption is limited to
// protocol receipt; only the separate same-origin completion can bind a user.
func isSAMLACS(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/auth/enterprise/saml/")
	if !ok {
		return false
	}
	id, suffix, ok := strings.Cut(rest, "/")
	return ok && suffix == "acs" && domain.ValidateEnterpriseID(id) == nil
}
func (s *Server) samlFailure(w http.ResponseWriter) {
	s.writeError(w, 401, "identity_verification_failed", "Identity verification was not applied. Sign in again and restart from your Enterprise page.")
}
func (s *Server) samlACS(w http.ResponseWriter, r *http.Request) {
	identityCallbackHeaders(w)
	b := s.samlBackend(w)
	if b == nil {
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		s.samlFailure(w)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<10))
	if err != nil {
		s.samlFailure(w)
		return
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil || len(form) != 2 || len(form["SAMLResponse"]) != 1 || len(form["RelayState"]) != 1 {
		s.samlFailure(w)
		return
	}
	xml, err := base64.StdEncoding.Strict().DecodeString(form.Get("SAMLResponse"))
	if err != nil || len(xml) > 256<<10 {
		s.samlFailure(w)
		return
	}
	finish, err := b.ReceiveSAML(r.Context(), r.PathValue("enterpriseID"), form.Get("RelayState"), xml)
	if err != nil {
		s.samlFailure(w)
		return
	}
	http.Redirect(w, r, "/api/v1/auth/enterprise/saml/finish?ticket="+url.QueryEscape(finish), http.StatusSeeOther)
}
func (s *Server) samlFinish(w http.ResponseWriter, r *http.Request) {
	identityCallbackHeaders(w)
	b := s.samlBackend(w)
	if b == nil {
		return
	}
	q := r.URL.Query()
	if len(q) != 1 || len(q["ticket"]) != 1 {
		s.samlFailure(w)
		return
	}
	slug, err := b.CompleteSAML(r.Context(), s.requestToken(r), q.Get("ticket"))
	if err != nil {
		s.samlFailure(w)
		return
	}
	http.Redirect(w, r, "/enterprises/"+url.PathEscape(slug)+"?tab=identity", http.StatusSeeOther)
}
