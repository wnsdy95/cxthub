package http

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// captureLog sends what the server logs to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logged bytes.Buffer
	output, flags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(output)
		log.SetFlags(flags)
	})
	return &logged
}

// An error the server did not expect is answered with a fixed sentence, and
// the server's log says which request it answered and what went wrong: an
// operator reads the cause there.
func TestAnInternalErrorIsLoggedWithTheRequestItAnswered(t *testing.T) {
	logged := captureLog(t)
	server := &Server{}
	cause := "list anchored sessions: ERROR: permission denied for table anchored_sessions\nforged line"
	failing := server.withCorrelation(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		server.respond(w, nil, errors.New(cause))
	}))
	response := httptest.NewRecorder()
	failing.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/repos/sha256:abc/anchored-sessions?code=private", nil))

	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"internal server error"`) || strings.Contains(response.Body.String(), "permission denied") {
		t.Fatalf("the answer: %d %s", response.Code, response.Body)
	}
	request := response.Header().Get("X-Request-ID")
	if !strings.HasPrefix(request, "req_") {
		t.Fatalf("the answer has no request ID: %q", request)
	}
	want := fmt.Sprintf("internal error: GET %q (%s): %q\n", "/api/v1/repos/sha256:abc/anchored-sessions", request, cause)
	if logged.String() != want {
		t.Fatalf("the log:\n%s\nwant:\n%s", logged, want)
	}
	if strings.Contains(logged.String(), "private") {
		t.Fatalf("the log has the query of the request: %s", logged)
	}
}

// Only an error the server did not expect is logged: an answer that says
// what went wrong in its own words is not.
func TestAnExpectedErrorIsNotLogged(t *testing.T) {
	logged := captureLog(t)
	server := &Server{}
	for _, err := range []error{fmt.Errorf("session 42: %w", domain.ErrNotFound), domain.ErrForbidden, domain.ErrValidation, domain.ErrResourceOverloaded} {
		server.respond(httptest.NewRecorder(), nil, err)
	}
	if logged.Len() != 0 {
		t.Fatalf("expected errors were logged: %s", logged)
	}
}

// An internal error answered outside a request still logs its cause, and one
// line keeps at most internalErrorLogBytes of it.
func TestAnInternalErrorOutsideARequestIsLogged(t *testing.T) {
	logged := captureLog(t)
	(&Server{}).writeError(httptest.NewRecorder(), http.StatusInternalServerError, "internal", "code generation failed")
	if logged.String() != "internal error (): \"code generation failed\"\n" {
		t.Fatalf("the log: %q", logged)
	}
	logged.Reset()
	(&Server{}).writeError(httptest.NewRecorder(), http.StatusInternalServerError, "internal", "x"+strings.Repeat("é", internalErrorLogBytes))
	// Each é is two bytes, so the last byte kept is half of one: the line
	// keeps whole characters only.
	if want := "internal error (): \"x" + strings.Repeat("é", internalErrorLogBytes/2-1) + "…\"\n"; logged.String() != want {
		t.Fatalf("a long error was logged in %d bytes, want %d: %.80q", logged.Len(), len(want), logged)
	}
}

// deadlineWriter is a connection's writer that takes a write deadline.
type deadlineWriter struct {
	http.ResponseWriter
	deadline time.Time
}

func (w *deadlineWriter) SetWriteDeadline(at time.Time) error {
	w.deadline = at
	return nil
}

// The deadlines of a request still reach the connection through the writer
// that names the request.
func TestTheRequestWriterKeepsTheDeadlinesOfItsConnection(t *testing.T) {
	connection := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	at := time.Now().Add(time.Minute)
	if err := http.NewResponseController(&requestWriter{ResponseWriter: connection}).SetWriteDeadline(at); err != nil || !connection.deadline.Equal(at) {
		t.Fatalf("the deadline did not reach the connection: %v %v", err, connection.deadline)
	}
}
