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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	mcpserver "github.com/wnsdy95/cxthub/backend/internal/adapters/delivery/mcp"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Opt-in: writes large fixtures to a dedicated disposable database. Never use a
// production DSN. Two independent pools/handlers exercise replica hand-offs.
func TestPostgresMultiInstanceLoad(t *testing.T) {
	dsn := os.Getenv("CXT_LOAD_DSN")
	if dsn == "" {
		t.Skip("CXT_LOAD_DSN unset")
	}
	ctx := context.Background()
	stores := make([]*store.PostgresStore, 2)
	for i := range stores {
		s, err := store.NewPostgresStore(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
		defer s.Close()
	}
	if _, err := stores[0].ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	ids := make([]*app.IdentityService, 2)
	servers := make([]*httptest.Server, 2)
	makeServer := func(i int) *httptest.Server {
		s := stores[i]
		svc := app.NewService(s, s, nil, gitengine.NewEngine(s), s)
		ids[i] = app.NewIdentityService(auth.NewDevVerifier(), s)
		m, err := mcpserver.NewServer(svc, ids[i], s, "https://load.example.test")
		if err != nil {
			t.Fatal(err)
		}
		rest := NewServer(svc, ids[i]).Handler()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/mcp" {
				m.Handler().ServeHTTP(w, r)
			} else {
				rest.ServeHTTP(w, r)
			}
		}))
	}
	for i := range servers {
		servers[i] = makeServer(i)
	}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()
	email := domain.NewID("load-") + "@example.test"
	user, session, err := ids[0].Login(ctx, "dev:"+email, "load test")
	if err != nil {
		t.Fatal(err)
	}
	repositoryRecord, err := ids[0].CreateRepository(ctx, user, "LoadTest")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(repositoryRecord.ID)), RepositoryID: repositoryRecord.ID, RemoteURL: "https://load.example.test/" + user.Username + "/load/app", DefaultBranch: "main"}
	if _, err := stores[0].PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	pair, err := ids[0].IssueMCPTokenPair(ctx, user.ID, "load-client")
	if err != nil {
		t.Fatal(err)
	}

	// Start on A, restart A, approve on B, redeem on A, authorize on B.
	var pending struct {
		Code string `json:"code"`
		Poll string `json:"poll_token"`
	}
	if status := doJSONAs(t, "", "POST", servers[0].URL+"/api/v1/auth/device/start", nil, &pending); status != 200 {
		t.Fatal(status)
	}
	servers[0].Close()
	servers[0] = makeServer(0)
	if status := doJSONAs(t, session.Token, "POST", servers[1].URL+"/api/v1/auth/device/approve", map[string]string{"code": pending.Code}, nil); status != 200 {
		t.Fatal(status)
	}
	var issued struct {
		Token string `json:"token"`
	}
	if status := doJSONAs(t, "", "POST", servers[0].URL+"/api/v1/auth/device/poll", map[string]string{"code": pending.Code, "poll_token": pending.Poll}, &issued); status != 200 || issued.Token == "" {
		t.Fatal("pairing issuance failed", status)
	}
	if status := doJSONAs(t, issued.Token, "GET", servers[1].URL+"/api/v1/me", nil, nil); status != 200 {
		t.Fatal(status)
	}

	put := func(doc domain.SessionDoc, parent domain.ContentHash) domain.ContentHash {
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err := stores[0].PutDoc(ctx, repo.ID, doc); err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: repo.ID, CreatedAt: time.Now().UTC(), Message: "Load fixture", Branch: "main", Provider: domain.ProviderUnknown, Fidelity: domain.FidelityFull}
		if parent != "" {
			snap.Parents = []domain.ContentHash{parent}
		}
		if err := stores[0].PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		return doc.Hash
	}
	var parent domain.ContentHash
	for i := 0; i < 1000; i++ {
		parent = put(domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.CIREvent{{Seq: 1, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("small fixture %d", i)}}}}}}, parent)
	}
	doc := domain.SessionDoc{}
	for i := 0; i < 4096; i++ {
		doc.CIR.Events = append(doc.CIR.Events, domain.CIREvent{Seq: i + 1, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("loadneedle event %d ", i) + strings.Repeat("0123456789", 2560)}}})
	}
	hash := put(doc, parent)
	doc = domain.SessionDoc{}
	t.Log("fixture: 100 MiB message text, 4096 events, 1001 snapshots, 2 server pools, 16 readers")
	if err := stores[0].CompareAndSwapRef(ctx, repo.ID, domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: "main", Target: hash}, ""); err != nil {
		t.Fatal(err)
	}
	type result struct {
		elapsed time.Duration
		size    int
		kind    string
	}
	results := make(chan result, 320)
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				kind := "REST"
				method := "GET"
				path := "/api/v1/repos/" + string(repo.ID) + "/docs/" + string(hash) + "/events?offset=0&limit=10"
				token := session.Token
				var body []byte
				if (worker+j)%2 == 1 {
					kind = "MCP"
					method = "POST"
					path = "/mcp"
					token = pair.AccessToken
					body, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "context_fetch", "arguments": map[string]any{"repository": repo.ID, "ref": hash, "events": 10}}})
				}
				req, _ := http.NewRequest(method, servers[worker%2].URL+path, bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				start := time.Now()
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					continue
				}
				data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || len(data) >= 1<<20 || bytes.Contains(data, []byte(`"isError":true`)) {
					t.Errorf("%s status=%d bytes=%d err=%v response=%.300s", kind, resp.StatusCode, len(data), err, data)
					continue
				}
				if kind == "MCP" && !bytes.Contains(data, []byte("next_cursor")) {
					t.Error("large MCP archive lost continuation")
				}
				results <- result{time.Since(start), len(data), kind}
			}
		}(worker)
	}
	wg.Wait()
	close(results)
	grouped := map[string][]time.Duration{}
	sizes := map[string]int{}
	for r := range results {
		grouped[r.kind] = append(grouped[r.kind], r.elapsed)
		sizes[r.kind] = max(sizes[r.kind], r.size)
	}
	for kind, samples := range grouped {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		t.Logf("%s n=%d p50=%s p95=%s p99=%s max_bytes=%d", kind, len(samples), samples[len(samples)/2], samples[(len(samples)-1)*95/100], samples[(len(samples)-1)*99/100], sizes[kind])
	}
	if len(grouped["REST"])+len(grouped["MCP"]) != 320 {
		t.Fatal("requests failed")
	}
}
