//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryPreparationCostPortPG struct {
	calls    int
	duration time.Duration
}

type memoryPreparationCostPagePG struct {
	mode         string
	rows         int
	snapshotRows int
	valueBytes   int
	entryBytes   int
	duration     time.Duration
}

type memoryPreparationCostStorePG struct {
	*store.PostgresStore
	ports        map[string]memoryPreparationCostPortPG
	pages        []memoryPreparationCostPagePG
	graphContext context.Context
	input        domain.MemoryCaptureBundle
	output       domain.MemoryReadBundle
}

func (probe *memoryPreparationCostStorePG) record(name string, started time.Time) {
	port := probe.ports[name]
	port.calls++
	port.duration += time.Since(started)
	probe.ports[name] = port
}

func (probe *memoryPreparationCostStorePG) FindMemoryCaptureAnchor(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, expiresAt time.Time) (domain.ContentHash, error) {
	defer probe.record("FindMemoryCaptureAnchor", time.Now())
	return probe.PostgresStore.FindMemoryCaptureAnchor(ctx, checkpoint, expiresAt)
}

func (probe *memoryPreparationCostStorePG) ReadMemoryCapture(ctx context.Context, repo, identity domain.ContentHash) (domain.MemoryCaptureBundle, error) {
	defer probe.record("ReadMemoryCapture", time.Now())
	return probe.PostgresStore.ReadMemoryCapture(ctx, repo, identity)
}

func (probe *memoryPreparationCostStorePG) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	started := time.Now()
	page, err := probe.PostgresStore.RepositoryGraphSourceChanges(ctx, repo, request)
	probe.record("RepositoryGraphSourceChanges", started)
	if err != nil {
		return page, err
	}
	if probe.graphContext != nil && probe.graphContext != ctx {
		return domain.RepositoryViewSourcePage{}, domain.ErrConflict
	}
	probe.graphContext = ctx
	observed := memoryPreparationCostPagePG{mode: page.Mode, rows: len(page.Entries), duration: time.Since(started)}
	for _, entry := range page.Entries {
		observed.valueBytes += len(entry.Value)
		observed.entryBytes += len(entry.Kind) + len(entry.Key) + len(entry.Value)
		if entry.Kind == "snapshot" {
			observed.snapshotRows++
		}
	}
	probe.pages = append(probe.pages, observed)
	return page, nil
}

func (probe *memoryPreparationCostStorePG) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, repo domain.Repo, entries []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	defer probe.record("BuildGraphSourceReader", time.Now())
	return probe.PostgresStore.BuildGraphSourceReader(ctx, checkpoint, repo, entries)
}

func (probe *memoryPreparationCostStorePG) CaptureMemoryObject(ctx context.Context, repo, hash domain.ContentHash, budget int) (domain.CapturedMemoryObject, error) {
	defer probe.record("CaptureMemoryObject", time.Now())
	return probe.PostgresStore.CaptureMemoryObject(ctx, repo, hash, budget)
}

func (probe *memoryPreparationCostStorePG) WriteMemoryCapture(ctx context.Context, bundle domain.MemoryCaptureBundle, expiresAt time.Time) error {
	defer probe.record("WriteMemoryCapture", time.Now())
	err := probe.PostgresStore.WriteMemoryCapture(ctx, bundle, expiresAt)
	if err == nil {
		probe.input = bundle
	}
	return err
}

func (probe *memoryPreparationCostStorePG) WriteMemoryRead(ctx context.Context, bundle domain.MemoryReadBundle) error {
	defer probe.record("WriteMemoryRead", time.Now())
	err := probe.PostgresStore.WriteMemoryRead(ctx, bundle)
	if err == nil {
		probe.output = bundle
	}
	return err
}

