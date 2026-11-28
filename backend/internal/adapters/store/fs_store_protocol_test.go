package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// A data directory written by an earlier version can hold a repository's
// context protocol in a file of its own. Nothing writes that file any more;
// what is there is still read with the repository, and a file that says
// anything but the one protocol there ever was is not trusted.
func TestFSRepositoryIsReadWithTheProtocolAnEarlierVersionLeft(t *testing.T) {
	ctx := context.Background()
	storage := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	// A client cannot set it: what is stored and answered carries none.
	if stored, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", ContextProtocol: 1}); err != nil || stored.ContextProtocol != 0 {
		t.Fatalf("storing a repository with a protocol set by the caller: %+v %v", stored, err)
	}
	// Without the file there is none.
	if got, err := storage.GetRepo(ctx, repo); err != nil || got.ContextProtocol != 0 || got.DefaultBranch != "main" {
		t.Fatalf("a repository without the file: %+v %v", got, err)
	}
	file := filepath.Join(storage.repoDir(repo), "context-protocol")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("1\n")
	if got, err := storage.GetRepo(ctx, repo); err != nil || got.ContextProtocol != 1 {
		t.Fatalf("the file an earlier version left: %+v %v", got, err)
	}
	for _, content := range []string{"", "1", "2\n", "1\n\n", " 1\n", "one\n"} {
		write(content)
		if got, err := storage.GetRepo(ctx, repo); !errors.Is(err, domain.ErrIntegrity) || got.ID != "" {
			t.Errorf("a file that says %q: %+v %v", content, got, err)
		}
	}
	// A file that cannot be read is an error, not a repository without one.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := storage.GetRepo(ctx, repo); err == nil || errors.Is(err, domain.ErrIntegrity) || got.ID != "" {
		t.Fatalf("a file that cannot be read: %+v %v", got, err)
	}
}
