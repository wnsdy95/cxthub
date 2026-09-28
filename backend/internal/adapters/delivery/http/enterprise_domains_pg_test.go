//go:build postgres

package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type domainHTTPResolver struct{ challenge string }

func (r *domainHTTPResolver) LookupTXT(context.Context, string) ([]string, error) {
	return []string{r.challenge}, nil
}
func TestPGEnterpriseDomainHTTPVerification(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated test DSN required")
	}
	ctx := context.Background()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	resolver := &domainHTTPResolver{}
	ids := app.NewIdentityService(auth.NewDevVerifier(), st).WithDomainResolver(resolver)
	u, session, err := ids.Login(ctx, "dev:"+domain.NewID("domain-http")+"@example.test", "Domain test")
	if err != nil {
		t.Fatal(err)
	}
	e, err := ids.CreateEnterprise(ctx, u, "Domains", "domains-"+domain.NewID("")[:12])
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(app.NewService(st, st, nil, gitengine.NewEngine(st), st), ids).Handler()
	post := func(suffix string, body any, contentType string) (int, app.EnterpriseDomainView) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/api/v1/enterprises/"+e.ID+"/domains"+suffix, strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+session.Token)
		req.Header.Set("Content-Type", contentType)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		var d app.EnterpriseDomainView
		if res.Code == 200 {
			if err := json.Unmarshal(res.Body.Bytes(), &d); err != nil {
				t.Fatal(err)
			}
		}
		return res.Code, d
	}
	command := map[string]string{"domain": domain.NewID("") + ".example.test", "revision": ""}
	if status, _ := post("", command, "text/plain"); status < 400 {
		t.Fatal("non-JSON accepted", status)
	}
	status, d := post("", command, "application/json")
	if status != 200 {
		t.Fatal("request", status)
	}
	resolver.challenge = d.Challenge
	command["revision"] = d.Revision
	status, v := post("/verify", command, "application/json")
	if status != 200 || v.State != "verified" {
		t.Fatal("verify", status, v.State)
	}
	if status, _ := post("/release", command, "application/json"); status != 409 {
		t.Fatal("stale release", status)
	}
	command["revision"] = v.Revision
	if status, _ := post("/release", command, "application/json"); status != 200 {
		t.Fatal("release", status)
	}
}
