package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphProjectionEncodingView(repo domain.ContentHash, branchCount int) domain.RepositoryView {
	view := domain.RepositoryView{
		DefaultBranch: "main",
		Revision:      domain.RepositoryRevision{Graph: 1<<53 + 1, Pending: 1<<53 + 3, Evidence: 1<<53 + 5},
		Snapshots:     make([]domain.Snapshot, 1024),
	}
	for index := range view.Snapshots {
		identity := hh(fmt.Sprintf("graph-encoding-snapshot-%d", index))
		snapshot := domain.Snapshot{ID: identity, DocHash: identity, RepoID: repo, Branch: "main"}
		if index > 0 {
			snapshot.Parents = []domain.ContentHash{view.Snapshots[index-1].ID}
		}
		view.Snapshots[index] = snapshot
	}
	tip := view.Snapshots[len(view.Snapshots)-1].ID
	for index := range branchCount {
		branch := "main"
		if index > 0 {
			branch = fmt.Sprintf("feature/%03d", index)
		}
		view.Refs = append(view.Refs, domain.Ref{
			Kind: domain.RefBranch, Name: branch, RepoID: repo, Target: tip,
			BranchID: domain.LegacyContextBranchID(string(repo), branch),
		})
	}
	view.History = []domain.HistoryEvent{{
		ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), Kind: "position", Branch: "main",
		BranchID: view.Refs[0].BranchID, Target: tip,
		CreatedAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}}
	return view
}

func TestGraphProjectionEncodingLargeGraphCacheWarmRead(t *testing.T) {
	svc, storage := newFsckSvc(t)
	svc.meta = &graphCacheReadState{FSStore: storage, readOnly: true}
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	repo := hh(t.Name())
	view := graphProjectionEncodingView(repo, 128)
	position := view.History[0].ID
	first, err := svc.cachedGraphState(ctx, repo, view, position)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= graphProjectionCacheBytes || len(raw) >= graphProjectionExpandedBytes {
		t.Fatalf("fixture must exceed the old raw budget: bytes=%d", len(raw))
	}
	if first.Revision != view.Revision || first.PositionEvent != position || first.PrimaryBranch != "main" || len(first.SnapshotIDs) != len(view.Snapshots) {
		t.Fatal("projection lost its revision, position, branch, or snapshots")
	}
	if len(first.Positions) != 1 || first.Positions[0].BranchID != view.Refs[0].BranchID || first.Positions[0].Snapshot != view.History[0].Target {
		t.Fatal("projection lost selected position identity")
	}
	encoded, err := encodeGraphProjection(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeGraphProjection(ctx, encoded)
	if err != nil || !reflect.DeepEqual(decoded, first) {
		t.Fatalf("large graph round trip changed the projection: %v", err)
	}
	if len(svc.graphCache.entries) != 1 {
		t.Fatalf("large graph was not retained: entries=%d", len(svc.graphCache.entries))
	}
	entry := svc.graphCache.entries[0]
	if len(entry.value) == 0 || svc.graphCache.bytes != len(entry.value) || svc.graphCache.bytes > graphProjectionCacheBytes {
		t.Fatalf("cache must account for compressed bytes: retained=%d entry=%d", svc.graphCache.bytes, len(entry.value))
	}
	if entry.key.repo != repo || entry.key.revision != view.Revision || entry.key.position != position || len(svc.graphCache.building) != 0 {
		t.Fatal("retained entry lost identity or leaked its build reservation")
	}
	t.Logf("raw graph=%d bytes, retained gzip=%d bytes", len(raw), len(entry.value))
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	interactive, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead()
	warm, err := svc.cachedGraphState(interactive, repo, view, position)
	if err != nil || !reflect.DeepEqual(warm, first) {
		t.Fatalf("large warm graph changed or required occupied bulk capacity: %v", err)
	}
	if len(svc.graphCache.entries) != 1 || &svc.graphCache.entries[0].value[0] != &entry.value[0] {
		t.Fatal("warm read replaced its retained encoding")
	}
	view.Revision.Pending++
	if _, err := svc.cachedGraphState(interactive, repo, view, position); !errors.Is(err, domain.ErrResourceOverloaded) || len(svc.graphCache.building) != 0 {
		t.Fatalf("new revision bypassed bulk admission or leaked its reservation: %v", err)
	}
	releaseRead()
	releaseBulk()
	assertResourcesReleased(t, gate)
}

type graphProjectionCancellationContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining atomic.Int32
}

