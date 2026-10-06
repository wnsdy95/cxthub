package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func TestWorkingMemoryStagingRequiresExactOrdinaryWitness(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "memory differs", "alias differs", "unrelated publication"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newFrozenFixture(t)
			f.op.Version = domain.StagingCommitVersion
			op, err := f.store.FinalizeStagingCommit(ctx, f.op)
			if err != nil {
				t.Fatal(err)
			}
			p := op.Position
			witness := domain.StagingObservations(op)[0]
			path := filepath.Join(f.store.storeDir(), "history", witness.ID+".json")
			switch mode {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "memory differs":
				witness.MemoryPinned = false
			case "alias differs":
				witness.LocalBranch = "different"
			case "unrelated publication":
				e := *p.Selection
				e.ID = strings.Repeat("d", 32)
				p.Selection = &e
				if err := f.store.PutWorkingPosition(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "memory differs" || mode == "alias differs" {
				raw, _ := json.Marshal(witness)
				if err := writeAtomic(path, raw); err != nil {
					t.Fatal(err)
				}
			}
			h, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: p.Snapshot, Summary: "first staged memory"})
			if err != nil {
				t.Fatal(err)
			}
			beforeHistory, err := f.store.ListHistoryEvents(ctx, f.repo)
			if err != nil {
				t.Fatal(err)
			}
			c := outbound.WorkingMemoryCommit{RepoID: f.repo, Snapshot: p.Snapshot, Memory: h, ExpectedPosition: &p}
			err = f.store.CommitWorkingMemory(ctx, c)
			if mode == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := f.store.GetWorkingPosition(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if got.Selection.MemorySelectionParent != witness.ID {
					t.Fatal("publish used as authority")
				}
				if err = f.store.CommitWorkingMemory(ctx, c); err != nil {
					t.Fatal("exact retry", err)
				}
				return
			}
			if err == nil {
				t.Fatal("unverified ordinary witness accepted")
			}
			snap, err := f.store.GetSnapshot(ctx, p.Snapshot)
			if err != nil || snap.MemoryHash != "" {
				t.Fatal("failed witness attached memory", err)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, p) {
				t.Fatal("failed witness changed selection", err)
			}
			afterHistory, err := f.store.ListHistoryEvents(ctx, f.repo)
			if err != nil || !reflect.DeepEqual(beforeHistory, afterHistory) {
				t.Fatal("failed witness changed history", err)
			}
			if _, err := os.Stat(f.store.workingMemoryPath()); !os.IsNotExist(err) {
				t.Fatal("failed witness accepted redo", err)
			}
		})
	}
}

func TestWorkingMemoryStagingWitnessRecovery(t *testing.T) {
	for _, prefix := range []string{"accepted", "attachment", "position"} {
		t.Run(prefix, func(t *testing.T) {
			ctx := context.Background()
			f := newFrozenFixture(t)
			f.op.Version = domain.StagingCommitVersion
			op, err := f.store.FinalizeStagingCommit(ctx, f.op)
			if err != nil {
				t.Fatal(err)
			}
			h, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: op.Position.Snapshot, Summary: "first staged memory"})
			if err != nil {
				t.Fatal(err)
			}
			c := outbound.WorkingMemoryCommit{RepoID: f.repo, Snapshot: op.Position.Snapshot, Memory: h, ExpectedPosition: &op.Position}
			j, err := f.store.prepareWorkingMemory(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(j)
			if err = writeAtomic(f.store.workingMemoryPath(), raw); err != nil {
				t.Fatal(err)
			}
			if prefix != "accepted" {
				writeMemoryPrefix(t, f.store, c.Snapshot, c.Memory)
			}
			if prefix == "position" {
				if err = f.store.writePosition(*j.Next); err != nil {
					t.Fatal(err)
				}
			}
			if err = f.store.PutRef(ctx, op.Ref); err != nil {
				t.Fatal(err)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, *j.Next) {
				t.Fatal("wrong recovery after image", err)
			}
			if err = f.store.CommitWorkingMemory(ctx, c); err != nil {
				t.Fatal("retry", err)
			}
		})
	}
}
