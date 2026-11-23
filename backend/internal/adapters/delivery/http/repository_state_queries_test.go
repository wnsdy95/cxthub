package http

import (
	"context"
	"net/http/httptest"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type repositoryStatePoisonedBackend struct{ Backend }

func (*repositoryStatePoisonedBackend) RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
	panic("repository revision read reached the broad backend")
}

type repositoryStateStreamWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (writer *repositoryStateStreamWriter) Flush() {
	writer.ResponseRecorder.Flush()
	writer.cancel()
}
