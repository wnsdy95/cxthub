package storage

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func replayPersistentFiles(t *testing.T, s *FileStore) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(s.storeDir(), func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(raw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
func prepareRootReplayBoundary(t *testing.T, f frozenFixture, phase string) {
	t.Helper()
	ctx := context.Background()
	if phase == "prepared" {
		if err := f.store.writeStagingOperation(f.op); err != nil {
			t.Fatal(err)
		}
		return
	}
	op, err := f.store.FinalizeStagingCommit(ctx, f.op)
	if err != nil {
		t.Fatal(err)
	}
	if phase == "applied-before-ack" {
		op.LocalFinalized = false
		if err := f.store.writeStagingOperation(op); err != nil {
			t.Fatal(err)
		}
		if err := f.store.writeStaging(f.index); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRootStagingReplayRejectsDamagedClosureWithoutAcknowledgement(t *testing.T) {
	for _, phase := range []string{"prepared", "applied-before-ack", "finalized"} {
		for _, damage := range []string{"corrupt-chunk", "missing-chunk", "wrong-snapshot-identity", "canceled", "memory", "memory-source"} {
			t.Run(phase+"/"+damage, func(t *testing.T) {
				f, chunk := rootFrozenFixture(t, false)
				ctx := context.Background()
				var memory domain.ContentHash
				if damage == "memory" || damage == "memory-source" {
					owner := f.op.Ref.Target
					if damage == "memory-source" {
						owner = f.op.ExpectedRef.Target
						f.op.Position.MemorySource = owner
						f.op.Position.Selection.MemorySource = owner
					}
					var err error
					memory, err = f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner, Summary: "frozen memory"})
					if err != nil {
						t.Fatal(err)
					}
					f.op.Position.MemoryHash = memory
					f.op.Position.Selection.MemoryHash = memory
				}
				prepareRootReplayBoundary(t, f, phase)
				switch damage {
				case "corrupt-chunk":
					if err := os.WriteFile(f.store.objectPath("chunks", chunk), []byte("bad current bytes"), 0600); err != nil {
						t.Fatal(err)
					}
				case "missing-chunk":
					if err := os.Remove(f.store.objectPath("chunks", chunk)); err != nil {
						t.Fatal(err)
					}
				case "wrong-snapshot-identity":
					snap, err := f.store.GetSnapshot(ctx, f.op.Ref.Target)
					if err != nil {
						t.Fatal(err)
					}
					snap.DocIdentity = ""
					raw, err := json.Marshal(snap)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.store.objectPath("snapshots", snap.ID), raw, 0600); err != nil {
						t.Fatal(err)
					}
				case "memory":
					if err := os.WriteFile(f.store.objectPath("memories", memory), []byte("bad memory"), 0600); err != nil {
						t.Fatal(err)
					}
				case "memory-source":
					if err := os.WriteFile(f.store.objectPath("docs", f.op.Position.MemorySource), []byte("bad source"), 0600); err != nil {
						t.Fatal(err)
					}
				case "canceled":
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = canceled
				}
				before := replayPersistentFiles(t, f.store)
				_, err := f.store.ResumeStagingCommit(ctx, f.repo, f.op.ID)
				if err == nil {
					t.Fatal("damaged/canceled root operation acknowledged")
				}
				if after := replayPersistentFiles(t, f.store); !reflect.DeepEqual(before, after) {
					t.Fatal("failed replay mutated index, journal, refs or history/outbox")
				}
			})
		}
	}
}
func TestRootStagingReplayPreservesLaterPositionAndReadd(t *testing.T) {
	for _, phase := range []string{"applied-before-ack", "finalized"} {
		t.Run(phase, func(t *testing.T) {
			f, _ := rootFrozenFixture(t, false)
			ctx := context.Background()
			prepareRootReplayBoundary(t, f, phase)
			winner := f.index
			winner.Sequence++
			winner.Entries = append([]domain.StagedSession{}, winner.Entries...)
			winner.Entries[0].CapturedAt = winner.Entries[0].CapturedAt.Add(time.Second)
			winner = winner.WithRevision()
			if err := f.store.writeStaging(winner); err != nil {
				t.Fatal(err)
			}
			later := f.op.Position
			later.Selection = nil
			later.MemoryPinned = false
			if err := f.store.PutWorkingPosition(ctx, later); err != nil {
				t.Fatal(err)
			}
			later, err := f.store.GetWorkingPosition(ctx)
			if err != nil {
				t.Fatal(err)
			}
			done, err := f.store.ResumeStagingCommit(ctx, f.repo, f.op.ID)
			if err != nil || !done.LocalFinalized {
				t.Fatal(done, err)
			}
			index, position, err := f.store.ReadStaging(ctx, f.repo)
			if err != nil || index.Revision != winner.Revision || !reflect.DeepEqual(position, later) {
				t.Fatal("replayed selection or consumed re-add", err)
			}
		})
	}
}
func TestRootStagingMetadataListDoesNotVerifyHistoricalBodies(t *testing.T) {
	f, chunk := rootFrozenFixture(t, false)
	prepareRootReplayBoundary(t, f, "finalized")
	if err := os.Remove(f.store.objectPath("chunks", chunk)); err != nil {
		t.Fatal(err)
	}
	if ops, err := f.store.ListStagingCommits(context.Background(), f.repo); err != nil || len(ops) != 1 {
		t.Fatal(ops, err)
	}
}

func TestRootStagingReplayRejectsContributionMemory(t *testing.T) {
	f, _ := rootFrozenFixture(t, false)
	ctx := context.Background()
	memory, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: f.op.Ref.Target, Summary: "contribution memory"})
	if err != nil {
		t.Fatal(err)
	}
	contribution := *f.op.Position.Selection
	contribution.ID = strings.Repeat("e", 32)
	contribution.MemoryHash = memory
	f.op.Publications = []domain.HistoryEvent{contribution}
	prepareRootReplayBoundary(t, f, "applied-before-ack")
	if err := os.Remove(f.store.objectPath("memories", memory)); err != nil {
		t.Fatal(err)
	}
	before := replayPersistentFiles(t, f.store)
	if _, err := f.store.ResumeStagingCommit(ctx, f.repo, f.op.ID); err == nil {
		t.Fatal("missing contributor memory acknowledged")
	}
	if !reflect.DeepEqual(before, replayPersistentFiles(t, f.store)) {
		t.Fatal("failed contribution verification mutated persistent state")
	}
}
