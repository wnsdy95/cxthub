package http

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleLegacyBackend struct {
	Backend
	eventCalls int
	viewCalls  int
	base       domain.ContentHash
	offset     int
	limit      int
	view       string
}

func (backend *docVisibleLegacyBackend) ReadDocViewEvents(_ context.Context, _, _, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	backend.viewCalls++
	backend.base, backend.offset, backend.limit, backend.view = base, offset, limit, view
	return domain.DocViewPage{Next: -1}, nil
}

type docVisibleDeadlineWriter struct {
	*deadlineDocWriter
	readCalls, writeCalls int
}

func (writer *docVisibleDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.readCalls++
	return writer.deadlineDocWriter.SetReadDeadline(deadline)
}

func (writer *docVisibleDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	writer.writeCalls++
	return writer.deadlineDocWriter.SetWriteDeadline(deadline)
}
