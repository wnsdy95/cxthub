package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

type recordingGitHubConnections struct {
	GitHubConnections
	calls int
	fail  error
}

func (g *recordingGitHubConnections) Receive(context.Context, string, string, []byte) error {
	g.calls++
	return g.fail
}
func TestGitHubAppWebhookRequiresSignatureAndDurability(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	server := NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st))
	record := &recordingGitHubConnections{}
	server.SetGitHubConnections(record, "fixture-secret", "https://example.test/connect/github")
	h := server.Handler()
	send := func(body []byte, signed bool) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/github/webhook", bytes.NewReader(body))
		r.Header.Set("X-GitHub-Delivery", "delivery-id")
		r.Header.Set("X-GitHub-Event", "push")
		if signed {
			mac := hmac.New(sha256.New, []byte("fixture-secret"))
			mac.Write(body)
			r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := send([]byte(`{}`), false); code != 401 || record.calls != 0 {
		t.Fatalf("unsigned event: %d, %d calls", code, record.calls)
	}
	if code := send(make([]byte, 1<<20+1), true); code != 413 || record.calls != 0 {
		t.Fatalf("oversize event: %d", code)
	}
	record.fail = errors.New("storage unavailable")
	if code := send([]byte(`{}`), true); code < 500 {
		t.Fatalf("acknowledged failed persistence: %d", code)
	}
	record.fail = nil
	if code := send([]byte(`{}`), true); code != 200 {
		t.Fatalf("valid event: %d", code)
	}
}
