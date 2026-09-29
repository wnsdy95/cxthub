package http

import (
	"context"
	"net/http"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type ownerRecoveryBackend interface {
	GetOwnerRecovery(context.Context, string, string, string) (app.OwnerRecoveryView, error)
	PrepareOwnerRecovery(context.Context, string, string, string, string) (app.PreparedOwnerRecovery, error)
	ConfirmOwnerRecovery(context.Context, string, string, string, string, string) error
	RevokeOwnerRecovery(context.Context, string, string, string, string) error
	RedeemOwnerRecovery(context.Context, string, string, string, string) (app.OwnerRecoveryView, error)
	AssessEnterpriseCredential(context.Context, string, string, string) (app.CredentialAssessment, error)
}

func (s *Server) registerOwnerRecoveryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/credential-assessment", s.requireUser(s.credentialAssessment))
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/owner-recovery", s.requireUser(s.ownerRecovery))
	handler := func(action string) http.HandlerFunc {
		return s.requireUser(s.rateLimit(5, time.Minute, func(w http.ResponseWriter, r *http.Request) { s.ownerRecoveryAction(w, r, action) }))
	}
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/owner-recovery/prepare", handler("prepare"))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/owner-recovery/confirm", handler("confirm"))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/owner-recovery/revoke", handler("revoke"))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/owner-recovery/redeem", handler("redeem"))
}
func (s *Server) ownerRecoveryBackend(w http.ResponseWriter) ownerRecoveryBackend {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	b, ok := s.id.(ownerRecoveryBackend)
	if !ok {
		s.respond(w, nil, domain.ErrFederationUnavailable)
		return nil
	}
	return b
}
func (s *Server) ownerRecovery(w http.ResponseWriter, r *http.Request) {
	b := s.ownerRecoveryBackend(w)
	if b == nil {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.GetOwnerRecovery(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r))
	s.respond(w, out, err)
}
func (s *Server) credentialAssessment(w http.ResponseWriter, r *http.Request) {
	b := s.ownerRecoveryBackend(w)
	if b == nil {
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.AssessEnterpriseCredential(r.Context(), u.ID, r.PathValue("enterpriseID"), s.requestToken(r))
	s.respond(w, out, err)
}
func (s *Server) ownerRecoveryAction(w http.ResponseWriter, r *http.Request, action string) {
	b := s.ownerRecoveryBackend(w)
	if b == nil {
		return
	}
	var in struct {
		Revision string `json:"revision"`
		Code     string `json:"code"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	// Never echo supplied recovery material in error messages or audit.
	if len(in.Code) > 128 || len(in.Revision) > 128 {
		s.respond(w, nil, domain.ErrValidation)
		return
	}
	u, _ := userFrom(r.Context())
	ctx, ep, token := r.Context(), r.PathValue("enterpriseID"), s.requestToken(r)
	var out any = map[string]string{"status": "saved"}
	var err error
	switch action {
	case "prepare":
		out, err = b.PrepareOwnerRecovery(ctx, u.ID, ep, token, in.Revision)
	case "confirm":
		err = b.ConfirmOwnerRecovery(ctx, u.ID, ep, token, in.Revision, in.Code)
	case "revoke":
		err = b.RevokeOwnerRecovery(ctx, u.ID, ep, token, in.Revision)
	case "redeem":
		out, err = b.RedeemOwnerRecovery(ctx, u.ID, ep, token, in.Code)
	}
	s.respond(w, out, err)
}
