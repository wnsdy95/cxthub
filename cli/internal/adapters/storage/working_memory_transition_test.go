package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func TestWorkingMemoryFirstSelectionRecordsOnlyExactPredecessor(t *testing.T) {
	for _, mode := range []string{"exact", "reopened", "rewound", "changed code", "unrelated selection", "nonempty predecessor", "nonroot memory"} {
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
				copy := *p.Selection
				copy.ID = strings.Repeat("c", 32)
				copy.LocalBranch = "another-alias"
				p.Selection = &copy
			}
			var previous domain.ContentHash
			if mode == "nonempty predecessor" || mode == "nonroot memory" {
				var err error
				previous, err = f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: p.Snapshot, Summary: "previous"})
				if err != nil {
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
			if err := f.store.CompareAndSwapSnapshotMemory(ctx, p.Snapshot, "", memory); err != nil {
				t.Fatal(err)
			}
			if mode == "reopened" {
				f.store = NewWorktreeFileStore(f.store.repoRoot, filepath.Join(f.store.repoRoot, ".git"), "main", f.store.gitCommit)
			}
			if err := f.store.RecordWorkingMemory(ctx, p.Snapshot, memory); err != nil {
				t.Fatal(err)
			}
			after, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			wantEdge := mode == "exact" || mode == "reopened"
			if after.Selection.BindingParent != "" {
				t.Fatal("memory capture invented an identity dependency")
			}
			if err := domain.ValidateHistoryEvent(*after.Selection); err != nil {
				t.Fatal(err)
			}
			if (after.Selection.MemorySelectionParent == before.Selection.ID) != wantEdge {
				t.Fatalf("causal edge mismatch: mode=%s parent=%s", mode, after.Selection.MemorySelectionParent)
			}
			if err := f.store.RecordWorkingMemory(ctx, p.Snapshot, memory); err != nil {
				t.Fatal(err)
			}
			again, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(after, again) {
				t.Fatalf("retry changed immutable selection: %v", err)
			}
			peer, err := f.peer.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(peer, f.peerPosition) {
				t.Fatalf("another worktree was repinned: %v", err)
			}
		})
	}
}
