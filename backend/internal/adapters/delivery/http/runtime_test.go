package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeviceFlowCrossesInstancesAndRestart(t *testing.T) {
	dir := t.TempDir()
	server := func() *httptest.Server {
		st := store.NewFSStore(dir)
		svc := app.NewService(st, st, nil, gitengine.NewEngine(st), st)
		return httptest.NewServer(NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st)).Handler())
	}
	a := server()
	b := server()
	defer b.Close()
	var start struct {
		Code string `json:"code"`
		Poll string `json:"poll_token"`
	}
	if code := doJSONAs(t, "", "POST", a.URL+"/api/v1/auth/device/start", map[string]string{"label": "cross-instance CLI"}, &start); code != 200 {
		t.Fatal(code)
	}
	a.Close()
	a = server()
	defer a.Close()
	if code := doJSON(t, "POST", b.URL+"/api/v1/auth/device/approve", map[string]string{"code": start.Code}, nil); code != 200 {
		t.Fatal(code)
	}
	if code := doJSONAs(t, "", "POST", a.URL+"/api/v1/auth/device/poll", map[string]string{"code": start.Code, "poll_token": "wrong"}, nil); code != 404 {
		t.Fatal(code)
	}
	var token struct {
		Status string `json:"status"`
		Token  string `json:"token"`
	}
	if code := doJSONAs(t, "", "POST", a.URL+"/api/v1/auth/device/poll", map[string]string{"code": start.Code, "poll_token": start.Poll}, &token); code != 200 || token.Status != "approved" || token.Token == "" {
		t.Fatalf("poll status=%d result status=%s", code, token.Status)
	}
	if code := doJSONAs(t, "", "POST", b.URL+"/api/v1/auth/device/poll", map[string]string{"code": start.Code, "poll_token": start.Poll}, nil); code != 404 {
		t.Fatalf("replay=%d", code)
	}
	if code := doJSONAs(t, token.Token, "GET", b.URL+"/api/v1/me", nil, nil); code != 200 {
		t.Fatalf("token rejected by peer: %d", code)
	}
}
func TestRateAllowanceSharedAndUnavailableFailsClosed(t *testing.T) {
	dir := t.TempDir()
	a := &Server{runtime: store.NewFSStore(dir)}
	b := &Server{runtime: store.NewFSStore(dir)}
	next := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
	for i, h := range []http.HandlerFunc{a.rateLimit(1, time.Minute, next), b.rateLimit(1, time.Minute, next), (&Server{}).rateLimit(1, time.Minute, next)} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest("POST", "/limit", nil))
		want := []int{204, 429, 503}[i]
		if w.Code != want {
			t.Fatalf("instance %d status=%d want=%d", i, w.Code, want)
		}
	}

}
