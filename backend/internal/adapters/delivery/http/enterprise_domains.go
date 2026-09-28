package http

import (
	"context"
	"net/http"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type enterpriseDomainBackend interface {
	ListEnterpriseDomains(context.Context, string, string) ([]app.EnterpriseDomainView, error)
	RequestEnterpriseDomain(context.Context, string, string, string, string) (app.EnterpriseDomainView, error)
	VerifyEnterpriseDomain(context.Context, string, string, string, string) (app.EnterpriseDomainView, error)
	ReleaseEnterpriseDomain(context.Context, string, string, string, string) error
}

func (s *Server) registerEnterpriseDomainRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/enterprises/{enterpriseID}/domains", s.requireUser(s.enterpriseDomains))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/domains", s.requireUser(s.rateLimit(20, time.Minute, s.enterpriseDomains)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/domains/verify", s.requireUser(s.rateLimit(10, time.Minute, s.enterpriseDomains)))
	mux.HandleFunc("POST /api/v1/enterprises/{enterpriseID}/domains/release", s.requireUser(s.rateLimit(20, time.Minute, s.enterpriseDomains)))
}
func (s *Server) enterpriseDomains(w http.ResponseWriter, r *http.Request) {
	b, ok := s.id.(enterpriseDomainBackend)
	if !ok {
		s.respond(w, nil, domain.ErrDomainVerificationUnavailable)
		return
	}
	u, _ := userFrom(r.Context())
	id := r.PathValue("enterpriseID")
	if r.Method == http.MethodGet {
		out, err := b.ListEnterpriseDomains(r.Context(), u.ID, id)
		s.respond(w, out, err)
		return
	}
	var body struct {
		Domain   string `json:"domain"`
		Revision string `json:"revision"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	switch r.Pattern {
	case "POST /api/v1/enterprises/{enterpriseID}/domains/verify":
		out, err := b.VerifyEnterpriseDomain(r.Context(), u.ID, id, body.Domain, body.Revision)
		s.respond(w, out, err)
	case "POST /api/v1/enterprises/{enterpriseID}/domains/release":
		err := b.ReleaseEnterpriseDomain(r.Context(), u.ID, id, body.Domain, body.Revision)
		s.respond(w, map[string]string{"status": "released"}, err)
	default:
		out, err := b.RequestEnterpriseDomain(r.Context(), u.ID, id, body.Domain, body.Revision)
		s.respond(w, out, err)
	}
}
