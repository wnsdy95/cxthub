package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func securityHash(c string) domain.ContentHash {
	return domain.ContentHash("sha256:" + strings.Repeat(c, 64))
}

func TestFSStoreRejectsTraversalRepoID(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	st := NewFSStore(dataDir)

	err := st.PutSecretsEnvelope(ctx, domain.ContentHash("sha256:../../outside"), []byte(`{}`))
	if !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("expected ErrIntegrity, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(base, "outside")); !os.IsNotExist(statErr) {
		t.Fatalf("outside path was created or stat failed unexpectedly: %v", statErr)
	}
}

func TestFSStoreRejectsTraversalRefName(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	st := NewFSStore(dataDir)
	repo := securityHash("0")
	target := securityHash("1")

	err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
		Kind:   domain.RefBranch,
		Name:   "../../../../outside",
		RepoID: repo,
		Target: target,
	}, "")
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, "outside")); !os.IsNotExist(statErr) {
		t.Fatalf("outside path was created or stat failed unexpectedly: %v", statErr)
	}
}

func TestFSStoreAllowsSlashRefName(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := securityHash("0")
	target := securityHash("1")

	if err := st.CompareAndSwapRef(ctx, repo, domain.Ref{
		Kind:   domain.RefBranch,
		Name:   "feature/auth-flow",
		RepoID: repo,
		Target: target,
	}, ""); err != nil {
		t.Fatalf("valid slash ref rejected: %v", err)
	}
	got, err := st.GetRef(ctx, repo, domain.RefBranch, "feature/auth-flow")
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}
	if got.Target != target {
		t.Fatalf("target mismatch: got %s want %s", got.Target, target)
	}
}

func TestFSStorePointerKeysDoNotCollapse(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := securityHash("0")

	for i, sessionID := range []string{"terminal/a", "terminal?a"} {
		if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: sessionID, Target: securityHash(string(rune('1' + i)))}); err != nil {
			t.Fatalf("PutPending(%q): %v", sessionID, err)
		}
	}
	pendings, err := st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 2 {
		t.Fatalf("pending collision: len=%d err=%v", len(pendings), err)
	}
	if err := st.DeletePending(ctx, repo, "terminal/a"); err != nil {
		t.Fatal(err)
	}
	pendings, err = st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 1 || pendings[0].SessionID != "terminal?a" {
		t.Fatalf("deleting one pending affected another: %+v err=%v", pendings, err)
	}

	for i, branch := range []string{"feature/foo", "feature.foo"} {
		if err := st.PutUnsync(ctx, repo, domain.Unsync{RepoID: repo, User: "alex", Branch: branch, Target: securityHash(string(rune('3' + i)))}); err != nil {
			t.Fatalf("PutUnsync(%q): %v", branch, err)
		}
	}
	unsyncs, err := st.ListUnsyncs(ctx, repo)
	if err != nil || len(unsyncs) != 2 {
		t.Fatalf("unsync collision: len=%d err=%v", len(unsyncs), err)
	}
}

func TestFSStoreCompareAndDeletePendingUsesTargetCAS(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := securityHash("0")
	oldTarget := securityHash("1")
	newTarget := securityHash("2")
	const sessionID = "terminal/cas"

	if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: sessionID, Target: newTarget}); err != nil {
		t.Fatal(err)
	}
	result, err := st.CompareAndDeletePending(ctx, repo, sessionID, oldTarget)
	if err != nil {
		t.Fatal(err)
	}
	if result != domain.PendingDeleteKept {
		t.Fatal("stale expected target deleted a newer server pending capture")
	}
	pendings, err := st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 1 || pendings[0].Target != newTarget {
		t.Fatalf("newer pending was not preserved: %+v err=%v", pendings, err)
	}

	result, err = st.CompareAndDeletePending(ctx, repo, sessionID, newTarget)
	if err != nil || result != domain.PendingDeleteDeleted {
		t.Fatalf("matching target compare-delete: result=%v err=%v", result, err)
	}
	pendings, err = st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 0 {
		t.Fatalf("matching pending remains: %+v err=%v", pendings, err)
	}
}

