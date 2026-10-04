package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestHistoryTurnsHTTPBoundsAuthorizationAndRevocation(t *testing.T) {
	ctx := systemTestContext()
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	ids := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, ids).Handler())
	defer ts.Close()
	var me domain.User
	if doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me) != 200 {
		t.Fatal("login")
	}
	var record domain.Repository
	if doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "HistoryTurns"}, &record) != 200 {
		t.Fatal("repository")
	}
	remote := "http://cxthub.test/" + me.Username + "/" + record.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote}, nil) != 200 {
		t.Fatal("repo")
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "http-history"}, Events: []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: 0, Blocks: []domain.ContentBlock{{Type: "text", Text: "first"}}},
		{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: 1, Blocks: []domain.ContentBlock{{Type: "text", Text: "second"}}},
	}}}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	endpoint := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/docs/" + url.PathEscape(string(doc.Hash)) + "/turns"
	var page domain.AgentHistoryPage
	if code := doJSON(t, "GET", endpoint+"?limit=1", nil, &page); code != 200 || page.Version != domain.AgentHistoryPageVersion || page.OmittedTail != nil || page.NextBefore != 1 || len(page.Turns) != 1 {
		t.Fatal(code, page)
	}
	wire, _ := json.Marshal(page.Turns[0].Events)
	if page.Turns[0].Hash != domain.HashContent(wire) {
		t.Fatal("wire hash")
	}
	if code := doJSON(t, "GET", endpoint+"?incomplete_tail=omit&limit=1", nil, &page); code != 200 || page.Version != domain.AgentHistoryProjectionVersion || page.OmittedTail != nil || page.NextBefore != 1 || len(page.Turns) != 1 {
		t.Fatal("complete opt-in page", code, page)
	}
	for _, query := range []string{"before=-2", "before=x", "before=3", "limit=0", "limit=101", "max_bytes=0", "max_bytes=4194305", "covered_by=bad", "before=1&before=0", "max_bytes=", "incomplete_tail=invalid", "incomplete_tail=OMIT", "incomplete_tail=omit&incomplete_tail=omit", "incomplete_tail=&incomplete_tail=omit"} {
		if code := doJSON(t, "GET", endpoint+"?"+query, nil, nil); code != 422 {
			t.Fatal(query, code)
		}
	}
	tailDoc := doc
	tailDoc.CIR.Events = append(append([]domain.CIREvent(nil), doc.CIR.Events...),
		domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleUser, Seq: 2, Blocks: []domain.ContentBlock{{Type: "text", Text: "unfinished"}}},
		domain.CIREvent{Kind: domain.EventToolCall, Seq: 3, CallID: "pending", ToolName: "read"})
	tailRaw, _ := domain.CanonicalBytes(tailDoc.CIR)
	tailDoc.Hash = domain.HashContent(tailRaw)
	if _, err := st.PutDoc(ctx, repo, tailDoc); err != nil {
		t.Fatal(err)
	}
	tailEndpoint := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/docs/" + url.PathEscape(string(tailDoc.Hash)) + "/turns"
	if code := doJSON(t, "GET", tailEndpoint, nil, nil); code != 422 {
		t.Fatal("legacy request accepted unfinished tail", code)
	}
	page = domain.AgentHistoryPage{}
	if code := doJSON(t, "GET", tailEndpoint+"?incomplete_tail=omit&limit=1", nil, &page); code != 200 || page.Version != domain.AgentHistoryProjectionVersion || page.Before != 4 || page.Total != 4 || page.Covered || page.OmittedTail == nil || *page.OmittedTail != (domain.AgentHistoryTail{Start: 2, End: 4, Reason: "incomplete_tool_pair"}) || len(page.Turns) != 1 || page.Turns[0].End != 2 || page.NextBefore != 1 {
		t.Fatal("whole-tail opt-in page", code, page)
	}
	page = domain.AgentHistoryPage{}
	if code := doJSON(t, "GET", tailEndpoint+"?incomplete_tail=omit&max_bytes=1", nil, &page); code != 200 || page.OmittedTail == nil || len(page.Turns) != 0 || page.NextBefore != 2 {
		t.Fatal("gap-only opt-in page", code, page)
	}
	page = domain.AgentHistoryPage{}
	if code := doJSON(t, "GET", tailEndpoint+"?incomplete_tail=omit&before=0", nil, &page); code != 200 || page.Version != domain.AgentHistoryProjectionVersion || page.OmittedTail != nil || len(page.Turns) != 0 || page.NextBefore != -1 {
		t.Fatal("metadata opt-in page", code, page)
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	budgetReq, err := http.NewRequest(http.MethodGet, endpoint+"?max_bytes=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	budgetReq.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
	budgetResp, err := http.DefaultClient.Do(budgetReq)
	if err != nil {
		t.Fatal(err)
	}
	decodeErr := json.NewDecoder(budgetResp.Body).Decode(&failure)
	budgetResp.Body.Close()
	if budgetResp.StatusCode != 422 || decodeErr != nil || failure.Error.Code != "context_budget_exceeded" {
		t.Fatal("budget code", budgetResp.StatusCode, failure, decodeErr)
	}
	if code := doJSONAs(t, "", "GET", tailEndpoint+"?incomplete_tail=omit", nil, nil); code != 401 {
		t.Fatal("anonymous", code)
	}
	const outsider = "dev:history-viewer@example.test:Viewer"
	if code := doJSONAs(t, outsider, "GET", tailEndpoint+"?incomplete_tail=omit", nil, nil); code != 403 {
		t.Fatal("outsider", code)
	}
	viewer, err := ids.ResolveUser(ctx, outsider)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.AddMember(ctx, domain.Membership{RepositoryID: record.ID, UserID: viewer.ID, Role: domain.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if code := doJSONAs(t, outsider, "GET", endpoint+"?limit=1", nil, &page); code != 200 {
		t.Fatal("viewer", code)
	}
	if err = st.RemoveMember(ctx, record.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if code := doJSONAs(t, outsider, "GET", tailEndpoint+"?before=2&incomplete_tail=omit", nil, nil); code != 403 {
		t.Fatal("revoked membership returned cached page", code)
	}
	session, err := ids.CreateCLIToken(ctx, me.ID, "history test")
	if err != nil {
		t.Fatal(err)
	}
	if code := doJSONAs(t, session.Token, "GET", endpoint, nil, &page); code != 200 {
		t.Fatal("CLI token", code)
	}
	if err = ids.RevokeCLIToken(ctx, me.ID, domain.TokenHint(session.Token)); err != nil {
		t.Fatal(err)
	}
	if code := doJSONAs(t, session.Token, "GET", tailEndpoint+"?before=2&incomplete_tail=omit", nil, nil); code != 401 {
		t.Fatal("revoked token", code)
	}
	foreign := domain.HashContent([]byte("foreign repo"))
	doc.CIR.Envelope.SessionOriginID = "foreign"
	raw, _ = domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err = st.PutDoc(ctx, foreign, doc); err != nil {
		t.Fatal(err)
	}
	if code := doJSON(t, "GET", endpoint+"?covered_by="+url.QueryEscape(string(doc.Hash)), nil, nil); code != 404 {
		t.Fatal("foreign coverage source", code)
	}
}

func TestHistoryTurnsOpenAPIFields(t *testing.T) {
	for name, typ := range map[string]reflect.Type{"AgentHistoryPage": reflect.TypeOf(domain.AgentHistoryPage{}), "AgentHistoryTurn": reflect.TypeOf(domain.AgentHistoryTurn{}), "AgentHistoryTail": reflect.TypeOf(domain.AgentHistoryTail{})} {
		props := schemaProps(t, "../../../../../schemas/openapi.yaml", name)
		fields := jsonFields(typ)
		if len(props) != len(fields) {
			t.Fatal(name, "field count", props, fields)
		}
		for _, field := range fields {
			if !props[field] {
				t.Fatal(name, "missing field", field)
			}
		}
	}
}
