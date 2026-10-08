package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var errPendingMemoryUnavailable = errors.New("synthetic memory attachment unavailable")

// Snapshot creation deliberately does not attach memory, matching the server.
// Only the existing causal attachment fake can advance the memory pointer.
type pendingMemoryRemote struct {
	retryPendingRemote
	memory                    causalPushRemote
	steps                     []string
	failMemory                bool
	manifestReads             int
	refWrites, historyWrites  int
	afterMemory, afterPending func()
}

func (r *pendingMemoryRemote) PreflightDocumentReferences(context.Context, string, []domain.DocumentRef) error {
	return nil
}

func (r *pendingMemoryRemote) RemoteManifest(ctx context.Context, repo string) (domain.Manifest, error) {
	r.manifestReads++
	m, err := r.memory.RemoteManifest(ctx, repo)
	m.Refs = r.manifest.Refs
	return m, err
}

func (r *pendingMemoryRemote) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	r.refWrites += len(refs)
	clean := append([]domain.Snapshot(nil), snaps...)
	for i := range clean {
		clean[i].MemoryHash = ""
		r.steps = append(r.steps, "snapshot")
	}
	return r.retryPendingRemote.Push(ctx, repo, clean, docs, refs, force, appendMode)
}

func (r *pendingMemoryRemote) PushDocChunks(ctx context.Context, _ string, doc outbound.DocumentChunks) (bool, error) {
	if doc.Representation.Identity == domain.DocumentIdentityLegacy {
		return false, nil
	}
	m, err := doc.Representation.ConversationManifest()
	if err != nil {
		return false, err
	}
	if err := domain.VerifyConversationManifest(ctx, doc.Representation.Hash, m, doc.ReadChunk); err != nil {
		return false, err
	}
	if r.docs == nil {
		r.docs = map[domain.ContentHash]bool{}
	}
	r.docs[doc.Representation.Hash] = true
	return true, nil
}

func (r *pendingMemoryRemote) PushMemory(ctx context.Context, repo string, d domain.MemoryDigest) error {
	r.steps = append(r.steps, "memory")
	if r.snapshots[d.SnapshotID] == "" {
		return errors.New("memory before snapshot")
	}
	if r.failMemory {
		return errPendingMemoryUnavailable
	}
	if err := r.memory.PushMemory(ctx, repo, d); err != nil {
		return err
	}
	if r.afterMemory != nil {
		r.afterMemory()
	}
	return nil
}

func (r *pendingMemoryRemote) PullMemory(ctx context.Context, repo string, id domain.ContentHash) (domain.MemoryDigest, error) {
	return r.memory.PullMemory(ctx, repo, id)
}

func (r *pendingMemoryRemote) PullMemoryObject(ctx context.Context, repo string, h domain.ContentHash) (domain.MemoryDigest, error) {
	return r.memory.PullMemoryObject(ctx, repo, h)
}

func (r *pendingMemoryRemote) PushPending(ctx context.Context, repo string, p domain.Pending) error {
	r.steps = append(r.steps, "pending")
	if err := r.retryPendingRemote.PushPending(ctx, repo, p); err != nil {
		return err
	}
	if r.afterPending != nil {
		r.afterPending()
	}
	return nil
}

func (r *pendingMemoryRemote) PushHistoryEvent(context.Context, domain.HistoryEvent) error {
	r.historyWrites++
	return errors.New("unexpected history write")
}

func pendingMemoryFixture(t *testing.T, root, known bool) (*storage.FileStore, *pendingMemoryRemote, inbound.SyncInput, domain.Snapshot) {
	t.Helper()
	ctx := context.Background()
	path := t.TempDir()
	st := storage.NewFileStore(path)
	repo := domain.HashContent([]byte(t.Name()))
	snap := domain.Snapshot{RepoID: repo, Branch: "main", SessionID: "live"}
	if root {
		rep, bodies, _ := p4AppRoot(t)
		for h, b := range bodies {
			if err := st.PutChunk(ctx, h, b); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.PutConversationManifest(ctx, rep); err != nil {
			t.Fatal(err)
		}
		snap.ID, snap.DocHash, snap.DocIdentity = rep.Hash, rep.Hash, rep.Identity
	} else {
		snap.ID = pendingRetrySnapshot(t, st, repo, "pending memory")
		snap.DocHash = snap.ID
	}
	snap.MemoryHash = putMemoryObject(t, ctx, st, domain.MemoryDigest{SnapshotID: snap.ID, Summary: "inherited memory"})
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, domain.Pending{RepoID: repo, SessionID: "live", Target: snap.ID}); err != nil {
		t.Fatal(err)
	}
	r := &pendingMemoryRemote{memory: causalPushRemote{objects: map[domain.ContentHash]domain.MemoryDigest{}}}
	if known {
		r.snapshots = map[domain.ContentHash]domain.ContentHash{snap.ID: snap.DocHash}
		r.docs = map[domain.ContentHash]bool{snap.DocHash: true}
		r.manifest.Refs = []domain.Ref{{Kind: domain.RefTag, Name: "archive", Target: snap.ID}}
	}
	t.Cleanup(func() {
		if r.refWrites != 0 || r.historyWrites != 0 {
			t.Errorf("pending moved refs/history: %d/%d", r.refWrites, r.historyWrites)
		}
	})
	return st, r, inbound.SyncInput{RepoID: repo, Cwd: path, PendingSessionID: "live"}, snap
}

