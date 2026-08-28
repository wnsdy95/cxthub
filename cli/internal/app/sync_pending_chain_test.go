package app

import (
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// pendingChainRemote is a fake that records objects sent by SyncPendings to the remote.
type pendingChainRemote struct {
	outbound.RemoteSync
	manifest       domain.Manifest
	pullRefs       []domain.Ref
	pushes         [][]domain.Snapshot
	pendings       []domain.Pending
	unsyncs        []domain.Unsync
	deletedPending map[string]domain.ContentHash
	onCASDelete    func(sessionID string, expected domain.ContentHash)
}

func (r *pendingChainRemote) RemoteManifest(_ context.Context, _ string) (domain.Manifest, error) {
	return r.manifest, nil
}

func (r *pendingChainRemote) Pull(_ context.Context, _ string, _ map[domain.ContentHash]domain.ContentHash, _ []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	return nil, nil, r.pullRefs, nil
}

func (r *pendingChainRemote) Push(_ context.Context, _ string, snaps []domain.Snapshot, _ []domain.SessionDoc, _ []domain.Ref, _, _ bool) error {
	r.pushes = append(r.pushes, snaps)
	return nil
}

func (r *pendingChainRemote) PushPending(_ context.Context, _ string, p domain.Pending) error {
	r.pendings = append(r.pendings, p)
	return nil
}

func (r *pendingChainRemote) PushUnsync(_ context.Context, _ string, u domain.Unsync) error {
	r.unsyncs = append(r.unsyncs, u)
	return nil
}

func (r *pendingChainRemote) DeleteUnsyncRemote(_ context.Context, _, _ string) error { return nil }

func (r *pendingChainRemote) CompareAndDeletePendingRemote(_ context.Context, _, sessionID string, expected domain.ContentHash) (bool, error) {
	if r.deletedPending == nil {
		r.deletedPending = make(map[string]domain.ContentHash)
	}
	r.deletedPending[sessionID] = expected
	if r.onCASDelete != nil {
		r.onCASDelete(sessionID, expected)
	}
	return true, nil
}

// TestSyncPendingsPushesUnpushedAncestorChain ensures that when pushing a pending target, its unpushed ancestor chain is pushed as well. Pushing the target snapshot alone results in the parent (unpushed commit) being marked as an orphan in the server graph (bug: unpushed sessions showed new contexts as orphans until push, then attached to the old commit).
func TestSyncPendingsPushesUnpushedAncestorChain(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("repo")))

	// Local ancestry: A(server is asleep) ← B(unpushed commit) ← P(pending hook capture).
	mkDoc := func(marker string) domain.ContentHash {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{}}
		doc.CIR.Envelope.CIRVersion = "1"
		doc.CIR.Envelope.SessionOriginID = marker
		h, err := st.PutDoc(ctx, doc)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	a, b, p := mkDoc("a"), mkDoc("b"), mkDoc("p")
	put := func(id domain.ContentHash, parents ...domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repoID, Branch: "main", Parents: parents}); err != nil {
			t.Fatal(err)
		}
	}
	put(a)
	put(b, a)
	put(p, b)
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: b}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: "s1", Branch: "main", Provider: domain.ProviderClaude, Target: p, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	remote := &pendingChainRemote{manifest: domain.Manifest{Refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: a}}}}
	svc := NewSyncRepoService(st, remote, nil)

	if _, err := svc.SyncPendings(ctx, inbound.SyncInput{RepoID: repoID}, nil); err != nil {
		t.Fatal(err)
	}
	if len(remote.pushes) == 0 {
		t.Fatal("pending push did not occur")
	}
	got := map[domain.ContentHash]bool{}
	for _, s := range remote.pushes[0] {
		got[s.ID] = true
	}
	if !got[p] || !got[b] {
		t.Fatalf("missing unpushed ancestor in pending chain push: got %v, want {%s,%s}", remote.pushes[0], p, b)
	}
	if got[a] {
		t.Fatalf("remote already knows ancestor %s included in retransmission target", a)
	}
	if len(remote.pendings) != 1 || remote.pendings[0].Target != p {
		t.Fatalf("missing pending pointer upsert: %v", remote.pendings)
	}
}

