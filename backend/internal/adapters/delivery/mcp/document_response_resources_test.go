package mcp

import (
	"net/http/httptest"
	"time"
)

type documentResponseWriter struct {
	*httptest.ResponseRecorder
	write     func()
	cause     error
	deadlines []time.Time
}

func (writer *documentResponseWriter) Write(body []byte) (int, error) {
	writer.write()
	if writer.cause != nil {
		return 0, writer.cause
	}
	return writer.ResponseRecorder.Write(body)
}

func (writer *documentResponseWriter) SetReadDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	return nil
}

func (writer *documentResponseWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	return nil
}
