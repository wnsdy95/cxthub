package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func safetyRoot(t *testing.T, store *FileStore, repo, text string) (domain.Snapshot, domain.ConversationManifest, map[domain.ContentHash][]byte) {
	t.Helper()
	ctx := context.Background()
	manifest, bodies, err := domain.ConversationManifestForCIR(sampleCIR(text))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for h, body := range bodies {
		if err := store.PutChunk(ctx, h, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutConversationManifest(ctx, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: raw}); err != nil {
		t.Fatal(err)
	}
	snap := domain.Snapshot{ID: hash, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1, RepoID: repo}
	if err := store.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	return snap, manifest, bodies
}

func safetyCorruptChunk(t *testing.T, store *FileStore, m domain.ConversationManifest, bodies map[domain.ContentHash][]byte) (string, []byte) {
	t.Helper()
	if len(m.Chunks) == 0 {
		t.Fatal("fixture requires chunks")
	}
	hash := m.Chunks[0].Hash
	bad := bytes.Clone(bodies[hash])
	bad[len(bad)/2] ^= 1
	path := store.objectPath("chunks", hash)
	inspectionWrite(t, path, docCompress(bad))
	raw, err := readCxtFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func TestLocalRootSafetyCheckout(t *testing.T) {
	for _, mode := range []string{"root", "legacy", "corrupt-target", "wrong-tag", "corrupt-memory-owner"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s := NewWorktreeFileStore(dir, filepath.Join(dir, ".git"), "main", strings.Repeat("a", 40))
			repo := domain.HashContent([]byte(t.Name()))
			snap, m, bodies := safetyRoot(t, s, repo, "checkout root")
			if mode == "legacy" {
				id := repairFixture(t, s, repo, "legacy checkout")
				var err error
				snap, err = s.GetSnapshot(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.ReadCheckoutState(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			owner, om, ob := safetyRoot(t, s, repo, "historical memory owner")
			mh, err := s.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner.ID, Summary: "historical root memory"})
			if err != nil {
				t.Fatal(err)
			}
			change := outbound.CheckoutTransition{RepoID: repo, Expected: before, Head: domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Target: snap.ID}, MemoryPin: &domain.AgentMemoryPin{SnapshotID: owner.ID, MemoryHash: mh}}
			switch mode {
			case "corrupt-target":
				safetyCorruptChunk(t, s, m, bodies)
			case "wrong-tag":
				snap.DocIdentity = domain.DocumentIdentityLegacy
				inspectionWriteJSON(t, s.objectPath("snapshots", snap.ID), snap)
			case "corrupt-memory-owner":
				safetyCorruptChunk(t, s, om, ob)
			}
			err = s.CommitCheckout(ctx, change)
			if mode == "root" || mode == "legacy" {
				if err != nil {
					t.Fatal(err)
				}
				p, err := s.ReadWorkingPosition(ctx, repo)
				if err != nil || p.Snapshot != snap.ID || p.MemoryHash != mh || p.MemorySource != owner.ID || !p.MemoryPinned {
					t.Fatalf("position=%+v err=%v", p, err)
				}
			} else {
				if err == nil {
					t.Fatal("unchecked document accepted")
				}
				after, e := s.ReadCheckoutState(ctx, repo)
				if e != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("failed checkout changed position", e)
				}
				if _, e := os.Stat(s.checkoutJournalPath()); !os.IsNotExist(e) {
					t.Fatal("failed checkout accepted journal", e)
				}
			}
		})
	}
}

func TestLocalRootSafetyCheckoutRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewWorktreeFileStore(dir, filepath.Join(dir, ".git"), "main", strings.Repeat("a", 40))
	repo := domain.HashContent([]byte(t.Name()))
	snap, m, bodies := safetyRoot(t, s, repo, "accepted root")
	owner, _, _ := safetyRoot(t, s, repo, "accepted memory owner")
	mh, err := s.PutMemory(ctx, domain.MemoryDigest{SnapshotID: owner.ID, Summary: "accepted memory"})
	if err != nil {
		t.Fatal(err)
	}
	op := checkoutJournal{Version: 2, Transition: outbound.CheckoutTransition{RepoID: repo, Head: domain.Ref{RepoID: repo, Kind: domain.RefHEAD, Name: "HEAD", Target: snap.ID}, MemoryPin: &domain.AgentMemoryPin{SnapshotID: owner.ID, MemoryHash: mh}}, Position: &domain.WorkingPosition{RepoID: repo, WorktreeID: s.worktreeID, Snapshot: snap.ID, BranchID: "position", LocalBranch: s.gitBranch, GitCommit: s.gitCommit, MemorySource: owner.ID, MemoryHash: mh, MemoryPinned: true, Rewound: true}}
	inspectionWriteJSON(t, s.checkoutJournalPath(), op)
	path, _ := safetyCorruptChunk(t, s, m, bodies)
	if err := s.recoverCheckoutTransition(); err == nil {
		t.Fatal("recovery trusted a prior root proof")
	}
	if _, err := os.Stat(s.checkoutJournalPath()); err != nil {
		t.Fatal("failed replay lost evidence", err)
	}
	if _, err := s.readPosition(); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("failed replay wrote position", err)
	}
	inspectionWrite(t, path, docCompress(bodies[m.Chunks[0].Hash]))
	// Recovery must not recursively acquire a shared lease under collection's exclusive lease.
	acquired, err := s.TryCollectObjects(ctx, func() error { return s.withMutationLock(ctx, "refs", "repo", s.recoverCheckoutTransition) })
	if !acquired || err != nil {
		t.Fatal("recovery under collection lock", acquired, err)
	}
	p, err := s.ReadWorkingPosition(ctx, repo)
	if err != nil || p.Snapshot != snap.ID || p.MemorySource != owner.ID || p.MemoryHash != mh {
		t.Fatal("wrong recovered selection", p, err)
	}
	if _, err := os.Stat(s.checkoutJournalPath()); !os.IsNotExist(err) {
		t.Fatal("completed journal remains", err)
	}
}

func TestLocalRootSafetyReplicaRepair(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	local, source := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	text := strings.Repeat("a", 3*domain.ConversationManifestChunkBytes)
	snap, m, bodies := safetyRoot(t, source, repo, text)
	safetyRoot(t, local, repo, text)
	if len(m.Chunks) < 3 || m.Chunks[1].Hash != m.Chunks[2].Hash {
		t.Fatal("fixture needs repeated dependencies")
	}
	ahead := repairFixture(t, local, repo, "keep local-only history", snap.ID)
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: ahead}
	if err := local.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	beforeAhead, err := readCxtFile(local.objectPath("snapshots", ahead))
	if err != nil {
		t.Fatal(err)
	}
	path, badChunk := safetyCorruptChunk(t, local, m, bodies)
	missing := m.Chunks[len(m.Chunks)-1].Hash
	if missing == m.Chunks[0].Hash {
		t.Fatal("fixture needs a distinct missing chunk")
	}
	if err := os.Remove(local.objectPath("chunks", missing)); err != nil {
		t.Fatal(err)
	}
	badDoc := []byte("damaged descriptor")
	inspectionWrite(t, local.objectPath("docs", snap.DocHash), badDoc)
	wrong := snap
	wrong.DocIdentity = domain.DocumentIdentityLegacy
	inspectionWriteJSON(t, local.objectPath("snapshots", snap.ID), wrong)
	badSnap, err := readCxtFile(local.objectPath("snapshots", snap.ID))
	if err != nil {
		t.Fatal(err)
	}
	unowned := domain.HashContent([]byte("unlisted chunk"))
	inspectionWrite(t, source.objectPath("chunks", unowned), []byte("unverified unlisted source bytes"))
	backup := t.TempDir()
	refs := []domain.Ref{{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: snap.ID}}
	report, err := local.RepairFromReplica(ctx, source, repo, refs, backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.VerifyStoredDocReference(ctx, snap.DocumentRef()); err != nil {
		t.Fatal(err)
	}
	got, err := local.GetSnapshot(ctx, snap.ID)
	if err != nil || got.DocumentRef() != snap.DocumentRef() {
		t.Fatal("identity not restored", got, err)
	}
	if _, err := local.GetDoc(ctx, snap.DocHash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		t.Fatal("root relabeled as legacy", err)
	}
	raw, err := readRootObject(ctx, local.objectPath("docs", snap.DocHash), domain.MaxConversationManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := domain.CanonicalConversationManifest(m)
	if err != nil || !bytes.Equal(raw, expected) {
		t.Fatal("root descriptor changed", err)
	}
	gotRef, err := local.GetRef(ctx, repo, ref.Kind, ref.Name)
	if err != nil || gotRef != ref {
		t.Fatal("local ahead ref replaced", err)
	}
	afterAhead, err := readCxtFile(local.objectPath("snapshots", ahead))
	if err != nil || !bytes.Equal(beforeAhead, afterAhead) {
		t.Fatal("local-only snapshot changed", err)
	}
	if local.HasChunk(unowned) {
		t.Fatal("repair copied an unowned source dependency")
	}
	for path, raw := range map[string][]byte{path: badChunk, local.objectPath("docs", snap.DocHash): badDoc, local.objectPath("snapshots", snap.ID): badSnap} {
		relative, err := filepath.Rel(local.storeDir(), path)
		if err != nil {
			t.Fatal(err)
		}
		saved := filepath.Join(backup, ".cxt", relative+"."+hexOf(domain.HashContent(raw)))
		got, err := readCxtFile(saved)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("quarantine lost predecessor", saved, err)
		}
	}
	seen := map[string]bool{}
	for _, p := range report.Repaired {
		if seen[p] {
			t.Fatal("duplicate dependency repair", p)
		}
		seen[p] = true
	}
	again, err := local.RepairFromReplica(ctx, source, repo, refs, backup)
	if err != nil || len(again.Repaired) != 0 {
		t.Fatal("repair retry rewrote healthy data", again, err)
	}
}

