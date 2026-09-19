package http

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// Graph facts repeat identifiers to keep the client stateless. Compress these
// JSON reads on the wire; auth guards run outside this adapter and SSE is never
// wrapped. No documents or credentials are added to the projection.
func compressedGraphRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGraphGzip(r.Header.Get("Accept-Encoding")) {
			next(w, r)
			return
		}
		z, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		defer z.Close()
		w.Header().Set("Content-Encoding", "gzip")
		next(graphGzipWriter{ResponseWriter: w, z: z}, r)
	}
}

type graphGzipWriter struct {
	http.ResponseWriter
	z *gzip.Writer
}

func (w graphGzipWriter) Write(p []byte) (int, error) { return w.z.Write(p) }
func (w graphGzipWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func acceptsGraphGzip(header string) bool {
	wildcard := false
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		coding := strings.TrimSpace(parts[0])
		quality := 1.0
		for _, param := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(key, "q") {
				q, err := strconv.ParseFloat(value, 64)
				if err != nil || q < 0 || q > 1 {
					quality = 0
				} else {
					quality = q
				}
			}
		}
		if strings.EqualFold(coding, "gzip") {
			return quality > 0
		}
		if coding == "*" {
			wildcard = quality > 0
		}
	}
	return wildcard
}
