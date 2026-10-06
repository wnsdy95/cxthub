package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func memoryCommitFixture(t *testing.T) (positionCASFixture, outbound.WorkingMemoryCommit) {
	t.Helper()
	f := newPositionCASFixture(t)
	p := f.expected
	p.Rewound = false
	if err := f.store.PutWorkingPosition(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h, err := f.store.PutMemory(context.Background(), domain.MemoryDigest{SnapshotID: p.Snapshot, Summary: "root"})
	if err != nil {
		t.Fatal(err)
	}
	return f, outbound.WorkingMemoryCommit{RepoID: p.RepoID, Snapshot: p.Snapshot, Memory: h, ExpectedPosition: &p}
}
func acceptMemoryFixture(t *testing.T, f positionCASFixture, c outbound.WorkingMemoryCommit) workingMemory {
	t.Helper()
	j, err := f.store.prepareWorkingMemory(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeAtomic(f.store.workingMemoryPath(), raw); err != nil {
		t.Fatal(err)
	}
	return j
}
func writeMemoryPrefix(t *testing.T, s *FileStore, id, h domain.ContentHash) {
	t.Helper()
	snap, err := s.GetSnapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	snap.MemoryHash = h
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeAtomic(s.objectPath("snapshots", id), raw); err != nil {
		t.Fatal(err)
	}
}

func TestWorkingMemoryRecoveryEveryPrefix(t *testing.T) {
	for _, prefix := range []string{"accepted", "predecessor", "successor", "attachment", "position"} {
		t.Run(prefix, func(t *testing.T) {
			f, c := memoryCommitFixture(t)
			ctx := context.Background()
			j := acceptMemoryFixture(t, f, c)
			if prefix != "accepted" {
				if err := f.store.putHistoryEvent(*c.ExpectedPosition.Selection); err != nil {
					t.Fatal(err)
				}
			}
			if prefix == "successor" || prefix == "attachment" || prefix == "position" {
				if err := f.store.putHistoryEvent(*j.Next.Selection); err != nil {
					t.Fatal(err)
				}
			}
			if prefix == "attachment" || prefix == "position" {
				writeMemoryPrefix(t, f.store, c.Snapshot, c.Memory)
			}
			if prefix == "position" {
				if err := f.store.writePosition(*j.Next); err != nil {
					t.Fatal(err)
				}
			}
			// An unrelated worktree's ordinary ref writer recovers the owner's cursor.
			if err := f.peer.PutRef(ctx, f.ref); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(f.store.workingMemoryPath()); !os.IsNotExist(err) {
				t.Fatalf("redo not completed: %v", err)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, *j.Next) {
				t.Fatalf("wrong after-image: %v", err)
			}
			snap, err := f.store.GetSnapshot(ctx, c.Snapshot)
			if err != nil || snap.MemoryHash != c.Memory {
				t.Fatalf("attachment missing: %v", err)
			}
			peer, err := f.peer.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(peer, f.peerPosition) {
				t.Fatalf("recovery repinned peer: %v", err)
			}
			ref, err := f.store.GetRef(ctx, f.ref.RepoID, f.ref.Kind, f.ref.Name)
			if err != nil || ref != f.ref {
				t.Fatalf("shared ref changed: %v", err)
			}
			events, err := f.store.ListHistoryEvents(ctx, c.RepoID)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.CommitWorkingMemory(ctx, c); err != nil {
				t.Fatalf("lost acknowledgement retry: %v", err)
			}
			again, err := f.store.ListHistoryEvents(ctx, c.RepoID)
			if err != nil || !reflect.DeepEqual(events, again) {
				t.Fatalf("retry changed history: %v", err)
			}
			// A newer explicit same-tuple selection must fence even the exact old request.
			user := got
			e := *got.Selection
			e.ID = strings.Repeat("d", 32)
			e.MemorySelectionParent = ""
			user.Selection = &e
			if err = f.store.PutWorkingPosition(ctx, user); err != nil {
				t.Fatal(err)
			}
			if err = f.store.CommitWorkingMemory(ctx, c); !errors.Is(err, domain.ErrSelectionChanged) {
				t.Fatalf("old receipt retargeted new user selection: %v", err)
			}
		})
	}
}

func TestWorkingMemoryWritersRecoverBeforeAdvancing(t *testing.T) {
	for _, writer := range []string{"cas", "put-snapshot"} {
		t.Run(writer, func(t *testing.T) {
			f, c := memoryCommitFixture(t)
			j := acceptMemoryFixture(t, f, c)
			ctx := context.Background()
			h2, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: c.Snapshot, PreviousMemoryHash: c.Memory, Summary: "later"})
			if err != nil {
				t.Fatal(err)
			}
			if writer == "cas" {
				err = f.peer.CompareAndSwapSnapshotMemory(ctx, c.Snapshot, c.Memory, h2)
			} else {
				snap, e := f.store.GetSnapshot(ctx, c.Snapshot)
				if e != nil {
					t.Fatal(e)
				}
				snap.MemoryHash = c.Memory
				err = f.peer.PutSnapshot(ctx, snap)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(got, *j.Next) {
				t.Fatalf("writer skipped recovery: %v", err)
			}
			if writer == "cas" {
				// Pull-style attachment advance deliberately leaves the old pinned memory.
				snap, err := f.store.GetSnapshot(ctx, c.Snapshot)
				if err != nil || snap.MemoryHash != h2 {
					t.Fatal("later attachment missing")
				}
				c.ExpectedMemory, c.Memory, c.ExpectedPosition = h2, h2, &got
				if err = f.store.CommitWorkingMemory(ctx, c); err != nil {
					t.Fatal(err)
				}
				got, err = f.store.GetWorkingPosition(ctx)
				if err != nil || got.MemoryHash != h2 {
					t.Fatalf("no-op attachment did not repin causally: %v", err)
				}
			}
		})
	}
}

func TestWorkingMemoryAttachmentOnlyNeverDiscoversPosition(t *testing.T) {
	f, c := memoryCommitFixture(t)
	ctx := context.Background()
	c.ExpectedPosition = nil
	j := acceptMemoryFixture(t, f, c)
	if j.Next != nil {
		t.Fatal("nil expectation discovered cursor")
	}
	p, err := f.peer.GetWorkingPosition(ctx)
	if err != nil {
		t.Fatal(err)
	} // triggers recovery
	if !reflect.DeepEqual(p, f.peerPosition) {
		t.Fatal("peer moved")
	}
	owner, err := f.store.GetWorkingPosition(ctx)
	if err != nil || owner.MemoryHash != "" {
		t.Fatalf("attachment-only repinned: %v", err)
	}
	if err = f.store.CommitWorkingMemory(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func TestWorkingMemoryRejectsBeforeAcceptance(t *testing.T) {
	for _, mode := range []string{"selection", "memory-source", "foreign-owner", "wrong-attachment-parent", "sibling", "immutable-collision"} {
		t.Run(mode, func(t *testing.T) {
			f, c := memoryCommitFixture(t)
			ctx := context.Background()
			switch mode {
			case "selection":
				p := *c.ExpectedPosition
				e := *p.Selection
				e.ID = strings.Repeat("e", 32)
				p.Selection = &e
				if err := f.store.PutWorkingPosition(ctx, p); err != nil {
					t.Fatal(err)
				}
			case "memory-source":
				c.ExpectedPosition.MemorySource = f.ref.Target
			case "foreign-owner":
				h, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.ref.Target, Summary: "foreign"})
				if err != nil {
					t.Fatal(err)
				}
				c.Memory = h
			case "wrong-attachment-parent":
				c.ExpectedMemory = c.Memory
				h, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: c.Snapshot, Summary: "sibling"})
				if err != nil {
					t.Fatal(err)
				}
				c.Memory = h
			case "sibling":
				p := *c.ExpectedPosition
				h, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: c.Snapshot, Summary: "other selected root"})
				if err != nil {
					t.Fatal(err)
				}
				p.MemoryHash, p.Selection = h, nil
				if err = f.store.PutWorkingPosition(ctx, p); err != nil {
					t.Fatal(err)
				}
				p, err = f.store.GetWorkingPosition(ctx)
				if err != nil {
					t.Fatal(err)
				}
				c.ExpectedPosition = &p
			case "immutable-collision":
				j, err := f.store.prepareWorkingMemory(ctx, c)
				if err != nil {
					t.Fatal(err)
				}
				e := *j.Next.Selection
				e.GitAfter = strings.Repeat("b", 40)
				if err = f.store.PutHistoryEvent(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			before, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.store.CommitWorkingMemory(ctx, c); err == nil {
				t.Fatal("unsafe operation accepted")
			}
			if _, err = os.Stat(f.store.workingMemoryPath()); !os.IsNotExist(err) {
				t.Fatal("failed acceptance left redo")
			}
			snap, err := f.store.GetSnapshot(ctx, c.Snapshot)
			if err != nil || snap.MemoryHash != "" {
				t.Fatalf("failed acceptance attached: %v", err)
			}
			after, err := f.store.GetWorkingPosition(ctx)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed acceptance repinned: %v", err)
			}
		})
	}
}

