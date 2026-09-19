package http

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGraphReadEncoding(t *testing.T) {
	body := `{"ids":["` + strings.Repeat("sha256:0123456789abcdef", 2000) + `"]}`
	for _, tc := range []struct {
		header     string
		compressed bool
	}{{"", false}, {"gzip", true}, {"br, gzip;q=0.8", true}, {"gzip;q=0, *;q=1", false}, {"*", true}, {"gzip;q=invalid", false}, {"identity", false}} {
		t.Run(tc.header, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/graph-state", nil)
			r.Header.Set("Accept-Encoding", tc.header)
			w := httptest.NewRecorder()
			compressedGraphRead(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = io.WriteString(w, body)
			})(w, r)
			if w.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatal("missing negotiation header")
			}
			if !tc.compressed {
				if w.Body.String() != body || w.Header().Get("Content-Encoding") != "" {
					t.Fatal("identity changed")
				}
				return
			}
			if w.Header().Get("Content-Encoding") != "gzip" || w.Body.Len() >= len(body)/10 {
				t.Fatal("graph not compressed")
			}
			z, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatal(err)
			}
			defer z.Close()
			raw, err := io.ReadAll(z)
			if err != nil || string(raw) != body {
				t.Fatal("corrupt response", err)
			}
		})
	}
}
