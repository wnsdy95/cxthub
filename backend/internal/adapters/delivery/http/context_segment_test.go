package http

import (
	"encoding/json"
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

func TestContextSegmentHTTPUsesApplicationContractAndAuthorization(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	ids := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, ids).Handler())
	defer ts.Close()
	var me struct{ Username string }
	if doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me) != 200 {
		t.Fatal("login")
	}
	var record struct{ Slug string }
	if doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Segments"}, &record) != 200 {
		t.Fatal("repository")
	}
	remote := "http://cxthub.test/" + me.Username + "/" + record.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote}, nil) != 200 {
		t.Fatal("repo")
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{SourceProvider: "codex", SessionOriginID: "segment-http"}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic source"}}}}}}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(systemTestContext(), repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(systemTestContext(), domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/context-query"
	if code := doJSONAs(t, "dev:outsider@example.test:Other", "GET", base+"?segment_limit=1", nil, nil); code != 403 {
		t.Fatalf("segment metadata exposed: %d", code)
	}
	var got domain.ContextQueryView
	if code := doJSON(t, "GET", base+"?segment_limit=1", nil, &got); code != 200 {
		t.Fatalf("query: %d", code)
	}
	want, err := svc.QueryContext(systemTestContext(), repo, domain.ContextSelection{SegmentLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Compare wire values, including zero timestamps and omitted optional fields.
	wire, _ := json.Marshal(want)
	var expected domain.ContextQueryView
	if err = json.Unmarshal(wire, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) || got.Segments.Entries[0].Kind != "full_source" {
		t.Fatal("HTTP diverged from application projection")
	}
	for _, query := range []string{"segment_limit=nope", "segment_limit=51", "segment_limit=1&segment_offset=-1", "segment_limit=1&segment_offset=1", "segment_offset=1"} {
		if code := doJSON(t, "GET", base+"?"+query, nil, nil); code != 422 {
			t.Fatalf("%s => %d", query, code)
		}
	}
	wrong := url.QueryEscape(string(domain.HashContent([]byte("outdated"))))
	if code := doJSON(t, "GET", base+"?segment_limit=1&segment_state_hash="+wrong, nil, nil); code != 409 {
		t.Fatalf("stale state: %d", code)
	}
}