func (ctx *graphProjectionCancellationContext) Err() error {
	if ctx.remaining.Add(-1) == 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestGraphProjectionEncodingCancellation(t *testing.T) {
	graph := domain.GraphState{Version: 1, ScopeLabels: map[string]string{"main": strings.Repeat("label", 1<<15)}}
	encoded, err := encodeGraphProjection(context.Background(), graph)
	if err != nil {
		t.Fatal(err)
	}
	for _, helper := range []string{"encode", "decode"} {
		for _, check := range []int32{1, 2, 3} {
			t.Run(fmt.Sprintf("%s/cancel-at-check-%d", helper, check), func(t *testing.T) {
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx := &graphProjectionCancellationContext{Context: parent, cancel: cancel}
				ctx.remaining.Store(check)
				if helper == "encode" {
					result, err := encodeGraphProjection(ctx, graph)
					if !errors.Is(err, context.Canceled) || len(result) != 0 {
						t.Fatalf("canceled encoder returned bytes=%d err=%v", len(result), err)
					}
				} else {
					result, err := decodeGraphProjection(ctx, encoded)
					if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(result, domain.GraphState{}) {
						t.Fatalf("canceled decoder exposed a partial graph: %v", err)
					}
				}
			})
		}
	}
}

func graphProjectionTestGzip(t *testing.T, raw io.Reader) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(writer, raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestGraphProjectionEncodingRejectsCorruption(t *testing.T) {
	raw := `{"version":1,"revision":{"graph":"9007199254740993","pending":"7"}}`
	valid := graphProjectionTestGzip(t, strings.NewReader(raw))
	checksum := bytes.Clone(valid)
	checksum[len(checksum)-8] ^= 1
	size := bytes.Clone(valid)
	size[len(size)-4] ^= 1
	for _, scenario := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"not gzip", []byte(raw)},
		{"truncated header", valid[:5]},
		{"truncated deflate", valid[:len(valid)/2]},
		{"missing trailer", valid[:len(valid)-8]},
		{"truncated trailer", valid[:len(valid)-1]},
		{"checksum mismatch", checksum},
		{"size mismatch", size},
		{"empty JSON", graphProjectionTestGzip(t, strings.NewReader(""))},
		{"malformed JSON", graphProjectionTestGzip(t, strings.NewReader(`{"version":}`))},
		{"truncated JSON", graphProjectionTestGzip(t, strings.NewReader(`{"version":1`))},
		{"wrong JSON type", graphProjectionTestGzip(t, strings.NewReader(`{"snapshot_ids":1}`))},
		{"trailing JSON object", graphProjectionTestGzip(t, strings.NewReader(raw+` {}`))},
		{"trailing JSON null", graphProjectionTestGzip(t, strings.NewReader(raw+` null`))},
		{"trailing JSON garbage", graphProjectionTestGzip(t, strings.NewReader(raw+` !`))},
		{"trailing gzip garbage", append(bytes.Clone(valid), []byte("not gzip")...)},
		{"second gzip JSON member", append(bytes.Clone(valid), graphProjectionTestGzip(t, strings.NewReader(`{}`))...)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph, err := decodeGraphProjection(context.Background(), scenario.body)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(graph, domain.GraphState{}) {
				t.Fatalf("corrupt cache entry was accepted or exposed a partial graph: %v", err)
			}
		})
	}
	t.Run("valid JSON whitespace", func(t *testing.T) {
		encoded := graphProjectionTestGzip(t, strings.NewReader(raw+" \t\r\n"))
		graph, err := decodeGraphProjection(context.Background(), encoded)
		if err != nil || graph.Version != 1 || graph.Revision != (domain.RepositoryRevision{Graph: 1<<53 + 1, Pending: 7}) {
			t.Fatalf("valid gzip JSON with trailing whitespace rejected: %v", err)
		}
	})
}

