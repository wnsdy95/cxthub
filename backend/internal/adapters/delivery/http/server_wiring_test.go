package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// A stand-in that is only handed over: none of its methods is called.
type wiringEventQueries struct{ inbound.DocumentEventQueries }

// The reader of document events is wired with a named profile, and is not a
// nil pointer dressed as a value. A refused wiring changes nothing.
func TestDocumentEventQueriesNeedAProfileAndAReader(t *testing.T) {
	server := &Server{}
	for name, test := range map[string]struct {
		queries inbound.DocumentEventQueries
		profile inbound.DocumentQueryProfile
	}{
		"no profile":         {&wiringEventQueries{}, 0},
		"an unknown profile": {&wiringEventQueries{}, 3},
		"no reader":          {nil, inbound.DocumentQueryCompatibility},
		"a nil pointer":      {(*wiringEventQueries)(nil), inbound.DocumentQueryVerifiedRoots},
	} {
		if err := server.SetDocumentEventQueries(test.queries, test.profile); !errors.Is(err, domain.ErrValidation) || server.documentQueryProfile != 0 {
			t.Errorf("%s: %v, profile now %d", name, err, server.documentQueryProfile)
		}
	}
	for _, profile := range []inbound.DocumentQueryProfile{inbound.DocumentQueryCompatibility, inbound.DocumentQueryVerifiedRoots} {
		if err := server.SetDocumentEventQueries(&wiringEventQueries{}, profile); err != nil || server.documentQueryProfile != profile {
			t.Errorf("profile %d: %v, profile now %d", profile, err, server.documentQueryProfile)
		}
	}
	// A value of a kind that cannot be nil is a reader.
	if err := server.SetDocumentEventQueries(wiringEventQueries{}, inbound.DocumentQueryCompatibility); err != nil {
		t.Errorf("a reader that is a plain value: %v", err)
	}
}

type wiringDeadlineWriter struct {
	*httptest.ResponseRecorder
	readErr, writeErr error
	calls             []string
	deadlines         []time.Time
}

func (writer *wiringDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.calls, writer.deadlines = append(writer.calls, "read"), append(writer.deadlines, deadline)
	return writer.readErr
}

func (writer *wiringDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	writer.calls, writer.deadlines = append(writer.calls, "write"), append(writer.deadlines, deadline)
	return writer.writeErr
}

// A request that works under the server's resource limit gets a deadline on
// its connection. Where the deadline cannot be set the limit cannot be kept,
// and the request is answered with an error instead of being served. A
// connection that has no deadlines at all is served: there is nothing to set.
func TestRequestIsNotServedWhenItsDeadlineCannotBeSet(t *testing.T) {
	failure := errors.New("synthetic: the connection is gone")
	for name, test := range map[string]struct {
		writer *wiringDeadlineWriter
		calls  []string
	}{
		"the read deadline":  {&wiringDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), readErr: failure}, []string{"read"}},
		"the write deadline": {&wiringDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: failure}, []string{"read", "write"}},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		got, release, ok := (&Server{}).resourceRequestDeadline(test.writer, request)
		if ok || release != nil || got != request {
			t.Errorf("%s could not be set and the request went on: ok=%v", name, ok)
		}
		if test.writer.Code != http.StatusInternalServerError || !reflect.DeepEqual(test.writer.calls, test.calls) {
			t.Errorf("%s could not be set: answered %d after %v", name, test.writer.Code, test.writer.calls)
		}
	}
	for name, writer := range map[string]http.ResponseWriter{
		"a connection without deadlines":       httptest.NewRecorder(),
		"a connection without a read one":      &wiringDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), readErr: http.ErrNotSupported},
		"a connection without a write one":     &wiringDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: http.ErrNotSupported},
		"a connection that takes both of them": &wiringDeadlineWriter{ResponseRecorder: httptest.NewRecorder()},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		before := time.Now()
		got, release, ok := (&Server{}).resourceRequestDeadline(writer, request)
		if !ok || release == nil || got == request {
			t.Fatalf("%s was not served", name)
		}
		deadline, set := got.Context().Deadline()
		if !set || deadline.Before(before.Add(resourceRequestLimit)) || deadline.After(time.Now().Add(resourceRequestLimit)) {
			t.Errorf("%s: the request's deadline is %v", name, deadline)
		}
		if recorded, isWriter := writer.(*wiringDeadlineWriter); isWriter {
			if !reflect.DeepEqual(recorded.calls, []string{"read", "write"}) || !recorded.deadlines[0].Equal(deadline) || !recorded.deadlines[1].Equal(deadline) || recorded.Body.Len() != 0 {
				t.Errorf("%s: deadlines set %v at %v, body %q", name, recorded.calls, recorded.deadlines, recorded.Body)
			}
		}
		release()
		if got.Context().Err() == nil {
			t.Errorf("%s: releasing the request did not end its context", name)
		}
	}
}
