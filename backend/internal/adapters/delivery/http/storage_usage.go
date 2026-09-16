package http

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"time"
)

type storageUsageBackend interface {
	StorageUsage(context.Context, string, string, time.Time, time.Time) (domain.StorageUsage, error)
	ReconcileStorageUsage(context.Context, string, string) error
}

func (s *Server) getStorageUsage(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.id.(storageUsageBackend)
	if !ok {
		s.respond(w, nil, domain.ErrUsageUnavailable)
		return
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := now
	if period := r.URL.Query().Get("month"); period != "" {
		var err error
		start, err = time.Parse("2006-01", period)
		if err != nil || start.After(now) {
			s.respond(w, nil, domain.ErrValidation)
			return
		}
		if next := start.AddDate(0, 1, 0); next.Before(now) {
			end = next
		}
	}
	u, _ := userFrom(r.Context())
	ns := r.PathValue("namespaceID")
	if ns == "" {
		ns = "self"
	}
	report, err := svc.StorageUsage(r.Context(), u.ID, ns, start, end)
	s.respond(w, report, err)
}
func (s *Server) reconcileStorageUsage(w http.ResponseWriter, r *http.Request) {
	if !isJSONBody(r) {
		s.writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "application/json required")
		return
	}
	svc, ok := s.id.(storageUsageBackend)
	if !ok {
		s.respond(w, nil, domain.ErrUsageUnavailable)
		return
	}
	u, _ := userFrom(r.Context())
	ns := r.PathValue("namespaceID")
	if ns == "" {
		ns = "self"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := svc.ReconcileStorageUsage(ctx, u.ID, ns)
	s.respond(w, map[string]bool{"reconciled": err == nil}, err)
}
