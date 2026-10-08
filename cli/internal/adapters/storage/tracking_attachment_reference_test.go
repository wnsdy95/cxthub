package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Keep the existing complete attachment/position/CAS fixture, replacing only
// its chosen and shared documents with real stored roots and a root-owned pin.
func trackingReferenceFixture(t *testing.T, root bool) (trackingFixture, domain.ConversationManifest, map[domain.ContentHash][]byte) {
	t.Helper()
	f := newTrackingFixture(t)
	if !root {
		return f, domain.ConversationManifest{}, nil
	}
	ctx := context.Background()
	repo := f.commit.Attachment.Event.RepoID
	chosen, manifest, bodies := safetyRoot(t, f.store, repo, "tracking chosen root")
	shared, _, _ := safetyRoot(t, f.store, repo, "tracking shared root")
	memory, err := f.store.PutMemory(ctx, domain.MemoryDigest{SnapshotID: chosen.ID, Summary: "root-owned tracking pin"})
	if err != nil {
		t.Fatal(err)
	}
	f.chosen, f.shared = chosen.ID, shared.ID
	a := &f.commit.Attachment
	for i := range a.Proof {
		a.Proof[i].Source, a.Proof[i].Target, a.Proof[i].MemoryHash = chosen.ID, chosen.ID, memory
	}
	a.Event.Source, a.Event.Target, a.Event.SharedTarget, a.Event.MemoryHash = chosen.ID, chosen.ID, shared.ID, memory
	a.ObservedRef.Target = shared.ID
	p := &f.commit.Position.Next
	p.Snapshot, p.SharedTarget, p.MemoryHash = chosen.ID, shared.ID, memory
	p.Selection.Target, p.Selection.SharedTarget, p.Selection.MemoryHash = chosen.ID, shared.ID, memory
	return f, manifest, bodies
}

func TestTrackingReferenceAcceptanceRootAndLegacy(t *testing.T) {
	for _, root := range []bool{false, true} {
		t.Run(fmt.Sprintf("root=%v", root), func(t *testing.T) {
			f, _, _ := trackingReferenceFixture(t, root)
			before, err := f.store.GetSnapshot(context.Background(), f.chosen)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.CommitTrackingAttachment(context.Background(), f.commit); err != nil {
				t.Fatal(err)
			}
			f.applied(t)
			after, err := f.store.GetSnapshot(context.Background(), f.chosen)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("attachment relabeled snapshot", err)
			}
		})
	}
}

func TestTrackingReferenceRejectsCurrentRootBeforeAcceptance(t *testing.T) {
	for _, mode := range []string{"changed-chunk", "missing-chunk", "stripped-scheme", "unknown-scheme"} {
		t.Run(mode, func(t *testing.T) {
			f, manifest, bodies := trackingReferenceFixture(t, true)
			ctx := context.Background()
			snap, err := f.store.GetSnapshot(ctx, f.chosen)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.VerifyStoredDocReference(ctx, snap.DocumentRef()); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "changed-chunk":
				safetyCorruptChunk(t, f.store, manifest, bodies)
			case "missing-chunk":
				if err := os.Remove(f.store.objectPath("chunks", manifest.Chunks[0].Hash)); err != nil {
					t.Fatal(err)
				}
			default:
				if mode == "stripped-scheme" {
					snap.DocIdentity = domain.DocumentIdentityLegacy
				}
				raw, err := json.Marshal(snap)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "unknown-scheme" {
					// The typed writer rejects unknown schemes; corrupt bytes directly.
					raw = bytes.Replace(raw, []byte(domain.DocumentIdentityRootV1), []byte("unknown"), 1)
				}
				if err := writeAtomic(f.store.objectPath("snapshots", snap.ID), raw); err != nil {
					t.Fatal(err)
				}
			}
			before := trackingMutableFiles(t, f.store)
			if err := f.store.CommitTrackingAttachment(ctx, f.commit); err == nil {
				t.Fatal("accepted changed current root")
			}
			if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
				t.Fatal("rejection changed mutable projections")
			}
			if _, err := os.Stat(f.store.trackingAttachmentPath()); !os.IsNotExist(err) {
				t.Fatal("invalid source accepted a journal", err)
			}
		})
	}
}

func TestTrackingReferenceAcceptedRedoUnderCollection(t *testing.T) {
	for _, root := range []bool{false, true} {
		for _, prefix := range []int{0, 6} {
			t.Run(fmt.Sprintf("root=%v/prefix=%d", root, prefix), func(t *testing.T) {
				f, _, _ := trackingReferenceFixture(t, root)
				f.prefix(t, f.journal(t), prefix)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				acquired, err := f.store.TryCollectObjects(ctx, func() error {
					// The reference verifier must not reacquire shared retention
					// while the collector owns it exclusively. Exercise both the
					// cancellable helper and actual accepted-redo entry point.
					j, err := f.store.readTrackingAttachment()
					if err != nil {
						return err
					}
					if err := f.store.verifyTrackingObjects(ctx, j); err != nil {
						return err
					}
					return f.store.withRefMutationLock(ctx, func() error { return nil })
				})
				if err != nil || !acquired {
					t.Fatal("accepted redo nested retention or failed", acquired, err)
				}
				f.applied(t)
			})
		}
	}
}

func TestTrackingReferenceAcceptedRedoRejectsWarmRootChanges(t *testing.T) {
	for _, mode := range []string{"changed-chunk", "missing-chunk", "stripped-scheme"} {
		t.Run(mode, func(t *testing.T) {
			f, manifest, bodies := trackingReferenceFixture(t, true)
			j := f.journal(t) // Successful full current proof before acceptance.
			f.prefix(t, j, 6) // Partial after-image, without completed receipt.
			var restore func()
			if mode == "stripped-scheme" {
				path := f.store.objectPath("snapshots", f.chosen)
				raw, err := readCxtFile(path)
				if err != nil {
					t.Fatal(err)
				}
				snap, err := f.store.GetSnapshot(context.Background(), f.chosen)
				if err != nil {
					t.Fatal(err)
				}
				snap.DocIdentity = domain.DocumentIdentityLegacy
				trackingJSON(t, path, snap)
				restore = func() {
					if err := writeAtomic(path, raw); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				path := f.store.objectPath("chunks", manifest.Chunks[0].Hash)
				raw, err := readCxtFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "changed-chunk" {
					safetyCorruptChunk(t, f.store, manifest, bodies)
				} else if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := writeAtomic(path, raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := trackingMutableFiles(t, f.store)
			accepted, err := readCxtFile(f.store.trackingAttachmentPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.withRefMutationLock(context.Background(), func() error { return nil }); err == nil {
				t.Fatal("redo reused acceptance proof after corruption")
			}
			if !reflect.DeepEqual(before, trackingMutableFiles(t, f.store)) {
				t.Fatal("failed redo changed projections or wrote receipt")
			}
			after, err := readCxtFile(f.store.trackingAttachmentPath())
			if err != nil || string(after) != string(accepted) {
				t.Fatal("failed redo lost accepted intent", err)
			}
			restore()
			if err := f.store.withRefMutationLock(context.Background(), func() error { return nil }); err != nil {
				t.Fatal("restored closure failed retry", err)
			}
			f.applied(t)
		})
	}
}

func TestTrackingReferenceVerificationCancellation(t *testing.T) {
	f, _, _ := trackingReferenceFixture(t, true)
	j := trackingAttachmentJournal{Commit: f.commit}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.store.verifyTrackingObjects(ctx, j); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled proof accepted", err)
	}
}
