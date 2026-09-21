package http

import (
	"context"
	"net/http"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type notificationBackend interface {
	ListNotifications(context.Context, string, string) ([]domain.NotificationJob, error)
	RetryNotification(context.Context, string, string, string) error
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	b, ok := s.id.(notificationBackend)
	if !ok {
		s.writeError(w, 503, "unavailable", "Notification storage unavailable")
		return
	}
	u, _ := userFrom(r.Context())
	out, err := b.ListNotifications(r.Context(), u.ID, r.PathValue("repositoryID"))
	s.respond(w, out, err)
}
func (s *Server) retryNotification(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !s.decode(w, r, &input) {
		return
	}
	b, ok := s.id.(notificationBackend)
	if !ok {
		s.writeError(w, 503, "unavailable", "Notification storage unavailable")
		return
	}
	u, _ := userFrom(r.Context())
	err := b.RetryNotification(r.Context(), u.ID, r.PathValue("repositoryID"), r.PathValue("notificationID"))
	s.respond(w, map[string]string{"status": "queued"}, err)
}
