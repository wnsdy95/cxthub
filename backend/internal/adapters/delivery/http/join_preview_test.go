package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestJoinPreviewWireAndStaleConfirmation(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, nil, nil, st)
	identity := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, identity).Handler())
	defer ts.Close()
	var ws domain.Workspace
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]string{"name": "JoinPreview"}, &ws); code != 200 {
		t.Fatalf("workspace %d", code)
	}
	repo := domain.HashContent([]byte("wire-join"))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: ws.ID}); err != nil {
		t.Fatal(err)
	}
	ids := make([]domain.ContentHash, 4)
	for i, name := range []string{"parent", "head", "source", "tip"} {
		ids[i] = domain.HashContent([]byte(name))
	}
	for i, id := range ids {
		var parents []domain.ContentHash
		if i > 0 {
			parents = []domain.ContentHash{ids[0]}
		}
		if i == 3 {
			parents = []domain.ContentHash{ids[2]}
		}
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Parents: parents}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: ids[1]}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddGraftParents(ctx, repo, ids[1], []domain.ContentHash{ids[3]}); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/v1/repos/" + string(repo) + "/join"
	var preview inbound.JoinPreviewOutput
	if code := doJSON(t, "GET", base+"/preview?snapshot="+string(ids[2]), nil, &preview); code != 200 || preview.AllRevision == "" || preview.Descendants != 1 {
		t.Fatalf("preview code %d %+v", code, preview)
	}
	request := map[string]any{"branch": preview.Branch, "branch_id": preview.BranchID, "snapshot": preview.Snapshot, "expected_head": preview.ExpectedHead, "plan_revision": preview.AllRevision, "include_descendants": true}
	if code := doJSONAs(t, "dev:outsider@example.test:Outsider", "GET", base+"/preview?snapshot="+string(ids[2]), nil, nil); code != 403 {
		t.Fatalf("outsider preview %d", code)
	}
	if code := doJSON(t, "POST", base, map[string]any{"branch": "main", "snapshot": ids[2]}, nil); code != 409 {
		t.Fatalf("legacy unapproved command %d", code)
	}
	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "other", Target: ids[0]}, ""); err != nil {
		t.Fatal(err)
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	raw, _ := json.Marshal(request)
	req, _ := http.NewRequest("POST", base, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cxt-CSRF", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 409 || failure.Error.Code != "join_preview_changed" {
		t.Fatalf("stale %d %+v", resp.StatusCode, failure)
	}
	if code := doJSON(t, "GET", base+"/preview?snapshot="+string(ids[2]), nil, &preview); code != 200 {
		t.Fatal(code)
	}
	request["plan_revision"] = preview.AllRevision
	var result inbound.JoinOutput
	if code := doJSON(t, "POST", base, request, &result); code != 200 || result.Head != ids[3] {
		t.Fatalf("fresh confirmation %d %+v", code, result)
	}
}
