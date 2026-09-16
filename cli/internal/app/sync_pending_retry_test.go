package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type retryPendingRemote struct {
	pendingChainRemote
	failPointer      bool
	attempts         int
	snapshots        map[domain.ContentHash]domain.ContentHash
	docs             map[domain.ContentHash]bool
	pointers         map[string]domain.ContentHash
	failCAS          bool
	failFirstPointer bool
}

func (r *retryPendingRemote) NegotiatePushObjects(_ context.Context, _ string, snapshots, docs []domain.ContentHash) (outbound.PushObjectWants, error) {
	var wants outbound.PushObjectWants
	for _, h := range snapshots {
		if r.snapshots[h] == "" {
			wants.Snapshots = append(wants.Snapshots, h)
		}
	}
	for _, h := range docs {
		if !r.docs[h] {
			wants.Docs = append(wants.Docs, h)
		}
	}
	return wants, nil
}

func (r *retryPendingRemote) Push(ctx context.Context, repoID string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	if r.snapshots == nil {
		r.snapshots = map[domain.ContentHash]domain.ContentHash{}
	}
	if r.docs == nil {
		r.docs = map[domain.ContentHash]bool{}
	}
	for _, snap := range snaps {
		r.snapshots[snap.ID] = snap.DocHash
	}
	for _, doc := range docs {
		r.docs[doc.Hash] = true
	}
	return r.pendingChainRemote.Push(ctx, repoID, snaps, docs, refs, force, appendMode)
}

func (r *retryPendingRemote) PushPending(ctx context.Context, repoID string, p domain.Pending) error {
	r.attempts++
	if !r.docs[r.snapshots[p.Target]] {
		return errors.New("pointer published before objects")
	}
	if r.failPointer || (r.failFirstPointer && r.attempts == 1) {
		return errors.New("pointer upload unavailable")
	}
	if r.pointers == nil {
		r.pointers = map[string]domain.ContentHash{}
	}
	r.pointers[p.SessionID] = p.Target
	return r.pendingChainRemote.PushPending(ctx, repoID, p)
}

func (r *retryPendingRemote) CompareAndDeletePendingRemote(ctx context.Context, repo, session string, expected domain.ContentHash) (bool, error) {
	if r.failCAS {
		return false, errors.New("CAS unavailable")
	}
	if _, err := r.pendingChainRemote.CompareAndDeletePendingRemote(ctx, repo, session, expected); err != nil {
		return false, err
	}
	if current := r.pointers[session]; current != "" && current != expected {
		return false, nil
	}
	delete(r.pointers, session)
	return true, nil
}

func pendingRetrySnapshot(t *testing.T, st *storage.FileStore, repo, marker string) domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	h, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SessionOriginID: marker}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: h, DocHash: h, RepoID: repo, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPushRetriesPendingPointerAfterHelperFailure(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("pending retry")))
	target := pendingRetrySnapshot(t, st, repo, "uncommitted")
	if err := st.PutPending(ctx, domain.Pending{RepoID: repo, SessionID: "session", Provider: domain.ProviderClaude, Branch: "main", Target: target}); err != nil {
		t.Fatal(err)
	}
	remote := &retryPendingRemote{failPointer: true}
	svc := NewSyncRepoService(st, remote, nil)
	in := inbound.SyncInput{RepoID: repo}
	if _, err := svc.SyncPendings(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	if remote.attempts != 1 || len(remote.pendings) != 0 {
		t.Fatalf("helper failure not exercised: attempts=%d published=%v", remote.attempts, remote.pendings)
	}
	remote.failPointer = false
	pushCount := len(remote.pushes)
	if _, err := svc.Push(ctx, in); err != nil {
		t.Fatal(err)
	}
	if remote.attempts != 2 || len(remote.pendings) != 1 || remote.pendings[0].Target != target {
		t.Fatalf("ordinary push did not retry the durable pending pointer: attempts=%d published=%v", remote.attempts, remote.pendings)
	}
	for _, snaps := range remote.pushes[pushCount:] {
		if len(snaps) != 0 {
			t.Fatalf("pointer retry retransmitted known snapshots: %v", snaps)
		}
	}
	remaining, err := st.ListPendings(ctx, repo)
	if err != nil || len(remaining) != 1 || remaining[0].Target != target {
		t.Fatalf("uncommitted capture lost after publishing: %v err=%v", remaining, err)
	}
}

