//go:build postgres

package serverruntime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestFullExecutionProcessesPGMixedProgress(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	fixture := newExecutionProcessFixture(test, ctx)
	fixture.activateFullProfile(test)
	api := fixture.start(test, ctx, outbound.ProcessAPI)
	api.expect(test, ctx, "opened", "")
	mcp := fixture.start(test, ctx, outbound.ProcessMCP)
	mcp.expect(test, ctx, "opened", "")
	worker := fixture.start(test, ctx, outbound.ProcessWorker)
	worker.expect(test, ctx, "opened", "")
	children := []*executionProcessChild{api, mcp, worker}
	for _, child := range children {
		child.waitReady(test, ctx, true)
	}
	fixture.assertSessions(test, ctx, 3, 0)
	origin := time.Now()
	sample := func(phase string) {
		started := time.Since(origin)
		resident := childrenRSSBytes(test, children...)
		var rss int64
		var allocated, heap, runtimeSystem uint64
		for _, child := range children {
			stats := child.sample(test, ctx)
			rss += resident[child.role]
			allocated += stats.HeapAlloc
			heap += stats.HeapSys
			runtimeSystem += stats.RuntimeSys
		}
		test.Logf("local phase=%s sample_window=%s..%s aggregate_single_ps_rss_bytes=%d sequential_heap_alloc_bytes=%d sequential_heap_sys_bytes=%d sequential_runtime_sys_bytes=%d", phase, started, time.Since(origin), rss, allocated, heap, runtimeSystem)
	}
	test.Log("finite local observations only; sequential Go samples and single-call physical RSS are not whole-process hard bounds or performance thresholds")
	sample("roster-ready")
	worker.send(test, executionProcessCommand{Operation: "run"})
	worker.expect(test, ctx, "running", "")
	fixture.waitDocumentJob(test, ctx)
	fixture.publishSnapshot(test, ctx)
	client := &http.Client{
		Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	read := func(requestContext context.Context, child *executionProcessChild, hash domain.ContentHash, expected []domain.CIREvent) error {
		address := child.url + "/api/v1/repos/" + string(fixture.repo) + "/docs/" + string(hash) + "/events?offset=0&limit=" + strconv.Itoa(len(expected))
		method, token := http.MethodGet, fixture.session
		var payload []byte
		if child == mcp {
			var err error
			payload, err = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 51, "method": "tools/call", "params": map[string]any{
				"name": "context_fetch", "arguments": map[string]any{"repository": string(fixture.repo), "ref": string(hash), "branch": "main", "events": len(expected)},
			}})
			if err != nil {
				return err
			}
			address, method, token = child.url+"/mcp", http.MethodPost, fixture.mcpToken
		}
		request, err := http.NewRequestWithContext(requestContext, method, address, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		host := net.ParseIP(request.URL.Hostname())
		if request.URL.Scheme != "http" || host == nil || !host.IsLoopback() || request.URL.Port() == "" {
			return fmt.Errorf("mixed request refused a non-loopback endpoint")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Cxt-Doc-Identities", string(domain.DocumentIdentityRootV1))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("%s document request: %w", child.role, err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, (128<<10)+1))
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK || len(body) > 128<<10 {
			return fmt.Errorf("%s document response status=%d bytes=%d", child.role, response.StatusCode, len(body))
		}
		var events []domain.CIREvent
		if child == api {
			var page domain.DocEventPage
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			if page.Hash != hash || page.Total != len(expected) || page.Offset != 0 || page.Next != -1 || page.Inherited != 0 {
				return fmt.Errorf("API changed exact document identity or event bounds for %s", hash)
			}
			events = page.Events
		} else {
			var rpc struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      int             `json:"id"`
				Error   json.RawMessage `json:"error"`
				Result  struct {
					IsError bool `json:"isError"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(body, &rpc); err != nil {
				return err
			}
			if rpc.JSONRPC != "2.0" || rpc.ID != 51 || len(rpc.Error) != 0 && string(rpc.Error) != "null" || rpc.Result.IsError || len(rpc.Result.Content) != 1 || rpc.Result.Content[0].Type != "text" {
				return fmt.Errorf("MCP lost the successful exact tool result")
			}
			var page struct {
				Hash      domain.ContentHash        `json:"doc_hash"`
				Snapshot  domain.ContentHash        `json:"snapshot_id"`
				Total     int                       `json:"total_events"`
				Fragments []domain.DocEventFragment `json:"fragments"`
				Next      string                    `json:"next_cursor"`
			}
			if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &page); err != nil {
				return err
			}
			if page.Hash != hash || page.Snapshot != hash || page.Total != len(expected) || len(page.Fragments) != len(expected) || page.Next != "" {
				return fmt.Errorf("MCP changed exact snapshot/document identity or event bounds")
			}
			for index, fragment := range page.Fragments {
				var event domain.CIREvent
				if fragment.Index != index || fragment.Offset != 0 || !fragment.Complete {
					return fmt.Errorf("MCP returned an incomplete or reordered event")
				}
				if err := json.Unmarshal([]byte(fragment.JSON), &event); err != nil {
					return err
				}
				events = append(events, event)
			}
		}
		actualJSON, err := json.Marshal(events)
		if err != nil {
			return err
		}
		expectedJSON, err := json.Marshal(expected)
		if err != nil {
			return err
		}
		if !bytes.Equal(actualJSON, expectedJSON) {
			return fmt.Errorf("%s returned different exact marker/event bytes for %s", child.role, hash)
		}
		return nil
	}
	original := []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fixture.marker}}}}
	sample("published-cold")
	for _, phase := range []string{"cold", "warm"} {
		for _, child := range []*executionProcessChild{api, mcp} {
			started := time.Now()
			if err := read(ctx, child, fixture.job.DocHash, original); err != nil {
				test.Fatal(phase, child.role, err)
			}
			test.Logf("local phase=%s role=%s exact_read_latency=%s", phase, child.role, time.Since(started))
		}
	}
	fixture.assertDocumentReads(test, ctx, api, mcp)
	sample("warm-authority-checked")
	const batchSize, batches = 6, 2
	const documents = batchSize * batches
	const maximumProgressReads = 128
	type upload struct {
		job               domain.DocFinalizationJob
		events            []domain.CIREvent
		started, finished time.Time
	}
	uploads := make([]upload, 0, documents)
	type observation struct {
		role                                           outbound.ProcessRole
		before, after                                  domain.DocFinalizationJob
		checkedBefore, started, finished, checkedAfter time.Time
		err                                            error
		progress                                       bool
	}
	results := make(chan observation, 2*(documents+maximumProgressReads))
	readJobs := []chan domain.DocFinalizationJob{make(chan domain.DocFinalizationJob, documents), make(chan domain.DocFinalizationJob, documents)}
	mixed, stopMixed := context.WithTimeout(ctx, 3*time.Second)
	var readers sync.WaitGroup
	defer func() { stopMixed(); readers.Wait() }()
	for index, child := range []*executionProcessChild{api, mcp} {
		readers.Add(1)
		go func(child *executionProcessChild, jobs <-chan domain.DocFinalizationJob) {
			defer readers.Done()
			firstJob := true
			for {
				select {
				case <-mixed.Done():
					return
				case job, present := <-jobs:
					if !present {
						return
					}
					var initialVersion int64
					for attempt := 0; attempt < maximumProgressReads; attempt++ {
						row := observation{role: child.role}
						row.before, row.err = fixture.storage.GetDocJob(executionProcessSystemContext(mixed), fixture.repo, job.ID)
						if attempt == 0 {
							initialVersion = row.before.Version
						}
						row.checkedBefore = time.Now()
						row.started = time.Now()
						if row.err == nil {
							row.err = read(mixed, child, fixture.job.DocHash, original)
						}
						row.finished = time.Now()
						if row.err == nil {
							row.after, row.err = fixture.storage.GetDocJob(executionProcessSystemContext(mixed), fixture.repo, job.ID)
						}
						row.checkedAfter = time.Now()
						if row.err == nil && (row.before.ID != job.ID || row.after.ID != job.ID || row.before.RepoID != fixture.repo || row.after.RepoID != fixture.repo || row.before.DocumentRef() != job.DocumentRef() || row.after.DocumentRef() != job.DocumentRef() || row.after.Version < row.before.Version || row.after.Attempts < row.before.Attempts || row.before.State == "rejected" || row.after.State == "rejected") {
							row.err = fmt.Errorf("durable read bracket lost original identity, progress, or version fence")
						}
						row.progress = row.err == nil && row.after.Version > initialVersion
						results <- row
						if row.err != nil {
							return
						}
						if !firstJob || row.progress {
							break
						}
						select {
						case <-mixed.Done():
							return
						case <-time.After(10 * time.Millisecond):
						}
					}
					firstJob = false
				}
			}
		}(child, readJobs[index])
	}
	for index := 0; index < documents; index++ {
		events := make([]domain.CIREvent, 48)
		for sequence := range events {
			marker := fmt.Sprintf("mixed-%02d-event-%02d-", index, sequence)
			marker += strings.Repeat("x", 256-len(marker))
			events[sequence] = domain.CIREvent{Seq: sequence, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: marker}}}
		}
		manifest, chunks, err := domain.ConversationManifestForCIR(domain.CIRDocument{
			Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(fixture.repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull}, Events: events,
		})
		if err != nil {
			test.Fatal(err)
		}
		hash, err := domain.ConversationManifestHash(manifest)
		if err != nil {
			test.Fatal(err)
		}
		root, err := domain.CanonicalConversationManifest(manifest)
		if err != nil {
			test.Fatal(err)
		}
		job, err := domain.NewDocFinalizationJobForRepresentation(fixture.repo, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: root}, time.Now().UTC())
		if err != nil {
			test.Fatal(err)
		}
		entry := upload{job: job, events: events, started: time.Now()}
		stored, deduped, err := fixture.storage.PutChunks(executionProcessSystemContext(mixed), fixture.repo, chunks)
		if err != nil || stored != len(chunks) || deduped != 0 {
			test.Fatal("upload distinct genuine production chunks", stored, deduped, err)
		}
		receipt, err := fixture.storage.EnqueueDocJob(executionProcessSystemContext(mixed), job)
		if err != nil || receipt.ID != job.ID || receipt.RepoID != fixture.repo || receipt.DocumentRef() != job.DocumentRef() || receipt.State != "waiting" || receipt.Attempts != 0 || receipt.Version != 0 {
			test.Fatal("enqueue exact original durable upload receipt", err)
		}
		entry.finished = time.Now()
		uploads = append(uploads, entry)
		for _, jobs := range readJobs {
			jobs <- receipt
		}
		if index+1 == batchSize {
			sample("mixed-first-batch-enqueued")
		}
	}
	for _, jobs := range readJobs {
		close(jobs)
	}
	readers.Wait()
	close(results)
	if err := mixed.Err(); err != nil {
		test.Fatal("finite mixed interval expired", err)
	}
	stopMixed()
	latencies := make(map[outbound.ProcessRole][]time.Duration)
	overlaps := make(map[outbound.ProcessRole]int)
	uploadOverlaps := make(map[outbound.ProcessRole]int)
	workerProgress := make(map[outbound.ProcessRole]int)
	for row := range results {
		if row.err != nil {
			test.Fatal("mixed query loop", row.role, row.err)
		}
		latencies[row.role] = append(latencies[row.role], row.finished.Sub(row.started))
		if row.progress {
			workerProgress[row.role]++
		}
		if row.before.Pending() && row.after.Pending() {
			overlaps[row.role]++
			if overlaps[row.role] == 1 {
				test.Logf("unfinished durable overlap role=%s job=%s hash=%s before=%s/%d at=%s request=%s..%s after=%s/%d at=%s", row.role, row.before.ID, row.before.DocHash, row.before.State, row.before.Version, row.checkedBefore.Sub(origin), row.started.Sub(origin), row.finished.Sub(origin), row.after.State, row.after.Version, row.checkedAfter.Sub(origin))
			}
			for _, entry := range uploads {
				if row.started.Before(entry.finished) && entry.started.Before(row.finished) {
					uploadOverlaps[row.role]++
					break
				}
			}
		}
	}
	for _, child := range []*executionProcessChild{api, mcp} {
		elapsed := latencies[child.role]
		if len(elapsed) < documents || len(elapsed) > documents+maximumProgressReads || overlaps[child.role] == 0 || uploadOverlaps[child.role] == 0 || workerProgress[child.role] == 0 {
			test.Fatal("missing synchronized genuine mixed progress; no overlap claimed", child.role, len(elapsed), overlaps[child.role], uploadOverlaps[child.role], workerProgress[child.role])
		}
		sort.Slice(elapsed, func(left, right int) bool { return elapsed[left] < elapsed[right] })
		test.Logf("local mixed role=%s exact_requests=%d unfinished_brackets=%d concurrent_upload_brackets=%d worker_progress_brackets=%d latency_p50=%s latency_p95=%s", child.role, len(elapsed), overlaps[child.role], uploadOverlaps[child.role], workerProgress[child.role], elapsed[(len(elapsed)*50+99)/100-1], elapsed[(len(elapsed)*95+99)/100-1])
	}
	sample("mixed-reads-joined")
	completion, stopCompletion := context.WithTimeout(executionProcessSystemContext(ctx), 15*time.Second)
	defer stopCompletion()
	for _, entry := range uploads {
		for {
			receipt, err := fixture.storage.GetDocJob(completion, fixture.repo, entry.job.ID)
			if err != nil || receipt.ID != entry.job.ID || receipt.RepoID != fixture.repo || receipt.DocumentRef() != entry.job.DocumentRef() || receipt.State == "rejected" {
				test.Fatal("worker lost or rejected the original durable upload", entry.job.ID, err)
			}
			if receipt.State == "completed" {
				if receipt.Attempts < 1 || receipt.Version < 1 {
					test.Fatal("upload completed without its worker claim/version receipt")
				}
				break
			}
			select {
			case <-completion.Done():
				test.Fatal("worker Run did not finish every finite durable upload", entry.job.ID, receipt.State)
			case <-time.After(20 * time.Millisecond):
			}
		}
		proof, err := fixture.storage.VerifyStoredDoc(completion, fixture.repo, entry.job.DocHash)
		if err != nil || proof.DocumentRef() != entry.job.DocumentRef() {
			test.Fatal("completed upload lacks its exact verified stored root", entry.job.ID, err)
		}
		if err := fixture.storage.PutSnapshot(completion, domain.Snapshot{ID: entry.job.DocHash, RepoID: fixture.repo,
			DocHash: entry.job.DocHash, DocIdentity: domain.DocumentIdentityRootV1, Branch: "main",
			Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}); err != nil {
			test.Fatal("bind completed upload to an authorized snapshot without moving main", err)
		}
		if err := read(ctx, api, entry.job.DocHash, entry.events); err != nil {
			test.Fatal("completed upload exact HTTP content", err)
		}
	}
	ref, err := fixture.storage.GetRef(executionProcessSystemContext(ctx), fixture.repo, domain.RefBranch, "main")
	if err != nil || ref.Target != fixture.job.DocHash {
		test.Fatal("additional uploads moved the original published main ref", err)
	}
	fixture.assertDocumentReads(test, ctx, api, mcp)
	for _, child := range []*executionProcessChild{api, mcp} {
		if err := read(ctx, child, fixture.job.DocHash, original); err != nil {
			test.Fatal("post-mixed exact current-authority read", child.role, err)
		}
	}
	sample("all-uploads-published-authority-rechecked")
	test.Logf("finite actual worker publications=%d events_per_document=48 text_characters_per_event=256 original_main=%s", len(uploads), fixture.job.DocHash)
	worker.terminateAndAcknowledge(test, ctx, fixture)
	fixture.assertSessions(test, ctx, 2, 0)
	api.terminateAndAcknowledge(test, ctx, fixture)
	mcp.terminateAndAcknowledge(test, ctx, fixture)
	fixture.assertSessions(test, ctx, 0, 0)
}
