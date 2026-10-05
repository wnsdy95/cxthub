package app

import (
	"context"
	"errors"
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

// The manifest deliberately remains at an older tip than Pull. No timing or
// server revision assumptions are needed to expose a separate manifest read.
type resolvingBranchRemote struct {
	outbound.RemoteSync
	manifest                         domain.Manifest
	snapshots                        []domain.Snapshot
	docs                             []domain.SessionDoc
	refs                             []domain.Ref
	history                          []domain.HistoryEvent
	memories                         map[domain.ContentHash]domain.MemoryDigest
	protocolErr, pullErr, historyErr error
	manifestCalls, pullCalls         int
	historyCalls                     int
	pullRepos                        []string
	onPull                           func()
}

func (r *resolvingBranchRemote) RemoteManifest(context.Context, string) (domain.Manifest, error) {
	r.manifestCalls++
	return r.manifest, nil
}

func (r *resolvingBranchRemote) ContextProtocol(context.Context, string) (int, error) {
	return 0, r.protocolErr
}

func (r *resolvingBranchRemote) Pull(_ context.Context, repo string, _ map[domain.ContentHash]domain.ContentHash, _ []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	r.pullCalls++
	r.pullRepos = append(r.pullRepos, repo)
	if r.onPull != nil {
		r.onPull()
	}
	return r.snapshots, r.docs, r.refs, r.pullErr
}

func (r *resolvingBranchRemote) PullHistoryEvents(context.Context, string) ([]domain.HistoryEvent, error) {
	r.historyCalls++
	return r.history, r.historyErr
}

func (*resolvingBranchRemote) PushHistoryEvent(context.Context, domain.HistoryEvent) error {
	panic("branch resolution must not publish history")
}

func (r *resolvingBranchRemote) PullMemory(_ context.Context, _ string, snapshot domain.ContentHash) (domain.MemoryDigest, error) {
	for _, memory := range r.memories {
		if memory.SnapshotID == snapshot {
			return memory, nil
		}
	}
	return domain.MemoryDigest{}, domain.ErrNotFound
}

func (r *resolvingBranchRemote) PullMemoryObject(_ context.Context, _ string, hash domain.ContentHash) (domain.MemoryDigest, error) {
	memory, ok := r.memories[hash]
	if !ok {
		return domain.MemoryDigest{}, domain.ErrNotFound
	}
	return memory, nil
}

type resolvingBranchFixture struct {
	ctx    context.Context
	store  *storage.FileStore
	svc    *SyncRepoService
	remote *resolvingBranchRemote
	repo   string
	root   string
	doc    domain.SessionDoc
	snap   domain.Snapshot
	ref    domain.Ref
}

func newResolvingBranchFixture(t *testing.T) *resolvingBranchFixture {
	t.Helper()
	f := &resolvingBranchFixture{ctx: context.Background(), root: t.TempDir(), repo: string(domain.HashContent([]byte(t.Name())))}
	f.store = storage.NewWorktreeFileStore(f.root, filepath.Join(f.root, ".git"), "main", strings.Repeat("a", 40))
	f.doc = pullDoc(t, "already complete local branch")
	f.snap = domain.Snapshot{ID: f.doc.Hash, DocHash: f.doc.Hash, RepoID: f.repo, Branch: "main"}
	f.ref = domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", BranchID: "original-main", Target: f.snap.ID}
	if _, err := f.store.PutDoc(f.ctx, f.doc); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutSnapshot(f.ctx, f.snap); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutRef(f.ctx, f.ref); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutWorkingPosition(f.ctx, domain.WorkingPosition{RepoID: f.repo, Branch: "main", BranchID: f.ref.BranchID, GitCommit: strings.Repeat("a", 40), Snapshot: f.snap.ID, SharedTarget: f.snap.ID}); err != nil {
		t.Fatal(err)
	}
	f.remote = &resolvingBranchRemote{
		manifest:  domain.Manifest{RepoID: f.repo, Refs: []domain.Ref{f.ref}},
		snapshots: []domain.Snapshot{f.snap}, docs: []domain.SessionDoc{f.doc}, refs: []domain.Ref{f.ref},
	}
	f.svc = newTestSyncService(f.store, f.remote, historyGit{f.repo})
	return f
}

// Capture only applied state. Fetch is allowed to retain immutable objects and
// a remote observation, but must not apply refs, history, metadata or selection.
func (f *resolvingBranchFixture) unchanged(t *testing.T) func() {
	t.Helper()
	read := func() []any {
		t.Helper()
		refs, err := f.store.ListRefs(f.ctx, f.repo)
		if err != nil {
			t.Fatal(err)
		}
		history, err := f.store.ListHistoryEvents(f.ctx, f.repo)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := f.store.GetSnapshot(f.ctx, f.snap.ID)
		if err != nil {
			t.Fatal(err)
		}
		position, err := f.store.GetWorkingPosition(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return []any{refs, history, snapshot, position}
	}
	before := read()
	return func() {
		t.Helper()
		after := read()
		for i, label := range []string{"refs", "history", "snapshot metadata", "working position"} {
			if !reflect.DeepEqual(after[i], before[i]) {
				t.Errorf("resolution applied %s: before=%+v after=%+v", label, before[i], after[i])
			}
		}
	}
}

func (f *resolvingBranchFixture) calls(t *testing.T, pulls, histories int) {
	t.Helper()
	if f.remote.manifestCalls != 0 || f.remote.pullCalls != pulls || f.remote.historyCalls != histories {
		t.Errorf("manifest/pull/history calls = %d/%d/%d, want 0/%d/%d", f.remote.manifestCalls, f.remote.pullCalls, f.remote.historyCalls, pulls, histories)
	}
	for _, repo := range f.remote.pullRepos {
		if repo != f.repo {
			t.Errorf("pull repository = %q, want %q", repo, f.repo)
		}
	}
}

func TestResolveRemoteBranchUsesSingleFetchedRef(t *testing.T) {
	for _, staleLocal := range []bool{true, false} {
		name := "manifest target absent locally"
		if staleLocal {
			name = "manifest target already local"
		}
		t.Run(name, func(t *testing.T) {
			f := newResolvingBranchFixture(t)
			defer f.unchanged(t)()
			if !staleLocal {
				f.remote.manifest.Refs[0].Target = domain.HashContent([]byte("obsolete manifest target"))
			}
			doc := pullDoc(t, "new tip returned by the verified pull")
			snap := domain.Snapshot{ID: doc.Hash, DocHash: doc.Hash, RepoID: f.repo, Branch: "main", Parents: []domain.ContentHash{f.snap.ID}}
			want := f.ref
			want.Target = snap.ID
			other := f.ref
			other.Name = "other"
			f.remote.snapshots, f.remote.docs, f.remote.refs = []domain.Snapshot{snap}, []domain.SessionDoc{doc}, []domain.Ref{other, want}
			// Resolve the repository from Cwd too; a caller's other Ref/Force must
			// not turn this explicit branch lookup into an applying pull.
			got, err := f.svc.ResolveRemoteBranch(f.ctx, inbound.SyncInput{Cwd: f.root, Ref: "other", Force: true}, "main")
			if err != nil || got != want {
				t.Errorf("resolved=%+v err=%v, want fetched ref %+v", got, err, want)
			}
			f.calls(t, 1, 1)
			if _, err := f.store.GetSnapshot(f.ctx, snap.ID); err != nil {
				t.Errorf("fetched target unavailable: %v", err)
			}
			if _, err := f.store.GetDoc(f.ctx, doc.Hash); err != nil {
				t.Errorf("fetched document unavailable: %v", err)
			}
		})
	}
}

func TestResolveRemoteBranchFailsClosedWithCompleteLocalTarget(t *testing.T) {
	for _, pr := range []bool{false, true} {
		name := "branch"
		if pr {
			name = "PR branch"
		}
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{"missing branch", "missing branch before invalid batch", "empty name", "invalid name", "empty target", "wrong repository", "missing target", "missing parent", "tampered document", "duplicate ref", "authentication failure", "pull failure", "history failure", "canceled before fetch", "canceled during pull"} {
				t.Run(mode, func(t *testing.T) {
					f := newResolvingBranchFixture(t)
					defer f.unchanged(t)()
					ctx, cancel := context.WithCancel(f.ctx)
					defer cancel()
					branch, pulls, histories := "main", 1, 0
					var want error
					switch mode {
					case "missing branch", "missing branch before invalid batch":
						f.remote.refs[0].Name = "other"
						want = domain.ErrNotFound
						if mode == "missing branch before invalid batch" {
							// Ref=main must reach Pull's selection check before its
							// batch validation; an unscoped fetch would fail integrity.
							f.remote.docs[0] = pullDoc(t, "tampered")
							f.remote.docs[0].Hash = f.doc.Hash
						}
					case "empty name":
						branch, pulls, want = "", 0, domain.ErrInvalidRef
					case "invalid name":
						branch, pulls, want = "../main", 0, domain.ErrInvalidRef
					case "empty target":
						f.remote.refs[0].Target = ""
						want = domain.ErrInvalidRef
					case "wrong repository":
						f.remote.refs[0].RepoID = string(domain.HashContent([]byte("other repo")))
						want = domain.ErrHashMismatch
					case "missing target":
						f.remote.refs[0].Target = domain.HashContent([]byte("missing snapshot"))
						want = domain.ErrHashMismatch
					case "missing parent":
						f.remote.snapshots[0].Parents = []domain.ContentHash{domain.HashContent([]byte("missing parent"))}
						want = domain.ErrHashMismatch
					case "tampered document":
						f.remote.docs[0] = pullDoc(t, "tampered")
						f.remote.docs[0].Hash = f.doc.Hash
						want = domain.ErrHashMismatch
					case "duplicate ref":
						f.remote.refs = append(f.remote.refs, f.ref)
						want = domain.ErrHashMismatch
					case "authentication failure":
						want = errors.New("access revoked")
						f.remote.protocolErr, pulls = want, 0
					case "pull failure":
						want = errors.New("fresh pull unavailable")
						f.remote.pullErr = want
					case "history failure":
						want = errors.New("fresh history unavailable")
						f.remote.historyErr, histories = want, 1
					case "canceled before fetch":
						cancel()
						pulls, want = 0, context.Canceled
					case "canceled during pull":
						f.remote.onPull, want = cancel, context.Canceled
					}
					resolve := f.svc.ResolveRemoteBranch
					if pr {
						resolve = f.svc.ResolveRemotePRBranch
					}
					got, err := resolve(ctx, inbound.SyncInput{RepoID: f.repo}, branch)
					if !errors.Is(err, want) || got != (domain.Ref{}) {
						t.Errorf("resolved=%+v err=%v, want zero ref and %v", got, err, want)
					}
					f.calls(t, pulls, histories)
				})
			}
		})
	}
}

