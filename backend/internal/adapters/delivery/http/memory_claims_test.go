package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestTypedMemoryAttachmentVersionGateAndAuthorization(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	ts := httptest.NewServer(NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st)).Handler())
	defer ts.Close()
	var me struct {
		Username string `json:"username"`
	}
	var ws struct {
		Slug string `json:"slug"`
	}
	if status := doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me); status != 200 {
		t.Fatal(status)
	}
	if status := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]string{"name": "Typed-memory"}, &ws); status != 200 {
		t.Fatal(status)
	}
	remote := "http://cxthub.test/" + me.Username + "/" + ws.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if status := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote, "default_branch": "main"}, nil); status != 200 {
		t.Fatal(status)
	}
	id := domain.HashContent([]byte("typed-http-snapshot"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id}); err != nil {
		t.Fatal(err)
	}
	d := domain.MemoryDigest{SnapshotID: id, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Claims: []domain.MemoryClaim{{Kind: "decision", Text: "Keep accepted history."}}}}}
	prefix := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo))
	suffix := "/" + url.PathEscape(string(id))
	for _, endpoint := range []string{"memories", "memory-attachments"} {
		if status := doJSON(t, "PUT", prefix+"/"+endpoint+suffix, d, nil); status != 422 {
			t.Fatalf("%s status=%d", endpoint, status)
		}
	}
	if status := doJSONAs(t, "", "PUT", prefix+"/typed-memory-attachments"+suffix, d, nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", status)
	}
	snap, err := st.GetSnapshot(ctx, repo, id)
	if err != nil || snap.MemoryHash != "" {
		t.Fatalf("rejected request mutated attachment: %+v %v", snap, err)
	}
	var result struct {
		Hash domain.ContentHash `json:"memory_hash"`
	}
	if status := doJSON(t, "PUT", prefix+"/typed-memory-attachments"+suffix, d, &result); status != 200 {
		t.Fatal(status)
	}
	want, _ := domain.MemoryDigestHash(d)
	if result.Hash != want {
		t.Fatalf("hash=%s want=%s", result.Hash, want)
	}
	var got domain.MemoryDigest
	if status := doJSON(t, "GET", prefix+"/memory-objects/"+url.PathEscape(string(want)), nil, &got); status != 200 || got.ClaimsVersion != 1 || !got.HasMemoryClaims() {
		t.Fatalf("stored claims=%+v status=%d", got, status)
	}
	old := domain.MemoryDigest{SnapshotID: id, PreviousMemoryHash: want, Summary: "old client"}
	if status := doJSON(t, "PUT", prefix+"/memory-attachments"+suffix, old, nil); status != 422 {
		t.Fatalf("old client downgrade status=%d", status)
	}
	snap, _ = st.GetSnapshot(ctx, repo, id)
	if snap.MemoryHash != want {
		t.Fatal("old client moved pointer")
	}
}
