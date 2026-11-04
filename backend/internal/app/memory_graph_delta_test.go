package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryGraphDeltaReplayKeepsCurrentObjects(test *testing.T) {
	ctx := context.Background()
	for _, branch := range []bool{false, true} {
		test.Run(fmt.Sprint(branch), func(test *testing.T) {
			captured, request := memoryBundleFixture(test, branch)
			anchor, err := captured.Export(ctx)
			if err != nil {
				test.Fatal(err)
			}
			base := &memoryGraphAnchor{id: anchor.ID, graph: captured.graph}
			changed := domain.MemoryDigest{SnapshotID: request.Selection.SnapshotID, Summary: "current memory, not the anchor memory"}
			raw, err := json.Marshal(changed)
			if err != nil {
				test.Fatal(err)
			}
			hash := domain.HashContent(raw)
			captured.reader.objects = map[domain.ContentHash]json.RawMessage{hash: raw}
			checkpoint := captured.graph.checkpoint
			checkpoint.Sequence += 2
			var changes []domain.RepositoryViewSourceEntry
			for _, entry := range captured.graph.entries {
				if entry.Kind == "snapshot" && entry.Key == string(request.Selection.SnapshotID) {
					var snapshot domain.Snapshot
					if err := json.Unmarshal(entry.Value, &snapshot); err != nil {
						test.Fatal(err)
					}
					snapshot.MemoryHash = hash
					entry.Value, err = json.Marshal(snapshot)
					if err != nil {
						test.Fatal(err)
					}
					entry.Sequence = checkpoint.Sequence
					changes = append(changes, entry)
				}
			}
			delta := &memoryGraphDelta{}
			pages := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "delta", changes, "")}}
			captured.graph, err = captureGraphSource(ctx, pages, checkpoint.RepoID, &base.graph, delta)
			if err != nil {
				test.Fatal(err)
			}
			captured.generation.Graph = checkpoint
			captured.generation.Sequence++
			bundle, err := captured.exportWithGraphBase(ctx, base, delta)
			if err != nil || bundle.Header.Version != domain.MemoryCaptureDeltaVersion {
				test.Fatal("delta export", err)
			}
			for _, part := range bundle.Parts {
				if strings.HasPrefix(part.Kind, "graph:") {
					test.Fatal("delta copied full graph")
				}
			}
			restored, err := RestoreCapturedEffectiveMemoryWithBase(ctx, bundle, anchor, memoryBundleFactory{})
			if err != nil {
				test.Fatal("replay", err)
			}
			full, err := captured.Export(ctx)
			if err != nil {
				test.Fatal(err)
			}
			fullRestored, err := RestoreCapturedEffectiveMemory(ctx, full, memoryBundleFactory{})
			if err != nil {
				test.Fatal(err)
			}
			want, err := fullRestored.Project(ctx, request)
			if err != nil {
				test.Fatal(err)
			}
			got, err := restored.Project(ctx, request)
			if err != nil || !reflect.DeepEqual(got, want) || len(restored.Dependencies()) != 1 || restored.Dependencies()[0].Hash != hash {
				test.Fatal("delta changed result or inherited stale memory", err)
			}
			if _, err := RestoreCapturedEffectiveMemory(ctx, bundle, memoryBundleFactory{}); !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("delta replay accepted without base", err)
			}
			if _, err := RestoreCapturedEffectiveMemoryWithBase(ctx, bundle, bundle, memoryBundleFactory{}); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("recursive base accepted", err)
			}
			if _, err := RestoreCapturedEffectiveMemoryWithBase(ctx, bundle, full, memoryBundleFactory{}); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("wrong base accepted", err)
			}
			var withoutObjects []domain.MemoryCapturePart
			for _, part := range bundle.Parts {
				if part.Kind != "memory" {
					withoutObjects = append(withoutObjects, part)
				}
			}
			missing, err := domain.SealMemoryCapture(ctx, bundle.Header, withoutObjects)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := RestoreCapturedEffectiveMemoryWithBase(ctx, missing, anchor, memoryBundleFactory{}); err == nil {
				test.Fatal("missing child objects inherited anchor authority")
			}
		})
	}
}

func TestMemoryGraphDeltaCompactsChangesAndTombstones(test *testing.T) {
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	base := CapturedGraphSource{checkpoint: checkpoint, entries: entries}
	checkpoint.Sequence += 3
	changes := []domain.RepositoryViewSourceEntry{
		{Kind: "git_tree", Key: entries[0].Key, Sequence: checkpoint.Sequence - 2, Value: []byte(`{"tree":{"a":"b"}}`)},
		{Kind: "git_tree", Key: entries[0].Key, Sequence: checkpoint.Sequence - 1, Deleted: true},
		{Kind: "git_tree", Key: strings.Repeat("b", 40), Sequence: checkpoint.Sequence, Value: []byte(`{"tree":{}}`)},
	}
	delta := &memoryGraphDelta{}
	pages := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "delta", changes, "")}}
	want, err := captureGraphSource(ctx, pages, checkpoint.RepoID, &base, delta)
	if err != nil || len(delta.images) != 2 {
		test.Fatal("compaction", err)
	}
	bundle := domain.MemoryCaptureBundle{Header: domain.MemoryCaptureHeader{Generation: domain.RepositoryMemorySourceGeneration{Graph: checkpoint}}}
	for _, entry := range delta.entries() {
		bundle.Parts = append(bundle.Parts, domain.MemoryCapturePart{Kind: "graph_delta:" + entry.Kind, Key: entry.Key, Sequence: entry.Sequence, Value: entry.Value})
	}
	got, err := restoreMemoryGraphDelta(ctx, bundle, base)
	if err != nil || !reflect.DeepEqual(got, want) || len(base.entries) != 3 {
		test.Fatal("overlay differed or mutated base", err)
	}
}

func TestMemoryGraphDeltaOverflowFallsBackToFull(test *testing.T) {
	for _, mode := range []string{"rows", "bytes"} {
		test.Run(mode, func(test *testing.T) {
			captured, _ := memoryBundleFixture(test, false)
			full, err := captured.Export(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			delta := &memoryGraphDelta{}
			for index := 0; index <= domain.MaxMemoryCaptureDeltaParts; index++ {
				value := []byte(`{}`)
				if mode == "bytes" {
					value = make([]byte, domain.MaxMemoryCapturePartBytes)
				}
				delta.record(domain.RepositoryViewSourceEntry{Kind: "snapshot", Key: fmt.Sprint(index), Sequence: 9, Value: value})
				if delta.overflow {
					break
				}
			}
			if !delta.overflow || delta.images != nil || delta.bytes != 0 {
				test.Fatal("overflow retained unbounded overlay")
			}
			got, err := captured.exportWithGraphBase(context.Background(), &memoryGraphAnchor{id: full.ID, graph: captured.graph}, delta)
			if err != nil || !reflect.DeepEqual(got, full) {
				test.Fatal("overflow changed full fallback", err)
			}
		})
	}
}