func TestLocalRootSafetyRepairRejectsSource(t *testing.T) {
	for _, mode := range []string{"current-chunk", "wrong-tag"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			repo := domain.HashContent([]byte(t.Name()))
			local, source := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
			snap, m, bodies := safetyRoot(t, source, repo, "source verification")
			safetyRoot(t, local, repo, "source verification")
			if err := source.VerifyStoredDocReference(ctx, snap.DocumentRef()); err != nil {
				t.Fatal(err)
			}
			inspectionWrite(t, local.objectPath("docs", snap.DocHash), []byte("local damage"))
			if mode == "current-chunk" {
				safetyCorruptChunk(t, source, m, bodies)
			} else {
				snap.DocIdentity = domain.DocumentIdentityLegacy
				inspectionWriteJSON(t, source.objectPath("snapshots", snap.ID), snap)
			}
			before := inspectionDiskState(t, filepath.Join(local.storeDir(), "objects"))
			backup := t.TempDir()
			if _, err := local.RepairFromReplica(ctx, source, repo, nil, backup); err == nil {
				t.Fatal("unverified source accepted")
			}
			if !reflect.DeepEqual(before, inspectionDiskState(t, filepath.Join(local.storeDir(), "objects"))) {
				t.Fatal("bad source changed local objects")
			}
			if entries, err := os.ReadDir(backup); err != nil || len(entries) != 0 {
				t.Fatal("bad source wrote quarantine", err)
			}
		})
	}
}

