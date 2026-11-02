package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryPreparationRouteDisabledWithoutExplicitScopeAndAudience(test *testing.T) {
	api := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	})
	for _, scenario := range []struct {
		origin string
		repos  []domain.ContentHash
	}{
		{},
		{origin: "https://mcp.example.test"},
		{repos: []domain.ContentHash{domain.HashContent([]byte("repo"))}},
	} {
		handler, err := withMemoryPreparation(nil, api, scenario.origin, scenario.repos)
		if err != nil {
			test.Fatal(err)
		}
		for _, path := range []string{domain.MemoryReadPreparationPath, "/mcp", "/oauth/token"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
			if response.Code != http.StatusNotFound {
				test.Fatal("disabled API composition exposed MCP route", path, response.Code)
			}
		}
	}
}
