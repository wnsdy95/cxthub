package http

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestDocJobHTTPDurabilityAndAuthorization(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	id := app.NewIdentityService(auth.NewDevVerifier(), st)
	server := httptest.NewServer(NewServer(svc, id).Handler())
	defer server.Close()
	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, "GET", server.URL+"/api/v1/me", nil, &me); code != 200 {
		t.Fatal(code)
	}
	var repositoryRecord struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", server.URL+"/api/v1/repositories", map[string]any{"name": "DocumentJobs"}, &repositoryRecord); code != 200 {
		t.Fatal(code)
	}
	remote := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if code := doJSON(t, "POST", server.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote, "default_branch": "main"}, nil); code != 200 {
		t.Fatal(code)
	}
	base := server.URL + "/api/v1/repos/" + string(repo)
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "http durable upload"}}}}}
	cb, _ := domain.CanonicalBytes(cir)
	hash := domain.HashContent(cb)
	plan, _ := domain.PlanDocChunks(cb)
	var chunks []inbound.ChunkObject
	for h, b := range plan.Bodies {
		chunks = append(chunks, inbound.ChunkObject{Hash: h, Data: b})
	}
	if code := doJSON(t, "POST", base+"/push/chunks", chunksBody{Chunks: chunks}, nil); code != 200 {
		t.Fatal(code)
	}
	wire := inbound.ChunkedDoc{Hash: hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
	path := base + "/push/doc-jobs"
	if code := doJSONAs(t, "", "POST", path, wire, nil); code != 401 {
		t.Fatalf("anonymous %d", code)
	}
	if code := doJSON(t, "POST", path, nil, nil); code != 415 {
		t.Fatalf("CSRF %d", code)
	}
	if code := doJSON(t, "POST", path, map[string]string{"envelope": strings.Repeat("x", domain.MaxDocJobManifestBytes+2048)}, nil); code != 413 {
		t.Fatalf("oversized %d", code)
	}
	var job inbound.DocFinalizationStatus
	if code := doJSON(t, "POST", path, wire, &job); code != 202 || job.State != "waiting" {
		t.Fatalf("accept %d %+v", code, job)
	}
	status := path + "/" + job.ID
	if code := doJSONAs(t, "dev:outsider@t.io:Outsider", "GET", status, nil, nil); code != 403 && code != 404 {
		t.Fatalf("foreign read %d", code)
	}
	if err := svc.ProcessDocFinalizations(systemTestContext(), 1); err != nil {
		t.Fatal(err)
	}
	if code := doJSON(t, "GET", status, nil, &job); code != 200 || job.State != "completed" {
		t.Fatalf("completion %d %+v", code, job)
	}
	var neg inbound.PushNegotiateOutput
	if code := doJSON(t, "POST", base+"/push/negotiate", negotiateBody{DocHaves: []domain.ContentHash{hash}}, &neg); code != 200 || !neg.AsyncDocsSupported || len(neg.DocWants) != 0 {
		t.Fatalf("negotiation %d %+v", code, neg)
	}
}
