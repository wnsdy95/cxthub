package storage

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestWorkingMemoryFirstSelectionRecordsOnlyExactPredecessor(t *testing.T) {
	for _, mode := range []string{"exact", "rewound", "changed code", "unrelated selection", "nonempty predecessor", "nonroot memory"} {
		t.Run(mode, func(t *testing.T) {
			f := newPositionCASFixture(t)
			ctx := context.Background()
			p := f.expected
			p.Rewound = mode == "rewound"
			if mode == "changed code" {
				p.GitCommit = strings.Repeat("b", 40)
				p.Selection = nil
			}
			if mode == "unrelated selection" {
				e := *p.Selection
				e.ID = strings.Repeat("c", 32)
				e.LocalBranch = "another-alias"
				p.Selection = &e
			}
			var previous domain.ContentHash
			if mode == "nonempty predecessor" || mode == "nonroot memory" {
				var err error
				previous, err = f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: p.Snapshot, Summary: "previous"})
				if err != nil {
					t.Fatal(err)
				}
				if err = f.store.CompareAndSwapSnapshotMemory(ctx, p.Snapshot, "", previous); err != nil {
					t.Fatal(err)
				}
				if mode == "nonempty predecessor" {
					p.MemoryHash, p.Selection = previous, nil
				}
			}
			if err := f.store.PutWorkingPosition(ctx, p); err != nil {
				t.Fatal(err)
			}
			before, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			memory, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: p.Snapshot, PreviousMemoryHash: previous, Summary: "memorized"})
			if err != nil {
				t.Fatal(err)
			}
			c := outbound.WorkingMemoryCommit{RepoID: p.RepoID, Snapshot: p.Snapshot, ExpectedMemory: previous, Memory: memory, ExpectedPosition: &before}
			err = f.store.CommitWorkingMemory(ctx, c)
			if mode == "unrelated selection" {
				if !errors.Is(err, domain.ErrHashMismatch) {
					t.Fatalf("contradictory cursor: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if after.Selection.BindingParent != "" {
				t.Fatal("invented identity dependency")
			}
			if (after.Selection.MemorySelectionParent == before.Selection.ID) != (mode == "exact") {
				t.Fatalf("causal edge mode=%s", mode)
			}
			if mode == "rewound" || mode == "changed code" {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("checkpoint repinned")
				}
			}
			if err = f.store.CommitWorkingMemory(ctx, c); err != nil {
				t.Fatalf("exact retry: %v", err)
			}
			again, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(after, again) {
				t.Fatalf("retry changed selection: %v", err)
			}
			peer, err := f.peer.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(peer, f.peerPosition) {
				t.Fatalf("other worktree moved: %v", err)
			}
		})
	}
}
