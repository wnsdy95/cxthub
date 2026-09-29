package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"time"
)

type credentialAssuranceBackend interface {
	GetCredentialAssurances(context.Context, string, string, string) (app.CredentialAssurancesView, error)
	ConfigureAssurancePolicy(context.Context, string, string, string, app.AssurancePolicyInput) error
	ApproveCredential(context.Context, string, string, string, string, app.CredentialApprovalInput) error
	RevokeCredentialAssurance(context.Context, string, string, string, string) error
}

func (s *Server) registerCredentialAssuranceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/credential-assurances", s.requireUser(s.credentialAssurances))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/assurance-policy", s.requireUser(s.rateLimit(10, time.Minute, s.assurancePolicy)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/credential-assurances/{credentialID}", s.requireUser(s.rateLimit(20, time.Minute, s.credentialApprove)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/credential-assurances/{credentialID}/revoke", s.requireUser(s.rateLimit(20, time.Minute, s.credentialAssuranceRevoke)))
}
func (s *Server) credentialAssuranceBackend(w http.ResponseWriter) credentialAssuranceBackend {
	w.Header().Set("Cache-Control", "no-store")
	b, ok := s.id.(credentialAssuranceBackend)
	if !ok {
		s.respond(w, nil, domain.ErrFederationUnavailable)
		return nil
	}
	return b
}
func (s *Server) credentialAssurances(w http.ResponseWriter, r *http.Request) {
	b := s.credentialAssuranceBackend(w)
	if b == nil {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.GetCredentialAssurances(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r))
	s.respond(w, out, err)
}
func (s *Server) assurancePolicy(w http.ResponseWriter, r *http.Request) {
	b := s.credentialAssuranceBackend(w)
	if b == nil {
		return
	}
	var in app.AssurancePolicyInput
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	err := b.ConfigureAssurancePolicy(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r), in)
	s.respond(w, map[string]string{"status": "saved"}, err)
}
func (s *Server) credentialApprove(w http.ResponseWriter, r *http.Request) {
	b := s.credentialAssuranceBackend(w)
	if b == nil {
		return
	}
	var in app.CredentialApprovalInput
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	err := b.ApproveCredential(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r), r.PathValue("credentialID"), in)
	s.respond(w, map[string]string{"status": "approved"}, err)
}
func (s *Server) credentialAssuranceRevoke(w http.ResponseWriter, r *http.Request) {
	b := s.credentialAssuranceBackend(w)
	if b == nil {
		return
	}
	var in struct{}
	if !s.decode(w, r, &in) {
		return
	}
	u, _ := userFrom(r.Context())
	err := b.RevokeCredentialAssurance(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r), r.PathValue("credentialID"))
	s.respond(w, map[string]string{"status": "revoked"}, err)
}
