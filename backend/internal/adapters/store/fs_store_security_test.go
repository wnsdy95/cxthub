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
