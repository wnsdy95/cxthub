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
	"reflect"
	"sort"
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
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// Observes the real publication boundary without delaying it, changing leases,
// bypassing the worker or substituting persistence. This is not lock-hold time.
type publicationObservedStore struct {
	*store.PostgresStore
	publicationStart, publicationEnd time.Time
}

type publicationObserved struct {
	outbound.PreparedDocPublication
	owner *publicationObservedStore
}

func (s *publicationObservedStore) PrepareDocJob(ctx context.Context, doc domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
	p, err := s.PostgresStore.PrepareDocJob(ctx, doc)
	return publicationObserved{p, s}, err
}

func (p publicationObserved) Complete(ctx context.Context, j domain.DocFinalizationJob, now time.Time) error {
	p.owner.publicationStart = time.Now()
	err := p.PreparedDocPublication.Complete(ctx, j, now)
	p.owner.publicationEnd = time.Now()
	return err
}

// Opt-in synthetic load, never a development/production DSN. Unlike the older
// status-only pipeline test, this overlaps real same-repository ref writes and
// page reads with a 32 MiB finalization. Timings are observations, not an SLA.
func TestPostgresDocPublicationContention(t *testing.T) {
	dsn := os.Getenv("CXT_LOAD_DSN")
	if dsn == "" {
		t.Skip("CXT_LOAD_DSN unset; requires an empty disposable database")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 2*time.Minute)
	defer cancel()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	svc := app.NewService(st, st, nil, gitengine.NewEngine(st), st)
	ids := app.NewIdentityService(auth.NewDevVerifier(), st)
	server := httptest.NewServer(NewServer(svc, ids).Handler())
	defer server.Close()
	user, session, err := ids.Login(ctx, "dev:"+domain.NewID("contention-")+"@example.test", "contention")
	if err != nil {
		t.Fatal(err)
	}
	record, err := ids.CreateRepository(ctx, user, "Contention")
	if err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: domain.HashContent([]byte(record.ID)), RepositoryID: record.ID, DefaultBranch: "main"}
	if _, err = st.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	base := server.URL + "/api/v1/repos/" + string(repo.ID)
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	// Return errors rather than Fatal from request goroutines. Read every body,
	// and reject oversized/truncated/non-JSON successful responses as failures.
	requestBounded := func(method, path string, body, out any, status, maxBytes int) (int, error) {
		var raw []byte
		if body != nil {
			var e error
			raw, e = json.Marshal(body)
			if e != nil {
				return 0, e
			}
		}
		req, e := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(raw))
		if e != nil {
			return 0, e
		}
		req.Header.Set("Authorization", "Bearer "+session.Token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, e := client.Do(req)
		if e != nil {
			return 0, e
		}
		defer resp.Body.Close()
		data, e := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
		if e != nil {
			return 0, e
		}
		if resp.StatusCode != status || len(data) > maxBytes || !json.Valid(data) {
			return len(data), fmt.Errorf("%s %s: status=%d bytes=%d expected=%d", method, path, resp.StatusCode, len(data), status)
		}
		if out != nil {
			e = json.Unmarshal(data, out)
		}
		return len(data), e
	}
	request := func(method, path string, body, out any, status int) (int, error) {
		return requestBounded(method, path, body, out, status, 1<<20)
	}
	check := func(method, path string, body, out any, status int) {
		t.Helper()
		if _, e := request(method, path, body, out, status); e != nil {
			t.Fatal(e)
		}
	}
	seedEvent := domain.CIREvent{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "\uae30\uc874 \ubb38\uc11c: const preserved = true;"}}}
	seed := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}, Events: []domain.CIREvent{seedEvent}}}
	raw, err := domain.CanonicalBytes(seed.CIR)
	if err != nil {
		t.Fatal(err)
	}
	seed.Hash = domain.HashContent(raw)
	seedSnap := domain.Snapshot{ID: seed.Hash, DocHash: seed.Hash, RepoID: repo.ID, CreatedAt: time.Now().UTC(), Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	check("POST", "/push/objects", objectsBody{Docs: []domain.SessionDoc{seed}, Snapshots: []domain.Snapshot{seedSnap}}, nil, 200)
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: "synthetic-contention"}}
	// Deterministic varied event bodies exercise unique search/index records;
	// this is still a compressible synthetic corpus, not production entropy.
	for i := 0; i < 1024; i++ {
		var b strings.Builder
		for j := 0; b.Len() < 32<<10; j++ {
			fmt.Fprintf(&b, "\uac80\uc99d %04d %04d: const value_%d = %d; // preserve memory\n", i, j, j, (i+1)*(j+17))
		}
		cir.Events = append(cir.Events, domain.CIREvent{Seq: i, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: b.String()}}})
	}
	raw, err = domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash, canonicalBytes := domain.HashContent(raw), len(raw)
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		t.Fatal("chunk plan required")
	}
	var uploadJSONBytes int
	for _, h := range plan.Order {
		body := chunksBody{Chunks: []inbound.ChunkObject{{Hash: h, Data: plan.Bodies[h]}}}
		wire, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		uploadJSONBytes += len(wire)
		check("POST", "/push/chunks", body, nil, 200)
	}
	wire := inbound.ChunkedDoc{Hash: hash, Format: plan.Manifest.Format, Envelope: plan.Manifest.Envelope, Chunks: plan.Manifest.Chunks}
	var accepted inbound.DocFinalizationStatus
	check("POST", "/push/doc-jobs", wire, &accepted, 202)
	if accepted.State != "waiting" || accepted.DocHash != hash {
		t.Fatal("invalid acceptance", accepted)
	}
	largeSnap := domain.Snapshot{ID: hash, DocHash: hash, RepoID: repo.ID, Parents: []domain.ContentHash{seed.Hash}, CreatedAt: time.Now().UTC(), Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	check("POST", "/push/objects", objectsBody{Snapshots: []domain.Snapshot{largeSnap}}, nil, 422)
	check("GET", "/snapshots/"+string(hash), nil, nil, 404)
	workerStore, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer workerStore.Close()
	observed := &publicationObservedStore{PostgresStore: workerStore}
	worker := app.NewService(observed, observed, nil, gitengine.NewEngine(observed), observed)
	type sample struct {
		kind       string
		start, end time.Time
		bytes      int
		err        error
	}
	var mu sync.Mutex
	var samples []sample
	appendSample := func(s sample) { mu.Lock(); samples = append(samples, s); mu.Unlock() }
	start, finished := make(chan struct{}), make(chan struct{})
	var ready, requests sync.WaitGroup
	ready.Add(5)
	requests.Add(5)
	for actor := 0; actor < 5; actor++ {
		go func(actor int) {
			defer requests.Done()
			ready.Done()
			<-start
			for n := 0; n < 16384; n++ {
				if n >= 8 {
					select {
					case <-finished:
						return
					default:
					}
				}
				s := sample{start: time.Now()}
				switch {
				case actor == 4:
					s.kind = "ref_write"
					var out inbound.UpdateRefOutput
					s.bytes, s.err = request("PUT", fmt.Sprintf("/refs/tag/contention-%d", n), putRefBody{Target: seed.Hash}, &out, 200)
					if s.err == nil && (out.Ref.Name != fmt.Sprintf("contention-%d", n) || out.Ref.Kind != domain.RefTag || out.Ref.Target != seed.Hash) {
						s.err = fmt.Errorf("wrong ref write response")
					}
				case actor%2 == 0:
					s.kind = "page_read"
					var page domain.DocEventPage
					s.bytes, s.err = request("GET", "/docs/"+string(seed.Hash)+"/events?offset=0&limit=1", nil, &page, 200)
					if s.err == nil && (page.Hash != seed.Hash || page.Total != 1 || !reflect.DeepEqual(page.Events, seed.CIR.Events)) {
						s.err = fmt.Errorf("seed page changed")
					}
				default:
					s.kind = "status_read"
					var status inbound.DocFinalizationStatus
					s.bytes, s.err = request("GET", "/push/doc-jobs/"+accepted.ID, nil, &status, 200)
					if s.err == nil && (status.ID != accepted.ID || status.DocHash != hash || (status.State != "waiting" && status.State != "running" && status.State != "completed")) {
						s.err = fmt.Errorf("invalid status: %s", status.State)
					}
				}
				s.end = time.Now()
				appendSample(s)
				if s.err != nil {
					return
				}
				time.Sleep(10 * time.Millisecond) // bounded offered load, not a worker gate
			}
			appendSample(sample{kind: "limit", err: fmt.Errorf("request limit exhausted before worker finished")})
		}(actor)
	}
	ready.Wait()
	workerStart := time.Now()
	close(start)
	workErr := worker.ProcessDocFinalizations(ctx, 1)
	workerEnd := time.Now()
	close(finished)
	requests.Wait()
	if workErr != nil {
		t.Fatal(workErr)
	}
	if observed.publicationStart.IsZero() {
		t.Fatal("worker did not process the requested job")
	}
	var completed inbound.DocFinalizationStatus
	check("GET", "/push/doc-jobs/"+accepted.ID, nil, &completed, 200)
	if completed.State != "completed" {
		t.Fatal("job not completed", completed)
	}
	var committed inbound.CommitOutput
	check("POST", "/push/objects", objectsBody{Snapshots: []domain.Snapshot{largeSnap}}, &committed, 200)
	if committed.StoredSnapshots != 1 {
		t.Fatal("snapshot was not published", committed)
	}
	var storedSnap domain.Snapshot
	check("GET", "/snapshots/"+string(hash), nil, &storedSnap, 200)
	if storedSnap.ID != hash || storedSnap.DocHash != hash || !reflect.DeepEqual(storedSnap.Parents, largeSnap.Parents) {
		t.Fatal("snapshot changed")
	}
	var replay inbound.DocFinalizationStatus
	check("POST", "/push/doc-jobs", wire, &replay, 202)
	if replay.ID != accepted.ID || replay.State != "completed" {
		t.Fatal("replay changed completed receipt", replay)
	}
	var got domain.SessionDoc
	downloadJSONBytes, err := requestBounded("GET", "/docs/"+string(hash), nil, &got, 200, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = domain.ValidateSessionDocHash(got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.CIR, cir) {
		t.Fatal("large document changed")
	}
	var tail domain.DocEventPage
	check("GET", "/docs/"+string(hash)+"/events?offset=1023&limit=1", nil, &tail, 200)
	if tail.Total != 1024 || tail.Hash != hash || !reflect.DeepEqual(tail.Events, cir.Events[1023:]) {
		t.Fatal("large tail page changed")
	}
	grouped := map[string][]time.Duration{}
	overlaps, publications, totals := map[string]int{}, map[string]int{}, map[string]int{}
	for _, s := range samples {
		if s.err != nil {
			t.Errorf("%s: %v", s.kind, s.err)
			continue
		}
		grouped[s.kind] = append(grouped[s.kind], s.end.Sub(s.start))
		totals[s.kind] += s.bytes
		if s.start.Before(workerEnd) && s.end.After(workerStart) {
			overlaps[s.kind]++
		}
		if s.start.Before(observed.publicationEnd) && s.end.After(observed.publicationStart) {
			publications[s.kind]++
		}
	}
	for _, kind := range []string{"page_read", "status_read", "ref_write"} {
		durations := grouped[kind]
		if len(durations) < 8 || overlaps[kind] == 0 || publications[kind] == 0 {
			t.Fatalf("insufficient %s samples/worker/publication overlap: %d/%d/%d", kind, len(durations), overlaps[kind], publications[kind])
		}
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		t.Logf("contention kind=%s n=%d worker_overlap=%d publication_overlap=%d p50=%s p95=%s max=%s response_bytes=%d", kind, len(durations), overlaps[kind], publications[kind], durations[len(durations)/2], durations[(len(durations)-1)*95/100], durations[len(durations)-1], totals[kind])
	}
	var refs []domain.Ref
	if _, err := requestBounded("GET", "/refs", nil, &refs, 200, 8<<20); err != nil {
		t.Fatal(err)
	}
	if len(refs) != len(grouped["ref_write"]) {
		t.Fatalf("ref writes lost or unexpected ref: %d/%d", len(refs), len(grouped["ref_write"]))
	}
	expectedNames := map[string]bool{}
	for n := range len(grouped["ref_write"]) {
		expectedNames[fmt.Sprintf("contention-%d", n)] = true
	}
	for _, ref := range refs {
		if ref.Kind != domain.RefTag || ref.Target != seed.Hash || !expectedNames[ref.Name] {
			t.Fatal("unexpected ref mutation", ref)
		}
		delete(expectedNames, ref.Name)
	}
	if len(expectedNames) != 0 {
		t.Fatal("missing written refs")
	}
	t.Logf("contention fixture canonical_bytes=%d upload_json_bytes=%d download_json_bytes=%d chunks=%d events=1024 worker=%s publication_boundary=%s; hash/page/receipt/snapshot/ref-count verified", canonicalBytes, uploadJSONBytes, downloadJSONBytes, len(plan.Order), workerEnd.Sub(workerStart), observed.publicationEnd.Sub(observed.publicationStart))
}