func TestSyncPendingsMemoryFreshAndKnown(t *testing.T) {
	for _, root := range []bool{false, true} {
		for _, known := range []bool{false, true} {
			name := "legacy"
			if root {
				name = "root"
			}
			if known {
				name += "/known"
			} else {
				name += "/fresh"
			}
			t.Run(name, func(t *testing.T) {
				st, remote, in, snap := pendingMemoryFixture(t, root, known)
				n, err := newTestSyncService(st, remote, nil).SyncPendings(context.Background(), in, nil)
				if err != nil || n != 1 || remote.memory.current != snap.MemoryHash {
					t.Fatalf("count=%d error=%v attachment=%s want=%s", n, err, remote.memory.current, snap.MemoryHash)
				}
				want := []string{"memory", "pending"}
				if !known {
					want = append([]string{"snapshot"}, want...)
				}
				if !reflect.DeepEqual(remote.steps, want) {
					t.Fatalf("order=%v want=%v", remote.steps, want)
				}
				if remote.manifestReads != 1 {
					t.Fatalf("duplicate manifest scan: %d", remote.manifestReads)
				}
				remote.steps = nil
				if n, err := newTestSyncService(st, remote, nil).SyncPendings(context.Background(), in, nil); err != nil || n != 1 {
					t.Fatal(n, err)
				}
				if !reflect.DeepEqual(remote.steps, []string{"pending"}) {
					t.Fatalf("known attachment retransmitted: %v", remote.steps)
				}
				if remote.manifestReads != 2 {
					t.Fatalf("duplicate retry manifest scan: %d", remote.manifestReads)
				}
			})
		}
	}
}

func TestSyncPendingsMemoryCorruptionBlocksKnownObjectRepair(t *testing.T) {
	for _, root := range []bool{false, true} {
		name := "legacy"
		if root {
			name = "root"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, remote, in, snap := pendingMemoryFixture(t, root, true)
			path := filepath.Join(in.Cwd, ".cxt", "objects", "memories", strings.TrimPrefix(snap.MemoryHash, "sha256:"))
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			corrupt := strings.Replace(string(original), "inherited memory", "corrupted memory", 1)
			if corrupt == string(original) {
				t.Fatal("fixture did not corrupt memory")
			}
			if err := os.WriteFile(path, []byte(corrupt), 0600); err != nil {
				t.Fatal(err)
			}
			svc := newTestSyncService(st, remote, nil)
			if n, err := svc.SyncPendings(ctx, in, nil); n != 0 || !errors.Is(err, domain.ErrHashMismatch) {
				t.Fatalf("corruption acknowledged: %d %v", n, err)
			}
			if len(remote.steps) != 0 || remote.memory.current != "" {
				t.Fatalf("corrupt memory caused remote effects: %v", remote.steps)
			}
			pending, err := st.ListPendings(ctx, in.RepoID)
			if err != nil || len(pending) != 1 || pending[0].Target != snap.ID {
				t.Fatal(pending, err)
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if n, err := svc.SyncPendings(ctx, in, nil); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			if remote.memory.current != snap.MemoryHash || !reflect.DeepEqual(remote.steps, []string{"memory", "pending"}) {
				t.Fatalf("zero-wants repair failed: %v", remote.steps)
			}
		})
	}
}

