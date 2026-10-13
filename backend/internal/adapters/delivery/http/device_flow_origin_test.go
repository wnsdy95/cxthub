package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

func TestDeviceApprovalUsesConfiguredFrontendNotRequestHost(t *testing.T) {
	server := &Server{runtime: store.NewFSStore(t.TempDir())}
	server.SetDeviceVerificationOrigin("https://frontend.example.test/")
	request := httptest.NewRequest(http.MethodPost, "https://api.example.test/api/v1/auth/device/start", strings.NewReader(`{"label":"test CLI"}`))
	request.Host = "untrusted.example.test"
	request.Header.Set("X-Forwarded-Host", "untrusted.example.test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.deviceStart(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("start status = %d", response.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["verification_uri"] != "https://frontend.example.test/login/device" {
		t.Fatal("approval page did not use configured frontend")
	}
	complete, err := url.Parse(payload["verification_uri_complete"].(string))
	if err != nil || complete.Scheme != "https" || complete.Host != "frontend.example.test" || complete.Path != "/login/device" || len(complete.Query()) != 1 || complete.Query().Get("code") != payload["code"] {
		t.Fatal("approval URL changed origin or pairing code")
	}
	if strings.Contains(complete.String(), payload["poll_token"].(string)) {
		t.Fatal("poll credential exposed in browser URL")
	}
}
