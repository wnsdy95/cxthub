package http

import (
	"context"
	"net/http/httptest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type deadlineDocBackend struct {
	Backend
	read func(context.Context)
}

func (backend deadlineDocBackend) ReadDocViewEvents(ctx context.Context, _, _, _ domain.ContentHash, _, _ int, _ string) (domain.DocViewPage, error) {
	backend.read(ctx)
	return domain.DocViewPage{Next: -1}, nil
}

func (backend deadlineDocBackend) ReadDocViewFragment(ctx context.Context, _, _ domain.ContentHash, _, _, _ int) (domain.DocViewFragment, error) {
	backend.read(ctx)
	return domain.DocViewFragment{Next: -1}, nil
}

type deadlineDocWriter struct {
	*httptest.ResponseRecorder
	read, write time.Time
	failure     error
}

func (writer *deadlineDocWriter) SetReadDeadline(deadline time.Time) error {
	writer.read = deadline
	return writer.failure
}

func (writer *deadlineDocWriter) SetWriteDeadline(deadline time.Time) error {
	writer.write = deadline
	return writer.failure
}