func TestSyncPendingsMemoryFailureRetriesUnchangedCapture(t *testing.T) {
	for _, root := range []bool{false, true} {
		name := "legacy"
		if root {
			name = "root"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, remote, in, snap := pendingMemoryFixture(t, root, false)
			// A remote archival ref enables the chain path without resolving the pending.
			base := pendingRetrySnapshot(t, st, in.RepoID, "base")
			snap.Parents = []domain.ContentHash{base}
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			remote.manifest.Refs = []domain.Ref{{Kind: domain.RefTag, Name: "base", Target: base}}
			remote.failMemory = true
			svc := newTestSyncService(st, remote, nil)
			if n, err := svc.SyncPendings(ctx, in, nil); n != 0 || !errors.Is(err, errPendingMemoryUnavailable) {
				t.Fatalf("failed memory acknowledged: %d %v", n, err)
			}
			if !reflect.DeepEqual(remote.steps, []string{"snapshot", "memory"}) || remote.attempts != 0 {
				t.Fatalf("memory failure fell back to pointer: %v", remote.steps)
			}
			pending, err := st.ListPendings(ctx, in.RepoID)
			if err != nil || len(pending) != 1 || pending[0].Target != snap.ID {
				t.Fatal(pending, err)
			}
			remote.failMemory, remote.steps = false, nil
			if n, err := svc.SyncPendings(ctx, in, nil); err != nil || n != 1 {
				t.Fatal(n, err)
			}
			if remote.memory.current != snap.MemoryHash || !reflect.DeepEqual(remote.steps, []string{"memory", "pending"}) {
				t.Fatalf("retry=%v memory=%s", remote.steps, remote.memory.current)
			}
		})
	}
}

func TestSyncPendingsMemoryRemoteCausality(t *testing.T) {
	for _, relation := range []string{"descendant", "fork"} {
		t.Run(relation, func(t *testing.T) {
			ctx := context.Background()
			st, remote, in, snap := pendingMemoryFixture(t, true, true)
			local, err := st.GetMemory(ctx, snap.MemoryHash)
			if err != nil {
				t.Fatal(err)
			}
			d := domain.MemoryDigest{SnapshotID: snap.ID, Summary: "remote"}
			if relation == "descendant" {
				d.PreviousMemoryHash = snap.MemoryHash
			}
			h, err := domain.MemoryDigestHash(d)
			if err != nil {
				t.Fatal(err)
			}
			remote.memory.current = h
			remote.memory.objects[h], remote.memory.objects[snap.MemoryHash] = d, local
			n, err := newTestSyncService(st, remote, nil).SyncPendings(ctx, in, nil)
			if relation == "fork" {
				if n != 0 || !errors.Is(err, domain.ErrSyncConflict) || remote.attempts != 0 {
					t.Fatal(n, err, remote.attempts)
				}
			} else if err != nil || n != 1 || remote.attempts != 1 {
				t.Fatal(n, err, remote.attempts)
			}
			if remote.memory.current != h || len(remote.memory.pushes) != 0 {
				t.Fatal("remote attachment rewound")
			}
		})
	}
}

func TestSyncPendingsMemoryConcurrentLocalChange(t *testing.T) {
	for _, at := range []string{"before-pointer", "during-ack"} {
		for _, change := range []string{"target", "branch", "activity", "updated-at", "author", "dismissed", "memory", "resolved"} {
			t.Run(at+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				st, remote, in, snap := pendingMemoryFixture(t, true, false)
				activity := time.Unix(100, 0).UTC()
				want := domain.Pending{RepoID: in.RepoID, SessionID: "live", Target: snap.ID, Branch: "main", UpdatedAt: activity, ActivityAt: &activity}
				if _, err := st.ReplacePending(ctx, want); err != nil {
					t.Fatal(err)
				}
				mutate := func() {
					remote.afterMemory, remote.afterPending = nil, nil
					switch change {
					case "target":
						want.Target = pendingRetrySnapshot(t, st, in.RepoID, "replacement")
					case "branch":
						want.Branch = "feature"
					case "activity":
						next := activity.Add(time.Second)
						want.ActivityAt = &next
					case "updated-at":
						want.UpdatedAt = want.UpdatedAt.Add(time.Second)
					case "author":
						want.Author = domain.TeamIdentity{Name: "synthetic author"}
					case "dismissed":
						want.Dismissed = true
					case "memory":
						next := putMemoryObject(t, ctx, st, domain.MemoryDigest{SnapshotID: snap.ID, PreviousMemoryHash: snap.MemoryHash, Summary: "new local memory"})
						if err := st.CompareAndSwapSnapshotMemory(ctx, snap.ID, snap.MemoryHash, next); err != nil {
							t.Fatal(err)
						}
						return
					case "resolved":
						if _, err := st.CompareAndDeletePending(ctx, in.RepoID, "live", snap.ID); err != nil {
							t.Fatal(err)
						}
						return
					}
					if _, err := st.ReplacePending(ctx, want); err != nil {
						t.Fatal(err)
					}
				}
				if at == "before-pointer" {
					remote.afterMemory = mutate
				} else {
					remote.afterPending = mutate
				}
				svc := newTestSyncService(st, remote, nil)
				n, err := svc.SyncPendings(ctx, in, nil)
				wantAttempts := 0
				if at == "during-ack" {
					wantAttempts = 1
				}
				if remote.attempts != wantAttempts {
					t.Fatalf("pointer attempts=%d want=%d before retry", remote.attempts, wantAttempts)
				}
				if change == "resolved" {
					if n != 0 || err != nil {
						t.Fatal(n, err)
					}
				} else {
					if n != 0 || !errors.Is(err, domain.ErrSyncConflict) {
						t.Fatalf("stale upload marked complete: %d %v", n, err)
					}
					current, err := st.ListPendings(ctx, in.RepoID)
					if err != nil || len(current) != 1 || !reflect.DeepEqual(current[0], want) {
						t.Fatalf("local replacement lost: %v %v", current, err)
					}
					if n, err := svc.SyncPendings(ctx, in, nil); n != 1 || err != nil {
						t.Fatalf("unchanged capture retry: %d %v", n, err)
					}
					if remote.pointers["live"] != want.Target || !reflect.DeepEqual(remote.pendings[len(remote.pendings)-1], want) {
						t.Fatalf("retry published stale pending: %v want=%v", remote.pendings, want)
					}
					if change == "memory" {
						latest, err := st.GetSnapshot(ctx, snap.ID)
						if err != nil || remote.memory.current != latest.MemoryHash {
							t.Fatal(latest.MemoryHash, err)
						}
					}
				}
			})
		}
	}
}