func qualifyMemoryPreparationCostPG(test *testing.T, service *Service, storage *store.PostgresStore, repo domain.ContentHash, request domain.EffectiveMemoryRequest) {
	test.Helper()
	ctx := systemTestContext()
	metadata, err := storage.GetRepo(ctx, repo)
	if err != nil {
		test.Fatal(err)
	}
	node, err := domain.NewGitTreeNode(map[string]domain.GitEntry{"file": {OID: effectiveOID(101), Mode: "100644"}}, 40)
	if err != nil {
		test.Fatal(err)
	}
	for number := 1; number <= 3; number++ {
		commit := effectiveOID(number)
		delta := domain.GitCommitDelta{Commit: commit, Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{}}
		if number == 1 {
			delta.Changes = []domain.GitPathChange{{Path: "file", After: domain.GitEntry{OID: effectiveOID(101), Mode: "100644"}}}
		} else {
			delta.Parent = effectiveOID(number - 1)
			delta.Parents = []string{delta.Parent}
		}
		job := domain.NewGitScan(repo, metadata.GitRemoteURL, commit, time.Now().UTC())
		if err := storage.EnqueueGitScan(ctx, job); err != nil {
			test.Fatal(err)
		}
		job, err = storage.ClaimGitScan(ctx, repo, time.Now().UTC(), time.Minute)
		if err != nil || job.Commit != commit {
			test.Fatal("claim fixture Git evidence", job.Commit, err)
		}
		job.State, job.TreeIndexed, job.Indexed = "completed", true, true
		finish := domain.GitScanFinish{Job: job,
			Tree:   &domain.GitTreeEvidence{Commit: domain.GitCommitTree{Commit: commit, Tree: node.OID, Parents: delta.Parents}, Nodes: []domain.GitTreeNode{node}},
			Deltas: []domain.GitDeltaRecord{domain.NewGitDelta(repo, metadata.GitRemoteURL, delta)}}
		for _, parent := range delta.Parents {
			finish.Parents = append(finish.Parents, domain.NewGitScan(repo, metadata.GitRemoteURL, parent, time.Now().UTC()))
		}
		if err := storage.FinishGitScan(ctx, finish); err != nil {
			test.Fatal("persist fixture Git evidence", err)
		}
	}
	observation := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), BranchID: "cost-main", Branch: "main", Kind: "position",
		Source: request.Selection.SnapshotID, Target: request.Selection.SnapshotID, GitAfter: effectiveOID(1), CreatedAt: time.Now().UTC()}
	if err := service.RecordHistory(ctx, observation); err != nil {
		test.Fatal("record fixture observation", err)
	}
	publishPRSource(test, service, observation)
	request.Limit = 50
	page, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal("qualify fixture", err)
	}
	claim := itemByText(test, page, "qualified source code")
	if claim.State != "applied" || len(claim.PublicationIDs) == 0 || len(claim.Paths) != 1 || page.DeliveryStateHash == "" {
		test.Fatalf("fixture must exercise published, Git-verified code, not missing-evidence review: %+v", claim)
	}
	test.Logf("fixture_qualified code_state=%s publications=%d paths=%d git_commits=3; synthetic persisted Git trees/deltas and accepted publication; coverage=stored-memory/direct-linear-ancestry, not PR topology, branch pins, or provider ingestion", claim.State, len(claim.PublicationIDs), len(claim.Paths))
}

