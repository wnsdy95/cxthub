package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type workerWebhookBackend struct {
	Backend
	got   context.Context
	calls int
}

func (b *workerWebhookBackend) DocumentIdentitiesSupported() []domain.DocumentIdentity {
	return []domain.DocumentIdentity{domain.DocumentIdentityLegacy}
}
func (b *workerWebhookBackend) RootPublicationEnabled() bool { return false }
func (b *workerWebhookBackend) PromoteMergedPR(ctx context.Context, _ string, _ domain.PullRequestMerge) (int, error) {
	b.got = ctx
	b.calls++
	return 0, nil
}
func TestWorkerVerifiedWebhookUsesActualBinaryDeclaration(t *testing.T) {
	t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", "synthetic-only")
	body := `{"number":1,"action":"closed","pull_request":{"merged":true,"head":{"repo":{"full_name":"example/repo"}}},"repository":{"full_name":"example/repo","clone_url":"https://github.com/example/repo"}}`
	for _, signed := range []bool{false, true} {
		backend := &workerWebhookBackend{}
		server := NewServer(backend, nil)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", strings.NewReader(body))
		req = req.WithContext(inbound.WithDocumentIdentities(req.Context(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1}))
		req = req.WithContext(outbound.WithDocumentIdentityCompatibility(req.Context(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1}, []domain.DocumentIdentity{domain.DocumentIdentityRootV1}))
		req.Header.Set(documentIdentitiesHeader, string(domain.DocumentIdentityRootV1))
		req.Header.Set("X-GitHub-Event", "pull_request")
		if signed {
			mac := hmac.New(sha256.New, []byte("synthetic-only"))
			mac.Write([]byte(body))
			req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		rec := httptest.NewRecorder()
		server.githubWebhook(rec, req)
		if !signed {
			if rec.Code != 401 || backend.calls != 0 {
				t.Fatal("unsigned dispatch", rec.Code, backend.calls)
			}
			continue
		}
		if rec.Code != 200 || backend.calls != 1 {
			t.Fatal(rec.Code, rec.Body.String(), backend.calls)
		}
		if _, system := inbound.RepositoryActor(backend.got); !system {
			t.Fatal("trusted event lost actor")
		}
		if err := outbound.CheckDocumentIdentityCompatibility(backend.got, domain.DocumentIdentityRootV1); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
			t.Fatal("outbound spoof survived", err)
		}
		for _, id := range inbound.DocumentIdentities(backend.got) {
			if id != domain.DocumentIdentityLegacy {
				t.Fatal("request forged binary support", id)
			}
		}
	}
}
