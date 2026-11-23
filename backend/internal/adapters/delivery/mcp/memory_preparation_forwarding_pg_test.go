//go:build postgres

package mcp

import (
	"net/http"
	"net/http/httptest"
)

type memoryPreparationBufferedResponse struct {
	*httptest.ResponseRecorder
	underlying http.ResponseWriter
}

func (response memoryPreparationBufferedResponse) Unwrap() http.ResponseWriter {
	return response.underlying
}