func measureMemoryPreparationCostPG(test *testing.T, service *Service, storage *store.PostgresStore, repo domain.ContentHash, request domain.EffectiveMemoryRequest, size, sample int, previous *PreparedMemoryRead) *PreparedMemoryRead {
	test.Helper()
	mode := "cold/full"
	if previous != nil {
		mode = "warm/delta"
		if previous.graphAnchor != nil && previous.graphAnchor.graph.decoded == nil {
			mode += "/metadata-redecode"
		}
	}
	probe := &memoryPreparationCostStorePG{PostgresStore: storage, ports: make(map[string]memoryPreparationCostPortPG)}
	originalMeta, originalBlobs := service.meta, service.blobs
	service.meta, service.blobs = probe, probe
	defer func() { service.meta, service.blobs = originalMeta, originalBlobs }()
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	defer cancel()
	phases := make(map[string]time.Duration)
	measure := func(name string, operation func() error) error {
		started := time.Now()
		err := operation()
		phases[name] += time.Since(started)
		return err
	}
	var prepared *PreparedMemoryRead
	var expected domain.ContentHash
	referenceCalls := 0
	expiresAt := time.Now().UTC().Add(memoryReadResultRetention)
	started := time.Now()
	err := service.WithMemoryReadResources(ctx, repo, func(bound context.Context) error {
		bound, release := context.WithTimeout(bound, 30*time.Second)
		defer release()
		var base *memoryGraphAnchor
		if err := measure("anchor", func() error {
			var err error
			base, err = service.findMemoryGraphAnchor(bound, repo, expiresAt, previous)
			return err
		}); err != nil {
			return err
		}
		if (previous == nil) != (base == nil) || (previous != nil && base != previous.graphAnchor) {
			return fmt.Errorf("%s: unexpected durable anchor or in-process reuse", mode)
		}
		var graph *CapturedGraphSource
		var delta *memoryGraphDelta
		if base != nil {
			graph, delta = &base.graph, &memoryGraphDelta{}
		}
		reference := func(snapshot context.Context) error {
			referenceCalls++
			if snapshot != probe.graphContext || !storage.InReadOnlyTransaction(snapshot) {
				return fmt.Errorf("reference did not share graph capture's read snapshot")
			}
			return measure("reference", func() error {
				var err error
				expected, err = service.memoryReadReference(snapshot, repo, request)
				return err
			})
		}
		var captured *CapturedEffectiveMemory
		if err := measure("capture", func() error {
			var err error
			captured, err = service.captureEffectiveMemorySource(bound, repo, request, graph, delta, reference)
			return err
		}); err != nil {
			return err
		}
		if err := measure("prepare", func() error {
			var err error
			prepared, err = prepareCapturedMemoryRead(bound, captured, base, delta)
			return err
		}); err != nil {
			return err
		}
		if err := measure("compare", func() error {
			var err error
			prepared.comparison, err = newMemoryReadComparison(bound, prepared.output, expected)
			return err
		}); err != nil {
			return err
		}
		if !prepared.comparison.Matched() {
			return domain.ErrMemoryReadComparisonMismatch
		}
		if prepared.input.Header.Version == domain.MemoryCaptureVersion {
			expiresAt = time.Now().UTC().Add(memoryReadAnchorRetention)
		}
		return measure("persist", func() error { return service.persistPreparedMemoryRead(bound, prepared, expiresAt) })
	})
	total := time.Since(started)
	test.Logf("D3ab rows=%d sample=%d mode=%s total=%s anchor=%s capture_including_reference=%s reference=%s capture_excluding_reference=%s prepare=%s compare=%s persist=%s error=%v",
		size, sample, mode, total, phases["anchor"], phases["capture"], phases["reference"], phases["capture"]-phases["reference"], phases["prepare"], phases["compare"], phases["persist"], err)
	names := make([]string, 0, len(probe.ports))
	for name := range probe.ports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		port := probe.ports[name]
		test.Logf("D3ab rows=%d sample=%d mode=%s port=%s calls=%d duration=%s", size, sample, mode, name, port.calls, port.duration)
	}
	baselinePages, deltaPages, baselineSnapshots, deltaRows := 0, 0, 0, 0
	for index, page := range probe.pages {
		test.Logf("D3ab rows=%d sample=%d mode=%s graph_page=%d graph_mode=%s page_rows=%d snapshot_rows=%d value_bytes=%d entry_bytes=%d duration=%s",
			size, sample, mode, index+1, page.mode, page.rows, page.snapshotRows, page.valueBytes, page.entryBytes, page.duration)
		switch page.mode {
		case "baseline":
			baselinePages++
			baselineSnapshots += page.snapshotRows
		case "delta":
			deltaPages++
			deltaRows += page.rows
		default:
			test.Fatalf("unexpected graph page mode %q", page.mode)
		}
	}
	if err != nil {
		test.Fatal("measured pipeline", err)
	}
	if referenceCalls != 1 || expected == "" || prepared.comparison == nil || !prepared.comparison.Matched() {
		test.Fatal("missing same-snapshot comparison")
	}
	if probe.ports["FindMemoryCaptureAnchor"].calls != 1 || probe.ports["ReadMemoryCapture"].calls != 0 ||
		probe.ports["WriteMemoryCapture"].calls != 1 || probe.ports["WriteMemoryRead"].calls != 1 {
		test.Fatal("unexpected anchor reread or missing persistence", probe.ports)
	}
	base, err := domain.MemoryCaptureGraphBase(prepared.input)
	if err != nil {
		test.Fatal(err)
	}
	fullParts, deltaParts := 0, 0
	for _, part := range prepared.input.Parts {
		if strings.HasPrefix(part.Kind, "graph:") {
			fullParts++
		}
		if strings.HasPrefix(part.Kind, "graph_delta:") {
			deltaParts++
		}
	}
	if previous == nil {
		if prepared.input.Header.Version != domain.MemoryCaptureVersion || base.ID != "" || baselinePages == 0 || baselineSnapshots != size || deltaPages != 0 || fullParts == 0 || deltaParts != 0 {
			test.Fatal("cold sample did not capture the complete baseline", baselinePages, baselineSnapshots, fullParts, deltaParts)
		}
	} else if prepared.input.Header.Version != domain.MemoryCaptureDeltaVersion || base.ID != previous.input.ID || baselinePages != 0 || deltaPages == 0 || deltaRows == 0 || fullParts != 0 || deltaParts == 0 {
		test.Fatal("warm sample did not use a nonempty delta exclusively", baselinePages, deltaPages, deltaRows, fullParts, deltaParts)
	}
	inputHeader, err := json.Marshal(probe.input.Header)
	if err != nil {
		test.Fatal(err)
	}
	inputBytes := len(inputHeader)
	for _, part := range probe.input.Parts {
		inputBytes += len(part.Kind) + len(part.Key) + len(part.Value)
	}
	outputHeader, err := domain.MemoryReadHeaderBytes(ctx, probe.output.Header)
	if err != nil {
		test.Fatal(err)
	}
	outputBytes := len(outputHeader)
	for _, chunk := range probe.output.Chunks {
		outputBytes += len(chunk.Bytes)
	}
	test.Logf("D3ab rows=%d sample=%d mode=%s capture_input_bytes=%d output_bytes=%d input_parts=%d full_graph_parts=%d delta_graph_parts=%d output_chunks=%d output_items=%d matched=%t capture_id=%s read_id=%s",
		size, sample, mode, inputBytes, outputBytes, len(probe.input.Parts), fullParts, deltaParts, len(probe.output.Chunks), probe.output.Header.View.Total, prepared.comparison.Matched(), probe.input.ID, probe.output.ID)
	storedInput, err := storage.ReadMemoryCapture(systemTestContext(), repo, prepared.input.ID)
	if err != nil || !reflect.DeepEqual(storedInput, prepared.input) {
		test.Fatal("persisted capture differs", err)
	}
	storedHeader, err := storage.ReadMemoryReadHeader(systemTestContext(), repo, prepared.output.ID)
	if err != nil || !reflect.DeepEqual(storedHeader, prepared.output.Header) {
		test.Fatal("persisted output header differs", err)
	}
	for ordinal, chunk := range prepared.output.Chunks {
		storedChunk, err := storage.ReadMemoryReadChunk(systemTestContext(), repo, prepared.output.ID, ordinal)
		if err != nil || !reflect.DeepEqual(storedChunk, chunk.Bytes) {
			test.Fatal("persisted output chunk differs", ordinal, err)
		}
	}
	return prepared
}

