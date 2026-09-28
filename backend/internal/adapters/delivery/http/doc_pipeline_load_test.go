//go:build postgres

package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// Opt-in destructive fixtures in an explicitly disposable load database.
// Uses real HTTP, durable jobs, separate pools and API restart; no mocks of the
// wire contract, canonical validation, document ownership or publication.
func TestPostgresDocumentPipeline(t *testing.T) {
	dsn := os.Getenv("CXT_LOAD_DSN")
	if dsn == "" {
		t.Skip("CXT_LOAD_DSN unset")
	}
	ctx := systemTestContext()
	var st *store.PostgresStore
	var svc *app.Service
	var ids *app.IdentityService
	var server *httptest.Server
	open := func() {
		var err error
		st, err = store.NewPostgresStore(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = st.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
			t.Fatal(err)
		}
		svc = app.NewService(st, st, nil, gitengine.NewEngine(st), st)
		ids = app.NewIdentityService(auth.NewDevVerifier(), st)
		server = httptest.NewServer(NewServer(svc, ids).Handler())
	}
	open()
	defer func() { server.Close(); st.Close() }()
	user, session, err := ids.Login(ctx, "dev:"+domain.NewID("pipeline-")+"@example.test", "pipeline")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := ids.CreateRepository(ctx, user, "Pipeline")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repository.ID)), RepositoryID: repository.ID, DefaultBranch: "main"}
	if _, err = st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/repos/" + string(repo.ID)
	client := &http.Client{Timeout: 30 * time.Second}
	doJSONAs := func(t *testing.T, token, method, url string, body, out any) int {
		t.Helper()
		var raw []byte
		if body != nil {
			var err error
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, url, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			diagnostic, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			t.Logf("%s: HTTP %d: %s", req.URL.Path, resp.StatusCode, diagnostic)
		} else if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode
	}
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}}
	for i := 0; i < 1024; i++ {
		cir.Events = append(cir.Events, domain.CIREvent{Seq: i, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("item %d ", i) + strings.Repeat("transfer", 4096)}}})
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("missing chunk plan")
	}
	docBytes := len(raw)
	raw = nil
	cir = domain.CIRDocument{}
	sendChunk := func(h domain.ContentHash) {
		body := map[string]any{"chunks": []inbound.ChunkObject{{Hash: h, Data: plan.Bodies[h]}}}
		if code := doJSONAs(t, session.Token, "POST", server.URL+path+"/push/chunks", body, nil); code != 200 {
			t.Fatalf("chunk upload status=%d", code)
		}
	}
	// First batch survives an API restart; all remaining uploads retain the
	// same content hashes and repository ownership.
	sendChunk(plan.Order[0])
	server.Close()
	st.Close()
	open()
	uploaded := map[domain.ContentHash]bool{plan.Order[0]: true}
	for _, h := range plan.Order[1:] {
		if !uploaded[h] {
			sendChunk(h)
			uploaded[h] = true
		}
	}
	wire := inbound.ChunkedDoc{Hash: hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
	var accepted inbound.DocFinalizationStatus
	start := time.Now()
	if code := doJSONAs(t, session.Token, "POST", server.URL+path+"/push/doc-jobs", wire, &accepted); code != 202 {
		t.Fatalf("accept status=%d", code)
	}
	acceptTime := time.Since(start)
	if accepted.State != "waiting" {
		t.Fatalf("acceptance pretended to complete %s", accepted.State)
	}
	// No worker has run; an early snapshot must be rejected, not made visible.
	snap := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo.ID, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC(), Message: "pipeline"}
	body := map[string]any{"snapshots": []domain.Snapshot{snap}}
	if code := doJSONAs(t, session.Token, "POST", server.URL+path+"/push/objects", body, nil); code != 422 {
		t.Fatalf("unfinished document must fail integrity validation: HTTP %d", code)
	}
	server.Close()
	st.Close()
	open()
	var replay inbound.DocFinalizationStatus
	if code := doJSONAs(t, session.Token, "POST", server.URL+path+"/push/doc-jobs", wire, &replay); code != 202 || replay.ID != accepted.ID {
		t.Fatal("durable submission lost on restart")
	}
	// Independent worker pool; status and metadata requests must keep making
	// progress while validation/index construction publishes the document.
	worker, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	work := app.NewService(worker, worker, nil, gitengine.NewEngine(worker), worker)
	done := make(chan error, 1)
	workStart := time.Now()
	go func() { done <- work.ProcessDocFinalizations(ctx, 1) }()
	var wg sync.WaitGroup
	latencies := make(chan time.Duration, 32)
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				start := time.Now()
				var status inbound.DocFinalizationStatus
				if code := doJSONAs(t, session.Token, "GET", server.URL+path+"/push/doc-jobs/"+accepted.ID, nil, &status); code != 200 {
					t.Errorf("status read HTTP %d", code)
					return
				}
				latencies <- time.Since(start)
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	close(latencies)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	workerTime := time.Since(workStart)
	maxStatus := time.Duration(0)
	for elapsed := range latencies {
		maxStatus = max(maxStatus, elapsed)
	}
	if maxStatus > time.Second {
		t.Fatalf("control request blocked for %s", maxStatus)
	}
	var complete inbound.DocFinalizationStatus
	if code := doJSONAs(t, session.Token, "GET", server.URL+path+"/push/doc-jobs/"+accepted.ID, nil, &complete); code != 200 || complete.State != "completed" {
		t.Fatalf("finalization: %+v", complete)
	}
	if code := doJSONAs(t, session.Token, "POST", server.URL+path+"/push/objects", body, nil); code != 200 {
		t.Fatalf("publication status=%d", code)
	}
	var pull inbound.PullSendOutput
	if code := doJSONAs(t, session.Token, "POST", server.URL+path+"/pull/objects", pullBody{DocManifestWants: []domain.ContentHash{hash}, ChunkFormatsSupported: []string{domain.ChunkFormatV2}}, &pull); code != 200 || len(pull.DocManifests) != 1 {
		t.Fatalf("production manifest HTTP=%d", code)
	}
	got, err := st.GetDoc(context.Background(), repo.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err = domain.ValidateSessionDocHash(got); err != nil {
		t.Fatal(err)
	}
	refs, err := st.ListRefs(ctx, repo.ID)
	if err != nil || len(refs) != 0 {
		t.Fatal("object transfer moved refs")
	}
	t.Logf("bytes=%d chunks=%d acceptance=%s worker=%s max_status=%s; restart/idempotence/hash verified", docBytes, len(plan.Order), acceptTime, workerTime, maxStatus)
}
