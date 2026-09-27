package serverruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
)

func TestPublicOrigin(t *testing.T) {
	for _, value := range []string{"https://cxthub.example", "http://localhost:5173", "http://127.0.0.1:5173", "http://[::1]:5173"} {
		if got, err := PublicOrigin("127.0.0.1:8908", value); err != nil || got != value {
			t.Fatalf("%q: %q %v", value, got, err)
		}
	}
	for _, value := range []string{"", "http://public.example", "https://example.com/path", "https://user:secret@example.com", "https://example.com/?", "https://example.com/#fragment", "http://localhost:5173"} {
		if _, err := PublicOrigin(":8908", value); err == nil {
			t.Fatalf("accepted external origin %q", value)
		}
	}
}

type unavailableStore struct{ store.Store }

func (unavailableStore) Ping(context.Context) error {
	return errors.New("synthetic internal connection details")
}

func TestReadinessFailsOnDatabaseFailure(t *testing.T) {
	r := &Runtime{Store: unavailableStore{}}
	w := httptest.NewRecorder()
	r.HealthHandler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 || w.Body.String() != "database unavailable\n" {
		t.Fatalf("readiness = %d %s", w.Code, w.Body.String())
	}
}

func TestIncompleteFirebaseConfigurationFailsBeforeStorage(t *testing.T) {
	t.Setenv("CXT_AUTH", "firebase")
	t.Setenv("CXT_FIREBASE_PROJECT", "")
	t.Setenv("CXT_PUBLIC_URL", "https://example.test")
	_, err := Open(context.Background(), "127.0.0.1:8908", t.TempDir(), false)
	if err == nil {
		t.Fatal("missing Firebase project fell back to dev auth")
	}
}
