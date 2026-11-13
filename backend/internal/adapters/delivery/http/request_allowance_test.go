package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// failingAllowanceStore is a shared runtime store whose allowance always fails.
type failingAllowanceStore struct {
	outbound.RuntimeStore
	err   error
	calls int
}

func (store *failingAllowanceStore) AllowRequest(context.Context, string, int, time.Duration, time.Time) (bool, error) {
	store.calls++
	return false, store.err
}

func TestLocalAllowanceFollowsSharedGCRA(t *testing.T) {
	var allowance localAllowance
	start := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if !allowance.allow("k", 3, time.Minute, start) {
			t.Fatalf("burst request %d refused", i)
		}
	}
	for i := 0; i < 10; i++ {
		if allowance.allow("k", 3, time.Minute, start.Add(19*time.Second)) {
			t.Fatalf("request %d over the burst admitted", i)
		}
	}
	// Refused requests did not move the schedule: one step (20s) refills one request.
	if !allowance.allow("k", 3, time.Minute, start.Add(20*time.Second)) {
		t.Fatal("refilled request refused")
	}
	if allowance.allow("k", 3, time.Minute, start.Add(20*time.Second)) {
		t.Fatal("second request after one refill admitted")
	}
	if !allowance.allow("other", 3, time.Minute, start) {
		t.Fatal("independent key refused")
	}
	later := start.Add(2 * time.Minute)
	for i := 0; i < 3; i++ {
		if !allowance.allow("k", 3, time.Minute, later) {
			t.Fatalf("burst after expiry request %d refused", i)
		}
	}
	if allowance.allow("k", 0, time.Minute, later) || allowance.allow("k", 1, 0, later) {
		t.Fatal("invalid allowance admitted")
	}
}

func TestLocalAllowanceBoundsKeys(t *testing.T) {
	var allowance localAllowance
	start := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	for i := 0; i < localAllowanceEntries; i++ {
		if !allowance.allow(fmt.Sprint(i), 1, time.Minute, start) {
			t.Fatalf("key %d refused below the bound", i)
		}
	}
	if allowance.allow("new", 1, time.Minute, start.Add(time.Second)) {
		t.Fatal("new key admitted into a full table")
	}
	if !allowance.allow("0", 1, time.Minute, start.Add(2*time.Minute)) {
		t.Fatal("existing expired key refused")
	}
	if !allowance.allow("new", 1, time.Minute, start.Add(2*time.Minute)) {
		t.Fatal("new key refused after expired keys could be dropped")
	}
	if len(allowance.entries) > localAllowanceEntries {
		t.Fatalf("table grew to %d entries", len(allowance.entries))
	}
}

func TestRateAllowanceUsesLocalDecisionWhenSharedStoreFails(t *testing.T) {
	for _, failure := range []error{domain.ErrResourceOverloaded, context.DeadlineExceeded, errors.New("connection reset")} {
		shared := &failingAllowanceStore{err: failure}
		s := &Server{runtime: shared}
		h := s.rateLimit(2, time.Minute, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		for i, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests, http.StatusTooManyRequests} {
			res := httptest.NewRecorder()
			h(res, httptest.NewRequest(http.MethodPost, "/limit", nil))
			if res.Code != want {
				t.Fatalf("%v: request %d status=%d want=%d", failure, i, res.Code, want)
			}
		}
		// Locally refused requests never reach the shared store.
		if shared.calls != 2 {
			t.Fatalf("%v: shared store calls=%d want 2", failure, shared.calls)
		}
	}
	shared := &failingAllowanceStore{err: domain.ErrValidation}
	res := httptest.NewRecorder()
	(&Server{runtime: shared}).rateLimit(2, time.Minute, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })(res, httptest.NewRequest(http.MethodPost, "/limit", nil))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid shared allowance status=%d want 503", res.Code)
	}
}

func TestRateLimitKeysSignedInUsersSeparately(t *testing.T) {
	s := &Server{runtime: store.NewFSStore(t.TempDir())}
	h := s.rateLimit(1, time.Minute, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	request := func(userID string) int {
		req := httptest.NewRequest(http.MethodPost, "/limit", nil)
		req.RemoteAddr = "10.0.0.5:4000" // every caller arrives through the same proxy
		if userID != "" {
			req = req.WithContext(context.WithValue(req.Context(), userCtxKey, domain.User{ID: userID}))
		}
		res := httptest.NewRecorder()
		h(res, req)
		return res.Code
	}
	for i, step := range []struct {
		user string
		want int
	}{{"alice", 204}, {"bob", 204}, {"alice", 429}, {"", 204}, {"", 429}, {"bob", 429}} {
		if got := request(step.user); got != step.want {
			t.Fatalf("step %d user %q status=%d want=%d", i, step.user, got, step.want)
		}
	}
}

func TestSessionLoginAllowanceIsPerVerifiedAccount(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, nil, gitengine.NewEngine(st), st)
	ts := httptest.NewServer(NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st)).Handler())
	defer ts.Close()
	login := func(token string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/session", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	// Everyone shares the test client's address, as they share the production proxy's.
	for i := 0; i < loginsPerAccount+5; i++ {
		if res := login(fmt.Sprintf("dev:user%d@example.test", i)); res.StatusCode != http.StatusOK {
			t.Fatalf("login of distinct account %d status=%d", i, res.StatusCode)
		}
	}
	for i := 0; i < 5; i++ {
		if res := login("not-a-token"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("invalid token %d status=%d want 401", i, res.StatusCode)
		}
	}
	for i := 1; i < loginsPerAccount; i++ {
		if res := login("dev:user0@example.test"); res.StatusCode != http.StatusOK {
			t.Fatalf("repeat login %d status=%d", i, res.StatusCode)
		}
	}
	res := login("dev:user0@example.test")
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("login over the account allowance status=%d retry-after=%q", res.StatusCode, res.Header.Get("Retry-After"))
	}
	for _, cookie := range res.Cookies() {
		if cookie.Name == sessionCookie && cookie.Value != "" {
			t.Fatal("refused login set a session cookie")
		}
	}
	if res := login("dev:user1@example.test"); res.StatusCode != http.StatusOK {
		t.Fatalf("other account after one account was limited status=%d", res.StatusCode)
	}
}

func TestSessionLoginFailsClosedWithoutSharedAllowance(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, nil, gitengine.NewEngine(st), st)
	s := NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st))
	s.runtime = nil
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session", nil)
	req.Header.Set("Authorization", "Bearer dev:closed@example.test")
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("login without shared allowance status=%d want 503", res.Code)
	}
	if _, err := st.GetUser(context.Background(), "dev:closed@example.test"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("refused login wrote the user: %v", err)
	}
}
