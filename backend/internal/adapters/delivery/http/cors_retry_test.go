package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapacityRetryHeaderExposedOnlyToConfiguredOrigins(t *testing.T) {
	server := &Server{cors: []string{"https://cxthub.io"}}
	handler := server.withCORS(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", "1")
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	for _, origin := range []string{"https://cxthub.io", "https://untrusted.example", ""} {
		t.Run(origin, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://api.cxthub.io/api/v1/repos/repo/view", nil)
			request.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
				t.Fatal("capacity response contract changed")
			}
			if origin == "https://cxthub.io" {
				if response.Header().Get("Access-Control-Expose-Headers") != "Retry-After" || response.Header().Get("Access-Control-Allow-Origin") != origin {
					t.Fatal("trusted browser cannot read retry hint")
				}
			} else if response.Header().Get("Access-Control-Expose-Headers") != "" || response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("untrusted origin received CORS access")
			}
		})
	}
}