func TestWorkingMemoryPendingDoctorPinsAndRepairGuard(t *testing.T) {
	f, c := memoryCommitFixture(t)
	acceptMemoryFixture(t, f, c)
	ctx := context.Background()
	pinned, err := f.store.HasWorkingMemoryPin(ctx, c.Snapshot)
	if err != nil || !pinned {
		t.Fatalf("redo root not retained: %v", err)
	}
	report := f.store.InspectReplica(ctx)
	if !strings.Contains(strings.Join(report.Issues, " "), "pending local transaction: working-memory.json") {
		t.Fatal("doctor missed redo")
	}
	destination := NewFileStore(t.TempDir())
	if _, err = destination.RepairFromReplica(ctx, f.store, c.RepoID, nil, filepath.Join(t.TempDir(), "quarantine")); err == nil {
		t.Fatal("pending source accepted for repair")
	}
	if err = writeAtomic(f.store.workingMemoryPath(), []byte(`{"version":1,"commit":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.HasWorkingMemoryPin(ctx, c.Snapshot); err == nil {
		t.Fatal("corrupt redo permits collection")
	}
	if err = f.store.PutRef(ctx, f.ref); err == nil {
		t.Fatal("writer ignored corrupt redo")
	}
	if _, err = os.Stat(f.store.workingMemoryPath()); err != nil {
		t.Fatal("corrupt evidence removed")
	}
}

func TestWorkingMemoryRecoveryRefusesForeignAfterImage(t *testing.T) {
	f, c := memoryCommitFixture(t)
	acceptMemoryFixture(t, f, c)
	// Unsupported/corrupt on-disk changes cannot be mistaken for our after-image.
	p := *c.ExpectedPosition
	e := *p.Selection
	e.ID = strings.Repeat("f", 32)
	p.Selection = &e
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeAtomic(f.store.positionPath(), raw); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.GetWorkingPosition(context.Background()); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatalf("foreign cursor overwritten: %v", err)
	}
	snap, err := f.store.GetSnapshot(context.Background(), c.Snapshot)
	if err != nil || snap.MemoryHash != "" {
		t.Fatal("foreign cursor caused partial apply")
	}
}

func TestWorkingMemoryConcurrentCommitChoosesOneWholePair(t *testing.T) {
	f, c := memoryCommitFixture(t)
	ctx := context.Background()
	other := c
	h, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: c.Snapshot, Summary: "competing root"})
	if err != nil {
		t.Fatal(err)
	}
	other.Memory = h
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, commit := range []outbound.WorkingMemoryCommit{c, other} {
		go func(commit outbound.WorkingMemoryCommit) { <-start; done <- f.store.CommitWorkingMemory(ctx, commit) }(commit)
	}
	close(start)
	one, two := <-done, <-done
	if (one == nil) == (two == nil) {
		t.Fatalf("expected one accepted operation: %v / %v", one, two)
	}
	snap, err := f.store.GetSnapshot(ctx, c.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.store.GetWorkingPosition(ctx)
	if err != nil || p.MemoryHash != snap.MemoryHash || p.Selection.MemoryHash != snap.MemoryHash {
		t.Fatalf("torn winning pair: %v", err)
	}
	if p.MemoryHash != c.Memory && p.MemoryHash != h {
		t.Fatal("unrecognized winner")
	}
	if _, err = os.Stat(f.store.workingMemoryPath()); !os.IsNotExist(err) {
		t.Fatal("losing operation left redo")
	}
}
