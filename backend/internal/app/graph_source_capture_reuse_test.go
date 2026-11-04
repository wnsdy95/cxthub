package app

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type unchangedGraphCaptureSource struct {
	page domain.RepositoryViewSourcePage
}

func (source unchangedGraphCaptureSource) RepositoryGraphSourceChanges(context.Context, domain.ContentHash, domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	return source.page, nil
}

func TestGraphCaptureEmptyDeltaReusesImmutableEntries(test *testing.T) {
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	baseline := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", entries, "")}}
	previous, err := CaptureGraphSource(ctx, baseline, checkpoint.RepoID, nil)
	if err != nil {
		test.Fatal(err)
	}
	next := checkpoint
	next.Sequence++
	unchanged, err := CaptureGraphSource(ctx, unchangedGraphCaptureSource{page: graphCapturePage(next, "delta", nil, "")}, next.RepoID, &previous)
	if err != nil || unchanged.Checkpoint() != next || !reflect.DeepEqual(unchanged.entries, previous.entries) {
		test.Fatal("empty delta changed retained inputs", err)
	}
	if &unchanged.entries[0] != &previous.entries[0] {
		test.Fatal("empty delta copied immutable entries")
	}
	changed := next
	changed.Sequence++
	delta := []domain.RepositoryViewSourceEntry{{Sequence: changed.Sequence, Kind: "git_tree", Key: entries[0].Key, Deleted: true}}
	updated, err := CaptureGraphSource(ctx, unchangedGraphCaptureSource{page: graphCapturePage(changed, "delta", delta, "")}, next.RepoID, &unchanged)
	if err != nil || len(updated.entries) != len(previous.entries)-1 || len(unchanged.entries) != len(previous.entries) {
		test.Fatal("later delta mutated retained inputs", err)
	}
	if previous.Checkpoint() != checkpoint || unchanged.Checkpoint() != next || previous.entries[0].Kind != "git_tree" {
		test.Fatal("delta mutated an earlier capture")
	}
}

func TestGraphCaptureEmptyDeltaAllocationCostIsIndependentOfGraphSize(test *testing.T) {
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	source := unchangedGraphCaptureSource{page: graphCapturePage(checkpoint, "delta", nil, "")}
	measure := func(count int) float64 {
		captured := CapturedGraphSource{checkpoint: checkpoint, entries: make([]domain.RepositoryViewSourceEntry, count)}
		for index := range captured.entries {
			captured.entries[index] = entries[0]
			captured.entries[index].Key = fmt.Sprintf("%040x", index)
		}
		return testing.AllocsPerRun(10, func() {
			result, err := CaptureGraphSource(ctx, source, checkpoint.RepoID, &captured)
			if err != nil || len(result.entries) != count {
				test.Fatal("empty delta failed", err)
			}
		})
	}
	small, large := measure(10), measure(10000)
	test.Logf("empty delta allocations: 10 entries %.0f, 10000 entries %.0f", small, large)
	if large > small+4 {
		test.Fatalf("unchanged capture allocation cost grew with graph size: small=%.0f large=%.0f", small, large)
	}
}