func (f *resolvingBranchFixture) bindingEvents() (birth, rename, archive, rebirth domain.HistoryEvent) {
	birth = domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: f.repo, BranchID: f.ref.BranchID, Branch: "main", Kind: "birth", Target: f.snap.ID, CreatedAt: time.Unix(100, 0).UTC()}
	rename = birth
	rename.ID, rename.Kind, rename.Branch, rename.PreviousBranch, rename.BindingParent = strings.Repeat("2", 32), "rename", "renamed", "main", birth.ID
	archive = birth
	archive.ID, archive.Kind, archive.BindingParent = strings.Repeat("3", 32), "archive", birth.ID
	rebirth = birth
	rebirth.ID, rebirth.BranchID, rebirth.BindingParent = strings.Repeat("4", 32), "replacement-main", archive.ID
	return
}

func TestResolveRemotePRBranchUsesRemoteAndLocalHistory(t *testing.T) {
	for _, mode := range []string{"no binding history", "unchanged birth", "legacy ref without identity", "identical duplicated birth", "remote archive", "remote rename", "remote rebirth at same target", "remote rename and reuse at same target", "local archive", "local rename", "conflicting remote duplicate", "conflicting local duplicate", "missing binding dependency", "missing name dependency", "cyclic dependencies", "active identity differs from ref"} {
		t.Run(mode, func(t *testing.T) {
			f := newResolvingBranchFixture(t)
			birth, rename, archive, rebirth := f.bindingEvents()
			f.remote.history = []domain.HistoryEvent{birth}
			var local []domain.HistoryEvent
			wantConflict := true
			switch mode {
			case "no binding history":
				f.remote.history, wantConflict = nil, false
			case "unchanged birth":
				wantConflict = false
			case "legacy ref without identity":
				f.remote.refs[0].BranchID, wantConflict = "", false
			case "identical duplicated birth":
				f.remote.history, local, wantConflict = []domain.HistoryEvent{birth, birth}, []domain.HistoryEvent{birth}, false
			case "remote archive":
				f.remote.history = []domain.HistoryEvent{archive, birth}
			case "remote rename":
				f.remote.history = []domain.HistoryEvent{rename, birth}
			case "remote rebirth at same target":
				f.remote.history = []domain.HistoryEvent{rebirth, archive, birth}
				f.remote.refs[0].BranchID = rebirth.BranchID
			case "remote rename and reuse at same target":
				rebirth.BindingParent = rename.ID
				f.remote.history = []domain.HistoryEvent{rebirth, rename, birth}
				f.remote.refs[0].BranchID = rebirth.BranchID
			case "local archive":
				local = []domain.HistoryEvent{birth, archive}
			case "local rename":
				local = []domain.HistoryEvent{birth, rename}
			case "conflicting remote duplicate":
				conflict := birth
				conflict.BranchID = "different-identity"
				f.remote.history = append(f.remote.history, conflict)
			case "conflicting local duplicate":
				local = []domain.HistoryEvent{birth}
				f.remote.history[0].BranchID = "different-identity"
			case "missing binding dependency":
				f.remote.history = []domain.HistoryEvent{rename}
			case "missing name dependency":
				rename.NameParent = strings.Repeat("f", 32)
				f.remote.history = []domain.HistoryEvent{birth, rename}
			case "cyclic dependencies":
				birth.BindingParent = rename.ID
				f.remote.history = []domain.HistoryEvent{birth, rename}
			case "active identity differs from ref":
				// Ref and history are separate reads. Even the same target cannot
				// make different active identities a safe name-only PR binding.
				f.remote.history[0].BranchID = "different-identity"
			}
			for _, event := range local {
				if err := f.store.PutHistoryEvent(f.ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			defer f.unchanged(t)()
			got, err := f.svc.ResolveRemotePRBranch(f.ctx, inbound.SyncInput{RepoID: f.repo}, "main")
			if wantConflict {
				if !errors.Is(err, domain.ErrSyncConflict) {
					t.Errorf("unsafe name-only resolution=%+v err=%v, want sync conflict", got, err)
				}
			} else if err != nil || got != f.remote.refs[0] {
				t.Errorf("safe resolution=%+v err=%v, want %+v", got, err, f.remote.refs[0])
			}
			f.calls(t, 1, 1)
			observation, err := f.store.ReadRemoteObservation(f.ctx, f.repo, "configured")
			if err != nil || !reflect.DeepEqual(observation.History, f.remote.history) {
				t.Errorf("fetch did not retain its history observation: %+v %v", observation.History, err)
			}
		})
	}
}

func TestResolveRemotePRBranchUsesHistoryFromItsOwnFetch(t *testing.T) {
	for _, fetchedArchive := range []bool{true, false} {
		name := "fetched archive survives later safe observation"
		if !fetchedArchive {
			name = "fetched safe history survives later archive observation"
		}
		t.Run(name, func(t *testing.T) {
			f := newResolvingBranchFixture(t)
			defer f.unchanged(t)()
			birth, _, archive, _ := f.bindingEvents()
			safe, released := []domain.HistoryEvent{birth}, []domain.HistoryEvent{birth, archive}
			f.remote.history = safe
			replacement := released
			if fetchedArchive {
				f.remote.history, replacement = released, safe
			}
			replacements := 0
			in := inbound.SyncInput{RepoID: f.repo, Progress: func(p inbound.SyncProgress) {
				if p.Operation != "pull" || p.Phase != "complete" {
					return
				}
				// Model another fetch replacing the shared observation at the
				// final progress boundary, without goroutines or sleep-based races.
				observation, err := f.store.ReadRemoteObservation(f.ctx, f.repo, "configured")
				if err != nil {
					t.Fatal(err)
				}
				previous := observation.Revision
				observation.History = replacement
				if err := f.store.CompareAndSwapRemoteObservation(f.ctx, previous, observation); err != nil {
					t.Fatal(err)
				}
				replacements++
			}}
			got, err := f.svc.ResolveRemotePRBranch(f.ctx, in, "main")
			if fetchedArchive {
				if !errors.Is(err, domain.ErrSyncConflict) {
					t.Errorf("own fetched archive lost: ref=%+v err=%v", got, err)
				}
			} else if err != nil || got != f.ref {
				t.Errorf("unrelated later observation changed resolution: ref=%+v err=%v", got, err)
			}
			if replacements != 1 {
				t.Errorf("observation replacements=%d, want one", replacements)
			}
			f.calls(t, 1, 1)
		})
	}
}

func TestResolveRemoteBranchDoesNotAdoptMemoryOrGraft(t *testing.T) {
	f := newResolvingBranchFixture(t)
	rootMemory := domain.MemoryDigest{SnapshotID: f.snap.ID, Summary: "locally selected memory"}
	oldHash := putMemoryObject(t, f.ctx, f.store, rootMemory)
	if err := f.store.CompareAndSwapSnapshotMemory(f.ctx, f.snap.ID, "", oldHash); err != nil {
		t.Fatal(err)
	}
	position, err := f.store.GetWorkingPosition(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	position.MemorySource, position.MemoryHash, position.MemoryPinned = f.snap.ID, oldHash, true
	if err := f.store.PutWorkingPosition(f.ctx, position); err != nil {
		t.Fatal(err)
	}
	defer f.unchanged(t)()
	newMemory := domain.MemoryDigest{SnapshotID: f.snap.ID, Summary: "remote memory descendant", PreviousMemoryHash: oldHash}
	newHash, err := domain.MemoryDigestHash(newMemory)
	if err != nil {
		t.Fatal(err)
	}
	f.remote.memories = map[domain.ContentHash]domain.MemoryDigest{newHash: newMemory}
	parentDoc := pullDoc(t, "remote graft parent")
	parent := domain.Snapshot{ID: parentDoc.Hash, DocHash: parentDoc.Hash, RepoID: f.repo}
	f.remote.snapshots[0].MemoryHash = newHash
	f.remote.snapshots[0].GraftParents = []domain.ContentHash{parent.ID}
	f.remote.snapshots[0].GraftSeq, f.remote.snapshots[0].Grafted = 1, true
	f.remote.snapshots = append(f.remote.snapshots, parent)
	f.remote.docs = append(f.remote.docs, parentDoc)
	birth, _, _, _ := f.bindingEvents()
	f.remote.history = []domain.HistoryEvent{birth}
	got, err := f.svc.ResolveRemoteBranch(f.ctx, inbound.SyncInput{RepoID: f.repo}, "main")
	if err != nil || got != f.ref {
		t.Errorf("resolution=%+v err=%v", got, err)
	}
	f.calls(t, 1, 1)
	observation, err := f.store.ReadRemoteObservation(f.ctx, f.repo, "configured")
	if err != nil {
		t.Fatal(err)
	}
	observed := false
	for _, snap := range observation.Snapshots {
		if snap.ID == f.snap.ID {
			observed = reflect.DeepEqual(snap, f.remote.snapshots[0])
		}
	}
	if !observed || !reflect.DeepEqual(observation.History, f.remote.history) {
		t.Errorf("remote metadata/history not retained in observation: %+v", observation)
	}
	if memory, err := f.store.GetMemory(f.ctx, newHash); err != nil || !reflect.DeepEqual(memory, newMemory) {
		t.Errorf("immutable remote memory not fetched: %+v %v", memory, err)
	}
}

func TestSyncPullReturnsFetchedHistory(t *testing.T) {
	for _, fetchOnly := range []bool{true, false} {
		name := "full pull"
		if fetchOnly {
			name = "fetch only"
		}
		t.Run(name, func(t *testing.T) {
			f := newResolvingBranchFixture(t)
			birth, _, _, _ := f.bindingEvents()
			f.remote.history = []domain.HistoryEvent{birth}
			if fetchOnly {
				defer f.unchanged(t)()
			}
			out, err := f.svc.Pull(f.ctx, inbound.SyncInput{RepoID: f.repo, Ref: "main", FetchOnly: fetchOnly})
			if err != nil || !reflect.DeepEqual(out.FetchedHistory, f.remote.history) || !reflect.DeepEqual(out.FetchedRefs, f.remote.refs) {
				t.Errorf("pull omitted fetched evidence: %+v err=%v", out, err)
			}
			f.calls(t, 1, 1)
		})
	}
}
