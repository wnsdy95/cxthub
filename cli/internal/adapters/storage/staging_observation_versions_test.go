package storage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestStagingObservationVersionsRecovery(t *testing.T) {
	for _, version := range []int{1, domain.StagingCommitVersion} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newFrozenFixture(t)
			op := f.op
			op.Version = version
			pub := *op.Position.Selection
			pub.ID = strings.Repeat("c", 32)
			op.Publications = []domain.HistoryEvent{pub}
			if err := f.store.writeStagingOperation(op); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			for attempt := 0; attempt < 2; attempt++ {
				got, err := f.store.ResumeStagingCommit(ctx, f.repo, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Version != version || !reflect.DeepEqual(got.Publications, op.Publications) {
					t.Fatal("recovery changed durable version or publication")
				}
				history, err := f.store.ListHistoryEvents(ctx, f.repo)
				if err != nil {
					t.Fatal(err)
				}
				want := domain.StagingObservations(op)
				var positions []domain.HistoryEvent
				for _, e := range history {
					if e.Kind == "position" && e.Target == op.Position.Snapshot {
						positions = append(positions, e)
					}
				}
				if len(positions) != len(want) {
					t.Fatalf("version %d retry %d positions=%d want %d", version, attempt, len(positions), len(want))
				}
				for _, e := range want {
					found := false
					for _, p := range positions {
						if reflect.DeepEqual(e, p) {
							found = true
						}
					}
					if !found {
						t.Fatal("recovery changed observation identity or payload")
					}
				}
			}
		})
	}
}

func TestStagingObservationVersionsValidation(t *testing.T) {
	for name, mutate := range map[string]func(*domain.StagingCommit){
		"unknown version": func(op *domain.StagingCommit) { op.Version = domain.StagingCommitVersion + 1 },
		"branch":          func(op *domain.StagingCommit) { op.Position.Selection.Branch = "other" },
		"alias":           func(op *domain.StagingCommit) { op.Position.Selection.LocalBranch = "other" },
		"source":          func(op *domain.StagingCommit) { op.Position.Selection.Source = op.ExpectedPosition.Snapshot },
		"unpinned":        func(op *domain.StagingCommit) { op.Position.Selection.MemoryPinned = false },
		"cursor unpinned": func(op *domain.StagingCommit) { op.Position.MemoryPinned = false },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFrozenFixture(t)
			op := f.op
			op.Version = domain.StagingCommitVersion
			mutate(&op)
			want := domain.ErrHashMismatch
			if name == "unknown version" {
				want = domain.ErrStagingVersion
			}
			if _, err := f.store.FinalizeStagingCommit(context.Background(), op); err == nil || (name != "source" && !errors.Is(err, want)) {
				t.Fatalf("got %v want %v", err, want)
			}
			p, err := f.store.GetWorkingPosition(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p, f.position) {
				t.Fatal("rejected operation changed selection")
			}
		})
	}
}