func TestGraphProjectionEncodingDecodeLimits(t *testing.T) {
	t.Run("compressed input", func(t *testing.T) {
		graph, err := decodeGraphProjection(context.Background(), make([]byte, graphProjectionCacheBytes+1))
		if !errors.Is(err, errGraphProjectionCacheLimit) || !reflect.DeepEqual(graph, domain.GraphState{}) {
			t.Fatalf("oversized encoded entry bypassed the compressed budget: %v", err)
		}
	})
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("expanded-budget-plus-%d", extra), func(t *testing.T) {
			padding := strings.Repeat(" ", 32<<10)
			readers := []io.Reader{strings.NewReader(`{}`)}
			for remaining := graphProjectionExpandedBytes + extra - 2; remaining > 0; {
				length := min(remaining, len(padding))
				readers = append(readers, strings.NewReader(padding[:length]))
				remaining -= length
			}
			encoded := graphProjectionTestGzip(t, io.MultiReader(readers...))
			if len(encoded) >= graphProjectionCacheBytes {
				t.Fatal("expansion fixture exceeded the compressed budget")
			}
			graph, err := decodeGraphProjection(context.Background(), encoded)
			if extra == 0 && err != nil {
				t.Fatalf("exact expansion budget rejected: %v", err)
			}
			if extra > 0 && !errors.Is(err, errGraphProjectionCacheLimit) {
				t.Fatalf("expanded trailing whitespace bypassed the budget: %v", err)
			}
			if !reflect.DeepEqual(graph, domain.GraphState{}) {
				t.Fatal("decode returned unexpected graph data")
			}
		})
	}
}

func TestGraphProjectionEncodingWriterCompressedLimit(t *testing.T) {
	writer := &graphProjectionWriter{ctx: context.Background(), writer: io.Discard, remaining: graphProjectionCacheBytes}
	chunk := make([]byte, 32<<10)
	for remaining := graphProjectionCacheBytes - 1; remaining > 0; {
		length := min(remaining, len(chunk))
		written, err := writer.Write(chunk[:length])
		if err != nil || written != length {
			t.Fatalf("write within compressed budget: written=%d err=%v", written, err)
		}
		remaining -= written
	}
	if written, err := writer.Write(chunk[:2]); written != 0 || !errors.Is(err, errGraphProjectionCacheLimit) || writer.remaining != 1 {
		t.Fatalf("over-budget write was partially accepted: written=%d remaining=%d err=%v", written, writer.remaining, err)
	}
	if written, err := writer.Write(chunk[:1]); written != 1 || err != nil || writer.remaining != 0 {
		t.Fatalf("exact compressed budget rejected: written=%d remaining=%d err=%v", written, writer.remaining, err)
	}
	if written, err := writer.Write(chunk[:1]); written != 0 || !errors.Is(err, errGraphProjectionCacheLimit) {
		t.Fatalf("exhausted compressed budget accepted bytes: written=%d err=%v", written, err)
	}
}

func TestGraphProjectionEncodingOversizedGraphSkipsCache(t *testing.T) {
	svc, storage := newFsckSvc(t)
	svc.meta = &graphCacheReadState{FSStore: storage, readOnly: true}
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	repo := hh(t.Name())
	view := graphProjectionEncodingView(repo, 450)
	position := view.History[0].ID
	graph, err := svc.cachedGraphState(ctx, repo, view, position)
	if err != nil {
		t.Fatalf("cache expansion budget rejected a valid graph query: %v", err)
	}
	if graph.Revision != view.Revision || graph.PositionEvent != position || len(graph.SnapshotIDs) != len(view.Snapshots) || len(graph.BranchSnapshots) != len(view.Refs) {
		t.Fatal("uncached graph query lost projection data")
	}
	minimumJSONBytes := 0
	for _, snapshots := range graph.BranchSnapshots {
		if len(snapshots) != len(view.Snapshots) {
			t.Fatal("fixture lost shared branch ancestry")
		}
		minimumJSONBytes += len(snapshots) * (len(view.Snapshots[0].ID) + 3)
	}
	if minimumJSONBytes <= graphProjectionExpandedBytes {
		t.Fatalf("fixture did not exercise the expansion limit: bytes>=%d", minimumJSONBytes)
	}
	encoded, err := encodeGraphProjection(ctx, graph)
	if !errors.Is(err, errGraphProjectionCacheLimit) || len(encoded) != 0 {
		t.Fatalf("encoder failed to enforce expansion budget: bytes=%d err=%v", len(encoded), err)
	}
	if len(svc.graphCache.entries) != 0 || svc.graphCache.bytes != 0 || len(svc.graphCache.building) != 0 {
		t.Fatal("oversized graph retained bytes or leaked its build reservation")
	}
	view.Refs = view.Refs[:1]
	if _, err := svc.cachedGraphState(ctx, repo, view, position); err != nil || len(svc.graphCache.entries) != 1 || len(svc.graphCache.building) != 0 {
		t.Fatalf("oversized query prevented a later cacheable query: %v", err)
	}
	assertResourcesReleased(t, gate)
}