func TestPGMemoryPreparationCostD3ab(test *testing.T) {
	if os.Getenv("CXT_MEMORY_COST_PROBE") != "1" {
		test.Skip("opt in with CXT_MEMORY_COST_PROBE=1 and CXT_TEST_DSN pointing only to a disposable PostgreSQL database")
	}
	if os.Getenv("CXT_TEST_DSN") == "" {
		test.Fatal("CXT_TEST_DSN must name a disposable cost-only PostgreSQL database")
	}
	test.Log("D3ab: 3 measured cold/full then warm/delta pairs per size, fresh repository per pair; setup, qualification, mutation, byte accounting and durable verification are outside phase timers; no percentile claims")
	test.Log("cold means no durable graph anchor, not cold PostgreSQL/OS caches; warm reuses the in-process anchor after one memory CAS; fixture has one chunked stored memory with code/rationale/decision claims")
	test.Log("wrapper port calls are NOT SQL statement counts; page rows are returned graph entries; value_bytes=sum(value); entry_bytes=sum(kind+key+value); capture_input_bytes=JSON header+part kinds/keys/values; output_bytes=canonical header+chunk bodies; bytes exclude SQL/wire/storage overhead and graph-base bytes reused by delta")
	test.Log("phase and port durations overlap: capture includes its same-snapshot reference; persistence includes both write ports; admission gate and existing 30s work deadlines remain enabled; worker scheduling/qualification receipt publication are not measured")
	for _, size := range []int{100, 1000} {
		test.Run(fmt.Sprintf("snapshots_%d", size), func(test *testing.T) {
			for sample := 1; sample <= 3; sample++ {
				test.Run(fmt.Sprintf("sample_%d", sample), func(test *testing.T) {
					service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
					qualifyMemoryPreparationCostPG(test, service, storage, repo, request)
					ctx := systemTestContext()
					for index := 0; index < size-1; index++ {
						collaborationSnapshot(test, storage, repo, fmt.Sprintf("unrelated-cost-metadata/%d", index))
					}
					var rows int
					if err := connection.QueryRow(ctx, `SELECT count(*) FROM snapshots WHERE repo_id=$1`, string(repo)).Scan(&rows); err != nil || rows != size {
						test.Fatal("incorrect fixture snapshot metadata row count", rows, err)
					}
					var database, version string
					var socketOnly bool
					if err := connection.QueryRow(ctx, `SELECT current_database(),version(),inet_server_addr() IS NULL`).Scan(&database, &version, &socketOnly); err != nil {
						test.Fatal(err)
					}
					test.Logf("fixture rows=%d unrelated_rows=%d sample=%d repo=%s database=%s unix_socket=%t postgres=%s", rows, size-1, sample, repo, database, socketOnly, version)
					cold := measureMemoryPreparationCostPG(test, service, storage, repo, request, size, sample, nil)
					digest, err := storage.GetMemory(ctx, repo, request.Selection.MemoryHash)
					if err != nil {
						test.Fatal(err)
					}
					digest.PreviousMemoryHash = request.Selection.MemoryHash
					digest.Summary += " warm delta"
					request.Selection.MemoryHash, err = service.PutMemoryDigestCAS(ctx, repo, digest)
					if err != nil {
						test.Fatal("unmeasured warm memory CAS", err)
					}
					measureMemoryPreparationCostPG(test, service, storage, repo, request, size, sample, cold)
					if os.Getenv("CXT_MEMORY_COST_COMPARE_DECODE") == "1" {
						control := *cold
						anchor := *cold.graphAnchor
						anchor.graph.decoded = nil
						control.graphAnchor = &anchor
						order := []*PreparedMemoryRead{cold, &control}
						if sample%2 == 0 {
							order[0], order[1] = order[1], order[0]
						}
						test.Log("D3ad paired repeats: both inputs/outputs already persisted; compare metadata reuse to forced redecoding, alternating order; duplicate persistence, not first-write latency")
						first := measureMemoryPreparationCostPG(test, service, storage, repo, request, size, sample, order[0])
						second := measureMemoryPreparationCostPG(test, service, storage, repo, request, size, sample, order[1])
						if !reflect.DeepEqual(first.input, second.input) || !reflect.DeepEqual(first.output, second.output) {
							test.Fatal("reused decoding changed complete capture or read bundle")
						}
					}
				})
			}
		})
	}
}
