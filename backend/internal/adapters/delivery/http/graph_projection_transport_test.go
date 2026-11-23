package http

import (
	"net/http/httptest"
	"time"
)

type graphAdmissionQueueWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
}

func (writer *graphAdmissionQueueWriter) SetWriteDeadline(time.Time) error {
	select {
	case <-writer.entered:
	default:
		close(writer.entered)
	}
	return nil
}
