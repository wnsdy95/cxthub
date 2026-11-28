package http

import (
	"log"
	"net/http"
	"strings"
)

// requestWriter is the writer every route answers through. It keeps the
// method and path of the request, which the log of an internal error names.
type requestWriter struct {
	http.ResponseWriter
	method, path string
}

// Unwrap lets http.ResponseController reach the writer of the connection,
// which the deadlines of a request are set on.
func (w *requestWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// internalErrorLogBytes is the most of an error's text one log line keeps.
const internalErrorLogBytes = 4096

// logInternalError writes to the server's log what went wrong when the answer
// only says "internal server error": the request (its method, its path and
// the X-Request-ID it was answered with) and the error. An operator reads the
// cause there; the client is not told it. The path and the error are quoted,
// so that what a request or an error holds cannot start another log line.
func logInternalError(w http.ResponseWriter, cause string) {
	if len(cause) > internalErrorLogBytes {
		cause = strings.ToValidUTF8(cause[:internalErrorLogBytes], "") + "…"
	}
	request := w.Header().Get("X-Request-ID")
	if named, ok := w.(*requestWriter); ok {
		log.Printf("internal error: %s %q (%s): %q", named.method, named.path, request, cause)
		return
	}
	log.Printf("internal error (%s): %q", request, cause)
}
