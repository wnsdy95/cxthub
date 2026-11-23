package http

import (
	"errors"
	"time"
)

type docFindHTTPDeadlineFailureWriter struct {
	*deadlineDocWriter
	writeFailure bool
}

func (writer *docFindHTTPDeadlineFailureWriter) SetReadDeadline(deadline time.Time) error {
	if !writer.writeFailure {
		return errors.New("read deadline failed")
	}
	return writer.deadlineDocWriter.SetReadDeadline(deadline)
}

func (writer *docFindHTTPDeadlineFailureWriter) SetWriteDeadline(time.Time) error {
	return errors.New("write deadline failed")
}
