package backendclient

import (
	"net/http"
	"strings"
	"testing"
)

func TestHTTPErrorNeverCopiesProxyPagesOrTokens(t *testing.T) {
	token := strings.Repeat("Q7x_", 24) + ".Nt" // synthetic opaque run; never a real credential
	html := newHTTPError(502, http.MethodPost, "/repos/x/push/objects", []byte("<!DOCTYPE html><html><script>window.__cf={t:'"+token+"'}</script></html>"))
	if strings.Contains(html.Error(), token) || strings.Contains(html.Error(), "<html") || !strings.Contains(html.Error(), "non-JSON response body omitted") {
		t.Fatalf("proxy page copied into error: %s", html.Error())
	}
	text := newHTTPError(503, http.MethodGet, "/repos/x", []byte("upstream reset token="+token))
	app := newHTTPError(503, http.MethodGet, "/repos/x", []byte(`{"error":{"code":"resource_overloaded","message":"busy `+token+`"}}`))
	for _, err := range []*HTTPError{text, app} {
		if strings.Contains(err.Error(), token) {
			t.Fatalf("opaque token copied into error: %s", err.Error())
		}
	}
	if app.Code != "resource_overloaded" || !strings.Contains(app.Error(), "busy") {
		t.Fatalf("application classification lost: %s", app.Error())
	}
}
