package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
