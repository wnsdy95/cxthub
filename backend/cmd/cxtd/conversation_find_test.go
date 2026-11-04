package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestConversationPreparationRouteRequiresExplicitMCPAudience(test *testing.T) {
	api := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNotFound) })
	handler, err := withConversationFindPreparation(nil, api, "")
	if err != nil {
		test.Fatal(err)
	}
	for _, route := range []string{domain.ConversationFindPreparationPath, "/mcp", "/oauth/token"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, route, nil))
		if response.Code != http.StatusNotFound {
			test.Fatal("disabled API composition exposed an MCP route", route, response.Code)
		}
	}
	if _, err := withConversationFindPreparation(nil, api, "https://mcp.example.test"); err == nil {
		test.Fatal("preparation route enabled without a configured API runtime")
	}
}
