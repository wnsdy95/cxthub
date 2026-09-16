package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.WorkingCommitCASStore = (*FileStore)(nil)

func TestWorkingCommitPositionCASPreservesSameRefSelectionWinner(t *testing.T) {
	for _, change := range []string{"none", "memory", "selection", "missing"} {
		t.Run(change, func(t *testing.T) {
			f := newPositionCASFixture(t)
			ctx := context.Background()
			ref := f.ref
			ref.Target = f.expected.Snapshot
			if err := f.store.PutRef(ctx, ref); err != nil {
				t.Fatal(err)
			}
			winner := f.expected
			switch change {
			case "memory", "selection":
				p := f.expected
				if change == "memory" {
					memory, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: p.Snapshot, Summary: "winner"})
					if err != nil {
						t.Fatal(err)
					}
					p.MemoryHash = memory
				}
				p.Selection = nil
				if change == "selection" {
					selection := *f.expected.Selection
					selection.ID = strings.Repeat("e", 32)
					p.Selection = &selection
				}
				if err := f.store.PutWorkingPosition(ctx, p); err != nil {
					t.Fatal(err)
				}
				var err error
				winner, err = f.store.GetWorkingPosition(ctx)
				if err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(f.store.positionPath()); err != nil {
					t.Fatal(err)
				}
			}
			err := f.store.CommitWorkingSnapshotIfCurrent(ctx, f.ref, ref.Target, f.expected, f.next, nil)
			if change == "none" {
				if err != nil {
					t.Fatal(err)
				}
				p, err := f.store.GetWorkingPosition(ctx)
				if err != nil || !reflect.DeepEqual(p, f.next) {
					t.Fatalf("commit position: %+v %v", p, err)
				}
			} else {
				if !errors.Is(err, domain.ErrSyncConflict) {
					t.Fatalf("stale %s commit = %v", change, err)
				}
				p, err := f.store.GetWorkingPosition(ctx)
				if change == "missing" {
					if !errors.Is(err, domain.ErrNotFound) {
						t.Fatalf("missing selection recreated: %+v %v", p, err)
					}
				} else if err != nil || !reflect.DeepEqual(p, winner) {
					t.Fatalf("selection winner lost: %+v %v", p, err)
				}
				gotRef, err := f.store.GetRef(ctx, ref.RepoID, ref.Kind, ref.Name)
				if err != nil || gotRef != ref {
					t.Fatalf("stale commit moved ref: %+v %v", gotRef, err)
				}
			}
			if _, err := os.Stat(f.store.workingCommitPath()); !os.IsNotExist(err) {
				t.Fatalf("journal remains: %v", err)
			}
			peer, err := f.peer.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(peer, f.peerPosition) {
				t.Fatalf("peer moved: %+v %v", peer, err)
			}
		})
	}
}

func TestWorkingCommitPositionCASRecoversBeforeComparing(t *testing.T) {
	for _, positionWritten := range []bool{false, true} {
		t.Run(map[bool]string{false: "journal-only", true: "position-written"}[positionWritten], func(t *testing.T) {
			f := newPositionCASFixture(t)
			ctx := context.Background()
			ref := f.ref
			ref.Target = f.expected.Snapshot
			if err := f.store.PutRef(ctx, ref); err != nil {
				t.Fatal(err)
			}
			op := workingCommit{Ref: f.ref, Expected: ref.Target, Position: f.next}
			raw, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			if positionWritten {
				if err := f.store.putRefRaw(f.ref); err != nil {
					t.Fatal(err)
				}
				if err := f.store.writePosition(f.next); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeAtomic(f.store.workingCommitPath(), raw); err != nil {
				t.Fatal(err)
			}
			attempt := f.next
			selection := *attempt.Selection
			selection.ID = strings.Repeat("d", 32)
			attempt.Selection = &selection
			if err := f.store.CommitWorkingSnapshotIfCurrent(ctx, f.ref, ref.Target, f.expected, attempt, nil); !errors.Is(err, domain.ErrSyncConflict) {
				t.Fatalf("stale capture survived recovery: %v", err)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, f.next) {
				t.Fatalf("accepted journal lost: %+v %v", got, err)
			}
			if _, err := os.Stat(f.store.workingCommitPath()); !os.IsNotExist(err) {
				t.Fatalf("journal not completed: %v", err)
			}
		})
	}
}