func TestFSStoreReplacePendingReturnsLinearizedPredecessor(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := securityHash("f")
	const sessionID = "terminal/replace"
	initial := securityHash("0")
	if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: sessionID, Target: initial}); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	oldTargets := make(chan domain.ContentHash, writers)
	var wg sync.WaitGroup
	for i := 1; i <= writers; i++ {
		target := securityHash(string(rune('0' + i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			old, err := st.ReplacePending(ctx, repo, domain.Pending{RepoID: repo, SessionID: sessionID, Target: target})
			if err != nil {
				t.Errorf("replace %s: %v", target, err)
				return
			}
			oldTargets <- old
		}()
	}
	wg.Wait()
	close(oldTargets)

	counts := make(map[domain.ContentHash]int, writers+1)
	for old := range oldTargets {
		counts[old]++
	}
	pendings, err := st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 1 {
		t.Fatalf("final pending: %+v err=%v", pendings, err)
	}
	counts[pendings[0].Target]++
	for i := 0; i <= writers; i++ {
		target := securityHash(string(rune('0' + i)))
		if counts[target] != 1 {
			t.Fatalf("target %s occurs %d times across predecessors+tip; replacement was not linearized", target, counts[target])
		}
	}
}

func TestFSStorePendingDismissMutationPreservesConcurrentTarget(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	repo := securityHash("0")
	oldTarget := securityHash("1")
	newTarget := securityHash("2")
	const sessionID = "terminal/dismiss"

	if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: sessionID, Target: oldTarget}); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetPendingDismissed(ctx, repo, sessionID, true); err != nil || !changed {
		t.Fatalf("dismiss: changed=%v err=%v", changed, err)
	}
	// A hook capture arriving after dismiss must advance the target while the
	// server-owned hidden bit remains sticky.
	if err := st.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: sessionID, Target: newTarget}); err != nil {
		t.Fatal(err)
	}
	pendings, err := st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 1 || pendings[0].Target != newTarget || !pendings[0].Dismissed {
		t.Fatalf("dismiss/capture merge lost state: %+v err=%v", pendings, err)
	}
	if changed, err := st.SetPendingDismissed(ctx, repo, sessionID, false); err != nil || !changed {
		t.Fatalf("undismiss: changed=%v err=%v", changed, err)
	}
	pendings, err = st.ListPendings(ctx, repo)
	if err != nil || len(pendings) != 1 || pendings[0].Target != newTarget || pendings[0].Dismissed {
		t.Fatalf("undismiss rewrote capture: %+v err=%v", pendings, err)
	}
}

func TestFSStoreNegotiationUsesCancelableRegularFileInventory(t *testing.T) {
	st := NewFSStore(t.TempDir())
	repo := securityHash("0")
	doc := securityHash("1")
	path := st.docPath(repo, doc)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// Negotiation is an inventory check. Byte-level verification remains on
	// PutDoc/GetDoc/fsck and must not reread every large document on each push.
	if err := os.WriteFile(path, []byte("not a valid CIR document"), 0o644); err != nil {
		t.Fatal(err)
	}
	have, err := st.HasDocs(context.Background(), repo, []domain.ContentHash{doc})
	if err != nil || len(have) != 1 || have[0] != doc {
		t.Fatalf("inventory result = %v, %v", have, err)
	}
	if _, err := st.GetDoc(context.Background(), repo, doc); err == nil {
		t.Fatal("corrupt inventory object passed byte-level read verification")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.HasDocs(canceled, repo, []domain.ContentHash{doc}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled negotiation error = %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HasDocs(context.Background(), repo, []domain.ContentHash{doc}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("symlink inventory error = %v, want ErrIntegrity", err)
	}
}