func TestLocalRootSafetyRepairResumesAndRecoversCheckout(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	local, source := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	snap, m, bodies := safetyRoot(t, source, repo, "repair then replay root")
	safetyRoot(t, local, repo, "repair then replay root")
	path, badChunk := safetyCorruptChunk(t, local, m, bodies)
	badDoc := []byte("damaged root manifest")
	inspectionWrite(t, local.objectPath("docs", snap.DocHash), badDoc)
	op := checkoutJournal{Version: 2, Transition: outbound.CheckoutTransition{RepoID: repo, Head: domain.Ref{RepoID: repo, Kind: domain.RefHEAD, Name: "HEAD", Target: snap.ID}}}
	inspectionWriteJSON(t, local.checkoutJournalPath(), op)
	backup := t.TempDir()
	blocked := filepath.Join(backup, ".cxt", "objects", "docs")
	inspectionWrite(t, blocked, []byte("blocked backup directory"))
	report, err := local.RepairFromReplica(ctx, source, repo, nil, backup)
	if err == nil || len(report.Repaired) != 1 {
		t.Fatal("expected interrupted repair after one chunk", report, err)
	}
	got, err := readCxtFile(local.objectPath("docs", snap.DocHash))
	if err != nil || !bytes.Equal(got, badDoc) {
		t.Fatal("descriptor replaced before backup", err)
	}
	if _, err := os.Stat(local.checkoutJournalPath()); err != nil {
		t.Fatal("interrupted repair lost journal", err)
	}
	relative, err := filepath.Rel(local.storeDir(), path)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(backup, ".cxt", relative+"."+hexOf(domain.HashContent(badChunk)))
	if got, err := readCxtFile(saved); err != nil || !bytes.Equal(got, badChunk) {
		t.Fatal("partial repair lost quarantine", err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := local.RepairFromReplica(ctx, source, repo, nil, backup); err != nil {
		t.Fatal(err)
	}
	head, err := local.GetRef(ctx, repo, domain.RefHEAD, "HEAD")
	if err != nil || head.Target != snap.ID {
		t.Fatal("accepted checkout not recovered", head, err)
	}
	if _, err := os.Stat(local.checkoutJournalPath()); !os.IsNotExist(err) {
		t.Fatal("journal remains", err)
	}
	if err := local.VerifyStoredDocReference(ctx, snap.DocumentRef()); err != nil {
		t.Fatal(err)
	}
}

func TestLocalRootSafetyRepairRetention(t *testing.T) {
	for _, side := range []string{"source", "destination"} {
		t.Run(side, func(t *testing.T) {
			ctx := context.Background()
			repo := domain.HashContent([]byte(t.Name()))
			local, source := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
			snap, _, _ := safetyRoot(t, source, repo, "retained repair")
			safetyRoot(t, local, repo, "retained repair")
			damage := []byte("local damage")
			inspectionWrite(t, local.objectPath("docs", snap.DocHash), damage)
			locked := source
			if side == "destination" {
				locked = local
			}
			acquired, err := locked.TryCollectObjects(ctx, func() error {
				limited, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				_, err := local.RepairFromReplica(limited, source, repo, nil, t.TempDir())
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("repair bypassed %s collection: %v", side, err)
				}
				return nil
			})
			if !acquired || err != nil {
				t.Fatal(acquired, err)
			}
			got, err := readCxtFile(local.objectPath("docs", snap.DocHash))
			if err != nil || !bytes.Equal(got, damage) {
				t.Fatal("blocked repair mutated bytes", err)
			}
		})
	}
}

func TestLocalRootSafetyRemoteRepair(t *testing.T) {
	for _, mode := range []string{"ref", "corrupt-ref", "wrong-tag", "memory", "corrupt-memory"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, plan := remoteRepairFixture(t)
			snap, m, bodies := safetyRoot(t, s, plan.RepoID, "remote root repair")
			after := *plan.AfterRef
			after.Target = snap.ID
			plan.AfterRef = &after
			observation := outbound.RemoteObservation{Version: 1, RepoID: plan.RepoID, Remote: plan.Remote, Refs: []domain.Ref{after}}
			if strings.Contains(mode, "memory") {
				mh, err := s.PutMemory(ctx, domain.MemoryDigest{SnapshotID: snap.ID, Summary: "remote root memory"})
				if err != nil {
					t.Fatal(err)
				}
				observed := snap
				observed.MemoryHash = mh
				observation.Snapshots = []domain.Snapshot{observed}
				plan.BeforeRef, plan.AfterRef = nil, nil
				plan.Snapshot, plan.AfterMemory = snap.ID, mh
			}
			if err := s.CompareAndSwapRemoteObservation(ctx, plan.Observation, observation); err != nil {
				t.Fatal(err)
			}
			observed, err := s.ReadRemoteObservation(ctx, plan.RepoID, plan.Remote)
			if err != nil {
				t.Fatal(err)
			}
			plan.Observation = observed.Revision
			plan.ID = outbound.RemoteRepairPlanID(plan)
			if strings.HasPrefix(mode, "corrupt") {
				safetyCorruptChunk(t, s, m, bodies)
			}
			if mode == "wrong-tag" {
				snap.DocIdentity = domain.DocumentIdentityLegacy
				inspectionWriteJSON(t, s.objectPath("snapshots", snap.ID), snap)
			}
			before := inspectionDiskState(t, s.storeDir())
			_, err = s.ApplyRemoteRepair(ctx, plan)
			if mode == "ref" || mode == "memory" {
				if err != nil {
					t.Fatal(err)
				}
				if mode == "memory" {
					got, err := s.GetSnapshot(ctx, snap.ID)
					if err != nil || got.MemoryHash != plan.AfterMemory || got.DocumentRef() != snap.DocumentRef() {
						t.Fatal("memory repair lost reference", got, err)
					}
				} else {
					got, err := s.GetRef(ctx, plan.RepoID, after.Kind, after.Name)
					if err != nil || got != after {
						t.Fatal("root ref not repaired", got, err)
					}
					if err := os.Remove(filepath.Join(s.storeDir(), "repair-receipts", hexOf(plan.ID)+".json")); err != nil {
						t.Fatal(err)
					}
					path, _ := safetyCorruptChunk(t, s, m, bodies)
					if _, err := s.ApplyRemoteRepair(ctx, plan); err == nil {
						t.Fatal("lost receipt replay trusted corrupt current root")
					}
					inspectionWrite(t, path, docCompress(bodies[m.Chunks[0].Hash]))
					if _, err := s.ApplyRemoteRepair(ctx, plan); err != nil {
						t.Fatal("safe receipt retry failed", err)
					}
				}
			} else {
				if err == nil {
					t.Fatal("remote repair adopted unchecked identity")
				}
				// Lock files are coordination only; neither intent nor immutable objects may change.
				now := inspectionDiskState(t, s.storeDir())
				for path, hash := range before {
					if strings.Contains(path, "/locks/") {
						continue
					}
					if now[path] != hash {
						t.Fatal("rejected repair changed", path)
					}
				}
				if _, err := os.Stat(filepath.Join(s.storeDir(), "repair-intents", hexOf(plan.ID)+".json")); !os.IsNotExist(err) {
					t.Fatal("rejected repair accepted intent", err)
				}
			}
		})
	}
}