func TestPushPendingRetryPreservesConcurrentCASReplacement(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("pending retry CAS")))
	old := pendingRetrySnapshot(t, st, repo, "committed")
	newer := pendingRetrySnapshot(t, st, repo, "newer capture")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: old}
	if err := st.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	p := domain.Pending{RepoID: repo, SessionID: "session", Provider: domain.ProviderClaude, Branch: "main", Target: old}
	if err := st.PutPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	remote := &retryPendingRemote{pointers: map[string]domain.ContentHash{p.SessionID: newer}}
	remote.manifest = domain.Manifest{Refs: []domain.Ref{ref}}
	remote.onCASDelete = func(sessionID string, expected domain.ContentHash) {
		if sessionID != p.SessionID || expected != old {
			t.Fatalf("CAS attempted to delete a newer capture: session=%s target=%s", sessionID, expected)
		}
		p.Target = newer
		if err := st.PutPending(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewSyncRepoService(st, remote, nil)
	if _, err := svc.Push(ctx, inbound.SyncInput{RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	remaining, err := st.ListPendings(ctx, repo)
	if err != nil || len(remaining) != 1 || remaining[0].Target != newer {
		t.Fatalf("newer capture lost to stale CAS: %v err=%v", remaining, err)
	}
	if len(remote.pendings) != 1 || remote.pendings[0].Target != newer {
		t.Fatalf("newer unresolved pointer was not published: %v", remote.pendings)
	}
	if remote.pointers[p.SessionID] != newer {
		t.Fatalf("stale CAS deleted remote newer capture: %v", remote.pointers)
	}
}

func TestPushDefersConcurrentPendingUntilObjectsUploaded(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("late pending")))
	old := pendingRetrySnapshot(t, st, repo, "committed")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: old}
	if err := st.PutRef(ctx, ref); err != nil {
		t.Fatal(err)
	}
	p := domain.Pending{RepoID: repo, SessionID: "session", Provider: domain.ProviderClaude, Branch: "main", Target: old}
	if err := st.PutPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	remote := &retryPendingRemote{}
	remote.onCASDelete = func(_ string, _ domain.ContentHash) {
		p.Target = pendingRetrySnapshot(t, st, repo, "created after manifest")
		if err := st.PutPending(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewSyncRepoService(st, remote, nil)
	in := inbound.SyncInput{RepoID: repo}
	if _, err := svc.Push(ctx, in); err != nil {
		t.Fatal(err)
	}
	if remote.attempts != 0 {
		t.Fatalf("late pointer published before its objects: attempts=%d", remote.attempts)
	}
	if _, err := svc.Push(ctx, in); err != nil {
		t.Fatal(err)
	}
	if remote.attempts != 1 || remote.pointers[p.SessionID] != p.Target {
		t.Fatalf("next sync did not publish the late capture: %v", remote.pointers)
	}
}

func TestPushPendingUploadFailureRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("ordinary pending failure")))
	target := pendingRetrySnapshot(t, st, repo, "uncommitted")
	p := domain.Pending{RepoID: repo, SessionID: "session", Provider: domain.ProviderClaude, Branch: "main", Target: target}
	if err := st.PutPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	remote := &retryPendingRemote{failPointer: true}
	svc := NewSyncRepoService(st, remote, nil)
	in := inbound.SyncInput{RepoID: repo}
	if _, err := svc.Push(ctx, in); err == nil {
		t.Fatal("ordinary push hid its pointer upload failure")
	}
	remaining, err := st.ListPendings(ctx, repo)
	if err != nil || len(remaining) != 1 || remaining[0].Target != target {
		t.Fatalf("failed upload lost durable retry: %v err=%v", remaining, err)
	}
	remote.failPointer = false
	if _, err := svc.Push(ctx, in); err != nil {
		t.Fatal(err)
	}
	if remote.attempts != 2 || remote.pointers[p.SessionID] != target {
		t.Fatalf("ordinary retry did not publish pointer: attempts=%d pointers=%v", remote.attempts, remote.pointers)
	}
}

func TestPushDoesNotRepublishSharedPendingAfterCASFailure(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("pending CAS failure")))
	target := pendingRetrySnapshot(t, st, repo, "committed")
	if err := st.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: target}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPending(ctx, domain.Pending{RepoID: repo, SessionID: "session", Provider: domain.ProviderClaude, Branch: "main", Target: target}); err != nil {
		t.Fatal(err)
	}
	remote := &retryPendingRemote{failCAS: true}
	svc := NewSyncRepoService(st, remote, nil)
	in := inbound.SyncInput{RepoID: repo}
	if _, err := svc.Push(ctx, in); err != nil {
		t.Fatal(err)
	}
	remaining, err := st.ListPendings(ctx, repo)
	if err != nil || len(remaining) != 1 || remote.attempts != 0 {
		t.Fatalf("failed resolution lost retry or republished committed capture: %v attempts=%d err=%v", remaining, remote.attempts, err)
	}
	remote.failCAS = false
	if _, err := svc.Push(ctx, in); err != nil {
		t.Fatal(err)
	}
	remaining, err = st.ListPendings(ctx, repo)
	if err != nil || len(remaining) != 0 || remote.attempts != 0 {
		t.Fatalf("resolution retry failed: %v attempts=%d err=%v", remaining, remote.attempts, err)
	}
}

func TestPushRetriesIndependentPendingPointersAfterOneFailure(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte("independent pending retries")))
	for _, session := range []string{"first", "second"} {
		target := pendingRetrySnapshot(t, st, repo, session)
		if err := st.PutPending(ctx, domain.Pending{RepoID: repo, SessionID: session, Provider: domain.ProviderClaude, Branch: "main", Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	remote := &retryPendingRemote{failFirstPointer: true}
	if _, err := NewSyncRepoService(st, remote, nil).Push(ctx, inbound.SyncInput{RepoID: repo}); err == nil {
		t.Fatal("pointer failure was hidden")
	}
	if remote.attempts != 2 || len(remote.pendings) != 1 {
		t.Fatalf("one pointer failure blocked an independent retry: attempts=%d published=%v", remote.attempts, remote.pendings)
	}
}
