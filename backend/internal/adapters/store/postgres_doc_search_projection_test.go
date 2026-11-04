//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docSearchStageCasePG struct {
	storage     *PostgresStore
	ctx         context.Context
	proof       domain.VerifiedSessionDoc
	raw         []byte
	want        domain.DocReadIndex
	job         domain.DocFinalizationJob
	publication pgDocPublication
	sourceImage string
}

func docSearchStageSourceImagePG(test *testing.T, fixture docSearchStageCasePG) string {
	test.Helper()
	var image string
	err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT COALESCE(jsonb_agg(
 jsonb_build_array(rb.hash,encode(digest(b.bytes,'sha256'),'hex'),rb.xmin::text,b.xmin::text)
 ORDER BY rb.hash)::text,'[]') FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash
 WHERE rb.repo_id=$1 AND rb.kind='chunk'`, fixture.job.RepoID).Scan(&image)
	if err != nil {
		test.Fatal(err)
	}
	return image
}

func docSearchStageFixturePG(test *testing.T, items int) docSearchStageCasePG {
	test.Helper()
	storage, ctx := chunkReusePG(test)
	repo := domain.HashContent([]byte(test.Name() + time.Now().UTC().String()))
	var hashes []domain.ContentHash
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, statement := range []string{
			`DELETE FROM doc_finalization_jobs WHERE repo_id=$1`,
			`DELETE FROM repo_blobs WHERE repo_id=$1`,
			`DELETE FROM repos WHERE id=$1`,
		} {
			if _, err := storage.pool.Exec(cleanup, statement, repo); err != nil {
				test.Error(err)
			}
		}
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM blobs b WHERE hash=ANY($1::text[])
 AND NOT EXISTS(SELECT 1 FROM repo_blobs rb WHERE rb.hash=b.hash)
 AND NOT EXISTS(SELECT 1 FROM snapshots s WHERE s.doc_hash=b.hash OR s.memory_hash=b.hash)`, hashes); err != nil {
			test.Error(err)
		}
	})
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	input := make(map[string]any, items)
	output := make([]any, items)
	values := []any{nil, true, float64(17), "toolpayloadonly"}
	for ordinal := range output {
		input[fmt.Sprintf("entry-%06d", ordinal)] = values[ordinal%len(values)]
		output[ordinal] = values[(ordinal+1)%len(values)]
	}
	callID := "call-" + string(repo)
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{
			{Kind: domain.EventTurn, Seq: 3, Role: domain.RoleSystem, ID: string(repo)},
			{Kind: domain.EventMessage, Seq: 7, Role: domain.RoleUser, Blocks: []domain.ContentBlock{
				{Type: "text", Text: "Visible Needle 50%_\\ \ud55c🙂 " + string(repo)},
				{Type: "text", Text: ""}, {Type: "text", Text: "second\tline \"quoted\"\n<&>"},
			}},
			{Kind: domain.EventToolCall, Seq: 11, CallID: callID, ToolName: "synthetic", Input: input},
			{Kind: domain.EventToolResult, Seq: 19, CallID: callID, Output: output},
			{Kind: domain.EventReasoning, Seq: 29, ID: string(repo), RedactedSummary: "Reasoning Needle\n\ud55c🙂",
				Locked: &domain.LockedBlob{Provider: domain.ProviderCodex, Scheme: "encrypted_content", Blob: "lockedonly"}},
			{Kind: domain.EventCompaction, Seq: 37, ReplacementComplete: true, Replacement: []domain.CIREvent{
				{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleAssistant,
					Blocks: []domain.ContentBlock{{Type: "text", Text: "replacementonly " + string(repo)}}},
				{Kind: domain.EventToolCall, Seq: 5, CallID: callID, ToolName: "synthetic", Input: input},
				{Kind: domain.EventToolResult, Seq: 8, CallID: callID, Output: output},
			}},
			{Kind: domain.EventMessage, Seq: 41, Role: domain.RoleAssistant,
				Blocks: []domain.ContentBlock{{Type: "text", Text: "Final Needle " + string(repo)}}},
		},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		test.Fatal(err)
	}
	proof, err := domain.VerifySessionDoc(domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir})
	if err != nil {
		test.Fatal(err)
	}
	chunks, ok := domain.PlanDocChunks(raw)
	if !ok {
		test.Fatal("fixture must use canonical source chunks")
	}
	hashes = append(hashes, proof.Hash())
	for hash := range chunks.Bodies {
		hashes = append(hashes, hash)
	}
	var parts struct {
		Envelope domain.CIREnvelope `json:"envelope"`
		Events   []json.RawMessage  `json:"events"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		test.Fatal(err)
	}
	want := domain.DocReadIndex{Version: 1, Hash: proof.Hash(), Envelope: parts.Envelope}
	offset := 0
	for ordinal, body := range parts.Events {
		var event domain.CIREvent
		if err := json.Unmarshal(body, &event); err != nil {
			test.Fatal(err)
		}
		if event.Kind == domain.EventToolCall && (len(event.Input) != items || len(body) <= 64<<10) {
			test.Fatal("fixture lost its many-small-value input", len(event.Input), len(body))
		}
		if event.Kind == domain.EventToolResult {
			payload, ok := event.Output.([]any)
			if !ok || len(payload) != items {
				test.Fatal("fixture lost its many-small-value output")
			}
		}
		want.Events = append(want.Events, domain.DocEventIndex{
			Index: ordinal, Offset: offset, Length: len(body), Hash: domain.HashContent(body),
			Seq: event.Seq, Role: string(event.Role), Text: domain.SearchableEventText(event),
		})
		offset += len(body) + 1
	}
	job, publication := stageJobForDocPG(test, storage, ctx, repo, proof)
	if len(publication.doc.read.blocks) != 1 {
		test.Fatal("fixture must fit one read block")
	}
	fixture := docSearchStageCasePG{storage: storage, ctx: ctx, proof: proof, raw: raw,
		want: want, job: job, publication: publication}
	fixture.sourceImage = docSearchStageSourceImagePG(test, fixture)
	stageDerivativesPG(test, storage, ctx, publication, false)
	stageInvisiblePG(test, storage, ctx, job)
	return fixture
}

func docSearchStageAssertPG(test *testing.T, fixture docSearchStageCasePG) {
	test.Helper()
	storage, ctx, job, publication := fixture.storage, fixture.ctx, fixture.job, fixture.publication
	stagePinsPG(test, storage, ctx, job, publication, true)
	stageDerivativesPG(test, storage, ctx, publication, true)
	stageInvisiblePG(test, storage, ctx, job)
	for _, want := range fixture.want.Events {
		var got domain.DocEventIndex
		var lower string
		err := storage.pool.QueryRow(ctx, `SELECT e.ordinal,e.byte_offset,e.byte_length,e.event_hash,e.seq,e.role,
 t.search_text,t.search_lower FROM doc_read_block_events_v3 e JOIN doc_search_events_v2 t ON t.hash=e.event_hash
 WHERE e.block_hash=$1 AND e.ordinal=$2`, publication.doc.read.blocks[0].Hash, want.Index).
			Scan(&got.Index, &got.Offset, &got.Length, &got.Hash, &got.Seq, &got.Role, &got.Text, &lower)
		if err != nil || !reflect.DeepEqual(got, want) || lower != strings.ToLower(want.Text) {
			test.Fatalf("staged full-CIREvent oracle mismatch: got=%+v want=%+v lower=%q err=%v", got, want, lower, err)
		}
	}
	if docSearchStageSourceImagePG(test, fixture) != fixture.sourceImage {
		test.Fatal("staging changed original chunk bytes or ownership rows")
	}
	if err := publication.Complete(ctx, job, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	stageCompletedPG(test, storage, ctx, job, publication)
	wantMetadata := fixture.want
	wantMetadata.Events = append([]domain.DocEventIndex(nil), fixture.want.Events...)
	for ordinal := range wantMetadata.Events {
		wantMetadata.Events[ordinal].Text = ""
	}
	if got, err := storage.DocReadIndex(ctx, job.RepoID, job.DocHash); err != nil || !reflect.DeepEqual(got, wantMetadata) {
		test.Fatal("published exact coordinates, hashes, sequences, roles, or envelope changed", err)
	}
	queries := []struct {
		text  string
		after int
		limit int
	}{
		{"", -1, 16}, {"needle", -1, 16}, {"needle", 1, 1}, {"needle", 4, 16},
		{"50%_\\", -1, 16}, {"\ud55c🙂", -1, 16}, {"second\tline", -1, 16},
		{"reasoning", -1, 16}, {"toolpayloadonly", -1, 16}, {"replacementonly", -1, 16}, {"lockedonly", -1, 16},
	}
	for _, query := range queries {
		want := []domain.DocEventIndex{}
		for _, event := range fixture.want.Events {
			if event.Index > query.after && strings.Contains(strings.ToLower(event.Text), query.text) && len(want) < query.limit {
				event.Offset, event.Length = 0, 0
				want = append(want, event)
			}
		}
		got, err := storage.SearchDocEvents(ctx, job.RepoID, job.DocHash, query.text, query.after, query.limit)
		if err != nil || !reflect.DeepEqual(got, want) {
			test.Fatalf("public search %q after=%d limit=%d: got=%+v want=%+v err=%v", query.text, query.after, query.limit, got, want, err)
		}
	}
	restored, err := storage.ReadVerifiedDoc(ctx, job.RepoID, job.DocHash)
	if err != nil || restored.DocumentRef() != fixture.proof.DocumentRef() ||
		!bytes.Equal(restored.Bytes(), fixture.raw) || domain.HashContent(fixture.raw) != job.DocHash {
		test.Fatal("publication changed original canonical source bytes, hash, or identity", err)
	}
	if docSearchStageSourceImagePG(test, fixture) != fixture.sourceImage {
		test.Fatal("completion or search changed original chunk bytes or ownership rows")
	}
	stageCountPG(test, storage, ctx, 1, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, job.RepoID, job.DocHash)
}

func TestPGDocSearchStageLargePayloadProjection(test *testing.T) {
	for _, items := range []int{8192, 16384} {
		test.Run(fmt.Sprintf("items=%d", items), func(test *testing.T) {
			fixture := docSearchStageFixturePG(test, items)
			if err := fixture.publication.stageReadBlocks(fixture.ctx, fixture.job); err != nil {
				test.Fatal(err)
			}
			docSearchStageAssertPG(test, fixture)
		})
	}
}

type docSearchStageHeapPG struct {
	peak    uint64
	samples int
}

func docSearchStageMeasurePG(test *testing.T, fixture docSearchStageCasePG, items, sample int) {
	test.Helper()
	ready, start, stop := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan docSearchStageHeapPG, 1)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	go func() {
		var observed docSearchStageHeapPG
		var memory runtime.MemStats
		close(ready)
		<-start
		for {
			select {
			case <-stop:
				done <- observed
				return
			case <-ticker.C:
				runtime.ReadMemStats(&memory)
				observed.peak = max(observed.peak, memory.HeapAlloc)
				observed.samples++
			}
		}
	}()
	<-ready
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	close(start)
	started := time.Now()
	err := fixture.publication.stageReadBlocks(fixture.ctx, fixture.job)
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	close(stop)
	observed := <-done
	runtime.KeepAlive(fixture)
	if err != nil {
		test.Fatal(err)
	}
	peak := max(observed.peak, after.HeapAlloc, before.HeapAlloc) - before.HeapAlloc
	test.Logf("stage-only items=%d sample=%d repo=%s doc=%s source_bytes=%d elapsed=%s alloc_bytes=%d alloc_objects=%d sampled_incremental_heap_bytes=%d heap_samples=%d",
		items, sample, fixture.job.RepoID, fixture.job.DocHash, len(fixture.raw), elapsed,
		after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, peak, observed.samples)
}

func TestPGDocSearchStageQualification(test *testing.T) {
	if os.Getenv("CXT_DOC_SEARCH_STAGE_QUALIFICATION") != "1" {
		test.Skip("set CXT_DOC_SEARCH_STAGE_QUALIFICATION=1 for cold PostgreSQL stage costs")
	}
	seen := make(map[domain.ContentHash]bool)
	for _, items := range []int{65536, 262144} {
		for sample := range 3 {
			test.Run(fmt.Sprintf("items=%d/sample=%d", items, sample), func(test *testing.T) {
				fixture := docSearchStageFixturePG(test, items)
				for _, hash := range []domain.ContentHash{fixture.job.RepoID, fixture.job.DocHash} {
					if seen[hash] {
						test.Fatal("qualification reused a repository or document identity", hash)
					}
					seen[hash] = true
				}
				docSearchStageMeasurePG(test, fixture, items, sample)
				docSearchStageAssertPG(test, fixture)
			})
		}
	}
}
