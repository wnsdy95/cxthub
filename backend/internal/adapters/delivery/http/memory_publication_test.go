package http

import (
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/url"
	"testing"
)

func TestMemoryPublicationAuthorizationAndRoundTrip(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me); code != 200 {
		t.Fatal(code)
	}
	var repositoryRecord struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Memory-publication"}, &repositoryRecord); code != 200 {
		t.Fatal(code)
	}
	remote := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote}, nil); code != 200 {
		t.Fatal(code)
	}
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo))
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: "archive"}}
	raw, _ := domain.CanonicalBytes(cir)
	id := domain.HashContent(raw)
	d := domain.MemoryDigest{SnapshotID: id, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Claims: []domain.MemoryClaim{{Kind: "decision", Text: "Preserve memory"}}}}}
	request := map[string]any{"objects": objectsBody{Snapshots: []domain.Snapshot{{ID: id, DocHash: id, RepoID: repo}}, Docs: []domain.SessionDoc{{Hash: id, CIR: cir}}}, "memory": d}
	for _, token := range []string{"", "dev:stranger@example.com:Stranger"} {
		code := doJSONAs(t, token, "POST", base+"/memory-publications", request, nil)
		if code != 401 && code != 403 && code != 404 {
			t.Fatalf("unauthorized publication: %d", code)
		}
	}
	if code := doJSON(t, "GET", base+"/snapshots/"+string(id), nil, nil); code != 404 {
		t.Fatalf("unauthorized write persisted: %d", code)
	}
	var out struct {
		MemoryHash domain.ContentHash `json:"memory_hash"`
	}
	if code := doJSON(t, "POST", base+"/memory-publications", request, &out); code != 200 {
		t.Fatal(code)
	}
	hash, _ := domain.MemoryDigestHash(d)
	if out.MemoryHash != hash {
		t.Fatalf("ack=%s want=%s", out.MemoryHash, hash)
	}
	var got domain.MemoryDigest
	if code := doJSON(t, "GET", base+"/memories/"+string(id), nil, &got); code != 200 {
		t.Fatal(code)
	}
	if gotHash, _ := domain.MemoryDigestHash(got); gotHash != hash {
		t.Fatal("typed memory changed")
	}
}
