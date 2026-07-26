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
	manifest domain.Manifest
	pushes   [][]domain.Snapshot
	pendings []domain.Pending
	unsyncs  []domain.Unsync
}

func (r *pendingChainRemote) RemoteManifest(_ context.Context, _ string) (domain.Manifest, error) {
	return r.manifest, nil
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

func (r *pendingChainRemote) DeletePendingRemote(_ context.Context, _, _ string) error { return nil }
func (r *pendingChainRemote) DeleteUnsyncRemote(_ context.Context, _, _ string) error  { return nil }

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
