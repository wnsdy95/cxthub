package http

import (
	"context"
	"net/http/httptest"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type repositoryRefHTTPBackend struct {
	Backend
	repo domain.Repo
}

func (backend *repositoryRefHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

type repositoryRefHTTPBody struct {
	*strings.Reader
	observe func()
}

func (body *repositoryRefHTTPBody) Read(destination []byte) (int, error) {
	body.observe()
	return body.Reader.Read(destination)
}

func (*repositoryRefHTTPBody) Close() error { return nil }

type repositoryRefHTTPWriter struct {
	*httptest.ResponseRecorder
	observe func()
	writes  int
	err     error
}

func (writer *repositoryRefHTTPWriter) Write(body []byte) (int, error) {
	writer.writes++
	writer.observe()
	if writer.err != nil {
		return 0, writer.err
	}
	return writer.ResponseRecorder.Write(body)
}