func TestSyncPendingsCASResolvesOnlySharedReachableTargets(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("repo-cas")))

	mkDoc := func(marker string) domain.ContentHash {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SessionOriginID: marker}}}
		h, err := st.PutDoc(ctx, doc)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	natural := mkDoc("natural")
	grafted := mkDoc("grafted")
	head := mkDoc("head")
	sessionTarget := mkDoc("session-ref")
	tagOnly := mkDoc("tag-only")
	unresolved := mkDoc("unresolved")
	put := func(id domain.ContentHash, parents, grafts []domain.ContentHash) {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repoID, Branch: "main", Parents: parents, GraftParents: grafts}); err != nil {
			t.Fatal(err)
		}
	}
	put(natural, nil, nil)
	put(grafted, nil, nil)
	put(head, []domain.ContentHash{natural}, []domain.ContentHash{grafted})
	put(sessionTarget, nil, nil)
	put(tagOnly, nil, nil)
	put(unresolved, nil, nil)
	refs := []domain.Ref{
		{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: head},
		{Kind: domain.RefSession, Name: "main/session", RepoID: repoID, Target: sessionTarget},
		{Kind: domain.RefTag, Name: "archive", RepoID: repoID, Target: tagOnly},
	}
	for _, ref := range refs {
		if err := st.PutRef(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	for sid, target := range map[string]domain.ContentHash{
		"natural":    natural,
		"grafted":    grafted,
		"session":    sessionTarget,
		"tag-only":   tagOnly,
		"unresolved": unresolved,
	} {
		if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: sid, Branch: "main", Target: target, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}

	remote := &pendingChainRemote{manifest: domain.Manifest{Refs: refs}}
	svc := NewSyncRepoService(st, remote, nil)
	if _, err := svc.SyncPendings(ctx, inbound.SyncInput{RepoID: repoID}, nil); err != nil {
		t.Fatal(err)
	}
	for sid, target := range map[string]domain.ContentHash{"natural": natural, "grafted": grafted, "session": sessionTarget} {
		if remote.deletedPending[sid] != target {
			t.Errorf("shared %s CAS delete = %s, want %s", sid, remote.deletedPending[sid], target)
		}
	}
	for _, sid := range []string{"tag-only", "unresolved"} {
		if _, deleted := remote.deletedPending[sid]; deleted {
			t.Errorf("non-shared %s pending was deleted", sid)
		}
	}
	remaining, err := st.ListPendings(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.ContentHash{}
	for _, p := range remaining {
		got[p.SessionID] = p.Target
	}
	if len(got) != 2 || got["tag-only"] != tagOnly || got["unresolved"] != unresolved {
		t.Fatalf("remaining pending pointers = %v", got)
	}
	for _, p := range remote.pendings {
		if p.SessionID == "natural" || p.SessionID == "grafted" || p.SessionID == "session" {
			t.Errorf("resolved pending was re-pushed: %+v", p)
		}
	}
}

func TestSyncPendingsCASDoesNotDeleteConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("repo-race")))
	oldTarget := domain.HashContent([]byte("old"))
	newTarget := domain.HashContent([]byte("new"))
	for _, id := range []domain.ContentHash{oldTarget, newTarget} {
		if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repoID, Branch: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: oldTarget}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: "race", Branch: "main", Target: oldTarget}); err != nil {
		t.Fatal(err)
	}
	remote := &pendingChainRemote{manifest: domain.Manifest{Refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: oldTarget}}}}
	remote.onCASDelete = func(sessionID string, _ domain.ContentHash) {
		if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: sessionID, Branch: "main", Target: newTarget, UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewSyncRepoService(st, remote, nil)
	_, _ = svc.SyncPendings(ctx, inbound.SyncInput{RepoID: repoID}, nil)
	remaining, err := st.ListPendings(ctx, repoID)
	if err != nil || len(remaining) != 1 || remaining[0].Target != newTarget {
		t.Fatalf("concurrent replacement lost: %+v err=%v", remaining, err)
	}
}

func TestPullReconcilesPendingMadeReachableByAdoptedRef(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repoID := string(domain.HashContent([]byte("repo-pull-cas")))
	base := domain.HashContent([]byte("pull base"))
	target := domain.HashContent([]byte("pull target"))
	for _, snap := range []domain.Snapshot{
		{ID: base, DocHash: base, RepoID: repoID, Branch: "main"},
		{ID: target, DocHash: target, RepoID: repoID, Branch: "main", Parents: []domain.ContentHash{base}},
	} {
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutRef(ctx, domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: base}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, domain.Pending{RepoID: repoID, SessionID: "pull-session", Branch: "main", Target: target}); err != nil {
		t.Fatal(err)
	}
	remoteRef := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repoID, Target: target}
	remote := &pendingChainRemote{pullRefs: []domain.Ref{remoteRef}}
	out, err := NewSyncRepoService(st, remote, nil).Pull(ctx, inbound.SyncInput{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.NewRefs) != 1 || out.NewRefs[0].Target != target {
		t.Fatalf("remote ref was not adopted: %+v", out.NewRefs)
	}
	if remote.deletedPending["pull-session"] != target {
		t.Fatalf("pull did not CAS-resolve reachable pending: %v", remote.deletedPending)
	}
	pendings, err := st.ListPendings(ctx, repoID)
	if err != nil || len(pendings) != 0 {
		t.Fatalf("reachable local pending remains after pull: %+v err=%v", pendings, err)
	}
}
