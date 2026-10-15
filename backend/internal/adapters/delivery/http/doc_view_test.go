package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocViewHTTPAuthorizationAndLazyContract(t *testing.T) {
	ctx := systemTestContext()
	storage := store.NewFSStore(t.TempDir())
	identity := app.NewIdentityService(auth.NewDevVerifier(), storage)
	const token = "dev:doc-view@example.test:Viewer"
	owner, err := identity.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	record, err := identity.CreateRepository(ctx, owner, "document-view")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(record.ID))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID}); err != nil {
		t.Fatal(err)
	}
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "question"}}},
		{Kind: domain.EventToolResult, Seq: 1, Output: strings.Repeat("private tool output ", 1000)},
		{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: "answer"}}},
	}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	if _, err := storage.PutDoc(ctx, repo, domain.SessionDoc{Hash: hash, CIR: cir}); err != nil {
		t.Fatal(err)
	}
	service := app.NewService(storage, storage, nil, nil, storage)
	server := httptest.NewServer(NewServer(service, identity).Handler())
	defer server.Close()
	base := server.URL + "/api/v1/repos/" + string(repo) + "/docs/" + string(hash)
	var page struct {
		Events []struct {
			Index    int  `json:"event_index"`
			Deferred bool `json:"deferred"`
			Output   any  `json:"output"`
		}
		Total, Next int
	}
	if code := doJSONAs(t, token, "GET", base+"/events?view=chat&limit=2", nil, &page); code != 200 || len(page.Events) != 3 || page.Next != -1 || page.Total != 3 || !page.Events[1].Deferred || page.Events[1].Index != 1 || page.Events[1].Output != nil {
		t.Fatalf("view status=%d page=%+v", code, page)
	}
	var detail domain.DocEventPage
	if code := doJSONAs(t, token, "GET", base+"/events?view=raw&offset=1&limit=1", nil, &detail); code != 200 || len(detail.Events) != 1 || detail.Events[0].Output != cir.Events[1].Output {
		t.Fatalf("detail status=%d", code)
	}
	var fragment domain.DocViewFragment
	if code := doJSONAs(t, token, "GET", base+"/event-fragments?index=1&budget=64", nil, &fragment); code != 200 || fragment.Index != 1 || fragment.Next != 64 || len(fragment.JSON) != 64 {
		t.Fatalf("fragment status=%d value=%+v", code, fragment)
	}
	for _, suffix := range []string{"/events?view=chat", "/event-fragments?index=1"} {
		for _, caller := range []struct {
			token  string
			status int
		}{{"", 401}, {"dev:outsider@example.test:Outsider", 403}} {
			if code := doJSONAs(t, caller.token, http.MethodGet, base+suffix, nil, nil); code != caller.status {
				t.Fatalf("authorization %s status=%d want=%d", suffix, code, caller.status)
			}
		}
	}
	for _, suffix := range []string{
		"/events?view=unknown", "/events?view=chat&view=raw", "/events?offset=0&offset=1", "/events?view=chat&offset=-2", "/events?view=chat&base=invalid", "/events?view=%ZZ",
		"/event-fragments", "/event-fragments?index=-1", "/event-fragments?index=3", "/event-fragments?index=1&index=2", "/event-fragments?index=1&offset=bad", "/event-fragments?index=1&budget=65537",
	} {
		if code := doJSONAs(t, token, http.MethodGet, base+suffix, nil, nil); code != 422 {
			t.Fatalf("validation %s status=%d", suffix, code)
		}
	}
}