func TestSyncPendingsMemoryEquivalentMetadataTimes(t *testing.T) {
	for _, at := range []string{"before-pointer", "during-ack"} {
		t.Run(at, func(t *testing.T) {
			ctx := context.Background()
			st, remote, in, snap := pendingMemoryFixture(t, true, false)
			activity := time.Unix(100, 0).UTC()
			p := domain.Pending{RepoID: in.RepoID, SessionID: "live", Target: snap.ID, UpdatedAt: activity, ActivityAt: &activity}
			if _, err := st.ReplacePending(ctx, p); err != nil {
				t.Fatal(err)
			}
			mutate := func() {
				otherZone := activity.In(time.FixedZone("fixture", -7*60*60))
				p.UpdatedAt, p.ActivityAt = otherZone, &otherZone
				if _, err := st.ReplacePending(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			if at == "before-pointer" {
				remote.afterMemory = mutate
			} else {
				remote.afterPending = mutate
			}
			if n, err := newTestSyncService(st, remote, nil).SyncPendings(ctx, in, nil); err != nil || n != 1 {
				t.Fatalf("equivalent timestamps conflicted: %d %v", n, err)
			}
		})
	}
}

func TestSyncPendingsMemoryScopedSharedResolutionFailure(t *testing.T) {
	ctx := context.Background()
	st, remote, in, snap := pendingMemoryFixture(t, false, true)
	if err := st.PutRef(ctx, domain.Ref{RepoID: in.RepoID, Kind: domain.RefBranch, Name: "main", Target: snap.ID}); err != nil {
		t.Fatal(err)
	}
	remote.failCAS = true
	if n, err := newTestSyncService(st, remote, nil).SyncPendings(ctx, in, nil); n != 0 || err == nil {
		t.Fatalf("unresolved obligation acknowledged: %d %v", n, err)
	}
	if len(remote.steps) != 0 {
		t.Fatal(remote.steps)
	}
	remote.failCAS = false
	if n, err := newTestSyncService(st, remote, nil).SyncPendings(ctx, in, nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestSyncPendingsMemoryUnscopedPartialSuccess(t *testing.T) {
	ctx := context.Background()
	st, remote, in, _ := pendingMemoryFixture(t, false, false)
	other := pendingRetrySnapshot(t, st, in.RepoID, "independent without memory")
	if err := st.PutPending(ctx, domain.Pending{RepoID: in.RepoID, SessionID: "other", Target: other}); err != nil {
		t.Fatal(err)
	}
	remote.failMemory = true
	in.PendingSessionID = ""
	if n, err := newTestSyncService(st, remote, nil).SyncPendings(ctx, in, nil); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if len(remote.pointers) != 1 || remote.pointers["other"] != other {
		t.Fatal(remote.pointers)
	}
}
